# Level 2: pcap 録画・replay によるパケット解析層の検証 実装仕様書

目的: ゲームアップデートによるパース層破壊（No.23/34 型）と ncap 内部リグレッション
（session merge / donor 補完 / UID 抽出）を実機 8 台なしで検知する。

## 調査結果（前提）

- `NewCapDevice(device *pcap.Handle, deviceName string)` はハンドル注入式（cap_device.go:227）
- 本番ハンドル: `pcap.OpenLive(name, 1024*1024*10, true, BlockForever)` + BPF `"ip and tcp"`（main.go:622, cap_device.go:587）
- NIC 自動選択は exported: `ncap.GetActiveNetworkCards(devices, autoCheckTime)`（cap_helper.go:20）
- `Start()` の packet ループは EOF で `log.Fatalf` する（cap_device.go:611）→ replay に使えない
- `handlePacket(packet gopacket.Packet)` は unexported（cap_device.go:947）→ **ncap パッケージ内の新規ファイルから同期 pump する**
- `SetPortMapFile` を呼ばなければ `cd.portMap == nil` で portmap 経路は安全に無効（cap_device.go:2206）

**既存ファイルは 1 行も変更しない。追加のみ:**
- `ncap/replay.go`（新規・ncap パッケージ内）
- `cmd/pcap-record/main.go`（新規バイナリ）
- `cmd/pcap-replay/main.go`（新規バイナリ）
- `.gitignore`（新規: `*.pcap` — チャット平文を含むためリポジトリ混入防止）

## Phase D: cmd/pcap-record（録画・最優先）

本体アプリと並行起動可能なスタンドアロン録画ツール（Npcap は同一 NIC の複数ハンドル可）。
**今日のゲームアップデート前にベースライン録画を開始できることが最優先。**

```
cmd/pcap-record/main.go（~130 行）

フラグ:
  -out string      出力先。default "" = logs/capture_yyyyMMdd_HHmmss.pcap 自動命名
  -iface string    NIC の Description 完全一致。default "" = 自動選択
                   （pcap.FindAllDevs → ncap.GetActiveNetworkCards(devices, 3)）
  -duration int    録画秒数。0 = Ctrl+C まで（default 0）
  -snaplen int     default 10485760（本番と同一）

動作:
  1. NIC 決定（自動選択時は本体と同じサンプリング方式）
  2. pcap.OpenLive(name, snaplen, true, pcap.BlockForever)
  3. handle.SetBPFFilter("ip and tcp")            ← 本番と同一
  4. pcapgo.NewWriter(f) + WriteFileHeader(uint32(snaplen), handle.LinkType())
  5. ループ: handle.ReadPacketData() → w.WritePacket(ci, data)
  6. 10 秒毎に進捗ログ（packets / MB）
  7. os/signal (Ctrl+C) または -duration 経過で flush & close → 最終サマリ表示

エラー方針: 書き込み失敗は即 fatal（録れていないのに録れたつもりを防ぐ）
```

## Phase E: ncap/replay.go + cmd/pcap-replay（再生・golden 比較）

### ncap/replay.go（新規ファイル・~60 行）

```go
package ncap

// ReplayFile は .pcap ファイルを開き、全パケットを handlePacket へ同期投入する。
// Start() と異なり queue / cleanupSessions goroutine を使わず、EOF で正常 return する。
// 同期処理のためイベント発火順序は決定的（golden 比較可能）。
func (cd *CapDevice) ReplayFile(path string) (packets int, err error) {
    handle, err := pcap.OpenOffline(path)
    if err != nil { return 0, err }
    defer handle.Close()
    if err := handle.SetBPFFilter("ip and tcp"); err != nil { return 0, err }
    src := gopacket.NewPacketSource(handle, handle.LinkType())
    for packet := range src.Packets() {
        if packet == nil { continue }
        cd.handlePacket(packet)
        packets++
    }
    return packets, nil
}
```

注意: `cd.device` は使わない（nil で良い）。`Start()` は呼ばない。

