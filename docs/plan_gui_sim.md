# gui-sim: GUI×sim ハイブリッド検証 実装仕様書

目的: Level 1 sim 基盤（patrol-sim）に本番 `gui.Server` を載せ、
「ボタン押下（= HTTP API）→ 本番ハンドラ → 本番 Patroller → fake-adb → 画面反映」の
end-to-end を実機なしで自動検証する。設定変更の反映（POST /api/config → UpdatePatrollerCfg）も対象。

## 結論（フィージビリティ調査結果）

**mumu/ ncap/ appconfig/ を一切変更せず構築可能。gui/gui.go への変更は export ラッパー 1 関数のみ。**

根拠:
- `gui.New` は内部で `mumu.NewPatroller(mumuCfg)` を生成・所有する（gui.go:169）
  → gui-sim は Patroller を直接作らず、gui.Server 経由で全操作できる
- `guiServer.NotifyLineIDChange` / `NotifyPostLoadReady` は内部 Patroller への転送メソッド（gui.go:388, 396）
  → 既存 `sim.SignalInjector` の関数ポインタを gui.Server のメソッドに差し替えるだけ
- `startHTTP(ctx)` は WebView2 と独立しており、listen 後の実 URL を返す（gui.go:769, 857）
  → port=0（ephemeral）でそのまま動く。export ラッパーを 1 つ追加するだけ
- `handlePatrolStart` は POST ボディ（serials/channels/loop_mode）で `patroller.Start` を呼ぶ（handlers.go:552-590)
  → 「巡回開始ボタン」を本物の HTTP 経路で押せる
- `handleConfig` POST は saveConfigFn → getConfigFn 再読込 → `UpdatePatrollerCfg` 即時反映（handlers.go:436-537）
  → SetConfigFns を一時ファイル向け appconfig.Load/Save にすれば本物の保存・反映経路を検証できる
- `gold_history.json` のパスは patrolChannelsFile と同ディレクトリ由来（gui.go:159-162）
  → channels.txt を一時ディレクトリに置けば gold_history も自動的に隔離される

## アーキテクチャ

```
┌── gui-sim.exe（ハーネス・1プロセス）────────────────────────────┐
│                                                                │
│  実 gui.Server（本番コードそのまま）                             │
│     ├─ 実 mumu.Patroller（gui.New が内部生成）                  │
│     │     cfg.ADBPath = fake-adb.exe ──exec──┐                 │
│     └─ HTTP 127.0.0.1:ephemeral（WebView2なし）▼                │
│              ▲                    simサーバー ◄── fake-adb.exe  │
│              │                        │                        │
│  アクションドライバ                    │                         │
│     - シナリオの gui_actions を実行    │                         │
│       （POST /api/patrol/start 等     │                         │
│        = ボタン押下の自動化）          │                          │
│     - sweep_readonly: 全GET API疎通   │                         │
│                                       │                        │
│  ゲームサーバーシミュレータ + EventEngine（既存・無変更）          │
│     - ENTER 検知 → guiServer.NotifyLineIDChange /              │
│       NotifyPostLoadReady を注入（Patroller直接ではなくGUI経由）  │
│                                                                │
│  監視ループ: GET /api/patrol/status を100msポーリング            │
│     → PhaseTracker / EventEngine.Update（既存と同じ終了条件）    │
│                                                                │
│  アサーション: 既存 sim.Evaluate + APIアクション期待値検証        │
│     （ActualCh は GET /api/patrol/device-statuses から取得）     │
│                                                                │
│  一時ディレクトリ（プロセス毎に隔離・終了時削除）                  │
│     config.json / channels.txt / gold_history.json             │
└────────────────────────────────────────────────────────────────┘
```

patrol-sim との差分: Patroller 直接操作（p.Start/p.Status）を HTTP API 経由に置き換え、
シグナル注入先を gui.Server の転送メソッドに変更。sim パッケージの部品はすべて流用。

