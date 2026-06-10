# BPSR Patrol Cams

Blue Protocol Star Resonance のパケットキャプチャ・レアモンスター検知・Discord通知・MuMu ADB自動巡回。
Windows / Go 1.23+ / CGO (Npcap)。

---

## 0. Claude への指示（最優先・常時適用）

- 曖昧な要望は実装前に質問返す。情報が揃うまで着手しない
- 思考中に方針変更・別案が浮上したら **即座に報告**、勝手に進めない
- 応答は簡潔・箇条書き・敬語不要・同僚距離感
- **触る前に下記「§3 ファイル責務マップ」「§4 不変条件」「§5 過去リグレッション罠リスト」を必ず確認**
- ncap/ pb/ mumu/ MatchLineChange / processSyncContainerData / processSyncToMeDeltaInfo を変更する場合は **必ず先にユーザー確認**、変更は **§11 実装ワークフロー** に従う
- mumu/ ncap/ の変更は実機前に **§10 sim 検証**（`run_all.ps1` / `pcap-replay`）を通す
- config/ のキー名・スキーマ変更は **後方互換性の影響を必ず指摘**
- 変更時は `changelog.txt` に追記必須（連番 No.NN・`yyyy/mm/dd hh:mm`）

---

## 1. プロジェクト概要

- パケットキャプチャ（Npcap/gopacket）で BPSR の TCP セッションを監視
- レアエネミー検知 → Discord webhook 通知
- MuMu Player 複数インスタンスを ADB で自動巡回（ch切替）
- WebView2 ベースのローカル GUI（HTTP サーバー + Edge WebView2 ウィンドウ）

---

## 2. ビルド・実行

```powershell
.\build.ps1            # release ビルド (.\release\BPSR_patrol_cams.exe)
.\build.ps1 -Debug     # コンソール付き debug ビルド
go build ./...         # 型・ビルドエラーだけ確認したい時
go vet ./...           # 静的解析

# 実機レス検証（詳細は §10）
.\run_all.ps1                                          # Level 1+1.5: 全シナリオ一括（mumu/gui 変更時必須・約8分）
.\release\patrol-sim.exe -scenario scenarios\X.json    # Level 1: 個別シナリオ
.\release\gui-sim.exe -scenario scenarios\gui\X.json   # Level 1.5: GUI×sim ハイブリッド個別
.\release\pcap-record.exe                              # Level 2: 実トラフィック録画（本体と並行起動可）
.\release\pcap-replay.exe -pcap X.pcap -golden testdata\golden.jsonl   # Level 2: ncap 回帰比較
```

要件: Go 1.23+ / MinGW-w64 GCC (PATH 通っている事) / Npcap SDK (`C:\npcap-sdk`)。

---

## 3. ファイル責務マップ

