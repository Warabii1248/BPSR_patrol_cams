# BPSR Patrol Cams

## プロジェクト概要

ゲーム「Blue Protocol Star Resonance」のネットワークパケットをキャプチャし、レアモンスター出現を検知してDiscord通知を送るWindows向けGoアプリケーション。MuMuエミュレーター（ADB）を使ったチャンネル自動巡回機能も備える。


## Claude への指示（最優先）

- 質問・要望が曖昧な場合は、実装前に必要な情報が揃うまで質問を返すこと
- 思考中に方針変更・別案の可能性が浮上した場合は、勝手に進めず即座に報告すること
- 複数ファイルにまたがる変更を行う前に、変更計画を提示して確認を取ること
- 既存のパケット解析ロジック（`ncap/`・`pb/`）を変更する場合は特に慎重に確認を取ること
- `config/` 配下のファイル構造・キー名を変更する場合は後方互換性への影響を必ず指摘すること


## アーキテクチャ

```
main.go             エントリーポイント・全コンポーネント結線
ncap/               ネットワークキャプチャ（gopacket/pcap）
appconfig/          設定管理（config.json + filter.json の読み書き）
gui/                WebGUI サーバー（WebView2 + SSE）
mumu/               MuMuエミュレーター ADB自動化
notifier/           Discord Webhook 通知
location/           マップ地点名ストア
gasfetch/           Google Apps Script 討伐タイマー連携（現在未使用）
global/             キャッシュ・モンスター名DB
pb/                 Protobuf定義（パケット解析用）
gas_extension/      Chrome拡張（GASページスクレイプ → Go連携）
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

## GAS連携（Chrome拡張）

討伐タイマーサイト（Google Apps Script製）のチャンネル情報を巡回リストに反映する仕組み。

**仕組み:**
1. `gas_extension/content.js` (Chrome拡張) がGASページをDOMスクレイプ
2. 閾値（20h）以上経過した全エネミーのchをエネミー名付きでPOST
3. Go側でフィルタして `channels.txt` を更新

**送信フォーマット:**
```json
POST http://localhost:8080/api/patrol/channels/gas
{"entries": [{"channel": 5, "enemy": "金ウリボ"}, {"channel": 3, "enemy": "金ナッポ"}]}
```

**設定項目（config.json）:**

| キー | 説明 | デフォルト |
|---|---|---|
| `gas_target_enemy` | 取得対象エネミー名（部分一致） | `"金ウリボ"` |

GUI設定の「GAS 対象エネミー」ドロップダウンで切り替え可能（再起動不要）。

**Chrome拡張の使い方:**
- `gas_extension/` フォルダを Chrome の「パッケージ化されていない拡張機能を読み込む」で追加
- GASページを開いたままにしておくと10分ごとに自動送信される

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