## 検証できる範囲 / できない範囲

| 対象 | gui-sim | 備考 |
|---|---|---|
| 巡回開始/停止ボタン → Patroller 動作 → status 反映 | ✅ | POST start → phase 遷移 → POST stop → running=false |
| 全 GET 系 API の疎通（ボタン裏の読み出し全般） | ✅ | sweep_readonly（SSE・screenshot は除外、下記） |
| POST /api/config → 保存 → UpdatePatrollerCfg 即時反映 | ✅ | dwell 変更が反映ログ + 後続巡回に効くか |
| config GET→POST→GET の同一性（No.31 型 Load/Save 非対称の動的検出） | ✅ | config_identity_check アクション |
| イベント注入（native_move 等）中のボタン操作との競合 | ✅ | Phase B シナリオ |
| recover / clear-move-failed / remove-ch 等の巡回系ボタン | ✅ | fake-adb で吸収 |
| SSE (/events, /api/chat-events) のストリーム内容 | ❌ | long-poll のため sweep 対象外。Phase 外（将来検討） |
| /api/patrol/screenshot | ❌ | fake-adb の screencap 応答形式が PNG 変換経路と未整合。Phase 外 |
| /api/webhook/test（Discord 実送信） | ❌ | 意図的に対象外。sweep は GET のみなので誤爆しない |
| WebView2 ウィンドウ・JS/DOM 層 | ❌ | Layer B（Playwright 等）として別途。本仕様の対象外 |
| ncap 由来イベント（チャット・portmap pending 生成） | △ | エンドポイント疎通のみ（空データ応答の確認） |

## コンポーネント仕様

### 1. gui/gui.go（変更・~10行追加のみ）

```go
// StartServer はHTTPサーバーのみを起動する（WebView2なし・sim/テスト用）。
// 戻り値は実際の listen URL（例 "http://127.0.0.1:54321"）。
func (s *Server) StartServer(ctx context.Context) (string, error) {
	return s.startHTTP(ctx)
}
```

- 既存コードの変更なし（追加のみ）。RunWindow・startHTTP は不変
- 注意: startHTTP は patrolEnabled 時に 2 秒後 ListDevices を 1 回走らせる（gui.go:834-855）。
  fake-adb が `devices` に応答できなくても warning ログのみで無害（forbid パターンに含めない）

### 2. sim/harness.go（新規・~80行）— patrol-sim との共通部品集約

cmd/patrol-sim/main.go から以下を sim パッケージへ移動（乖離防止・chat-reporter No.31 の前例に倣う）:

- `BuildMumuConfig(scenario *Scenario, fakeADBPath string) mumu.Config`（buildMumuConfig を export 移動・ロジック無変更）
- `ResolveFakeADB(flagValue string) string`（resolveFakeADB を export 移動）
- `IsExecutable(path string) bool`（isExecutable を export 移動）

cmd/patrol-sim/main.go は呼び出しを `sim.BuildMumuConfig` 等へ置換（挙動変更ゼロ）。

### 3. sim/scenario.go（変更・additive のみ）

`Scenario` に `GUIActions []GUIActionDef` を追加（JSON キー `gui_actions`・省略時 nil = 従来挙動）:

```go
type GUIActionDef struct {
	AtPhase   string  `json:"at_phase"`   // フェーズ名で発火（EventDef と同じ意味論）
	AtCycle   int     `json:"at_cycle"`   // N 巡目（省略時=1）
	AfterSecs float64 `json:"after_secs"` // at_phase 省略時: 開始からの経過秒で発火
	Type      string  `json:"type"`       // "api" | "sweep_readonly" | "config_patch" | "config_identity_check"
	Method    string  `json:"method"`     // api 用: "GET"|"POST"
	Path      string  `json:"path"`       // api 用: "/api/..."
	Body      json.RawMessage `json:"body"`  // api 用 POST ボディ
	Patch     json.RawMessage `json:"patch"` // config_patch 用: マージするキー群
	ExpectStatus       int      `json:"expect_status"`        // 省略時=200
	ExpectBodyContains []string `json:"expect_body_contains"` // レスポンスボディ部分一致（任意）
}
```

