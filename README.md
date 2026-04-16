# LoyalBoarlet Monitor

[balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI) をベースに改変・拡張したツールです。

スターレゾナンスで複数エミュレータを使い、ゴールドウリボを自動検知して Discord に通知します。  
検知したチャンネルは巡回リストから自動削除され、次のチャンネルへ進みます。

---

# ///工事中///

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
