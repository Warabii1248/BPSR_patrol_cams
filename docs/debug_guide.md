# デバッグ／ビルド／シミュレーション 説明書

BPSR Patrol Cams の開発・検証で使うビルド方法、デバッグ手段、実機レス検証（sim）基盤の
システム構成と手動実行コマンドをまとめる。日常運用の一次情報は `CLAUDE.md §2 / §10 / §11`、
仕様詳細は `docs/plan_sim.md` / `docs/plan_sim_l2.md` / `docs/plan_gui_sim.md`。

---

## 1. バイナリ構成（全体像）

| バイナリ | ソース | 役割 | 区分 |
|---|---|---|---|
| `BPSR_patrol_cams.exe` | `main.go`（ルート） | 本体（capture + patroller + GUI） | 製品 |
| `chat-reporter.exe` | `cmd/chat-reporter` | チャット監視専用の別バイナリ | 製品 |
| `patrol-sim.exe` | `cmd/patrol-sim` | Level 1：mumu 層 sim ハーネス | 検証 |
| `gui-sim.exe` | `cmd/gui-sim` | Level 1.5：GUI API 層 sim ハーネス | 検証 |
| `fake-adb.exe` | `cmd/fake-adb` | 偽 adb（sim から呼ばれる） | 検証 |
| `pcap-record.exe` | `cmd/pcap-record` | 実トラフィック .pcap 録画 | 検証 |
| `pcap-replay.exe` | `cmd/pcap-replay` | .pcap 再生 + golden 比較（ncap 回帰） | 検証 |

検証バイナリは `release\` に出力される（`run_all.ps1` がビルドする）。製品は `build.ps1`。

### sim 基盤の共通部品（`sim/`）

| ファイル | 役割 |
|---|---|
| `sim/server.go` | SimServer（fake-adb からの TCP 接続を受ける疑似 ADB サーバ） |
| `sim/gameserver.go` | 疑似ゲームサーバ（lineID 変化 / 0x2E 完了シグナルを時間遅延付きで注入） |
| `sim/scenario.go` | シナリオ JSON のスキーマ（devices / patrol / server / events / assert / gui_actions） |
| `sim/events.go` | イベント注入（native_move など外乱パケット） |
| `sim/assert.go` | アサーション（forbid/require ログパターン・dwell 実測・ActualCh 追従） |
| `sim/harness.go` | patrol-sim / gui-sim 共通の起動部品（BuildMumuConfig / ResolveFakeADB 等） |

> 重要: sim 基盤は **本番コードに依存させない方向のみ**（本番→sim の依存は禁止）。
> patrol-sim / gui-sim はいずれも **本番 `mumu.Patroller` を無変更で起動** し、
> ADBPath を fake-adb に差し替え、シグナルを直接注入する。

---

## 2. ビルド

### 2-1. 製品ビルド（`build.ps1`）

```powershell
.\build.ps1            # release ビルド → .\release\BPSR_patrol_cams.exe + 配布 zip
.\build.ps1 -Debug     # コンソール付き debug ビルド（-H windowsgui を外す）
```

要件:
- Go 1.23+
- MinGW-w64 GCC が PATH（CGO 必須。build.ps1 が一般的な場所を自動探索、`-GccPath` で明示も可）
- Npcap SDK（既定 `C:\npcap-sdk`、`-NpcapSdk` で変更可）
- 実行先 PC に Npcap 本体（https://npcap.com/ ・WinPcap API 互換モード ON）

build.ps1 がやること: SDK/GCC チェック → CGO フラグ設定 → `go build -trimpath -ldflags "-s -w …"` →
`release\` に config/data/README をコピー → zip 化。

### 2-2. 型・静的解析だけ確認

```powershell
go build ./...   # 全パッケージのビルド（型エラー検出）
go vet ./...     # 静的解析
```

ncap/ mumu/ 変更時のコミット前は両方必須（`CLAUDE.md §6`）。

---

## 3. デバッグ（本体実行時）

### 3-1. ログ

- 出力先: 作業ディレクトリの `logs\log.txt`
- Verbose / Dedup ログは `debuglog/debuglog.go` 管轄
- debug ビルド（`build.ps1 -Debug`）はコンソールにも出る

### 3-2. grep パターン（`CLAUDE.md §8` と同一）

```powershell
# 巡回状態の追跡
Select-String -Path logs\log.txt -Pattern "MuMu|Patrol|0x2E|0x15|巡回|移動"

# バインド状態（probe / lineID / UID 確定）
Select-String -Path logs\log.txt -Pattern "Probe|0x2E|0x15|PortMap|lineID|onLine|バインド|識別"

