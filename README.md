# LoyalBoarlet Monitor

[balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI) をベースに改変・拡張したツールです。

スターレゾナンスで複数エミュレータを使い、ゴールドウリボを自動検知して Discord に通知します。  
検知したチャンネルは巡回リストから自動削除され、次のチャンネルへ進みます。

---

## 必要なソフトウェア

| ソフト | 用途 | 入手先 |
|---|---|---|
| **Npcap** | ネットワークキャプチャ | https://npcap.com/ |

> Npcap インストール時は **「WinPcap API-compatible Mode」にチェック**を入れてください。

---

## セットアップ

1. `config.json` の `discord_webhook` に通知先の Webhook URL を設定
2. `LoyalBoarlet.exe` を起動
3. GUI ウィンドウが開く（または `http://127.0.0.1:8080` をブラウザで開く）
4. エミュレータを起動し、デバイス一覧に認識されることを確認
5. 巡回チャンネルリストを編集して巡回を開始

### 配布ファイル一覧

```
LoyalBoarlet.exe        実行ファイル
config.json             設定ファイル
channels.txt            巡回チャンネルリスト（1行1番号）
data/
  locations.json        マップ場所名データ
```

---

## GUI

起動後、WebView2 ウィンドウまたはブラウザで GUI が開きます。

### パネル構成

| パネル | 内容 |
|---|---|
| 📱 デバイス一覧 & 手動切替 | ADB で認識した端末の一覧・個別/一括チャンネル切替 |
| 🔁 チャンネル巡回 | 巡回の開始・停止・設定、チャンネルリストの編集 |
| 🌟 金ウリボ検知履歴 | 検知した時刻・Ch・場所の履歴（最大 50 件、新しい順） |
| 📋 検知ログ | リアルタイムログ（テキスト選択・コピー可） |
| ⚙ 設定 | config.json の各設定をリアルタイム編集・保存 |

### パネル操作

| 操作 | 内容 |
|---|---|
| ヘッダーをドラッグ | 同カラム内での並び替え、または左右カラム間へ移動 |
| 下端をドラッグ | パネルの高さを変更 |
| 中央の縦線をドラッグ | 左右カラムの幅を調整 |
| 右上「─」ボタン | パネルを最小化（「＋」で展開） |

パネルの配置・幅・高さ・最小化状態は自動保存され、次回起動時に復元されます。

### 巡回機能

- **チャンネルリスト編集**：行ごとに追加・削除・編集、昇順/降順ソート、カンマ区切り一括入力対応
- **巡回方向**：正順・逆順をボタンで切り替え
- **ループ / 一巡モード**：全チャンネルを一周後に自動停止するモードあり
- **開始 Ch 指定**：任意チャンネルから開始（0 = 前回位置から再開）
- **ch 移動完了待ち**：`[0x2E]` パケット受信をトリガーにタイマー開始（グループ切替の遅延にも対応）
- **満員 Ch の自動スキップ**：移動完了シグナルが時間内に届かない場合、満員と判定してスキップ（クリアボタンあり）
- **金ウリボ検知時の自動削除**：検知された Ch は巡回リストから即時削除され `channels.txt` にも保存

---

## 設定ファイル (config.json)

設定は GUI の ⚙ 設定パネルから変更・保存できます。★ の項目は保存後すぐに反映されます（再起動不要）。

### キャプチャ設定

| キー | デフォルト | 説明 |
|---|---|---|
| `network` | `"auto"` | NIC 名。`"auto"` で最アクティブな NIC を自動選択 |
| `auto_check` | `3` | `"auto"` 時のサンプリング秒数 |
| `locations` | `"data/locations.json"` | マップ場所名データのパス |

### 通知設定

| キー | デフォルト | 説明 |
|---|---|---|
| `discord_webhook` | `""` | Discord Webhook URL。空で無効 |
| `debounce_seconds` | `30` | 同 Ch・同場所の重複通知を抑制する秒数 |
| `filter_file` | `"filter.json"` | チャットフィルター設定ファイルのパス |

## チャットフィルター設定 (filter.json)

チャット関連のフィルター条件は `config.json` ではなく `filter.json` を正規設定として扱います。

| キー | 説明 |
|---|---|
| `chat_exclude` | ワールドチャット検知を抑制するキーワード一覧 |
| `chat_report_senders` | 発見報告候補として扱う発言者フィルター |
| `chat_report_excluded_senders` | 候補から除外する発言者フィルター |
| `chat_report_location_rules` | 地点別名と出現候補モンスターの追加ルール |
| `chat_report_monster_alias_rules` | モンスター別名の追加ルール |

### GUI / ADB 設定

| キー | デフォルト | 説明 |
|---|---|---|
| `gui_port` | `8080` | Web GUI のポート番号。0 で GUI 無効 |
| `adb_path` | `"adb"` | adb.exe のパスまたはファイル名 |
| `mumu_serials` | `[]` | ADB シリアル一覧。空で自動検出 |
| `mumu_tap_x` | `975` | チャンネル入力欄のタップ X 座標 |
| `mumu_tap_y` | `664` | チャンネル入力欄のタップ Y 座標 |
| `mumu_clear_length` | `3` | 入力前に DEL を送る回数 |
| `mumu_pre_keycode` | `"KEYCODE_P"` | チャンネル入力欄を開くキーコード（P キー） |
| `mumu_delay_ms` | `800` | ADB コマンド間のウェイト (ms) |

### チャンネル巡回設定

| キー | デフォルト | 説明 |
|---|---|---|
| `patrol_channels_file` | `"channels.txt"` | 巡回チャンネルリストのファイルパス |
| `patrol_dwell_secs` | `60` | 移動完了後〜次 Ch 移動までの滞在秒数 ★ |
| `patrol_move_timeout_secs` | `30` | `[0x2E]` パケットを待つ最大秒数。0 = 無効 ★ |
| `parallel_limit` | `0` | 同時切替の最大台数。0 = 全台同時（グループ間ディレイも無効）★ |
| `parallel_group_delay_secs` | `0` | グループ間の待機秒数。`parallel_limit > 0` のとき有効 ★ |
| `patrol_serials` | `[]` | 巡回に使う ADB シリアル一覧。空で全デバイスを使用 |

> **グループ切替について**  
> `parallel_limit = 2`、デバイスが 8 台の場合、2 台ずつ 4 グループに分けて順番に切り替えます。  
> グループ間に `parallel_group_delay_secs` のウェイトが入ります。  
> 切替中に届いた `[0x2E]` パケットも正しくカウントされます。

---

## ビルド方法（開発者向け）

### 前提条件

- Go 1.23 以上
- MinGW-w64 (GCC) が PATH に存在すること
- [Npcap SDK](https://npcap.com/#download) を `C:\npcap-sdk` に展開

### build.bat を使う（推奨）

```bat
build.bat
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
