# BPSR Patrol Cams

Blue Protocol Star Resonance のパケットキャプチャ・レアモンスター検知・Discord通知・MuMu ADB自動巡回。
Windows / Go 1.23+ / CGO (Npcap)。

---

## 0. Claude への指示（最優先・常時適用）

- 曖昧な要望は実装前に質問返す。情報が揃うまで着手しない
- 思考中に方針変更・別案が浮上したら **即座に報告**、勝手に進めない
- 応答は簡潔・箇条書き・敬語不要・同僚距離感
- **触る前に下記「§3 ファイル責務マップ」「§4 不変条件」「§5 過去リグレッション罠リスト」を必ず確認**
- ncap/ pb/ mumu/ MatchLineChange / processSyncContainerData / processSyncToMeDeltaInfo を変更する場合は **必ず先にユーザー確認**
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
```

要件: Go 1.23+ / MinGW-w64 GCC (PATH 通っている事) / Npcap SDK (`C:\npcap-sdk`)。

---

## 3. ファイル責務マップ

| パス | 役割 | 行数 | 触る時の慎重度 |
|---|---|---|---|
| `main.go` | 起動・配線（capture↔patroller↔gui の接続） | ~615 | 中 |
| `ncap/cap_device.go` | パケット解析・session 管理・0x15/0x2E ハンドラ | ~2271 | **最高（要確認）** |
| `ncap/portmap.go` | port→ch マッピングの永続化（JSON） | ~185 | 中（フォーマット互換注意） |
| `ncap/cap_helper.go`, `byte_reader.go`, `queue.go` | バイナリ読み出しユーティリティ | 小 | 低 |
| `pb/bp.pb.go` | protobuf 生成コード（手動編集禁止） | - | **編集禁止** |
| `mumu/mumu.go` | Patroller（巡回ロジック）・状態機械・ADB呼出 | ~2443 | **最高（要確認）** |
| `gui/gui.go` | HTTP サーバー + HTML/JS/CSS 一体（巨大ファイル） | ~6017 | 高 |
| `appconfig/config.go` | config.json Load/Save・defaults | 小 | **高（Load/Save 非対称の罠あり）** |
| `notifier/discord.go` | Discord webhook 送信 | 小 | 低 |
| `debuglog/debuglog.go` | Verbose/Dedup ログ | 小 | 低 |
| `cmd/chat-reporter/main.go` | 別バイナリ（chat-reporter.exe） | ~329 | 中（main.go と設定同期必須） |
| `config/*.json`, `config/channels.txt` | ランタイム設定（後方互換必須） | - | 高 |
| `data/locations.json` | 場所名マスタ | - | 低 |

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

1. **`RecordPatrolMove` は SwitchGroup の前に呼ぶ**: ADB コマンド発行前に pendingProbes を登録する。さもないとサーバ側の新 TCP セッションが先行して `MatchLineChange` 時点で pending probe が空になる（No.35）
2. **`probeWindow` は 60 秒**: 5台直列切替で30s以上かかる（No.23）
3. **`MatchLineChange` の Probe マッチ条件**: `changedAt >= probe.sentAt - 2s` を必ずチェック。ADB 発行前の lineID 変化（本物クライアントの操作等）を拒否（No.33）
4. **`MatchLineChange` の二重チェック**: RLock で未バインド確認後に WriteLock 取得しても、ロック取得後に `serialToUID[bindSerial] != 0` を再チェックして early return（並走ゴルーチン対策・No.26）
5. **`moveSignal` の時刻フィルタは `switchStartAt` 基準**: `switchDoneAt` 基準にしない。ADB done がゲームサーバ応答より遅れることがある（No.30）
6. **完了シグナルは `0x2E UUID 受信 + 遅延`**: `NotifyPostLoadReady` → `notifyMoveSignal` 経路。遅延は自動（直近観測ベース）または手動（`PatrolLoadStabilizationSecs`）。lineID-change では発火しない（No.39）
7. **既到達ch（`CurrentCh == 目的ch`）のデバイス**: `switchTargets` から除外、ただし `RecordPatrolMove` は呼ぶ（ExpectedCh 更新のため）、`need` も除外後の件数（No.29）
8. **`labelToSerial` は `serialToLabel` の逆引きで常に同期更新**: `notifyMoveSignal` が serial で送るため（No.30）
9. **`excludeUIDs`**: 本物クライアントの UID を probe マッチから除外する。永続化（config.json `exclude_uids`）（No.33）
10. **`MatchLineChange` は `notifyMoveSignal` を発火しない**: 完了シグナルは `NotifyPostLoadReady`（0x2E UUID 受信時）が担当。MatchLineChange は ActualCh 更新のみ（No.39）
11. **`moveSignalMsg` は `lineID` を必須携帯**: wait loop（3箇所）は `msg.lineID == 0 || msg.lineID == targetCh` かつ `!respondedSet[msg.label]` を `got++` 前にガード。stale lineID 流用と同一 serial 重複カウントを防止（No.41）
12. **`NotifyPostLoadReady` は同 lineID 内 1 回のみ発火**: session に `postLoadFiredForLineID` を保持し、lineID 変化時にのみ再発火可能。戦闘中などで 0x2E が連射されても channel をスパムしない（No.41）

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
3. **チャット SSE dedup キー**: JS 側 `dedupeChatEvents()` のキーは `label||client_ip + ch + sender + msg`（No.24）
4. **`btn-ch-save` 等の DOM 参照**: `null` 安全化必須。`_setChSaveBtnDisabled()` ヘルパーに集約済み（No.14）

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
