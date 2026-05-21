package ncap

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// PortMapEntryInfo はポートマップの1エントリを表す（GUI向け）。
type PortMapEntryInfo struct {
	Ch        uint32    `json:"ch"`
	ServerIP  string    `json:"server_ip"`
	UpdatedAt time.Time `json:"updated_at"`
}

// portMapEntry はJSONファイル保存用の内部エントリ。
type portMapEntry struct {
	Ch        uint32    `json:"ch"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PortMap はサーバーアドレス("ip:port")とch番号の対応を管理する。
// 巡回モードで訪れたchを自動記録し、ポートが変わった場合は自動更新する。
type PortMap struct {
	mu        sync.RWMutex
	portToCh  map[string]uint32    // "ip:port" → ch
	updatedAt map[string]time.Time // "ip:port" → 最終更新時刻
	file      string
}

// LoadPortMap は既存のJSONファイルを読み込んでPortMapを返す。
// 新フォーマット {"ip:port": {"ch": N, "updated_at": "..."}} と
// 旧フォーマット {"ip:port": N} の両方に対応する。
func LoadPortMap(path string) *PortMap {
	pm := &PortMap{
		portToCh:  make(map[string]uint32),
		updatedAt: make(map[string]time.Time),
		file:      path,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pm
	}

	// 新フォーマットを試みる
	newFmt := make(map[string]portMapEntry)
	if jsonErr := json.Unmarshal(data, &newFmt); jsonErr == nil && isNewFormat(newFmt) {
		for ip, e := range newFmt {
			pm.portToCh[ip] = e.Ch
			pm.updatedAt[ip] = e.UpdatedAt
		}
		log.Printf("[PortMap] %d件読み込み: %s", len(pm.portToCh), path)
		return pm
	}

	// 旧フォーマット {"ip:port": N} にフォールバック
	oldFmt := make(map[string]uint32)
	if jsonErr := json.Unmarshal(data, &oldFmt); jsonErr != nil {
		log.Printf("[PortMap] 読み込みエラー (%s): %v", path, jsonErr)
		return pm
	}
	for ip, ch := range oldFmt {
		pm.portToCh[ip] = ch
	}
	// 旧フォーマット: ファイル更新日時をタイムスタンプとして使う（仕方なし）
	if fi, statErr := os.Stat(path); statErr == nil {
		mt := fi.ModTime()
		for ip := range pm.portToCh {
			pm.updatedAt[ip] = mt
		}
	}
	log.Printf("[PortMap] %d件読み込み (旧フォーマット→自動移行): %s", len(pm.portToCh), path)
	// 移行後すぐ新フォーマットで上書き保存
	pm.save()
	return pm
}

// isNewFormat は新フォーマット判定（空マップの場合は新フォーマット扱い）。
func isNewFormat(m map[string]portMapEntry) bool {
	for _, e := range m {
		if e.Ch > 0 {
			return true
		}
	}
	// すべてChが0の場合は旧フォーマットの可能性があるが、エントリなしなら新扱いで問題なし
	return true
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
			delete(pm.updatedAt, ip)
			break
		}
	}

	if _, existed := pm.portToCh[serverIP]; !existed {
		log.Printf("[PortMap] ch=%d 登録: %s", ch, serverIP)
	}
	pm.portToCh[serverIP] = ch
	pm.updatedAt[serverIP] = time.Now()
	pm.save()
}

// Entries は全エントリを PortMapEntryInfo のスライスとして返す。
func (pm *PortMap) Entries() []PortMapEntryInfo {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]PortMapEntryInfo, 0, len(pm.portToCh))
	for ip, ch := range pm.portToCh {
		out = append(out, PortMapEntryInfo{
			Ch:        ch,
			ServerIP:  ip,
			UpdatedAt: pm.updatedAt[ip],
		})
	}
	return out
}

// save はWriteロック保持中に呼び出す。
func (pm *PortMap) save() {
	out := make(map[string]portMapEntry, len(pm.portToCh))
	for ip, ch := range pm.portToCh {
		out[ip] = portMapEntry{
			Ch:        ch,
			UpdatedAt: pm.updatedAt[ip],
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Printf("[PortMap] JSON変換エラー: %v", err)
		return
	}
	if err := os.WriteFile(pm.file, data, 0644); err != nil {
		log.Printf("[PortMap] 保存失敗: %v", err)
	}
}