### cmd/pcap-replay/main.go（~250 行）

```
フラグ:
  -pcap string           入力 .pcap（必須）
  -record-golden string  イベント列を JSONL で書き出し
  -golden string         JSONL と比較、差分表示、exit 0/1
  -portmap string        default "" = portmap 無効。指定時は一時ディレクトリへ
                         コピーしてから SetPortMapFile（本番 config/ を汚染しない）
  -v                     全イベントを標準出力へ

動作:
  1. cd := ncap.NewCapDevice(nil, "replay")
  2. コールバック登録（seq カウンタ付きイベント収集）:
     SetLineIDObserver        → {"seq":N,"ev":"lineid","uid":U,"line_id":L}
     SetPostLoadReadyCallback → {"seq":N,"ev":"postload","uid":U,"line_id":L}
     SetChatNotifier          → {"seq":N,"ev":"chat","uid":U,"ch":C,"has_ch":B,"sender_hash":H,"msg_hash":H}
     SetNotifier              → {"seq":N,"ev":"detect","monster_id":...}
  3. n, err := cd.ReplayFile(*pcapPath)
  4. サマリ表示:
     - packets / sessions（GetCaptureSessions）/ UID 確定数 / lineID 確定数
     - イベント件数内訳（lineid / postload / chat / detect）
     - ★ UID 確定数 0 または lineid イベント 0 の場合は警告（パース層破壊の兆候）
  5. golden 比較: seq 順・全フィールド一致（時刻系フィールドは記録しない設計）
     差分は ev 単位で表示（expected/actual）

決定性の担保:
  - ReplayFile は同期 pump → 順序決定的
  - 時刻（changedAt / t）は golden に含めない（wall clock 依存）
  - debounce は wall clock 依存 → detect イベントは golden 比較からデフォルト除外
    （-strict-detect フラグで含める）
  - chat の sender/message は SHA-256 先頭 8 桁ハッシュ（プライバシー + 一致比較は可能）
```

## 運用フロー（ゲームアップデート対応）

```powershell
# 1. アップデート前（今日・最優先）: ベースライン録画
#    本体アプリ起動中のまま並行実行可。ログイン → ch 切替数回 → チャット受信 を含めて 5〜10 分
.\release\pcap-record.exe                      # logs/capture_*.pcap に保存

# 2. golden 生成
.\release\pcap-replay.exe -pcap logs\capture_XXXX.pcap -record-golden testdata\golden_vOLD.jsonl

# 3. アップデート後: 新バージョンで再録画 → サマリ確認
.\release\pcap-record.exe
.\release\pcap-replay.exe -pcap logs\capture_YYYY.pcap
#    → 「UID 確定数 0」「lineid イベント 0」警告が出れば No.23/34 型のパース層破壊
#    → 出なければパース層は生存。差分調査は -v で

# 4. 以後のコード変更時の回帰テスト
.\release\pcap-replay.exe -pcap logs\capture_XXXX.pcap -golden testdata\golden_vOLD.jsonl
```

## ビルド

`build.ps1` は触らず、単発ビルドで対応:
```powershell
go build -o release\pcap-record.exe .\cmd\pcap-record
go build -o release\pcap-replay.exe .\cmd\pcap-replay
```
（CGO/Npcap 依存は本体と同条件。後で build.ps1 統合は任意）

## 制約・禁止事項（実装 subagent 向け）

- **ncap/ の既存ファイル（cap_device.go / cap_helper.go / portmap.go / byte_reader.go / queue.go）は 1 行も変更しない**。必要が生じたら実装中断して報告
- pb/ は参照のみ（編集禁止）
- 本番 `config/` 配下への書き込み禁止（portmap は一時コピー方式）
- replay 中に Discord 通知が飛んではならない → `SetNotifier` はイベント収集のみ（notifier パッケージの送信関数を呼ばない）
- golden に生チャット文・生 UID 以外の個人情報を含めない（sender/msg はハッシュ）

## changelog 採番

- Phase D: No.57
- Phase E: No.58