| パス | 役割 | 行数 | 触る時の慎重度 |
|---|---|---|---|
| `main.go` | 起動・配線（capture↔patroller↔gui の接続） | ~630 | 中 |
| `ncap/cap_device.go` | パケット解析・session 管理・0x15/0x2E ハンドラ | ~2288 | **最高（要確認）** |
| `ncap/portmap.go` | port→ch マッピングの永続化（JSON） | ~185 | 中（フォーマット互換注意） |
| `ncap/cap_helper.go`, `byte_reader.go`, `queue.go` | バイナリ読み出しユーティリティ | 小 | 低 |
| `pb/bp.pb.go` | protobuf 生成コード（手動編集禁止） | - | **編集禁止** |
| `mumu/mumu.go` | Patroller（巡回ロジック）・状態機械・ADB呼出 | ~2707 | **最高（要確認）** |
| `mumu/screen.go` | スクリーンショット取得・ADB画面操作 | ~297 | 中 |
| `gui/gui.go` | HTTP サーバー起動・WebView2 ウィンドウ・HTML/JS/CSS | ~1012 | 高 |
| `gui/handlers.go` | HTTP ハンドラ群（gui.go から分離） | ~1423 | 高 |
| `appconfig/config.go` | config.json Load/Save・defaults | ~494 | **高（Load/Save 非対称の罠あり）** |
| `appconfig/filter.go` | filter.json のロード・保存 | ~61 | 低 |
| `global/cache.go` | グローバルキャッシュ（モンスター名等）の読み書き | ~52 | 低 |
| `global/monster_names.go` | MonsterNames マップ変数の宣言 | ~3 | 低 |
| `location/store.go` | 場所名マスタ・最近傍検索（locations.json ラッパー） | ~77 | 低 |
| `notifier/discord.go` | Discord webhook 送信 | ~186 | 低 |
| `debuglog/debuglog.go` | Verbose/Dedup ログ | ~216 | 低 |
| `cmd/chat-reporter/main.go` | 別バイナリ（chat-reporter.exe） | ~329 | 中（main.go と設定同期必須） |
| `config/*.json`, `config/channels.txt` | ランタイム設定（後方互換必須）。`gold_history.json` を含む | - | 高 |
| `data/locations.json` | 場所名マスタ（location/store.go が読む） | - | 低 |
| `ncap/replay.go` | pcap ファイルを実パーサに流す `ReplayFile`（cap_device.go 無変更の追加ファイル） | ~40 | 中 |
| `sim/` (server, gameserver, scenario, events, assert, harness) | Level 1 sim 基盤（SimServer・疑似ゲームサーバ・シナリオ・イベント注入・アサーション・ハーネス共通部品） | ~1300 | 中（本番コードに非依存方向のみ） |
| `cmd/patrol-sim/main.go` | sim ハーネス（本番 Patroller を fake-adb で起動） | ~280 | 中 |
| `cmd/gui-sim/main.go`, `actions.go` | Level 1.5 ハーネス（本番 gui.Server + Patroller を HTTP API で駆動） | ~850 | 中（main.go の guiServer 配線と同期必須） |
| `cmd/fake-adb/main.go` | 偽 adb.exe（PATROL_SIM_ADDR 必須・未設定なら即 exit 1） | ~150 | 低 |
| `cmd/pcap-record/main.go` | 実トラフィック .pcap 録画（本体と並行起動可） | ~139 | 低 |
| `cmd/pcap-replay/main.go` | .pcap 再生 + golden 記録/比較（順序非依存マルチセット） | ~360 | 中 |
| `scenarios/*.json` | Level 1 シナリオ定義 7本（`scenarios/gui/` は gui-sim 用） | - | 低 |
| `run_all.ps1` | Level 1 全シナリオ一括実行（FAIL で exit 1） | - | 低 |
| `docs/plan_sim.md`, `docs/plan_sim_l2.md` | sim 基盤の仕様書 | - | 低 |

---

## 4. 不変条件（破ると即リグレッション）

### 4-1. パケット解析（ncap/cap_device.go）

1. **session 重複除去キー**: `clientIP` ではなく `uid:<userUID>` または `ep:<endpoint>` を使う。MuMu NAT 環境で全インスタンスの clientIP が同一になるため
2. **`sess.userUID` の中身は UID (= rawUUID>>16)** であって raw UUID ではない。serialToUID マップとの整合のため（No.27）
3. **`onLineIDObserved` の発火条件**:
   - `lineID` が実際に変化した時（`oldCh != lineID`）
   - **OR** `charId` が 0→N に新規確定した時（`uidNewlySet=true`）
   - 毎パケット発火は禁止（誤バインドの原因・No.33）
4. **`0x15 SyncContainerData` の SceneData なしパス**: ゲームバージョンアップで SceneData が消えることがある。`sd == nil` でも `uidNewlySet && sess.lineID != 0` なら `onLineIDObserved` を発火する（No.23, No.34）
5. **`0x2E SyncToMeDeltaInfo` を ch 移動完了シグナルに使えない**: ch切替時はプレイヤーエンティティ保持でサーバが座標更新を送らないため `attr id==53` が一切来ない（No.31）
6. **fast-path (S→C 方向) でセッション新規作成時**: `handleClientToServer` を通らないので `tryPortMapLineID` を明示呼出が必要。さもないと `sess.lineID=0` のまま（No.23）
7. **`processSyncContainerData` の早期 return**: SceneData nil パスに早期 return を追加してはいけない（No.33 で追加 → No.34 でリグレッション）
8. **チャット dedup キー**: `clientIP` 単独ではなく `label+clientIP` を使う（同 IP 別インスタンス対応・No.24）

### 4-2. 巡回（mumu/mumu.go）