# チャット
Select-String -Path logs\log.txt -Pattern "\[CHAT-"
```

### 3-3. chat-reporter のデバッグフラグ

```powershell
.\release\chat-reporter.exe -ch-debug    # 全 methodId + scene-change パケット hex をログ
.\release\chat-reporter.exe -ch-watch    # ch 候補パスを監視し値変化時にログ
.\release\chat-reporter.exe -network "<NIC Description>" -webhook "<url>"
```

---

## 4. シミュレーション（実機レス検証）

実機・実ゲームは最終確認のみ。日常検証は 3 レベルで行う。

### レベル早見表

| Level | 対象層 | バイナリ | 実機 | 実ゲーム | 一括実行 |
|---|---|---|---|---|---|
| 1 | mumu（巡回ロジック・状態機械） | patrol-sim | 不要 | 不要 | `run_all.ps1` |
| 1.5 | GUI API（ボタン→HTTP→Patroller） | gui-sim | 不要 | 不要 | `run_all.ps1` |
| 2 | ncap（パケット解析） | pcap-replay | 不要 | 録画済 .pcap | 手動 |

`mumu/` 変更時は Level 1+1.5（`run_all.ps1`）必須。`ncap/` 変更時は Level 2（golden 比較）。

---

### 4-1. Level 1：patrol-sim（mumu 層）

**動作**: 本番 `mumu.Patroller` を起動 → `cfg.ADBPath` を fake-adb.exe に差し替え →
fake-adb は `PATROL_SIM_ADDR`（SimServer のアドレス）に接続 → SimServer/疑似ゲームサーバが
`NotifyLineIDChange` / `NotifyPostLoadReady` を時間遅延付きで Patroller に直接注入 →
シナリオの assert（forbid/require ログ・dwell 実測・ActualCh 追従）で合否判定。

```powershell
# 全シナリオ一括（ビルド込み・約7分・FAILで exit 1）
.\run_all.ps1

