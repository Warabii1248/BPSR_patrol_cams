# BPSR patrol cams

[balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI) をベースに改変・拡張したツールです。

スターレゾナンスで複数エミュレータを使い、指定モンスター（ウリボ・ゴールド、金ナッポ、銀ナッポ等）を自動検知して Discord に通知します。  
検知したチャンネルは巡回リストから自動削除され、次のチャンネルへ進みます。

---

## 目次

1. [概要](#概要)
2. [主な機能](#主な機能)
   - [パケットキャプチャによる自動検知](#パケットキャプチャによる自動検知自動)
   - [ワールドチャット解析による検知](#ワールドチャット解析による検知チャット)
   - [チャンネルパトロール](#チャンネルパトロール自動巡回)
   - [PortMap（チャンネル推定・確定システム）](#portmapチャンネル推定確定システム)
   - [Discord 通知](#discord-通知)
   - [GAS 連携](#gas-連携外部チャンネルリスト取得)
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

本ツールは Windows 上で動作し、MuMu エミュレータ（ADB 経由）でスターレゾナンスのチャンネルを自動巡回しながら、パケットキャプチャとワールドチャット解析の2経路でレアモンスターを検知します。  
GUI はブラウザベースのウィンドウ（WebView2）で表示され、巡回の開始・停止や設定変更をリアルタイムで操作できます。

---

## 主な機能

### パケットキャプチャによる自動検知（自動）

- Npcap を使用してゲームの通信パケットをキャプチャ
- モンスターの出現パケットを解析し、モンスター名・座標・チャンネルを抽出
- 検知ソース: `自動`
- `notify_enemies` で通知対象モンスターを設定

### ワールドチャット解析による検知（チャット）

- ゲーム内チャットを取得
- `filter.json` の位置ルール・モンスターエイリアスルールに基づき、チャットテキストから出現場所とモンスター種別を推定
- チャットに含まれるチャンネル番号を抽出して通知に付与
- 検知ソース: `チャット`
- `chat_exclude` キーワードに一致するメッセージはスキップ
- `chat_report_excluded_senders` で特定送信者のメッセージを除外(discordで報告してくれる人など)

### チャンネルパトロール（自動巡回）

- `channels.txt` に記載されたチャンネルを順番に巡回
- ADB 経由で MuMu エミュレータを操作し、チャンネル切り替えを自動実行
- 検知が発生したチャンネルは巡回リストから自動削除
- 巡回方向は昇順・降順を設定可能
- 指定台数を並列で切り替えてから `parallel_group_delay_secs` 秒待機する並列グループ制御
- 満員チャンネル自動スキップ: 接続エミュレータ数が `full_threshold` を下回った場合に満員と判定。`consecutive_full_threshold` 回連続した場合はクラッシュとみなして停止

### PortMap（チャンネル推定・確定システム）

- 各エミュレータが接続中のサーバーポート番号からチャンネル番号を推定
- 複数インスタンスの投票による多数決でポート→チャンネルのマッピングを確定
- `port_ch_map.json` にマッピングを永続化
- マッピング変更が検出されると GUI に確認モーダルが表示され、手動で承認・却下可能
- 未確定の推定値は通知に `(非確定)` と付与される

### Discord 通知

- `discord_webhook` に設定した Webhook URL へ通知を送信
- モンスター別に絵文字付きのタイトルを自動選択（ウリボ・ゴールド / 金ナッポ / 銀ナッポ）
- チャンネル番号（確定・非確定・推定）、出現場所、検知インスタンス、検知時刻を含む
- `debounce_seconds` で同一情報の重複通知を抑制

### GAS 連携（外部チャンネルリスト取得）

- Google Apps Script（GAS）のエンドポイントから定期的にチャンネルリストを取得
- `gas_url` に GAS の URL を設定
- `gas_fetch_interval_mins` で取得間隔（分）を設定
- `gas_spawn_threshold_hours` 時間以内にスポーン記録があるチャンネルのみを対象とする
- 取得したチャンネルは既存の巡回リストとマージされ、クールダウン処理を経て反映

---

## GUI 操作

ブラウザウィンドウ（WebView2）が起動し、以下のパネルを操作できます。

| パネル | 説明 |
|---|---|
| **巡回制御** | 巡回の開始・停止・再開、巡回チャンネルリストの編集、満員スキップリストのクリア |
| **デバイス管理** | 接続中の各エミュレータインスタンスの状態（チャンネル、接続先 IP、セッション数）をリアルタイム表示 |
| **レアエネミー検知履歴** | 過去の検知イベントを一覧表示、個別削除・全削除が可能 |
| **チャットログ** | キャプチャしたワールドチャットを新着順で表示（ポップアウトウィンドウ対応） |
| **ログ** | アプリケーションログをリアルタイムで表示 |
| **設定** | `config.json` の各パラメータをブラウザ上から編集・保存（一部設定は再起動不要でホットリロード） |

---

## 設定ファイル

### config.json

一度起動をすると`config.json`が生成される

| キー | 型 | 説明 |
|---|---|---|
| `network` | string | キャプチャ対象のネットワークインターフェース名（空欄で自動選択） |
| `auto_check` | int | 起動時に接続を確認するインスタンス数 |
| `locations` | string | 座標→場所名マッピングファイルのパス |
| `discord_webhook` | string | Discord Webhook URL |
| `debounce_seconds` | int | 同一検知の重複通知抑制秒数 |
| `filter_file` | string | filter.json のパス |
| `gui_port` | int | GUI サーバーのポート番号（デフォルト: 8080） |
| `adb_path` | string | adb 実行ファイルのパスまたはコマンド名 |
| `mumu_serials` | []string | ADB デバイスシリアル一覧（null で自動検出） |
| `mumu_tap_x` / `mumu_tap_y` | int | チャンネル入力欄のタップ座標 |
| `mumu_clear_length` | int | チャンネル入力前にバックスペースを送る回数 |
| `mumu_pre_keycode` | string | チャンネル入力前に送るキーコード（例: `KEYCODE_P`） |
| `mumu_delay_ms` | int | ADB 操作間のディレイ（ミリ秒） |
| `patrol_channels_file` | string | 巡回チャンネルリストのパス |
| `port_map_file` | string | PortMap ファイルのパス |
| `patrol_dwell_secs` | float | 各チャンネルでの滞在秒数（最低5秒） |
| `patrol_move_timeout_secs` | float | チャンネル移動完了待ちのタイムアウト秒数（0で無効） |
| `patrol_merge_timeout_secs` | float | セッションマージ待ちのタイムアウト秒数 |
| `parallel_limit` | int | 並列チャンネル切り替え台数（0で全台並列） |
| `parallel_group_delay_secs` | float | 並列グループ切り替え後の待機秒数 |
| `patrol_serials` | []string | パトロールに使うデバイスのシリアル（null で mumu_serials と同じ） |
| `active_device_count` | int | 期待するアクティブデバイス数（満員判定基準） |
| `full_threshold` | float | 満員と判定する接続数の閾値（active_device_count に対する割合） |
| `consecutive_full_threshold` | int | 連続満員スキップでクラッシュとみなす回数 |
| `scene_map_ids` | object | マップ名→マップ ID のマッピング（例: `{"阿斯特里亚平原": 7}`） |
| `monster_scan` | bool | モンスタースキャンの有効/無効 |
| `gas_url` | string | GAS エンドポイント URL |
| `gas_fetch_interval_mins` | float | GAS からのチャンネルリスト取得間隔（分） |
| `gas_spawn_threshold_hours` | float | GAS チャンネルフィルタ: この時間以内のスポーン記録があるチャンネルのみ使用 |
| `notify_enemies` | []string | 通知対象モンスター名一覧 |

### filter.json

チャット検知のフィルタルールを定義します。

| キー | 説明 |
|---|---|
| `chat_exclude` | このキーワードを含むチャットメッセージを無視する |
| `chat_report_excluded_senders` | このユーザーのチャットを無視する |
| `chat_report_location_rules` | 場所名と表記ゆれ・対象モンスールを定義。形式: `場所名\|別名1,別名2,...\|モンスター1,モンスター2` |
| `chat_report_monster_alias_rules` | モンスター名の表記ゆれを正規化。形式: `正規名\|表記ゆれ` |

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
通常は自動で更新されます。手動編集は PortMap 確認モーダルから行うことを推奨します。

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
.\build.ps1
```

`LoyalBoarlet\` フォルダにビルド済みファイルが出力されます。  
`winres\winres.json` と `winres\icon.ico` を置いておくと EXE にアイコンが自動埋め込みされます（`go-winres` が未インストールの場合は自動でインストールされます）。

### 手動ビルド

```powershell
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS  = "-IC:\npcap-sdk\Include"
$env:CGO_LDFLAGS = "-LC:\npcap-sdk\Lib\x64 -lwpcap"
go build -ldflags "-s -w -H windowsgui -X main.Version=dev" -o LoyalBoarlet.exe .
```

---

## ライセンス

[GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE)

本ソフトウェアは [balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI)（Copyright (C) balrogsxt）を元に改変・拡張したものです。

AGPL-3.0 に基づき、改変版を配布・公開する場合はソースコードも同ライセンスで公開する必要があります。