1. **`RecordPatrolMove` は SwitchGroup の前に呼ぶ**: ADB コマンド発行前に pendingProbes を登録する。さもないとサーバ側の新 TCP セッションが先行して `MatchLineChange` 時点で pending probe が空になる（No.35）。巡回本ループも No.56 で SwitchGroup 前（switchStartAt 時点）に統一済み
2. **`probeWindow` は 60 秒**: 5台直列切替で30s以上かかる（No.23）
3. **`MatchLineChange` の Probe マッチ条件**: `changedAt >= probe.sentAt - 2s` を必ずチェック。ADB 発行前の lineID 変化（本物クライアントの操作等）を拒否（No.33）
4. **`MatchLineChange` の二重チェック**: RLock で未バインド確認後に WriteLock 取得しても、ロック取得後に `serialToUID[bindSerial] != 0` を再チェックして early return（並走ゴルーチン対策・No.26）
5. **`moveSignal` の時刻フィルタは `switchStartAt` 基準**: `switchDoneAt` 基準にしない。ADB done がゲームサーバ応答より遅れることがある（No.30）
6. **完了シグナルは `0x2E UUID 受信 + 遅延`**: `NotifyPostLoadReady` → `notifyMoveSignal` 経路。遅延は自動（直近観測ベース）または手動（`PatrolLoadStabilizationSecs`）。lineID-change では発火しない（No.39）
7. **既到達ch（`CurrentCh == 目的ch`）のデバイス**: `switchTargets` から除外、ただし `RecordPatrolMove` は呼ぶ（ExpectedCh 更新のため）、`need` も除外後の件数（No.29）
8. **`labelToSerial` は `serialToLabel` の逆引きで常に同期更新**: `notifyMoveSignal` が serial で送るため（No.30）
9. **`excludeUIDs`**: 本物クライアントの UID を probe マッチから除外する。永続化（config.json `exclude_uids`）（No.33）
10. **`MatchLineChange` は `notifyMoveSignal` を発火しない**: 完了シグナルは `NotifyPostLoadReady`（0x2E UUID 受信時）が担当。MatchLineChange は ActualCh 更新のみ（No.39）
11. **`moveSignalMsg` は `lineID` を必須携帯**: wait loop（4箇所: buffered drain / フェーズ1 / フェーズ2 / 発行前追加待ち）は `msg.lineID == 0 || msg.lineID == targetCh` かつ `!respondedSet[msg.label]` を `got++` 前にガード。stale lineID 流用と同一 serial 重複カウントを防止（No.44）
12. **`NotifyPostLoadReady` は同 lineID 内 1 回のみ発火**: session に `postLoadFiredForLineID` を保持し、lineID 変化時にのみ再発火可能。戦闘中などで 0x2E が連射されても channel をスパムしない（No.44）

### 4-3. 設定（appconfig/config.go）

1. **`Load` は `defaultConfig` 起点**: 欠損キーはデフォルト値で埋まる
2. **`SaveWindowState` は JSON マップとして部分更新**: zero-value `Config` を base に unmarshal → 全フィールド書き戻しは **禁止**。デフォルト `true` な bool フィールドが旧 config.json で false に上書きされる（No.31）
3. **デフォルト `true` の bool フィールド**: `gas_enable`, `patrol_adaptive_timeout`, `patrol_load_stabilization_auto`, `show_no_device_dialog` 等は **キー欠損 → false 暗黙上書きの罠** がある
4. **キー名・型変更時の後方互換**:
   - リネームは必ず旧キー読み込みフォールバック付き
   - 例: `full_threshold → move_fail_threshold` の時は旧キーも読む or マイグレーション
   - `port_ch_map.json` のフォーマット変更（No.15）は旧形式自動検出→新形式移行で実装

### 4-4. GUI（gui/gui.go）

1. **`extractInstanceLabel`** は Go 標準ロガーのタイムスタンプ付き行に対応必須。`line[0]=='['` を前提にしない（`strings.Index(line, "[Instance-")` を使う・No.12）
2. **`guiWriter.Write` の `NotifyChMovePacket` 呼び出しに `!HasBinding()` ガードを付けてはいけない**（No.12 で削除済み、再導入禁止）
3. **チャット SSE dedup キー**: JS 側 `dedupeChatEvents()` のキーは `ch + sender + msg`（label/IP を含めない）。同一内容を 1 件に集約し `recv_count` バッジで台数表示（No.48）。label を含めると label 確定後に重複するため除外した。

### 4-5. 別バイナリ（cmd/chat-reporter）

- `main.go` と GAS 設定（`SetGASEnable` / `SetGASTargetEnemy`）の起動時呼出を必ず両方に入れる。chat-reporter 側で抜けると UI ON でも内部 OFF になる（No.31）

---

## 5. 過去リグレッション罠リスト（特に注意）

