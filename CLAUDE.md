# BPSR Patrol Cams

## プロジェクト概要

ゲーム「Blue Protocol Star Resonance」のネットワークパケットをキャプチャし、レアモンスター出現を検知してDiscord通知を送るWindows向けGoアプリケーション。MuMuエミュレーター（ADB）を使ったチャンネル自動巡回機能も備える。

## アーキテクチャ

```
main.go             エントリーポイント・全コンポーネント結線
ncap/               ネットワークキャプチャ（gopacket/pcap）
appconfig/          設定管理（config.json + filter.json の読み書き）
gui/                WebGUI サーバー（WebView2 + SSE）
mumu/               MuMuエミュレーター ADB自動化
notifier/           Discord Webhook 通知
location/           マップ地点名ストア
gasfetch/           Google Apps Script 討伐タイマー連携
global/             キャッシュ・モンスター名DB
pb/                 Protobuf定義（パケット解析用）
```

## ビルド方法

```powershell
# リリースビルド
.\build.ps1

# デバッグビルド（コンソール表示あり）
.\build.ps1 -Debug

# Npcap SDK パスを指定する場合
.\build.ps1 -NpcapSdk "C:\npcap-sdk"
```

**必須要件:**
- Go 1.23+
- MinGW-w64 (GCC) — PATHに含める
- Npcap SDK (`C:\npcap-sdk` または `-NpcapSdk` で指定)
- CGO_ENABLED=1（自動設定される）

**ターゲットPC実行時:** Npcap (https://npcap.com/) のインストールが必要。WinPcap API-compatible Mode を有効にする。

## 設定ファイル

| ファイル | 内容 |
|---|---|
| `config/config.json` | 全体設定（Network, Discord Webhook, ADB設定など） |
| `config/filter.json` | チャットフィルター設定（chat_exclude, report_senders等） |
| `config/channels.txt` | 巡回するチャンネル番号一覧（1行1ch） |
| `config/port_ch_map.json` | サーバーIP → チャンネル番号マッピング |
| `data/locations.json` | マップ地点名データ |

config.json とfilter.json は分離管理: `appconfig.Save()` は両方に分けて書き込む。

## 検知対象モンスター

- **ウリボ・ゴールド** (`LoyalBoarletTemplateID`)
- **金ナッポ** (`GoldNappoTemplateID`)
- **銀ナッポ** (`SilverNappoTemplateID`)

通知対象はアステリア平原（mapID=7）のch 1〜100のみ。ch 101以上はスキップ。

## 主要な起動フラグ

```
--config        config.jsonのパス (デフォルト: config/config.json)
--network       NIC説明文 ("auto" で自動選択)
--webhook       Discord Webhook URL（config.jsonを上書き）
--auto-check    auto選択時のサンプリング秒数
--ch-debug      全methodId・シーン変更パケットhexをログ出力
--ch-hunt N     全パケットからvarint Nを再帰スキャン
--ch-watch      候補パスの値変化をログ出力
--ch-map-file   ポート→ch対応収集モード（ターミナルからch番号入力）
```

## バージョン注入

```
go build -ldflags "-X main.Version=x.y.z"
```
`build.ps1` の `$Version` 変数で管理（現在 `2.0.0`）。

## ログ

`logs/log.txt` に起動ごと上書き保存。コンソールとファイルに同時出力（`io.MultiWriter`）。GUI SSEにも転送される。

## 主要な依存パッケージ

- `github.com/google/gopacket` — pcapパケットキャプチャ
- `github.com/AlecAivazis/survey/v2` — インタラクティブNIC選択
- `github.com/jchv/go-webview2` — WebView2 GUIウィンドウ
- `google.golang.org/protobuf` — パケットのProtobuf解析
- `github.com/klauspost/compress` — 圧縮

## 注意事項

- Windowsのみ対応（Npcap/WinPcap依存、WebView2依存）
- CGOが必須なため、クロスコンパイル不可
- ADB機能にはMuMuエミュレーター + adb.exe が必要
- `gui_port: 0` でGUI無効化（ヘッドレス動作）
