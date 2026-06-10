// cmd/pcap-record/main.go
// スタンドアロン pcap 録画ツール。本体アプリと並行起動可能（Npcap は同一 NIC の複数ハンドル可）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/ncap"
	"github.com/google/gopacket/pcap"
	"github.com/google/gopacket/pcapgo"
)

func main() {
	outFlag := flag.String("out", "", "出力先 pcap ファイル。省略時は logs/capture_yyyyMMdd_HHmmss.pcap")
	ifaceFlag := flag.String("iface", "", "NIC の Description 完全一致。省略時は自動選択")
	durationFlag := flag.Int("duration", 0, "録画秒数。0 = Ctrl+C まで（デフォルト 0）")
	snaplenFlag := flag.Int("snaplen", 10485760, "スナップ長（デフォルト 10485760 = 本番と同一）")
	flag.Parse()

	// --- NIC 決定 ---
	devices, err := pcap.FindAllDevs()
	if err != nil {
		log.Fatalf("NIC 一覧取得失敗: %v", err)
	}
	if len(devices) == 0 {
		log.Fatal("pcap インターフェースが見つかりません（Npcap/WinPcap 必須）")
	}

	selectedDesc := *ifaceFlag
	if selectedDesc == "" {
		log.Printf("NIC を自動選択中（3秒サンプリング）...")
		if active := ncap.GetActiveNetworkCards(devices, 3); active != nil {
			log.Printf("自動選択: %s (pkts=%d bytes=%d)", active.Desc, active.PacketCount, active.ByteCount)
			selectedDesc = active.Desc
		} else {
			log.Fatal("アクティブな NIC が見つかりませんでした")
		}
	}

	// Description → Name 変換して OpenLive
	nicName := ""
	for _, d := range devices {
		name := d.Description
		if name == "" {
			name = d.Name
		}
		if name == selectedDesc {
			nicName = d.Name
			break
		}
	}
	if nicName == "" {
		log.Fatalf("指定された NIC %q が見つかりません", selectedDesc)
	}

	handle, err := pcap.OpenLive(nicName, int32(*snaplenFlag), true, pcap.BlockForever)
	if err != nil {
		log.Fatalf("pcap.OpenLive 失敗: %v", err)
	}
	defer handle.Close()

	if err := handle.SetBPFFilter("ip and tcp"); err != nil {
		log.Fatalf("BPF フィルタ設定失敗: %v", err)
	}
	log.Printf("NIC: %s  snaplen: %d  BPF: ip and tcp", selectedDesc, *snaplenFlag)

	// --- 出力ファイル決定 ---
	outPath := *outFlag
	if outPath == "" {
		if err := os.MkdirAll("logs", 0755); err != nil {
			log.Fatalf("logs ディレクトリ作成失敗: %v", err)
		}
		outPath = fmt.Sprintf("logs/capture_%s.pcap", time.Now().Format("20060102_150405"))
	}

	f, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("出力ファイル作成失敗 %s: %v", outPath, err)
	}
	defer f.Close()

	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(uint32(*snaplenFlag), handle.LinkType()); err != nil {
		log.Fatalf("pcap ヘッダ書き込み失敗: %v", err)
	}
	log.Printf("録画開始: %s", outPath)

	// --- 書き込み直列化用 mutex ---
	var mu sync.Mutex
	var packetCount int64
	var byteCount int64

	// --- 停止チャネル ---
	stopCh := make(chan struct{})

	// Ctrl+C ハンドリング
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// duration タイマー
	var timerCh <-chan time.Time
	if *durationFlag > 0 {
		timerCh = time.After(time.Duration(*durationFlag) * time.Second)
		log.Printf("録画時間: %d 秒", *durationFlag)
	}

	// 停止シグナル待機 goroutine
	go func() {
		select {
		case <-sigCh:
			log.Println("Ctrl+C 受信 — 録画停止中...")
		case <-timerCh:
			log.Printf("%d 秒経過 — 録画停止中...", *durationFlag)
		}
		close(stopCh)
		handle.Close() // ReadPacketData のブロックを解除
	}()

	// 進捗ログ goroutine（10秒毎）
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				mu.Lock()
				pc := packetCount
				bc := byteCount
				mu.Unlock()
				log.Printf("進捗: %d パケット / %.2f MB", pc, float64(bc)/1024/1024)
			}
		}
	}()

	// --- パケット録画ループ ---
	for {
		data, ci, err := handle.ReadPacketData()
		if err != nil {
			// handle.Close() 後のエラー or EOF → 正常終了
			break
		}

		mu.Lock()
		if werr := w.WritePacket(ci, data); werr != nil {
			mu.Unlock()
			log.Fatalf("パケット書き込み失敗: %v", werr)
		}
		packetCount++
		byteCount += int64(len(data))
		mu.Unlock()
	}

	// flush（os.File は明示的な flush 不要だが sync で安全に）
	if err := f.Sync(); err != nil {
		log.Printf("ファイル sync 警告: %v", err)
	}

	mu.Lock()
	pc := packetCount
	bc := byteCount
	mu.Unlock()

	log.Printf("録画完了: %s", outPath)
	log.Printf("  パケット数: %d", pc)
	log.Printf("  合計サイズ: %.2f MB", float64(bc)/1024/1024)
}