| 発生 | 罠 | 起源 | 修正 |
|---|---|---|---|
| 1日に5連発 | 「修正の副作用で次のバグ」連鎖 | No.30→31→33→34→35 | 状態機械の変更は **必ず changelog で過去事例検索してから** |
| `onLineIDObserved` 過剰発火 | 本物クライアントの誤バインド | No.33 直前 | lineID 変化時のみ発火 |
| 早期 return 追加 | 別の正常パスを潰す | No.33→No.34 | 状態遷移を表化して全パス確認 |
| `RecordPatrolMove` タイミング | サーバ応答が ADB done より早い | No.35 | SwitchGroup の前に呼ぶ |
| Load/Save 非対称 | bool デフォルト値が消える | No.31 | JSON マップで部分更新 |
| ガード追加 | 正常経路を遮断 | No.12 (`!HasBinding()`) | 二重カウント防止は別レイヤで担保 |
| NAT 環境 | 全インスタンス同 IP | No.09, 24, 25 | 識別キーから clientIP を外す |

---

## 6. 変更時のチェックリスト

ncap/ または mumu/ を変更する場合、コミット前に以下を確認:

- [ ] `go build ./...` がエラーなく通る
- [ ] `go vet ./...` で警告ゼロ
- [ ] 変更したハンドラの全呼び出しパスを確認した（grep で全 caller 確認）
- [ ] **§4 不変条件** を新規に破っていないか確認
- [ ] **§5 過去罠リスト** で類似ケースが過去にないか changelog 検索
- [ ] **mumu/ 変更時**: `.\run_all.ps1` 全7シナリオ PASS（§10。FAIL したらまず1回再実行 = flake 切り分け）
- [ ] **ncap/ 変更時**: golden があれば `pcap-replay -golden` で回帰比較（§10）
- [ ] `changelog.txt` に追記（背景・原因・変更・効果・影響範囲を含む）
- [ ] 既存 config.json が無変更で動作するか（後方互換）

GUI のみの変更:
- [ ] HTML/JS/CSS のみで Go コードに影響なし
- [ ] `btn-*` 系の DOM 参照は null 安全
- [ ] SSE イベントの dedup キーに label を含む

---

## 7. changelog テンプレート

```
====--------------------------------------------------------------------------------
[No.NN] yyyy/mm/dd hh:mm — 一行サマリ

【背景 / 原因】
  …

【変更内容】
  ファイル名:
    - 変更点1
    - 変更点2

【効果】
  …

【影響範囲】
  - 変更ファイル列挙
  - 後方互換性: …

```

連番は最新 No.NN の続き。日時はコミット日時。

---

## 8. デバッグ用 grep パターン

```powershell
# 巡回状態の追跡
Select-String -Path logs\log.txt -Pattern "MuMu|Patrol|0x2E|0x15|巡回|移動"

# バインド状態
Select-String -Path logs\log.txt -Pattern "Probe|0x2E|0x15|PortMap|lineID|onLine|バインド|識別"

# チャット
Select-String -Path logs\log.txt -Pattern "\[CHAT-"
```

---

## 9. プロジェクト専用 subagent

`.claude/agents/` 配下に以下を配置（変更レビュー専用）:

- **packet-analyst**: ncap/ pb/ の変更を §4-1 不変条件と照合
- **patrol-flow-reviewer**: mumu/ の状態遷移変更を §4-2 と照合
- **config-compat-checker**: appconfig/ config/ の変更を §4-3 と照合し後方互換性を検証

該当ファイルを変更する時は **必ず該当 subagent でレビュー** してからコミット。
レビュー agent は静的照合のみ。動的検証（run_all.ps1 / pcap-replay）は主セッションが §10 に従って実行する。

---

## 10. 実機レス検証フロー（sim 基盤）

実機検証は最終確認のみ。日常の検証は以下のレベルで行う（仕様書: `docs/plan_sim.md` / `docs/plan_sim_l2.md` / `docs/plan_gui_sim.md`）。
skill 経由でも実行できる: `/sim [シナリオ名]`（Level 1・flake 自動再実行付き）、`/replay-check [pcap] [golden]`（Level 2）。

### Level 1: patrol-sim（mumu 層・実機/実ゲーム不要）

本番 `mumu.Patroller` を無変更のまま起動し、`cfg.ADBPath` を fake-adb.exe に差し替え、
`NotifyLineIDChange` / `NotifyPostLoadReady` を直接呼んで疑似パケットシグナルを注入する。