アクション型の意味論（実装は cmd/gui-sim 側）:

- **api**: Method/Path/Body をそのまま送信し、ステータスとボディを期待値照合
- **sweep_readonly**: ハーネス組込テーブルの GET 系全エンドポイントを順に叩き、全件期待ステータス一致を確認
- **config_patch**: GET /api/config → 取得 JSON に Patch のキーをマージ → POST /api/config。
  GUI JS の実挙動（全量GET→編集→全量POST）を模倣する。**部分 JSON を直接 POST してはいけない**
  （saveConfigFn は zero-value Config に unmarshal するため、欠損キーが false/0 で保存される =
  これは GUI が全量 POST する前提の仕様であり、模倣しないとテストが偽陽性で落ちる）
- **config_identity_check**: GET → そのまま POST → 再 GET → 正規化 JSON 比較で完全一致を確認。
  Load/Save 非対称（No.31 型）が混入すると、ここでデフォルト値の脱落が差分として検出される

### 4. cmd/gui-sim/main.go（新規・~450行）

patrol-sim main の流れを踏襲。差分のみ記す:

1. 一時ディレクトリ作成（`os.MkdirTemp` → defer 削除）:
   - `channels.txt`（空。Start 時の channelsFile 引数用）
   - `config.json`: 最小シード `{"adb_path": "<fakeADB絶対パス>", "mumu_serials": [...], "gui_port": 0,
     "patrol_dwell_secs": <scenario値>, ...}` を書き込み → 以後 appconfig.Load が欠損キーをデフォルトで補完
2. `gui.New(0, sim.BuildMumuConfig(scenario, fakeADBPath), scenario.Channels, tmpDir+"\channels.txt")`
   - port=0 → ephemeral。gold_history.json も tmpDir 側に隔離される
3. 配線（main.go の配線の subset。**ncap 依存のものは配線しない** → 該当ハンドラは nil ガードで
   503/空応答を返すため、sweep はそれを期待値とする）:
   - `SetConfigFns(appconfig.Load(tmpConfigPath)→Marshal, Unmarshal→appconfig.Save(tmpConfigPath))`
     （main.go:443-477 と同形。capDevice 反映部分は存在しないため省略）
   - `LoadSerialUIDMap` / `LoadSerialLabelMap`: シナリオ devices から（patrol-sim と同じ）
   - `SetSaveSerialUIDFn` / `SetSaveSerialLabelFn`: tmpConfig へ書き戻し（main.go:329-353 と同形）
   - `SetExcludeUIDs(nil)` / `SetShowNoDeviceDialog(false)`
   - `log.SetOutput(io.MultiWriter(os.Stderr, capture))`（LogCapture は guiServer.LogWriter を**通さない**。
     SSE 配信は検証対象外のため）
4. `SignalInjector{NotifyLineIDChange: guiServer.NotifyLineIDChange, NotifyPostLoadReady: guiServer.NotifyPostLoadReady}`
   → GameServer / EventEngine は既存のまま
5. `baseURL, err := guiServer.StartServer(ctx)`
6. **巡回開始はアクションとして実行**: 組込で `POST /api/patrol/start`
   `{"serials": [...], "channels": [...], "loop_mode": true}` を送信（= 開始ボタン押下）。
   レスポンス `{"ok": true}` を確認
7. 監視ループ: `GET /api/patrol/status` を 100ms ポーリング → JSON `{phase, current_channel, running}` を
   デコードして PhaseTracker / EventEngine.Update へ（runMonitor と同じ終了条件: targetDwells 到達 or
   running=false or 5分タイムアウト）
