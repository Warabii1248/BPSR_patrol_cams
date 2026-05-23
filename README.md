# BPSR patrol cams

https://github.com/yukkuman/bpsr-loyal-emu をベースに改変・拡張したツールです。

スターレゾナンスで複数エミュレータを使い、指定モンスター（ウリボ・ゴールド、金ナッポ、銀ナッポ等）を自動検知して Discord に通知します。
検知したチャンネルは巡回リストから自動削除され、次のチャンネルへ進みます。

---

## 目次

1. [概要](#概要)
2. [主な機能](#主な機能)
   - [パケットキャプチャによる自動検知](#パケットキャプチャによる自動検知自動)
   - [ワールドチャット解析による検知](#ワールドチャット解析による検知チャット)
   - [チャンネルパトロール](#チャンネルパトロール自動巡回)
   - [ロード完了判定モード](#ロード完了判定モード)
   - [PortMap（チャンネル推定・確定システム）](#portmapチャンネル推定確定システム)
   - [Probe-and-Match（シリアル↔UID 自動バインド）](#probe-and-matchシリアルuid-自動バインド)
   - [クラッシュ検知・自動復帰](#クラッシュ検知自動復帰)
   - [Discord 通知](#discord-通知)
   - [GAS 連携](#gas-連携chrome-拡張プッシュ)
3. [GUI 操作](#gui-操作)
4. [設定ファイル](#設定ファイル)
   - [config.json](#configjson)
   - [filter.json](#filterjson)
   - [channels.txt](#channelstxt)
   - [port_ch_map.json](#port_ch_mapjson)
5. [ビルド方法（開発者向け）](#ビルド方法開発者向け)
6. [ライセンス](#ライセンス)

---

## 概要

本ツールは Windows 上で動作し、MuMu Player（ADB 経由）でスターレゾナンスのチャンネルを自動巡回しながら、パケットキャプチャとワールドチャット解析の2経路でレアモンスターを検知します。
GUI はブラウザベースのウィンドウ（WebView2）で表示され、巡回の開始・停止や設定変更をリアルタイムで操作できます。

---

## 主な機能

### パケットキャプチャによる自動検知（自動）

- Npcap を使用してゲームの通信パケットをキャプチャ
- モンスターの出現パケットを解析し、モンスター名・座標・チャンネルを抽出
- 検知ソース: `自動`
- `notify_enemies` で通知対象モンスターを有効/無効で個別管理

### ワールドチャット解析による検知（チャット）

- ゲーム内チャットを取得
- `filter.json` の位置ルール・モンスターエイリアスルールに基づき、チャットテキストから出現場所とモンスター種別を推定
- チャットに含まれるチャンネル番号を抽出して通知に付与
- 検知ソース: `チャット`
- `chat_exclude` キーワードに一致するメッセージはスキップ
- `chat_report_excluded_senders` の発言者は Discord 通知対象外。ただし対象 ch は巡回リストから自動削除される（占有判断）
- `chat_report_min_length` / `chat_report_max_length` で報告候補の文字数レンジを制御
- チャットは内容ベース（`ch|sender|msg`）で重複排除し、複数台受信時は GUI に「N台受信」バッジ表示

### チャンネルパトロール（自動巡回）

- `channels.txt` に記載されたチャンネルを順番に巡回
- ADB 経由で MuMu Player を操作し、チャンネル切り替えを自動実行
- 検知が発生したチャンネルは巡回リストから自動削除し、一定時間クールダウン
- 巡回方向は昇順・降順を設定可能
- 指定台数を並列で切り替えてから `parallel_group_delay_secs` 秒待機する並列グループ制御
- 移動失敗判定: `move_fail_threshold` （0.0-1.0、稼働台数のうち何割が完了シグナルを送れば成功とみなすか）で判定。`consecutive_move_fail_threshold` 回連続失敗でクラッシュとみなす
- `patrol_adaptive_timeout` を有効にすると、直近 `patrol_adaptive_timeout_window` 件のロード時間から `move_timeout` / `merge_timeout` を自動調整
- 巡回サイクル KPI（平均サイクル時間・ch/hour）を GUI に表示

### ロード完了判定モード

`patrol_load_detect_mode` で、ch 切替後のロード完了判定方式を選択できる。

| モード | 説明 |
|---|---|
| `time` | 0x2E UUID 受信後、固定遅延（手動）または直近観測値（自動 `patrol_load_stabilization_auto=true`）で完了とみなす（従来動作） |
| `screen` | ADB スクショで指定矩形領域の黒画面消失を検知して完了とみなす。`patrol_screen_timeout_secs` でフォールバックタイムアウト |
| `either` | `time` と `screen` を並走し、先に完了した方で進行（取りこぼし耐性） |

監視矩形は GUI 上の「監視矩形プレビュー」からスクショ上をドラッグして指定可能。
MuMu Player は負荷軽減のため 1280x720 解像度での運用を前提（デフォルト矩形は中央 600x400）。

### PortMap（チャンネル推定・確定システム）

- 各エミュレータが接続中のサーバーポート番号からチャンネル番号を推定
- 複数インスタンスの投票による多数決でポート→チャンネルのマッピングを確定
- `port_ch_map.json` にマッピングを永続化
- マッピング変更が検出されると GUI に確認モーダルが表示され、手動で承認・却下可能
- 未確定の推定値は通知に `(非確定)` と付与される

### Probe-and-Match（シリアル↔UID 自動バインド）

- 巡回時の ch 切替パケットを観測することで、ADB シリアルとプレイヤー UID を自動でひもづける
- バインド結果は `serial_to_uid` として `config.json` に永続化
- 同一 PC で本物のクライアントを起動している場合は `exclude_uids` にその UID を登録することで誤バインドを防止可能（GUI から登録可）

### クラッシュ検知・自動復帰

- `crash_recovery_enabled=true` のとき、移動タイムアウト時に `game_package_name` のプロセス生死を ADB で確認
- 落ちていた場合は `game_launch_activity` （未指定時は `monkey -p`）でゲームを再起動し、`crash_recovery_delay_secs` 待機後に巡回を再開

### Discord 通知

- `discord_webhook` に設定した Webhook URL へ通知を送信
- `discord_chat_report_webhook` を設定すると、報告候補チャット（フィルタ通過済み）を別チャンネルへ通知可能
- モンスター別に絵文字付きのタイトルを自動選択（ウリボ・ゴールド / 金ナッポ / 銀ナッポ）
- チャンネル番号（確定・非確定）、出現場所、検知インスタンス、検知時刻を含む
- `debounce_seconds` で同一情報の重複通知を抑制

### GAS 連携（Chrome 拡張プッシュ）

- Chrome 拡張から POST `/api/patrol/channels/gas` でチャンネルリストを受信するプッシュ型
- `gas_target_enemy` で対象モンスター名を指定（部分一致、例: `金ウリボ` / `金ナッポ`）
- `gas_enable=false` にすると 403 を返してプッシュを拒否
- 受信したチャンネルは既存の巡回リストとマージされ、クールダウン処理を経て反映

---

## GUI 操作

ブラウザウィンドウ（WebView2）が起動し、以下のパネルを操作できます。

| パネル | 説明 |
|---|---|
| **ダッシュボード** | 巡回ステータス・サイクル KPI・接続デバイス概要 |
| **巡回制御** | 巡回の開始・停止・再開、巡回チャンネル編集（カンマ区切り入力対応）、ch マトリクス表示、満員/失敗リストのクリア |
| **デバイス管理** | 接続中の各エミュレータインスタンスの状態（チャンネル、接続先 IP、セッション数、UID バインド）をリアルタイム表示 |
| **レアエネミー検知履歴** | 過去の検知イベントを一覧表示、個別削除・全削除が可能 |
| **チャットログ** | キャプチャしたワールドチャットを新着順で表示（ポップアウト・カテゴリ chip フィルタ対応） |
| **ログ** | アプリケーションログをリアルタイムで表示（カテゴリ chip でフィルタ可） |
| **設定** | `config.json` / `filter.json` の各パラメータをブラウザ上から編集・保存（多くの項目はホットリロード） |
| **監視矩形プレビュー** | スクショ取得 → ドラッグで `screen` モードの監視矩形を視覚的に指定 |

---

## 設定ファイル

### config.json

初回起動時に `config/config.json` が生成されます。

#### キャプチャ・通知

| キー | 型 | 説明 |
|---|---|---|
| `network` | string | キャプチャ対象のネットワークインターフェース名（`auto` で自動選択） |
| `auto_check` | int | `network=auto` 時のサンプリング秒数（デフォルト: 3） |
| `locations` | string | 座標→場所名マッピングファイルのパス |
| `discord_webhook` | string | Discord Webhook URL（モンスター検知通知） |
| `discord_chat_report_webhook` | string | 報告候補チャット専用 Webhook URL（空で無効） |
| `debounce_seconds` | int | 同一検知の重複通知抑制秒数 |
| `notify_enemies` | []{name,enabled} | 通知対象モンスター（個別 ON/OFF） |
| `filter_file` | string | filter.json のパス |

#### GUI / ADB

| キー | 型 | 説明 |
|---|---|---|
| `gui_port` | int | GUI サーバーのポート番号（デフォルト: 8080） |
| `adb_path` | string | adb 実行ファイルのパスまたはコマンド名 |
| `mumu_serials` | []string | ADB デバイスシリアル一覧（null で自動検出） |
| `mumu_tap_x` / `mumu_tap_y` | int | チャンネル入力欄のタップ座標 |
| `mumu_clear_length` | int | チャンネル入力前にバックスペースを送る回数 |
| `mumu_pre_keycode` | string | チャンネル入力前に送るキーコード（例: `KEYCODE_P`） |
| `mumu_delay_ms` | int | ADB 操作間のディレイ（ミリ秒、デフォルト: 1200） |
| `serial_to_label` | map | ADB シリアル → 表示ラベルの手動マッピング |
| `show_no_device_dialog` | bool | 起動時にデバイスが見つからない場合にダイアログを出すか（デフォルト: true） |

#### 巡回制御

| キー | 型 | 説明 |
|---|---|---|
| `patrol_channels_file` | string | 巡回チャンネルリストのパス |
| `port_map_file` | string | PortMap ファイルのパス |
| `patrol_serials` | []string | 巡回に使うデバイスシリアル（null で全台） |
| `patrol_dwell_secs` | float | 各チャンネルでの滞在秒数（最低5秒） |
| `patrol_move_timeout_secs` | float | 全台シグナル待ちの最大秒数（0で無効、デフォルト: 180） |
| `patrol_merge_timeout_secs` | float | 1台目受信後、残り台数を待つ最大秒数（デフォルト: 15） |
| `parallel_limit` | int | 並列ch切替の最大台数（0で全台並列） |
| `parallel_group_delay_secs` | float | 並列グループ切替後の待機秒数 |
| `active_device_count` | int | 期待するアクティブデバイス数（0で自動） |
| `move_fail_threshold` | float | 移動成功判定: 稼働台数のうち何割が完了シグナルを送れば成功とみなすか（0.0-1.0、0=全台） |
| `consecutive_move_fail_threshold` | int | 連続移動失敗でクラッシュとみなす回数（デフォルト: 3） |
| `patrol_adaptive_timeout` | bool | 実ロード時間学習で MoveTimeout/MergeTimeout を自動調整（デフォルト: true） |
| `patrol_adaptive_timeout_window` | int | 学習に使うサンプル数（デフォルト: 10） |

#### ロード完了判定

| キー | 型 | 説明 |
|---|---|---|
| `patrol_load_detect_mode` | string | `time` / `screen` / `either`（デフォルト: `time`） |
| `patrol_load_stabilization_secs` | float | 手動安定化遅延秒（`time` モードかつ auto=false） |
| `patrol_load_stabilization_auto` | bool | 直近観測値で安定化遅延を自動算出（デフォルト: true） |
| `patrol_screen_poll_ms` | int | `screen`/`either` のスクショポーリング間隔（デフォルト: 500） |
| `patrol_screen_region_x/y/w/h` | int | 監視矩形（px、画像座標系、デフォルト: 340/160/600/400） |
| `patrol_screen_black_luma` | int | 黒判定の輝度閾値 0-255（デフォルト: 25） |
| `patrol_screen_black_pixel_ratio` | float | 黒画素割合下限 0.0-1.0（デフォルト: 0.95） |
| `patrol_screen_timeout_secs` | float | 画面判定フォールバックタイムアウト（デフォルト: 12） |

#### Probe-and-Match / クラッシュ復帰

| キー | 型 | 説明 |
|---|---|---|
| `serial_to_uid` | map | シリアル→UID 自動バインド結果（手動編集可） |
| `exclude_uids` | []uint64 | バインド候補から永久除外する UID 一覧（同 PC の本物クライアント等） |
| `game_package_name` | string | ゲームの Android パッケージ名（空で復帰機能無効） |
| `game_launch_activity` | string | 起動 Activity（省略時は `monkey -p`） |
| `crash_recovery_enabled` | bool | タイムアウト時にゲームプロセス確認と自動復帰を行う |
| `crash_recovery_delay_secs` | float | 復帰コマンド後のゲーム起動待機秒数（デフォルト: 30） |

#### GAS / その他

| キー | 型 | 説明 |
|---|---|---|
| `gas_target_enemy` | string | Chrome 拡張から受信する ch のフィルタ対象モンスター（部分一致、デフォルト: `金ウリボ`） |
| `gas_enable` | bool | `/api/patrol/channels/gas` POST を受け付けるか（デフォルト: true） |
| `scene_map_ids` | map | シーン名（中国語）→ mapID マッピング（locations.json と対応） |
| `debug_verbose` | bool | 詳細デバッグログ出力（デフォルト: false） |
| `window_state` | object | GUI ウィンドウ位置・サイズ（自動保存） |

### filter.json

チャット検知のフィルタルールを定義します（`config.json` から分離されています）。

| キー | 説明 |
|---|---|
| `chat_exclude` | このキーワードを含むチャットメッセージを無視する |
| `chat_report_senders` | 報告候補として優先的に拾う発言者（空可） |
| `chat_report_excluded_senders` | 通知対象外にする発言者（ただし対象 ch の自動削除は行う） |
| `chat_report_location_rules` | 場所名と表記ゆれ・出現候補モンスターを定義。形式: `場所名\|別名1,別名2,...\|モンスター1,モンスター2` |
| `chat_report_monster_alias_rules` | モンスター名の表記ゆれを正規化。形式: `正規名\|別名` |
| `chat_report_min_length` | 報告候補の最小文字数（デフォルト: 4） |
| `chat_report_max_length` | 報告候補の最大文字数（デフォルト: 80） |

### channels.txt

巡回するチャンネル番号を1行1つで記載します。

```
2
3
11
13
...
```

検知が発生したチャンネルは自動的にこのファイルから削除されます。
GAS 連携や GUI 上の編集でも更新されます。

### port_ch_map.json

PortMap システムが管理するポート番号→チャンネル番号のマッピングファイルです。
通常は自動で更新されます。手動編集は GUI の PortMap 確認モーダルから行うことを推奨します。

---

## ビルド方法（開発者向け）

### 前提条件

- Go 1.23 以上
- MinGW-w64 (GCC) が PATH に存在すること
- [Npcap SDK](https://npcap.com/#download) を `C:\npcap-sdk` に展開
- 実行環境に [Npcap](https://npcap.com/#download) がインストールされていること
- [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) がインストールされていること（通常 Windows 11 では同梱）

### build.ps1 を使う（推奨）

```powershell
.\build.ps1            # release ビルド
.\build.ps1 -Debug     # コンソール付き debug ビルド
```

`release\` フォルダにビルド済みファイルが出力され、実行ファイル名は `BPSR_patrol_cams.exe` です。
`winres\winres.json` と `winres\icon.ico` を置いておくと EXE にアイコンが自動埋め込みされます（`go-winres` 未インストール時は自動インストール）。

### 手動ビルド

```powershell
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS  = "-IC:\npcap-sdk\Include"
$env:CGO_LDFLAGS = "-LC:\npcap-sdk\Lib\x64 -lwpcap"
go build -ldflags "-s -w -H windowsgui -X main.Version=dev" -o BPSR_patrol_cams.exe .
```

### チャット報告専用アプリ（同期版）

公開前にメインアプリと同じロジックを共有するため、専用エントリを `cmd/chat-reporter` に追加しています。

- 既存の `build.ps1` には影響しません（メインアプリのビルド手順はそのまま）
- 設定ファイルは既存と同じ `config/config.json` と `config/filter.json` を利用
- GUI は流用し、巡回開始/停止と巡回チャンネル編集機能は無効化されています

実行:

```powershell
go run ./cmd/chat-reporter
```

ビルド:

```powershell
go build -o bpsr-chat-reporter.exe ./cmd/chat-reporter
```

---

## ライセンス

[GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE)

本ソフトウェアは [balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI)（Copyright (C) balrogsxt）を元に改変・拡張したものです。

AGPL-3.0 に基づき、改変版を配布・公開する場合はソースコードも同ライセンスで公開する必要があります。
