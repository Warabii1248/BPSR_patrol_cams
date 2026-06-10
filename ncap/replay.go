package ncap

import (
	"log"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

// ReplayFile は .pcap ファイルを開き、全パケットを handlePacket へ同期投入する。
// Start() と異なり queue / cleanupSessions goroutine を使わず、EOF で正常 return する。
//
// 注意: handlePacket 内のコールバック発火（onLineIDObserved / onPostLoadReady / notifyFn）は
// go ディスパッチのため、return 時点で全コールバックの完了は保証されず、発火順序も
// 非決定的。呼出側はイベント数の静止判定で完了を待ち、比較は順序非依存（マルチセット）で
// 行うこと（cmd/pcap-replay 参照）。
func (cd *CapDevice) ReplayFile(path string) (packets int, err error) {
	handle, err := pcap.OpenOffline(path)
	if err != nil {
		return 0, err
	}
	defer handle.Close()
	// BPF はノイズ削減の最適化に過ぎないため、リンクタイプ非対応等で失敗しても続行する
	if err := handle.SetBPFFilter("ip and tcp"); err != nil {
		log.Printf("[ReplayFile] BPF フィルタ設定失敗（続行）: %v", err)
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	for packet := range src.Packets() {
		if packet == nil {
			continue
		}
		cd.handlePacket(packet)
		packets++
	}
	return packets, nil
}
