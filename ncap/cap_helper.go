package ncap

import (
	"fmt"
	"log"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

type InterfaceStats struct {
	Name        string `json:"name"`
	Desc        string `json:"desc"`
	PacketCount int    `json:"packet_count"`
	ByteCount   int64  `json:"byte_count"`
}

// InterfaceInfo は GUI 向けの NIC 情報（説明・名前・IPアドレス）。
type InterfaceInfo struct {
	Name  string   `json:"name"`
	Desc  string   `json:"desc"`
	Addrs []string `json:"addrs"`
}

// ListInterfaces は利用可能な全 NIC を列挙する（サンプリングなし・即時）。
// Desc は main.go の openByDescription のマッチキーと同じ（空のときは Name にフォールバック）。
func ListInterfaces() ([]InterfaceInfo, error) {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceInfo, 0, len(devices))
	for _, d := range devices {
		info := InterfaceInfo{Name: d.Name, Desc: d.Description}
		for _, a := range d.Addresses {
			if a.IP != nil {
				info.Addrs = append(info.Addrs, a.IP.String())
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// GetActiveNetworkCards 現在使用可能なネットワークカードを取得する
func GetActiveNetworkCards(devices []pcap.Interface, autoCheckTime int) *InterfaceStats {
	if len(devices) == 0 {
		log.Fatal("ネットワークカードが見つかりません")
	}
	checkTime := autoCheckTime
	if 1 > checkTime {
		checkTime = 3
	}
	log.Println(fmt.Sprintf("すべてのネットワークカードフローの監視を開始します。%d秒お待ちください", checkTime))
	stats := make(map[string]*InterfaceStats)
	done := make(chan bool)
	for _, device := range devices {
		stats[device.Name] = &InterfaceStats{
			Name:        device.Name,
			Desc:        device.Description,
			PacketCount: 0,
			ByteCount:   0,
		}
		go monitorInterface(device.Name, stats[device.Name], done)
	}
	time.Sleep(time.Duration(checkTime) * time.Second)
	close(done) //关掉

	time.Sleep(100 * time.Millisecond)

	var maxPackets int
	var maxBytes int64
	var activeInterface *InterfaceStats

	for _, stat := range stats {
		if stat.PacketCount > maxPackets || (stat.PacketCount == maxPackets && stat.ByteCount > maxBytes) {
			maxPackets = stat.PacketCount
			maxBytes = stat.ByteCount
			activeInterface = stat
		}
	}
	if activeInterface != nil && activeInterface.PacketCount > 0 {
		return activeInterface
	} else {
		return nil
	}
}

func monitorInterface(deviceName string, stats *InterfaceStats, done chan bool) {
	// ネットワークカードを開いてパケットをキャプチャする
	handle, err := pcap.OpenLive(deviceName, 1600, true, pcap.BlockForever)
	if err != nil {
		// 一部のネットワークカードは開く可能性がありません。静かに無視してください
		return
	}
	defer handle.Close()

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	packets := packetSource.Packets()

	for {
		select {
		case <-done:
			return
		case packet := <-packets:
			if packet != nil {
				stats.PacketCount++
				stats.ByteCount += int64(len(packet.Data()))
			}
		}
	}
}