# 個別実行
.\release\patrol-sim.exe -scenario scenarios\baseline.json
.\release\patrol-sim.exe -scenario scenarios\baseline.json -v   # verbose
```

ログは `logs\sim\<name>.log`。FAIL したシナリオはパスが一覧表示される。

#### シナリオ一覧（`scenarios\*.json`・7本）

| シナリオ | 検証内容 |
|---|---|
| baseline | 正常巡回・dwell 実測 1.8〜3.5s |
| native_move | ネイティブクライアント干渉（別ch）を無視できるか |
| native_move_same_ch | 偶然同 ch のネイティブ干渉を無視できるか |
| burst_0x2e | 0x2E 10連射で moveSignal がスパムしないか（§4-2.12） |
| silent_one | 1台無応答時のタイムアウト処理 |
| slow_signal | 9s 遅延シグナルでも巡回が破綻しないか |
| screen_mode | `load_detect_mode=screen` の画面判定パス |

---

### 4-2. Level 1.5：gui-sim（GUI API 層）

**動作**: 本番 `gui.Server`（内部に本番 Patroller を所有）を **WebView2 なし** で起動
（`StartServer(ctx)`）→ ボタン押下を `/api/*` への HTTP リクエストとして自動実行 →
シグナル注入は `guiServer.NotifyLineIDChange` / `NotifyPostLoadReady` 経由 →
"ボタン操作 = HTTP POST → 本番ハンドラ → 本番 Patroller → fake-adb → status 反映" を end-to-end 検証。

```powershell
# 個別実行（run_all.ps1 にも内包される）
.\release\gui-sim.exe -scenario scenarios\gui\baseline.json
.\release\gui-sim.exe -scenario scenarios\gui\baseline.json -v
```

#### gui_actions の型（`scenarios\gui\*.json`）

| 型 | 内容 |
|---|---|
| `api` | 任意の `/api/*` を叩く |
| `sweep_readonly` | GET 系 API を一斉スイープ（17本） |
| `config_patch` | **全量 GET → マージ → 全量 POST**（GUI JS の実挙動を模倣） |
| `config_identity_check` | GET→POST→GET 完全一致（No.31 型 Load/Save 非対称の動的検出） |

> `config_patch` で部分 JSON を直接 POST してはいけない。zero-value unmarshal で
> 欠損キーが false/0 保存される（No.31 の罠）。必ず全量 GET→マージ→全量 POST。

#### シナリオ一覧（3本）

| シナリオ | 検証内容 |
|---|---|
| baseline | 開始/停止ボタン → 巡回 → status 反映 + GET API スイープ + config 同一性 |
| config_reflect | 巡回中の config_patch（dwell 2→4s）→ 即時反映 + 後続 dwell 変化 + 永続化 |
| interference_buttons | native_move 外乱とボタン操作の同時発生 |

---

### 4-3. Level 2：pcap-record / pcap-replay（ncap 層）

**動作**: `pcap-record` で実トラフィックを .pcap 録画 → `pcap-replay` が `ncap.ReplayFile`
（本番パーサ）に流す → イベント列を golden（JSONL）と**順序非依存マルチセット比較**。

```powershell
# 録画（本体と並行起動可・Ctrl+C で終了）
.\release\pcap-record.exe
.\release\pcap-record.exe -iface "<NIC Description>" -duration 300 -out logs\cap.pcap

# パース層の生存確認のみ
.\release\pcap-replay.exe -pcap logs\capture_X.pcap

# golden 記録 → 以後の回帰比較
.\release\pcap-replay.exe -pcap X.pcap -record-golden testdata\g.jsonl
.\release\pcap-replay.exe -pcap X.pcap -golden testdata\g.jsonl
.\release\pcap-replay.exe -pcap X.pcap -golden testdata\g.jsonl -strict-detect  # detect も比較対象に
```

#### フラグ

| バイナリ | フラグ | 意味 |
|---|---|---|
| pcap-record | `-out` | 出力先（省略時 `logs/capture_yyyyMMdd_HHmmss.pcap`） |
| | `-iface` | NIC Description 完全一致（省略時 自動選択） |
| | `-duration` | 録画秒数（0 = Ctrl+C まで） |
| | `-snaplen` | スナップ長（既定 10485760 = 本番同一） |
| pcap-replay | `-pcap` | 入力 .pcap（必須） |
| | `-record-golden` | イベント列を JSONL 書き出し |
| | `-golden` | JSONL と比較し差分表示 |
| | `-portmap` | ポートマップファイル（省略時無効） |
| | `-v` | 全イベントを標準出力 |
| | `-strict-detect` | golden 比較に detect も含める |

#### ゲームアップデート後の手順

1. 新版で録画（ログイン + ch切替 + チャット 5〜10分）
2. `pcap-replay` で生存確認 → **「lineid 0件 / UID確定 0件」警告 = パース層破壊の兆候**（No.23/34 型）
3. 問題なければ golden 更新（`-record-golden`）

---

## 5. シナリオ JSON スキーマ（共通）

```jsonc
{
  "name": "baseline",
  "seed": 42,                       // 乱数シード（再現性）
  "devices": [                      // 疑似デバイス
    {"serial": "emu-1", "uid": 41000001, "label": "Instance-1", "initial_ch": 1}
  ],
  "channels": [1, 2, 3],            // 巡回対象 ch
  "patrol": {
    "dwell_secs": 2,
    "move_timeout_secs": 60,
    "load_detect_mode": "time"      // time / screen / either（packet は無効=time扱い）
  },
  "server": {                       // 疑似サーバの応答遅延
    "line_id_change_delay": {"min_ms": 500, "max_ms": 1000},
    "post_load_delay":      {"min_ms": 800, "max_ms": 1500},
    "adb_cmd_delay_ms": 50
  },
  "events": [],                     // 外乱注入（native_move 等）
  "gui_actions": [],                // gui-sim 専用（api/sweep_readonly/config_patch/config_identity_check）
  "assert": {
    "max_cycles": 3,
    "forbid_log_patterns": ["移動失敗（完了シグナルなし）", "未応答", "Mismatch"],
    "require_log_patterns": [],
    "dwell_phase_secs": {"min": 1.8, "max": 3.5},
    "all_devices_actual_ch_follows": true
  }
}
```

---

## 6. 既知の注意点（FAIL 時の切り分け）

- **ビルド直後の初回 sim は flake を観測済み（AV スキャン疑い）。FAIL したらまず再実行**
- gui-sim の dwell 実測は HTTP ポーリング経由のため patrol-sim より **~1.2s 膨らむ**
  （dwell assert は余裕を持たせる）
- `mumu.go` は dwell_wait 設定後に extraWait（最大 MoveTimeout）が走るため、シグナル遅延時は
  dwell 実測が膨らむ（sim の計測特性・本番バグではない）
- forbid に `移動失敗` 単独は使わない。clear-move-failed のログ「移動失敗チャンネルリストを
  クリアしました」に偽陽性。実害シグネチャは `移動失敗（完了シグナルなし）`
- `load_detect_mode` の有効値は `screen` / `either` / それ以外=time（`packet` は無効値で time 扱い）
- fake-adb は `PATROL_SIM_ADDR` 未設定なら即 `exit 1`（必ず sim ハーネス経由で起動）

---

## 7. skill 経由のショートカット

- `/sim [シナリオ名]` — Level 1（flake 自動再実行付き）
- `/replay-check [pcap] [golden]` — Level 2

---

## 8. 危険領域変更時の標準フロー（要約・詳細は `CLAUDE.md §11`）

ncap/ mumu/ appconfig/ を変更する時（1 Phase = 1 commit = changelog 1 エントリ）:

1. diff 仕様書（`docs/plan_*.md`）を作成 → ユーザー承認
2. 実装 subagent（sonnet）に仕様書を渡して実装
3. 専用レビュー agent（§9: packet-analyst / patrol-flow-reviewer / config-compat-checker）でレビュー
4. 主セッションが diff を仕様書と照合
5. 動的検証: mumu→`run_all.ps1` / ncap→`pcap-replay` + `go build ./...` + `go vet ./...`
6. commit: changelog 追記 → `§6` チェックリスト → secret チェック → コミット