```powershell
.\run_all.ps1                                          # 全7シナリオ（約7分・ログは logs\sim\<name>.log）
.\release\patrol-sim.exe -scenario scenarios\X.json    # 個別（-v で verbose）
```

| シナリオ | 検証内容 |
|---|---|
| baseline | 正常巡回・dwell 実測 1.8〜3.5s |
| native_move / native_move_same_ch | ネイティブクライアント干渉（別ch / 偶然同ch）を無視できるか |
| burst_0x2e | 0x2E 10連射で moveSignal がスパムしないか（§4-2.12） |
| silent_one | 1台無応答時にタイムアウト処理が正しく動くか |
| slow_signal | 9s 遅延シグナルでも巡回が破綻しないか |
| screen_mode | load_detect_mode=screen の画面判定パス |

### Level 1.5: gui-sim（GUI API 層・実機/実ゲーム不要）

本番 `gui.Server`（内部に本番 Patroller を所有）を WebView2 なしで起動し、ボタン押下を HTTP API として
自動実行する。シグナル注入は `guiServer.Notify*` 経由。run_all.ps1 に内包（`scenarios\gui\*.json`）。

```powershell
.\release\gui-sim.exe -scenario scenarios\gui\baseline.json   # 個別実行（-v で verbose）
```

| シナリオ | 検証内容 |
|---|---|
| baseline (gui_baseline) | 開始/停止ボタン → 巡回 → status 反映 + 全 GET API スイープ + config GET→POST→GET 同一性（No.31 型の動的検出） |

- gui_actions の `config_patch` は「全量 GET → マージ → 全量 POST」で GUI JS の実挙動を模倣する。部分 JSON の直接 POST は禁止（zero-value unmarshal で欠損キーが false/0 保存されるため）
- main.go へ guiServer 配線を追加した時は cmd/gui-sim の配線への要否を判断する（§4-5 と同種の同期義務）

### Level 2: pcap-record / pcap-replay（ncap 層）

```powershell
.\release\pcap-record.exe                                                # 録画（本体と並行可・Ctrl+C 終了）
.\release\pcap-replay.exe -pcap logs\capture_X.pcap                      # パース層生存確認
.\release\pcap-replay.exe -pcap X.pcap -record-golden testdata\g.jsonl   # golden 記録
.\release\pcap-replay.exe -pcap X.pcap -golden testdata\g.jsonl          # 回帰比較
```

- replay 出力の「**lineid 0件 / UID確定 0件**」警告 = No.23/34 型のパース層破壊の兆候
- golden は**順序非依存マルチセット比較**（ncap のコールバックは go ディスパッチで順序非決定のため）。重複・欠落は件数差で検出される
- **ゲームアップデート後の手順**: ①新版で録画（ログイン+ch切替+チャット 5-10分）→ ② replay で生存確認 → ③ golden 更新

### 既知の注意点

- ビルド直後の初回 sim 実行で flake を観測済み（AV スキャン疑い）。**FAIL したらまず再実行**
- `mumu.go` は Phase=dwell_wait 設定後に extraWait（最大 MoveTimeout）が走るため、シグナル遅延時は dwell 実測が膨らむ（sim の計測特性・本番バグではない）。検知用に forbid「発行前追加待ちタイムアウト」をシナリオに入れてある
- `load_detect_mode` の有効値は `screen` / `either` / それ以外=time（`packet` は無効値で time 扱い）

---

## 11. 実装ワークフロー（危険領域の変更手順）

ncap/ mumu/ appconfig/ 等の危険領域を変更する時の標準フロー（1 Phase = 1 commit = changelog 1 エントリ）:

1. **diff 仕様書を作成**（`docs/plan_*.md`）→ ユーザー承認を得る
2. **実装 subagent（sonnet）** に仕様書を渡して実装させる（主セッションは直接編集しない）
3. **専用レビュー agent**（§9）でレビュー → BLOCKING があれば修正して再レビュー
4. **主セッション自身が diff を仕様書と照合**（subagent の逸脱検出）
5. **動的検証**: mumu→`run_all.ps1` / ncap→`pcap-replay`（§10）+ `go build ./...` + `go vet ./...`
6. **commit**: changelog No.NN 追記 → §6 チェックリスト確認 → コミット

軽微な変更（GUI のみ・docs・typo）はこのフローを省略してよいが、§6 チェックリストは常に適用。