8. アクションドライバ: 監視ループ内で gui_actions の発火条件（at_phase+at_cycle / after_secs）を判定して
   順次実行。結果（PASS/FAIL と詳細）を記録
9. 終了処理: `POST /api/patrol/stop` → 1 秒以内に `GET /api/patrol/status` で `running=false` を確認
   （= 停止ボタンの動作検証を全シナリオに組込）
10. アサーション: 既存 `sim.Evaluate`（ログ・dwell・ActualCh）+ 全アクション期待値 PASS。
    `getActualCh` は `GET /api/patrol/device-statuses` の JSON から取得
11. 終了コード: 既存と同じ（PASS=0 / FAIL・TIMEOUT=1）

#### sweep_readonly 組込テーブル

| Path | 期待 | 備考 |
|---|---|---|
| `/` , `/spawn-log` , `/chat-log` | 200 | HTML ページ |
| `/api/devices`, `/api/device-map`, `/api/logs` | 200 | fake-adb 応答 |
| `/api/patrol/status`, `/api/patrol/device-statuses`, `/api/patrol/channels` | 200 | |
| `/api/config` | 200 | tmpConfig |
| `/api/gold-history` | 200 | 空履歴 |
| `/api/adb/tap-loop/status` | 200 | |
| `/api/chat-log` | 200 | 空 |
| `/api/portmap/pending`, `/api/portmap/entries` | 200 | 未配線 → 空応答（ハンドラの nil 安全確認を兼ねる） |
| `/api/devices/identified`, `/api/devices/memory` | 200 | |
| 除外: `/events`, `/api/chat-events`（SSE）、`/api/patrol/screenshot`（fake-adb 未対応） | - | |

未配線ハンドラが nil デリファレンスで panic しないことの確認が sweep の隠れた価値
（= 本番で ncap 初期化失敗時の GUI 挙動の代理テスト）。
実装時に全 GET エンドポイントの実応答を確認し、テーブルの期待値を確定させる
（上記と異なる場合は仕様書ではなく実挙動を正とし、テーブルを修正してよい）。

### 5. scenarios/gui/*.json（新規ディレクトリ）

既存 run_all.ps1 は `scenarios` 直下のみ glob するため、サブディレクトリは patrol-sim の実行対象にならない（互換維持）。

**Phase A: baseline.json**（run_all 上の表示名は `gui_baseline`。ログ名衝突回避のため gui/ 配下はプレフィックスなしで命名する）
- 3台 × 3ch × 2周（baseline.json と同等のサーバーパラメータ）
- gui_actions:
  - `{type: "sweep_readonly", at_phase: "dwell_wait", at_cycle: 1}`
  - `{type: "config_identity_check", at_phase: "dwell_wait", at_cycle: 1}`
- assert: baseline 同等（forbid: 既存セット / dwell 1.8〜3.5s / all_devices_actual_ch_follows）

**Phase B: config_reflect.json**（実装済み・実測反映）
- 3台 × 3ch × 2周、初期 dwell=2s
- gui_actions:
  - `{type: "config_patch", at_phase: "dwell_wait", at_cycle: 1, patch: {"patrol_dwell_secs": 4}}`
  - `{type: "api", method: "GET", path: "/api/config", at_phase: "dwell_wait", at_cycle: 4,
     expect_body_contains: ["\"patrol_dwell_secs\":4"]}`（保存値の永続確認。at_cycle=4 = 2周目最初の dwell。
     マーシャル出力はスペースなしの `"key":value`）
- assert:
  - require_log_patterns: `["巡回設定を即時反映: 滞在=4s"]`（handleConfig の反映ログ・handlers.go:528）
  - dwell_phase_secs: min 1.8 / max 6.5
    （gui-sim の dwell 実測は HTTP ポーリング経由のため patrol-sim より ~1.2s 膨らむ。
     実測例: `[3.30 5.30 5.20 5.20 5.30]` — patch 前 2s 設定→3.3s、patch 後 4s 設定→5.2〜5.3s。
     反映の本体検証は require_log + GET 永続確認が担い、dwell 範囲は暴走ガード）

