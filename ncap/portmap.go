package ncap

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// PortMap はサーバーアドレス("ip:port")とch番号の対応を管理する。
// 巡回モードで訪れたchを自動記録し、ポートが変わった場合は自動更新する。
type PortMap struct {
	mu       sync.RWMutex
	portToCh map[string]uint32 // "ip:port" → ch
	file     string
}

// LoadPortMap は既存のJSONファイルを読み込んでPortMapを返す。
// ファイルが存在しない場合は空のPortMapを返す。
func LoadPortMap(path string) *PortMap {
	pm := &PortMap{
		portToCh: make(map[string]uint32),
		file:     path,
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if jsonErr := json.Unmarshal(data, &pm.portToCh); jsonErr != nil {
			log.Printf("[PortMap] 読み込みエラー (%s): %v", path, jsonErr)
		} else {
			log.Printf("[PortMap] %d件読み込み: %s", len(pm.portToCh), path)
		}
	}
	return pm
}

// LookupByPort はサーバーアドレスからch番号を返す。
func (pm *PortMap) LookupByPort(serverIP string) (uint32, bool) {
	if pm == nil || serverIP == "" {
		return 0, false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	ch, ok := pm.portToCh[serverIP]
	return ch, ok
}

// LookupByCh はch番号から現在登録されているサーバーアドレスを返す（逆引き）。
func (pm *PortMap) LookupByCh(ch uint32) (string, bool) {
	if pm == nil || ch == 0 {
		return "", false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for ip, c := range pm.portToCh {
		if c == ch {
			return ip, true
		}
	}
	return "", false
}

// Update はch番号に対応するサーバーアドレスを登録・更新する。
// ポートが前回と異なる場合は変更を検出してログ出力し、自動保存する。
// 変更がない場合は何もしない（高頻度呼び出しに対応）。
func (pm *PortMap) Update(ch uint32, serverIP string) {
	if pm == nil || ch == 0 || serverIP == "" {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 既存エントリの確認
	if existing, ok := pm.portToCh[serverIP]; ok && existing == ch {
		return // 変更なし
	}

	// 現在 ch に対応している古いポートを検索して削除
	for ip, c := range pm.portToCh {
		if c == ch && ip != serverIP {
			log.Printf("[PortMap] ★ ch=%d ポート変更検出: %s → %s", ch, ip, serverIP)
			delete(pm.portToCh, ip)
			break
		}
	}

	if _, existed := pm.portToCh[serverIP]; !existed {
		log.Printf("[PortMap] ch=%d 登録: %s", ch, serverIP)
	}
	pm.portToCh[serverIP] = ch
	pm.save()
}

// Count は登録件数を返す。
func (pm *PortMap) Count() int {
	if pm == nil {
		return 0
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.portToCh)
}

// save はWriteロック保持中に呼び出す。
func (pm *PortMap) save() {
	data, err := json.MarshalIndent(pm.portToCh, "", "  ")
	if err != nil {
		log.Printf("[PortMap] JSON変換エラー: %v", err)
		return
	}
	if err := os.WriteFile(pm.file, data, 0644); err != nil {
		log.Printf("[PortMap] 保存失敗: %v", err)
	}
}