**Phase B: interference_buttons.json**（実装済み・実測反映）
- native_move イベント（既存 EventDef・uid=430314 to_ch=7）を 1 周目 wait_0x2e で注入しつつ、
  gui_actions で `/api/patrol/device-statuses` 取得（同 wait_0x2e）・`/api/patrol/clear-move-failed` POST
  （dwell_wait 2回目）・`/api/patrol/status` running=true 確認（dwell_wait 3回目）を実行
- assert: forbid は `移動失敗（完了シグナルなし）` を使う。**`移動失敗` 単独は禁止** —
  clear-move-failed ボタン自身のログ「移動失敗チャンネルリストをクリアしました」に部分一致して偽陽性になる
  （実装時に実際に発生・修正済み）
- 目的: 実機で再現困難な「外乱とボタン操作の同時発生」の決定論的再現

### 6. run_all.ps1（変更）

- ビルド節に `go build -o release\gui-sim.exe ./cmd/gui-sim` を追加
- シナリオ実行節の後に第 2 ループ追加: `scenarios\gui\*.json` を `gui-sim.exe -scenario` で実行
  （ログは `logs\sim\gui_<name>.log`・結果一覧と exit code 判定は既存ロジックに合流）
- gui ディレクトリが空/不存在ならスキップ（後方互換）

## 実装フェーズ（1 Phase = 1 commit = changelog 1 エントリ）

| Phase | 内容 | changelog |
|---|---|---|
| A | StartServer export + sim/harness.go 共通化 + scenario.go 拡張 + cmd/gui-sim + scenarios/gui/baseline.json + run_all.ps1 統合 | No.64 |
| B | scenarios/gui/config_reflect.json + interference_buttons.json（+ A で不足が出た場合のドライバ修正） | No.65 |

Phase A の時点で「全 GET API 疎通 + 開始/停止ボタン + config 同一性」が回る。
Phase B で「設定変更の動的反映」「外乱×ボタン競合」を追加。

## 検証手順（各 Phase 共通）

1. `go build ./...` / `go vet ./...`
2. `.\run_all.ps1` — **既存 7 シナリオ PASS 維持**（patrol-sim 共通化のデグレ検出）+ 新規 gui シナリオ PASS
3. flake 時はまず 1 回再実行（§10 既知事項）
4. レビュー: mumu/ ncap/ appconfig/ は無変更のため専用 subagent レビューは対象外。
   gui/gui.go の追加分（StartServer）と patrol-sim の共通化置換は主セッションが diff 照合
5. 本体動作確認: `.\build.ps1 -Debug` でビルドが通ること（gui.go 変更が本体に影響しないこと）

## リスクと対策

| リスク | 対策 |
|---|---|
| main.go と gui-sim の配線乖離（chat-reporter と同種の二重管理） | 配線 subset を本仕様書に明記。main.go へ配線追加時は gui-sim への要否判断を §4-5 同様に意識（changelog に記載） |
| config_patch が adb_path を実 adb に戻して fake-adb が外れる | config_patch は GET 結果（adb_path=fakeADB 済）へのマージなので構造的に安全。raw POST の "api" 型で config を触るのは禁止（仕様で明記） |
| sweep の期待値が実装と食い違う | 実装時に実応答で確定（上記テーブルは初期仮説）。確定値はシナリオではなくハーネス組込テーブルに持つ |
| run_all 所要時間の増加 | Phase A+B で +3 シナリオ ≈ +3分。許容範囲（計~10分）。短縮が必要なら -gui スイッチで分離可能（将来） |
| ephemeral port の競合・firewall ダイアログ | 127.0.0.1 bind のみ（startHTTP 既存実装）なので発生しない |
