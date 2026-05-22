// Package gui はローカルHTTPサーバーとして動作するWebベースGUIを提供する。
// Edge WebView2を使った専用ウィンドウで表示する。
package gui

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"

	"github.com/balrogsxt/StarResonanceAPI/appconfig"
	"github.com/balrogsxt/StarResonanceAPI/debuglog"
	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
)

// DeviceSessionInfo はキャプチャセッション情報をGUIに渡す型
type DeviceSessionInfo struct {
	Label     string
	ClientIP  string
	UserUID   uint64
	MapID     uint32
	LineID    uint32
	CurrentCh uint32
	Confirmed bool
}

// Server はGUI用HTTPサーバー
type Server struct {
	port               int
	patroller          *mumu.Patroller
	tapLooper          *mumu.TapLooper
	patrolEnabled      bool
	patrolChannels     []uint32 // 起動時に設定から読み込んだチャンネルリスト
	patrolChannelsFile string   // channels.txt パス（ホットリロード用）
	goldHistoryFile    string
	getSessions        func() []DeviceSessionInfo
	testDetectFn       func(string)
	saveChannelsFn     func([]uint32) error
	channelNotifyFn    func(uint32) // Patrollerのチャンネル切替時コールバック
	getConfigFn        func() ([]byte, error)
	saveConfigFn       func([]byte) error
	loadWindowStateFn  func() (*appconfig.WindowState, error)
	saveWindowStateFn  func(*appconfig.WindowState) error
	getPortMapFn       func() []PortMapEntry // ポートマップ全件取得
	mapChFn            func(ch uint32)       // 現在セッションを指定chにマッピング
	mapAllFn           func() int            // 全セッションをLineIDでマッピング

	mu              sync.RWMutex
	logLines        []string             // 検知ログ（最大200件）
	clients         []chan string        // SSEクライアント
	goldBoarHistory []GoldBoarEvent      // 金ウリボ検知履歴（最大50件）
	cooldownChs     map[uint32]time.Time // 発見済みch → 除外期限（発見から30分）

	chatMu      sync.RWMutex
	chatLog     []ChatEvent   // チャットログ（最大500件）
	chatClients []chan string // チャットSSEクライアント

	gasTargetEnemy     string // Chrome拡張から受信したchのフィルタ対象エネミー名
	gasEnable          bool   // Chrome拡張からの GAS 連携 POST を受け入れるか
	showNoDeviceDialog bool   // 起動時デバイス未検出ダイアログを表示するか

	pendingPortMapMu  sync.Mutex
	pendingPortMaps   []PendingPortMapChange
	pendingPortMapSeq int
	portMapApplyFn    func(ch uint32, serverIP string)

	chatReportFn      func(notifier.Detection)
	chatReportDedupMu sync.Mutex
	chatReportDedup   map[string]time.Time

	getLabelByIPFn func(string) string // デバイスIP → インスタンスラベル解決コールバック

	identifyRunning bool   // デバイス識別フェーズ実行中フラグ
	assignmentsFile string // device_assignments.json のパス
}

// PortMapEntry はポートマップの1エントリ（GUI向け）
type PortMapEntry struct {
	Ch        uint32    `json:"ch"`
	ServerIP  string    `json:"server_ip"`
	UpdatedAt time.Time `json:"updated_at"`
}

// normalizeMonsterName は検知モンスター名を「金ウリボ」「金ナッポ」「銀ナッポ」の3種に正規化する
func normalizeMonsterName(name string) string {
	if strings.Contains(name, "銀ナッポ") {
		return "銀ナッポ"
	}
	if strings.Contains(name, "金ナッポ") || strings.Contains(name, "ナッポ") {
		return "金ナッポ"
	}
	return "金ウリボ"
}

// GoldBoarEvent は金ウリボ検知の1件分の記録
type GoldBoarEvent struct {
	Time        string `json:"time"`
	Timestamp   int64  `json:"timestamp"`
	Channel     uint32 `json:"channel"`
	Location    string `json:"location"`
	MonsterName string `json:"monster_name"`
}

// PendingPortMapChange はユーザーの確認待ちの portMap 変更1件を表す。
type PendingPortMapChange struct {
	ID        string    `json:"id"`
	Ch        uint32    `json:"ch"`
	NewIP     string    `json:"new_ip"`
	OldIP     string    `json:"old_ip"`
	VoteCount int       `json:"vote_count"`
	ArrivedAt time.Time `json:"arrived_at"`
}

// ChatEvent はチャット受信の1件分の記録
type ChatEvent struct {
	Time     string `json:"time"`
	ClientIP string `json:"client_ip"`
	Label    string `json:"label"`
	UserUID  uint64 `json:"user_uid,omitempty"`
	Sender   string `json:"sender"`
	Message  string `json:"message"`
	Channel  uint32 `json:"channel"`
	HasCh    bool   `json:"has_ch"`
}

const maxGoldHistoryEntries = 50

// Options は GUI サーバーの挙動オプション。
type Options struct {
	PatrolEnabled bool
}

// New はGUIサーバーを作成する
func New(port int, mumuCfg mumu.Config, patrolChannels []uint32, patrolChannelsFile string) *Server {
	return NewWithOptions(port, mumuCfg, patrolChannels, patrolChannelsFile, Options{PatrolEnabled: true})
}

// NewWithOptions はGUIサーバーをオプション付きで作成する。
func NewWithOptions(port int, mumuCfg mumu.Config, patrolChannels []uint32, patrolChannelsFile string, opts Options) *Server {
	historyFile := filepath.Join("config", "gold_history.json")
	if patrolChannelsFile != "" {
		historyFile = filepath.Join(filepath.Dir(patrolChannelsFile), "gold_history.json")
	}
	patrolEnabled := true
	if !opts.PatrolEnabled {
		patrolEnabled = false
	}
	s := &Server{
		port:               port,
		patroller:          mumu.NewPatroller(mumuCfg),
		tapLooper:          mumu.NewTapLooper(),
		patrolEnabled:      patrolEnabled,
		patrolChannels:     patrolChannels,
		patrolChannelsFile: patrolChannelsFile,
		goldHistoryFile:    historyFile,
		cooldownChs:        make(map[uint32]time.Time),
		gasTargetEnemy:     "金ウリボ",
	}
	if err := s.loadGoldHistoryFromDisk(); err != nil {
		log.Printf("[GUI] gold history load failed: %v", err)
	}
	// Patroller のチャンネル切替を channelNotifyFn に転送する
	if s.patrolEnabled {
		s.patroller.SetOnChannelSwitch(func(ch uint32) {
			s.mu.RLock()
			fn := s.channelNotifyFn
			s.mu.RUnlock()
			if fn != nil {
				fn(ch)
			}
		})
	} else {
		log.Printf("[GUI] patrol mode disabled")
	}
	return s
}

func clampGoldHistory(events []GoldBoarEvent) []GoldBoarEvent {
	if len(events) <= maxGoldHistoryEntries {
		return events
	}
	return events[len(events)-maxGoldHistoryEntries:]
}

func (s *Server) loadGoldHistoryFromDisk() error {
	if s.goldHistoryFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.goldHistoryFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var history []GoldBoarEvent
	if err := json.Unmarshal(data, &history); err != nil {
		return err
	}
	history = clampGoldHistory(history)
	for i := range history {
		history[i].MonsterName = normalizeMonsterName(history[i].MonsterName)
	}
	s.mu.Lock()
	s.goldBoarHistory = history
	s.mu.Unlock()
	return nil
}

func (s *Server) persistGoldHistoryLocked() error {
	if s.goldHistoryFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.goldBoarHistory, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.goldHistoryFile, data, 0644)
}

// appendLogLocked は s.mu を保持した状態で呼ぶこと。
// ログに1行追記し、上限200件を維持した上でSSEクライアント一覧のスナップショットを返す。
func (s *Server) appendLogLocked(line string) []chan string {
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	return clients
}

// broadcastSSE はSSEクライアントにメッセージをノンブロッキング送信する。
func broadcastSSE(clients []chan string, msg string) {
	for _, ch := range clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// writeJSON は Content-Type を設定してvをJSONエンコードして書き込む。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeOK は {"ok":true} のJSONレスポンスを書き込む。
func writeOK(w http.ResponseWriter) {
	writeJSON(w, map[string]bool{"ok": true})
}

// SetSessionProvider はADD ↔ UID 対応に使うセッション情報提供関数を設定する。
func (s *Server) SetSessionProvider(fn func() []DeviceSessionInfo) {
	s.getSessions = fn
}

// SetTestDetectFn はテスト通知ボタンから呼ばれるコールバックを設定する。
func (s *Server) SetTestDetectFn(fn func(string)) {
	s.testDetectFn = fn
}

// SetSaveChannelsFn はチャンネルリスト保存コールバックを設定する。
func (s *Server) SetSaveChannelsFn(fn func([]uint32) error) {
	s.saveChannelsFn = fn
}

// filterCooldown は s.mu を保持した状態で呼ぶこと。
// 発見から30分以内のクールダウン中チャンネルを除外して返す。
// 副作用として期限切れエントリをマップから削除する。
func (s *Server) filterCooldown(chs []uint32) []uint32 {
	if len(s.cooldownChs) == 0 {
		return chs
	}
	now := time.Now()
	for ch, exp := range s.cooldownChs {
		if now.After(exp) {
			delete(s.cooldownChs, ch)
		}
	}
	if len(s.cooldownChs) == 0 {
		return chs
	}
	filtered := make([]uint32, 0, len(chs))
	for _, ch := range chs {
		if exp, ok := s.cooldownChs[ch]; ok && now.Before(exp) {
			continue // クールダウン中 → 除外
		}
		filtered = append(filtered, ch)
	}
	return filtered
}

// SetGASTargetEnemy は Chrome拡張から受信するチャンネルのフィルタ対象エネミー名を設定する。
func (s *Server) SetGASTargetEnemy(target string) {
	s.mu.Lock()
	s.gasTargetEnemy = target
	s.mu.Unlock()
}

// SetGASEnable は Chrome拡張からの GAS 連携 POST を受け入れるかを設定する。
func (s *Server) SetGASEnable(v bool) {
	s.mu.Lock()
	s.gasEnable = v
	s.mu.Unlock()
}

// SetShowNoDeviceDialog は起動時デバイス未検出ダイアログの表示フラグを設定する。
func (s *Server) SetShowNoDeviceDialog(v bool) {
	s.mu.Lock()
	s.showNoDeviceDialog = v
	s.mu.Unlock()
}

// UpdateChannelsFromGAS は GAS から取得したチャンネルリストで巡回リストを上書きする。
// channels.txt に保存することで、Patroller のホットリロード機能が自動的に検知して反映する。
func (s *Server) UpdateChannelsFromGAS(chs []uint32) {
	s.mu.Lock()
	filtered := s.filterCooldown(chs)
	s.patrolChannels = filtered
	saveFn := s.saveChannelsFn
	s.mu.Unlock()
	if saveFn != nil {
		if err := saveFn(filtered); err != nil {
			log.Printf("[GASFetch] channels.txt 保存失敗: %v", err)
			return
		}
		excluded := len(chs) - len(filtered)
		if excluded > 0 {
			log.Printf("[GASFetch] channels.txt 更新: %d件 (%d件クールダウン除外)", len(filtered), excluded)
		} else {
			log.Printf("[GASFetch] channels.txt 更新: %d件", len(filtered))
		}
	}
}

// SetChannelNotifyFn は Patroller がチャンネルを切り替えるたびに呼ばれるコールバックを設定する。
// CapDevice に現在チャンネルを通知するために使用する。
func (s *Server) SetChannelNotifyFn(fn func(uint32)) {
	s.channelNotifyFn = fn
}

// SetConfigFns は config.json の読み書きコールバックを設定する。
func (s *Server) SetConfigFns(getFn func() ([]byte, error), saveFn func([]byte) error) {
	s.getConfigFn = getFn
	s.saveConfigFn = saveFn
}

// SetWindowStateFns はウィンドウ位置・サイズの読み書きコールバックを設定する。
func (s *Server) SetWindowStateFns(loadFn func() (*appconfig.WindowState, error), saveFn func(*appconfig.WindowState) error) {
	s.loadWindowStateFn = loadFn
	s.saveWindowStateFn = saveFn
}

// NotifyChMovePacket は ncap が [0x2E] パケットを受信したとき main.go から呼ぶ。
// label は "Instance-N" 形式のインスタンスラベル。巡回中であれば Patroller に転送する。
func (s *Server) NotifyChMovePacket(label string) {
	if !s.patrolEnabled {
		return
	}
	s.patroller.NotifyChMovePacket(label)
}

// NotifyLineIDChange は ncap が 0x15/0x16 で LineID を観測したとき main.go から呼ぶ。
// per-device CH 追跡のため Patroller に転送する（巡回中・停止中を問わない）。
func (s *Server) NotifyLineIDChange(uid uint64, lineID uint32, changedAt time.Time) {
	if s.patrolEnabled {
		s.patroller.NotifyLineIDChange(uid, lineID, changedAt)
	}
}



// SetExcludeUIDs はバインド候補から除外する UID リストを Patroller に設定する。
func (s *Server) SetExcludeUIDs(uids []uint64) {
	if s.patrolEnabled {
		s.patroller.SetExcludeUIDs(uids)
	}
}

// LoadSerialUIDMap は config.json の serial_to_uid を Patroller に初期設定する。
func (s *Server) LoadSerialUIDMap(m map[string]uint64) {
	if s.patrolEnabled {
		s.patroller.LoadSerialUIDMap(m)
	}
}

// SetSaveSerialUIDFn は serial↔UID バインド成立時のコールバックを Patroller に設定する。
func (s *Server) SetSaveSerialUIDFn(fn func(map[string]uint64)) {
	if s.patrolEnabled {
		s.patroller.SetSaveSerialUIDFn(fn)
	}
}

// LoadSerialLabelMap は config.json の serial_to_label を Patroller に初期設定する。
func (s *Server) LoadSerialLabelMap(m map[string]string) {
	if s.patrolEnabled {
		s.patroller.LoadSerialLabelMap(m)
	}
}

// SetSaveSerialLabelFn は serial→label の自動確立時のコールバックを Patroller に設定する。
func (s *Server) SetSaveSerialLabelFn(fn func(map[string]string)) {
	if s.patrolEnabled {
		s.patroller.SetSaveSerialLabelFn(fn)
	}
}

// UpdatePatrollerCfg は Patroller の設定をリアルタイムで更新する。
func (s *Server) UpdatePatrollerCfg(cfg mumu.Config) {
	if !s.patrolEnabled {
		return
	}
	s.patroller.UpdateConfig(cfg)
}

// appendDetectionHistoryLocked は検知イベントをログ・goldBoarHistory に追記する（mu 保持前提）。
// 通知の有無に関わらず共通して実行される処理。silent=true のとき音なし検知タグを使用する。
func (s *Server) appendDetectionHistoryLocked(det notifier.Detection, silent bool) ([]chan string, string) {
	tag := "[DETECTION]"
	if silent {
		tag = "[DETECTION:SILENT]"
	}
	line := fmt.Sprintf("[%s] %s %s", det.Time.Format("15:04:05"), notifier.Format(det), tag)
	detCh := det.LineID
	clients := s.appendLogLocked(line)
	monName := normalizeMonsterName(det.MonsterName)
	loc := det.Location
	if loc == "" {
		loc = "不明"
	}
	event := GoldBoarEvent{
		Time:        det.Time.Format("01/02 15:04:05"),
		Timestamp:   det.Time.Unix(),
		Channel:     detCh,
		Location:    loc,
		MonsterName: monName,
	}
	s.goldBoarHistory = append(s.goldBoarHistory, event)
	s.goldBoarHistory = clampGoldHistory(s.goldBoarHistory)
	if err := s.persistGoldHistoryLocked(); err != nil {
		log.Printf("[GUI] gold history save failed: %v", err)
	}
	return clients, line
}

// OnDetectHistoryOnly は検知履歴への記録のみ行い、巡回ch削除・クールダウンは行わない。
// Discord 通知対象外エネミー（Enabled=false）の検知時に使用する。
func (s *Server) OnDetectHistoryOnly(det notifier.Detection) {
	s.mu.Lock()
	clients, line := s.appendDetectionHistoryLocked(det, true)
	s.mu.Unlock()
	broadcastSSE(clients, line)
}

// OnDetect は検知イベントをGUIのログに追加するコールバック
func (s *Server) OnDetect(det notifier.Detection) {
	detCh := det.LineID // 検知されたチャンネル番号

	s.mu.Lock()
	clients, line := s.appendDetectionHistoryLocked(det, false)
	// 発見したchを30分間クールダウンに追加（GAS経由で再追加されないよう）
	s.cooldownChs[detCh] = time.Now().Add(30 * time.Minute)
	// 巡回チャンネルリストから該当chを削除
	newChs := make([]uint32, 0, len(s.patrolChannels))
	removed := false
	for _, pc := range s.patrolChannels {
		if pc == detCh {
			removed = true
			continue
		}
		newChs = append(newChs, pc)
	}
	if removed {
		s.patrolChannels = newChs
	}
	saveChannelsFn := s.saveChannelsFn
	s.mu.Unlock()

	// ロック外で log.Printf（guiWriter 経由で再ロックするためデッドロック回避）
	if removed {
		log.Printf("[GUI] 金ウリボ検知: Ch%d を巡回リストから削除 (残%d ch)", detCh, len(newChs))
		if saveChannelsFn != nil {
			if err := saveChannelsFn(newChs); err != nil {
				log.Printf("[GUI] channels.txt 保存失敗: %v", err)
			}
		}
	}
	// SSEで全クライアントに通知
	broadcastSSE(clients, line)
}

// AddLog は1行のログをGUIのSSEストリームとlogLinesに追加する
func (s *Server) AddLog(line string) {
	s.mu.Lock()
	clients := s.appendLogLocked(line)
	s.mu.Unlock()
	broadcastSSE(clients, line)
}

// SetPortMapApplyFn は portMap 変更の確認後に呼ぶコールバックを設定する。
func (s *Server) SetPortMapApplyFn(fn func(ch uint32, serverIP string)) {
	s.portMapApplyFn = fn
}

// SetGetPortMapFn はポートマップ全件取得コールバックを設定する。
func (s *Server) SetGetPortMapFn(fn func() []PortMapEntry) {
	s.getPortMapFn = fn
}

// SetAssignmentsFile はデバイス分担設定ファイルのパスを設定する。
func (s *Server) SetAssignmentsFile(file string) {
	s.mu.Lock()
	s.assignmentsFile = file
	s.mu.Unlock()
}

// LoadDeviceAssignments はデバイス分担設定をファイルから読み込み、Patrollerに反映する。
func (s *Server) LoadDeviceAssignments(file string) error {
	assignments, err := mumu.LoadDeviceAssignments(file)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.assignmentsFile = file
	s.mu.Unlock()
	s.patroller.SetDeviceAssignments(assignments)
	return nil
}

// SetMapChFn は現在セッションを指定chにマッピングするコールバックを設定する。
func (s *Server) SetMapChFn(fn func(ch uint32)) {
	s.mapChFn = fn
}

// SetMapAllFn は全セッションをLineIDでマッピングするコールバックを設定する。
func (s *Server) SetMapAllFn(fn func() int) {
	s.mapAllFn = fn
}

// SetChatReportFn はチャット候補確定時にDiscord通知を送るコールバックを設定する。
func (s *Server) SetChatReportFn(fn func(notifier.Detection)) {
	s.chatReportFn = fn
}

// SetGetLabelByIPFn はデバイスIP → インスタンスラベル解決コールバックを設定し、
// 巡回設定にも即時反映する。
func (s *Server) SetGetLabelByIPFn(fn func(string) string) {
	s.getLabelByIPFn = fn
	if s.patrolEnabled {
		cfg := s.patroller.Config()
		cfg.GetLabelByIP = fn
		s.patroller.UpdateConfig(cfg)
	}
}

// AddPortMapPending はクォーラム成立した portMap 変更を確認待ちキューに追加し、SSE で通知する。
// 同一 ch の既存エントリがあれば上書き（最新情報に差し替え）。
func (s *Server) AddPortMapPending(ch uint32, newIP, oldIP string, voteCount int) {
	s.pendingPortMapMu.Lock()
	s.pendingPortMapSeq++
	id := fmt.Sprintf("pm-%d", s.pendingPortMapSeq)
	change := PendingPortMapChange{
		ID:        id,
		Ch:        ch,
		NewIP:     newIP,
		OldIP:     oldIP,
		VoteCount: voteCount,
		ArrivedAt: time.Now(),
	}
	found := false
	for i, p := range s.pendingPortMaps {
		if p.Ch == ch {
			s.pendingPortMaps[i] = change
			found = true
			break
		}
	}
	if !found {
		s.pendingPortMaps = append(s.pendingPortMaps, change)
	}
	s.mu.RLock()
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	s.mu.RUnlock()
	s.pendingPortMapMu.Unlock()

	broadcastSSE(clients, "[PORTMAP_PENDING]")
}

// OnChat はチャット受信イベントをログに追加しSSEで配信する
func (s *Server) OnChat(clientIP, label string, userUID uint64, sender, message string, channel uint32, hasCh bool) {
	ev := ChatEvent{
		Time:     time.Now().Format("15:04:05"),
		ClientIP: clientIP,
		Label:    label,
		UserUID:  userUID,
		Sender:   sender,
		Message:  message,
		Channel:  channel,
		HasCh:    hasCh,
	}
	data, _ := json.Marshal(ev)
	jsonStr := string(data)

	s.chatMu.Lock()
	s.chatLog = append(s.chatLog, ev)
	if len(s.chatLog) > 500 {
		s.chatLog = s.chatLog[len(s.chatLog)-500:]
	}
	chatClients := make([]chan string, len(s.chatClients))
	copy(chatClients, s.chatClients)
	s.chatMu.Unlock()

	broadcastSSE(chatClients, jsonStr)
}

// guiWriter は標準 log 出力をGUIのSSEにも転送する io.Writer
type guiWriter struct {
	base io.Writer
	srv  *Server
	buf  bytes.Buffer
}

func (w *guiWriter) Write(p []byte) (int, error) {
	n, err := w.base.Write(p)
	w.buf.Write(p)
	for {
		b := w.buf.Bytes()
		idx := bytes.IndexByte(b, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(b[:idx]), "\r")
		w.buf.Next(idx + 1)
		if line != "" {
			w.srv.AddLog(line)
			// "[0x2E] UUID=" にのみ反応する:
			// - "[Instance-N][0x2E] UUID=..." は実パケット → カウント対象
			// - "[Instance-N][0x2E] lineID補完: ..." は補助情報 → 二重カウント防止
			// - "[MuMu] 巡回: Ch%d [0x2E] ..." は自ログ → フィードバックループ防止
			if w.srv.patrolEnabled && strings.Contains(line, "[0x2E] UUID=") {
				w.srv.patroller.NotifyChMovePacket(extractInstanceLabel(line))
			}
		}
	}
	return n, err
}

// extractInstanceLabel は "... [Instance-7][0x2E] UUID=..." 形式のログ行から
// インスタンスラベル文字列（"Instance-7"）を抽出する。見つからない場合は空文字列を返す。
// log.Printf が先頭にタイムスタンプを付けるため、行頭からではなく "[Instance-" を検索する。
func extractInstanceLabel(line string) string {
	idx := strings.Index(line, "[Instance-")
	if idx < 0 {
		return ""
	}
	rest := line[idx+1:]
	end := strings.Index(rest, "]")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// getDeviceIPCtx は mumu.GetDeviceIP をコンテキストのキャンセルに対応させるラッパー。
// GetDeviceIP 自体はコンテキストを受け取らないため、内部ゴルーチンで実行し ctx 終了時に空文字を返す。
func getDeviceIPCtx(ctx context.Context, serial string, cfg mumu.Config) string {
	ch := make(chan string, 1)
	go func() {
		ip, err := mumu.GetDeviceIP(ctx, serial, cfg)
		if err != nil {
			log.Printf("[MuMu] GetDeviceIP %s: %v", serial, err)
			ip = ""
		}
		ch <- ip
	}()
	select {
	case ip := <-ch:
		return ip
	case <-ctx.Done():
		return ""
	}
}

// LogWriter は log.SetOutput() に渡す io.Writer を返す。
func (s *Server) LogWriter(base io.Writer) io.Writer {
	return &guiWriter{base: base, srv: s}
}

// isAllowedOrigin は CORS を許可するオリジンかどうかを判定する。
// localhost / 127.0.0.1（WebView2・同一ホスト）と GAS ページ（googleusercontent.com）
// およびブラウザ拡張機能のみ許可し、任意の外部サイトからの CSRF を防ぐ。
func isAllowedOrigin(origin string) bool {
	return strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasSuffix(origin, ".googleusercontent.com") ||
		origin == "https://script.google.com" ||
		strings.HasPrefix(origin, "chrome-extension://") ||
		strings.HasPrefix(origin, "moz-extension://")
}

// corsMiddleware は GAS 拡張機能等の許可済みオリジンからのリクエストのみ CORS を許可する。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startHTTP はHTTPサーバーをバックグラウンドで起動する（内部用）
func (s *Server) startHTTP(ctx context.Context) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/device-map", s.handleDeviceMap)
	mux.HandleFunc("/api/switch", s.handleSwitch)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/patrol/start", s.handlePatrolStart)
	mux.HandleFunc("/api/patrol/stop", s.handlePatrolStop)
	mux.HandleFunc("/api/patrol/identify", s.handlePatrolIdentify)
	mux.HandleFunc("/api/patrol/status", s.handlePatrolStatus)
	mux.HandleFunc("/api/patrol/device-statuses", s.handlePatrolDeviceStatuses)
	mux.HandleFunc("/api/patrol/recover", s.handlePatrolRecover)
	mux.HandleFunc("/api/patrol/clear-move-failed", s.handlePatrolClearMoveFailed)
	mux.HandleFunc("/api/patrol/channels", s.handlePatrolChannels)
	mux.HandleFunc("/api/patrol/channels/gas", s.handlePatrolChannelsGAS)
	mux.HandleFunc("/api/test-detect", s.handleTestDetect)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/gold-history", s.handleGoldHistory)
	mux.HandleFunc("/api/gold-history/delete", s.handleDeleteGoldHistory)
	mux.HandleFunc("/api/gold-history/clear", s.handleClearGoldHistory)
	mux.HandleFunc("/api/adb/restart", s.handleADBRestart)
	mux.HandleFunc("/api/adb/kill-server", s.handleADBKillServer)
	mux.HandleFunc("/api/adb/connect", s.handleADBConnect)
	mux.HandleFunc("/api/adb/tap-loop/start", s.handleTapLoopStart)
	mux.HandleFunc("/api/adb/tap-loop/stop", s.handleTapLoopStop)
	mux.HandleFunc("/api/adb/tap-loop/status", s.handleTapLoopStatus)
	mux.HandleFunc("/api/webhook/test", s.handleWebhookTest)
	mux.HandleFunc("/api/chat-log", s.handleChatLog)
	mux.HandleFunc("/api/chat-events", s.handleChatEvents)
	mux.HandleFunc("/api/chat-report/notify", s.handleChatReportNotify)
	mux.HandleFunc("/api/portmap/pending", s.handlePortMapPending)
	mux.HandleFunc("/api/portmap/confirm", s.handlePortMapConfirm)
	mux.HandleFunc("/api/portmap/entries", s.handlePortMapEntries)
	mux.HandleFunc("/api/portmap/map-ch", s.handlePortMapMapCh)
	mux.HandleFunc("/api/portmap/map-all", s.handlePortMapMapAll)
	mux.HandleFunc("/api/portmap/manual", s.handlePortMapManual)
	mux.HandleFunc("/api/devices/identified", s.handleDevicesIdentified)
	mux.HandleFunc("/api/devices/memory", s.handleDevicesMemory)
	mux.HandleFunc("/api/serial_uid/delete", s.handleDeleteSerialUID)
	mux.HandleFunc("/api/devices/assignments/compute", s.handleComputeAssignments)
	mux.HandleFunc("/spawn-log", s.handleSpawnLog)
	mux.HandleFunc("/chat-log", s.handleChatLogPage)
	mux.HandleFunc("/events", s.handleSSE)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("GUI server listen: %w", err)
	}

	srv := &http.Server{Handler: corsMiddleware(mux)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
			log.Printf("[GUI] HTTP server error: %v", err)
		}
	}()

	// 巡回モード時のみ、起動後にデバイスを1回確認する
	if s.patrolEnabled {
		go func() {
			time.Sleep(2 * time.Second)
			cfg := s.patroller.Config()
			devices, err := mumu.ListDevices(context.Background(), cfg)
			if err != nil {
				log.Printf("[MuMu] 起動時デバイス確認失敗: %v", err)
				return
			}
			if len(devices) == 0 {
				log.Println("[MuMu] デバイスが見つかりません。MuMu Player が起動しているか確認してください")
				s.mu.RLock()
				show := s.showNoDeviceDialog
				clients := make([]chan string, len(s.clients))
				copy(clients, s.clients)
				s.mu.RUnlock()
				if show {
					broadcastSSE(clients, "[NO_DEVICE]")
				}
			}
		}()
	}

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	return url, nil
}

const legacyWindowStateFile = "config/window_state.json"

func isValidWindowState(ws *appconfig.WindowState) bool {
	return ws != nil && ws.Width >= 200 && ws.Height >= 200
}

// win32RECT は Win32 の RECT 構造体
type win32RECT struct {
	Left, Top, Right, Bottom int32
}

var (
	modUser32         = syscall.NewLazyDLL("user32.dll")
	procGetWindowRect = modUser32.NewProc("GetWindowRect")
	procSetWindowPos  = modUser32.NewProc("SetWindowPos")
)

func (s *Server) loadWindowState() *appconfig.WindowState {
	if s.loadWindowStateFn != nil {
		ws, err := s.loadWindowStateFn()
		if err == nil && isValidWindowState(ws) {
			return ws
		}
		if err != nil {
			log.Printf("[GUI] window state load failed: %v", err)
		}
	}
	data, err := os.ReadFile(legacyWindowStateFile)
	if err != nil {
		return nil
	}
	var ws appconfig.WindowState
	if err := json.Unmarshal(data, &ws); err != nil {
		return nil
	}
	if !isValidWindowState(&ws) {
		return nil
	}
	return &ws
}

func (s *Server) saveWindowState(hwnd uintptr) {
	var r win32RECT
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return
	}
	w := int(r.Right - r.Left)
	h := int(r.Bottom - r.Top)
	if w < 200 || h < 200 {
		return
	}
	ws := &appconfig.WindowState{
		X: int(r.Left), Y: int(r.Top),
		Width: w, Height: h,
	}
	if s.saveWindowStateFn != nil {
		if err := s.saveWindowStateFn(ws); err != nil {
			log.Printf("[GUI] window state save failed: %v", err)
		}
		return
	}
	data, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(legacyWindowStateFile, data, 0644); err != nil {
		log.Printf("[GUI] window state save failed: %v", err)
	}
}

// RunWindow はHTTPサーバーを起動しEdge WebView2の専用ウィンドウを開く。
func (s *Server) RunWindow(ctx context.Context) error {
	url, err := s.startHTTP(ctx)
	if err != nil {
		return err
	}
	log.Printf("[GUI] opening window: %s", url)

	ws := s.loadWindowState()
	winWidth, winHeight := 1000, 720
	centerWin := true
	if ws != nil {
		winWidth = ws.Width
		winHeight = ws.Height
		centerWin = false
	}

	w := webview.NewWithOptions(webview.WebViewOptions{
		Debug: false,
		WindowOptions: webview.WindowOptions{
			Title:  "BPSR patrol cams",
			Width:  uint(winWidth),
			Height: uint(winHeight),
			Center: centerWin,
		},
	})
	if w == nil {
		log.Println("[GUI] WebView2 unavailable, falling back to browser")
		openBrowser(url)
		<-ctx.Done()
		return nil
	}
	// 前回の位置を復元
	hwnd := uintptr(w.Window())
	if ws != nil {
		const SWP_NOZORDER = 0x0004
		procSetWindowPos.Call(hwnd, 0,
			uintptr(ws.X), uintptr(ws.Y),
			uintptr(ws.Width), uintptr(ws.Height),
			SWP_NOZORDER)
	}
	// ウィンドウが生きている間、2秒ごとに位置・サイズを保存する
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.saveWindowState(hwnd)
			}
		}
	}()
	w.Navigate(url)
	w.Run()
	close(done)
	// Run() 直後にも保存を試みる（まだ HWND が有効な場合がある）
	s.saveWindowState(hwnd)
	w.Destroy()
	return nil
}

func openBrowser(url string) {
	cmd := exec.Command("cmd", "/c", "start", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Printf("[GUI] browser open failed: %v", err)
	}
}

// handleIndex はメインHTMLページを返す
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, s.renderIndexHTML())
}

func (s *Server) renderIndexHTML() string {
	if s.patrolEnabled {
		return processedIndexHTML
	}
	return processedIndexHTML + patrolDisabledScript
}

// handleDeviceMap はADBデバイスのエミュレータIPを取得し、
// キャプチャセッション（UID等）と紐付けた一覧をJSONで返す。
func (s *Server) handleDeviceMap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	adbDevices, err := mumu.ListDevices(ctx, s.patroller.Config())
	if err != nil {
		log.Printf("[MuMu] device-map: ListDevices error: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	uidToSess := make(map[uint64]DeviceSessionInfo)
	ipToSess := make(map[string]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.UserUID != 0 {
				uidToSess[sess.UserUID] = sess
			}
			if sess.ClientIP != "" {
				ipToSess[sess.ClientIP] = sess
			}
		}
	}

	type DeviceEntry struct {
		Serial    string `json:"serial"`
		DeviceIP  string `json:"device_ip"`
		UserUID   uint64 `json:"user_uid"`
		Label     string `json:"label"`
		MapID     uint32 `json:"map_id"`
		LineID    uint32 `json:"line_id"`
		CurrentCh uint32 `json:"current_ch"`
		Confirmed bool   `json:"confirmed"`
	}

	cfg := s.patroller.Config()
	entries := make([]DeviceEntry, len(adbDevices))
	var wg sync.WaitGroup
	for i, serial := range adbDevices {
		entries[i] = DeviceEntry{Serial: serial}
		wg.Add(1)
		go func(idx int, ser string) {
			defer wg.Done()
			devIP := getDeviceIPCtx(ctx, ser, cfg)
			entries[idx].DeviceIP = devIP
			// u512Au5148u9806u4F8D1: serialu2192UID u30D0u30A4u30F3u30C9u3067u76F4u63A5u7167u5408uFF08NAT u74B0u5883u5BFEu5FDCuFF09
			if uid := s.patroller.GetSerialUID(ser); uid != 0 {
				if sess, ok := uidToSess[uid]; ok {
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].CurrentCh = sess.CurrentCh
					entries[idx].Confirmed = sess.Confirmed
				} else {
					entries[idx].UserUID = uid
				}
			} else if devIP != "" {
				// u512Au5148u9806u4F8D2: IP u7167u5408uFF08u975E NAT u74B0u5883u30D5u30A9u30FCu30EBu30D0u30C3u30AFuFF09
				if sess, ok := ipToSess[devIP]; ok {
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].CurrentCh = sess.CurrentCh
					entries[idx].Confirmed = sess.Confirmed
				}
			}
		}(i, serial)
	}
	wg.Wait()

	// DeviceStatusuFF08ADB serial u30D9u30FCu30B9uFF09u3067 CurrentCh u3092u4E0Au66F8u304Du3059u308Bu3002
	// ActualChuFF08u30D1u30B1u30C3u30C8u89B3u6E2Cu306Eu5B9FCHuFF09u3092u6700u512Au5148u3057u3001u6B21u3044u3067 CurrentChuFF08ADBu547Du4EE4u5024uFF09u3092u4F7Fu3046u3002
	// u507Du30D5u30A9u30FCu30EBu30D0u30C3u30AFuFF08u5DE1u56DEu5168u4F53CHu3092u672Au89E3u6C7Au30C7u30D0u30A4u30B9u306Bu4E0Au66F8u304DuFF09u306Fu610Fu56F3u7684u306Bu524Au9664u3002
	if s.patroller != nil {
		statuses := s.patroller.GetDeviceStatuses()
		for i := range entries {
			for _, ds := range statuses {
				if ds.Serial != entries[i].Serial {
					continue
				}
				if ds.ActualCh > 0 {
					entries[i].CurrentCh = ds.ActualCh
				} else if ds.CurrentCh > 0 {
					entries[i].CurrentCh = ds.CurrentCh
				}
				break
			}
		}
	}

	writeJSON(w, map[string]interface{}{"devices": entries})
}

// handleDevices はADB接続デバイス一覧をJSONで返す
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	cfg := s.patroller.Config()
	devices, err := mumu.ListDevices(r.Context(), cfg)
	if err != nil {
		log.Printf("[MuMu] adb devices エラー: %v", err)
	}
	if err == nil && len(devices) == 0 {
		// 接続試行してから再確認
		mumu.ConnectConfiguredSerials(r.Context(), cfg)
		devices, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil {
			log.Printf("[MuMu] adb devices 再試行エラー: %v", err)
		}
	}
	if devices == nil {
		devices = []string{}
	}
	writeJSON(w, map[string]interface{}{"devices": devices})
}

// handleSwitch はチャンネル切替リクエストを処理する
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial  string   `json:"serial"`
		Serials []string `json:"serials"`
		Channel uint32   `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	type result struct {
		Serial string `json:"serial"`
		Error  string `json:"error,omitempty"`
		OK     bool   `json:"ok"`
	}
	var results []result

	cfg := s.patroller.Config()

	// serials が指定されていれば複数切替、なければ単体切替
	serials := req.Serials
	if len(serials) == 0 && req.Serial == "" {
		// 何も指定なし → 全台を ListDevices で取得
		var err error
		serials, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	if len(serials) > 0 {
		switchResults := make(map[string]error, len(serials))
		var switchMu sync.Mutex
		limit := mumu.ParallelLimit(cfg, len(serials))
		for start := 0; start < len(serials); start += limit {
			end := start + limit
			if end > len(serials) {
				end = len(serials)
			}
			if start > 0 && cfg.ParallelGroupDelay > 0 {
				time.Sleep(cfg.ParallelGroupDelay)
			}
			mumu.SwitchGroup(r.Context(), serials, start, end, req.Channel, cfg, switchResults, &switchMu)
		}
		for serial, err := range switchResults {
			res := result{Serial: serial, OK: err == nil}
			if err != nil {
				res.Error = err.Error()
			}
			results = append(results, res)
		}
	} else {
		// 単体切替
		err := mumu.SwitchChannel(r.Context(), req.Serial, req.Channel, cfg)
		res := result{Serial: req.Serial, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		results = append(results, res)
	}

	// 成功した切替を DeviceStatus に即時反映し、次回 device-map 取得時に最新 Ch が返るようにする
	if s.patroller != nil {
		for _, res := range results {
			if res.OK {
				s.patroller.SetDeviceCh(res.Serial, req.Channel)
			}
		}
	}

	writeJSON(w, map[string]interface{}{"results": results})
}

// handleLogs は既存ログ一覧をJSONで返す
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{"logs": lines})
}

// handlePatrolChannels はconfig読み込み済みのチャンネルリストを返す（GET）または保存する（POST）
func (s *Server) handlePatrolChannels(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]interface{}{"channels": []uint32{}})
			return
		}
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Channels []uint32 `json:"channels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		filtered := s.filterCooldown(req.Channels)
		s.patrolChannels = filtered
		s.mu.Unlock()
		if s.saveChannelsFn != nil {
			if err := s.saveChannelsFn(filtered); err != nil {
				log.Printf("[GUI] channels保存失敗: %v", err)
				http.Error(w, "save failed: "+err.Error(), 500)
				return
			}
			excluded := len(req.Channels) - len(filtered)
			if excluded > 0 {
				log.Printf("[GUI] channels.txt に %d件保存しました (%d件クールダウン除外)", len(filtered), excluded)
			} else {
				log.Printf("[GUI] channels.txt に %d件保存しました", len(filtered))
			}
		}
		writeOK(w)
		return
	}
	s.mu.RLock()
	chs := make([]uint32, len(s.patrolChannels))
	copy(chs, s.patrolChannels)
	s.mu.RUnlock()
	writeJSON(w, map[string]interface{}{"channels": chs})
}

// handlePatrolChannelsGAS は Chrome拡張からエネミー名付きチャンネルを受信し、
// GASTargetEnemy 設定でフィルタして巡回リストを更新する。
func (s *Server) handlePatrolChannelsGAS(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	gasEnabled := s.gasEnable
	s.mu.RUnlock()
	if !gasEnabled {
		http.Error(w, "GAS sync disabled", http.StatusForbidden)
		return
	}
	if !s.patrolEnabled {
		writeJSON(w, map[string]interface{}{"ok": true, "ignored": true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Entries []struct {
			Channel uint32 `json:"channel"`
			Enemy   string `json:"enemy"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	s.mu.RLock()
	target := s.gasTargetEnemy
	s.mu.RUnlock()

	var chs []uint32
	for _, e := range req.Entries {
		if target == "" || strings.Contains(e.Enemy, target) {
			chs = append(chs, e.Channel)
		}
	}
	log.Printf("[GASSync] 受信: 全%d件 → 対象(%s): %d件", len(req.Entries), target, len(chs))
	s.UpdateChannelsFromGAS(chs)
	writeOK(w)
}

// handleGoldHistory は金ウリボ検知履歴を返す
func (s *Server) handleGoldHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := make([]GoldBoarEvent, len(s.goldBoarHistory))
	copy(h, s.goldBoarHistory)
	s.mu.RUnlock()
	// 新しい順に返す
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	writeJSON(w, h)
}

// handleDeleteGoldHistory は検知履歴の1件を削除する
func (s *Server) handleDeleteGoldHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", 405)
		return
	}
	var req struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.Timestamp == 0 {
		http.Error(w, "timestamp is required", 400)
		return
	}

	s.mu.Lock()
	idx := -1
	var evCh uint32
	for i, ev := range s.goldBoarHistory {
		if ev.Timestamp == req.Timestamp {
			idx = i
			evCh = ev.Channel
			break
		}
	}
	var saveChannelsFn func([]uint32) error
	var restoredChs []uint32
	if idx >= 0 {
		s.goldBoarHistory = append(s.goldBoarHistory[:idx], s.goldBoarHistory[idx+1:]...)
		if err := s.persistGoldHistoryLocked(); err != nil {
			s.mu.Unlock()
			log.Printf("[GUI] gold history save failed: %v", err)
			http.Error(w, "save failed", 500)
			return
		}
		// クールダウン解除（GAS経由の再追加を許可）
		delete(s.cooldownChs, evCh)
		// 巡回リストに復元（未登録の場合のみ追加）
		alreadyIn := false
		for _, pc := range s.patrolChannels {
			if pc == evCh {
				alreadyIn = true
				break
			}
		}
		if !alreadyIn && evCh > 0 {
			s.patrolChannels = append(s.patrolChannels, evCh)
			restoredChs = make([]uint32, len(s.patrolChannels))
			copy(restoredChs, s.patrolChannels)
		}
		saveChannelsFn = s.saveChannelsFn
	}
	s.mu.Unlock()

	if idx < 0 {
		http.Error(w, "not found", 404)
		return
	}
	if restoredChs != nil && saveChannelsFn != nil {
		if err := saveChannelsFn(restoredChs); err != nil {
			log.Printf("[GUI] channels.txt 保存失敗 (誤報取消): %v", err)
		} else {
			log.Printf("[GUI] 誤報取消: Ch%d を巡回リストに復元 (計%d ch)", evCh, len(restoredChs))
		}
	}
	writeOK(w)
}

// handleClearGoldHistory は検知履歴を全件削除する
func (s *Server) handleClearGoldHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", 405)
		return
	}
	s.mu.Lock()
	s.goldBoarHistory = nil
	if err := s.persistGoldHistoryLocked(); err != nil {
		s.mu.Unlock()
		log.Printf("[GUI] gold history save failed: %v", err)
		http.Error(w, "save failed", 500)
		return
	}
	s.mu.Unlock()
	writeOK(w)
}

// handleConfig は config.json の読み込み（GET）または保存（POST）を行う。
// 保存後は再起動で反映される。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.saveConfigFn == nil {
			http.Error(w, "save not configured", 503)
			return
		}
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.saveConfigFn(buf); err != nil {
			log.Printf("[GUI] config保存失敗: %v", err)
			http.Error(w, "save failed: "+err.Error(), 500)
			return
		}
		log.Printf("[GUI] config.json を保存しました")

		// 保存した config を読み直して Patroller に即時反映
		if s.getConfigFn != nil {
			if savedData, rerr := s.getConfigFn(); rerr == nil {
				var appCfg struct {
					ADBPath                string   `json:"adb_path"`
					MumuSerials            []string `json:"mumu_serials"`
					MumuTapX               int      `json:"mumu_tap_x"`
					MumuTapY               int      `json:"mumu_tap_y"`
					MumuClearLength        int      `json:"mumu_clear_length"`
					MumuPreKeycode         string   `json:"mumu_pre_keycode"`
					MumuDelayMs            int      `json:"mumu_delay_ms"`
					ParallelLimit          int      `json:"parallel_limit"`
					ParallelGroupDelaySecs float64  `json:"parallel_group_delay_secs"`
					PatrolMoveTimeoutSecs            float64 `json:"patrol_move_timeout_secs"`
					PatrolMergeTimeoutSecs           float64 `json:"patrol_merge_timeout_secs"`
					PatrolLoadStabilizationSecs      float64 `json:"patrol_load_stabilization_secs"`
					PatrolDwellSecs             float64 `json:"patrol_dwell_secs"`
					PatrolAdaptiveTimeout       bool              `json:"patrol_adaptive_timeout"`
					PatrolAdaptiveTimeoutWindow int               `json:"patrol_adaptive_timeout_window"`
					SerialToLabel               map[string]string `json:"serial_to_label"`
					GamePackageName             string            `json:"game_package_name"`
					GameLaunchActivity          string            `json:"game_launch_activity"`
					CrashRecoveryEnabled        bool              `json:"crash_recovery_enabled"`
					CrashRecoveryDelaySecs      float64           `json:"crash_recovery_delay_secs"`
					DebugVerbose                bool              `json:"debug_verbose"`
					ExcludeUIDs                 []uint64          `json:"exclude_uids"`
				}
				if json.Unmarshal(savedData, &appCfg) == nil {
					newCfg := mumu.Config{
						ADBPath:            appCfg.ADBPath,
						TapX:               appCfg.MumuTapX,
						TapY:               appCfg.MumuTapY,
						ClearLength:        appCfg.MumuClearLength,
						PreKeycode:         appCfg.MumuPreKeycode,
						ConnectSerials:     appCfg.MumuSerials,
						GlobalDelay:        time.Duration(appCfg.MumuDelayMs) * time.Millisecond,
						ParallelLimit:      appCfg.ParallelLimit,
						ParallelGroupDelay: time.Duration(appCfg.ParallelGroupDelaySecs * float64(time.Second)),
						MoveTimeout:               time.Duration(appCfg.PatrolMoveTimeoutSecs * float64(time.Second)),
						MergeTimeout:              time.Duration(appCfg.PatrolMergeTimeoutSecs * float64(time.Second)),
						LoadStabilizationDuration: time.Duration(appCfg.PatrolLoadStabilizationSecs * float64(time.Second)),
						DwellDuration:         time.Duration(appCfg.PatrolDwellSecs * float64(time.Second)),
						AdaptiveTimeout:        appCfg.PatrolAdaptiveTimeout,
						AdaptiveTimeoutWindow:  appCfg.PatrolAdaptiveTimeoutWindow,
						GetLabelByIP:           s.getLabelByIPFn,
						SerialToLabel:          appCfg.SerialToLabel,
						GamePackageName:        appCfg.GamePackageName,
						GameLaunchActivity:     appCfg.GameLaunchActivity,
						CrashRecoveryEnabled:   appCfg.CrashRecoveryEnabled,
						CrashRecoveryDelaySecs: appCfg.CrashRecoveryDelaySecs,
					}
					s.UpdatePatrollerCfg(newCfg)
					s.SetExcludeUIDs(appCfg.ExcludeUIDs)
					debuglog.Verbose = appCfg.DebugVerbose
					log.Printf("[GUI] 巡回設定を即時反映: 滞在=%.0fs, 初回待ち=%.0fs, マージ待ち=%.0fs, グループ間=%.0fs, 並列=%d, デバッグ=%v",
						appCfg.PatrolDwellSecs, appCfg.PatrolMoveTimeoutSecs, appCfg.PatrolMergeTimeoutSecs,
						appCfg.ParallelGroupDelaySecs, appCfg.ParallelLimit, appCfg.DebugVerbose)
				}
			}
		}

		writeOK(w)
		return
	}
	if s.getConfigFn == nil {
		http.Error(w, "config not available", 503)
		return
	}
	data, err := s.getConfigFn()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handlePatrolStart は巡回を開始する
func (s *Server) handlePatrolStart(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serials      []string `json:"serials"`
		Channels     []uint32 `json:"channels"`
		Reversed     bool     `json:"reversed"`
		LoopMode     bool     `json:"loop_mode"`
		StartChannel uint32   `json:"start_channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		s.mu.RLock()
		channels = make([]uint32, len(s.patrolChannels))
		copy(channels, s.patrolChannels)
		s.mu.RUnlock()
	}
	if len(channels) == 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "チャンネルリストが空です"})
		return
	}
	opts := mumu.PatrolOptions{
		Reversed:     req.Reversed,
		LoopMode:     req.LoopMode,
		StartChannel: req.StartChannel,
	}
	s.patroller.Start(req.Serials, channels, s.patrolChannelsFile, opts)
	writeOK(w)
}

// handleTestDetect はテスト用ウリボ・ゴールド検知を発火する
func (s *Server) handleTestDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.testDetectFn == nil {
		http.Error(w, "test detect not configured", 503)
		return
	}
	monster := r.URL.Query().Get("monster")
	go s.testDetectFn(monster)
	writeJSON(w, map[string]interface{}{"ok": true, "monster": monster})
}

// handlePatrolClearMoveFailed は移動失敗リストをクリアする
func (s *Server) handlePatrolClearMoveFailed(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.ClearMoveFailedChannels()
	writeOK(w)
}

// handlePatrolStop は巡回を停止する
func (s *Server) handlePatrolStop(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.Stop()
	writeOK(w)
}

// handlePatrolIdentify はデバイス識別フェーズ（Stagger Probe）を手動実行する。
// 巡回中は 409、識別実行中は 429 を返す。
// 識別はバックグラウンドで実行され、即座に 200 を返す。
func (s *Server) handlePatrolIdentify(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.patroller.Status().Running {
		http.Error(w, "巡回中は識別できません", http.StatusConflict)
		return
	}
	s.mu.Lock()
	if s.identifyRunning {
		s.mu.Unlock()
		http.Error(w, "識別フェーズ実行中です", http.StatusTooManyRequests)
		return
	}
	s.identifyRunning = true
	s.mu.Unlock()

	var req struct {
		Serials  []string `json:"serials"`
		Channels []uint32 `json:"channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.mu.Lock()
		s.identifyRunning = false
		s.mu.Unlock()
		http.Error(w, err.Error(), 400)
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		s.mu.RLock()
		channels = make([]uint32, len(s.patrolChannels))
		copy(channels, s.patrolChannels)
		s.mu.RUnlock()
	}
	if len(channels) == 0 {
		s.mu.Lock()
		s.identifyRunning = false
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"ok": false, "error": "チャンネルリストが空です"})
		return
	}

	go func() {
		defer func() {
			s.mu.Lock()
			s.identifyRunning = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.patroller.Identify(ctx, req.Serials, channels); err != nil {
			log.Printf("[GUI] デバイス識別失敗: %v", err)
		}
	}()

	writeOK(w)
}

// handleADBRestart は ADB サーバーの復旧を行う。
// 既存接続を維持する手順を優先し、必要時のみハード再起動へフォールバックする。
func (s *Server) handleADBRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := mumu.RecoverServer(r.Context(), s.patroller.Config()); err != nil {
		log.Printf("[MuMu] ADB復旧失敗: %v", err)
	}
	writeOK(w)
}

// handleADBKillServer は adb kill-server を実行してADBサーバーを停止する。
func (s *Server) handleADBKillServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	cfg := s.patroller.Config()
	out, err := exec.CommandContext(r.Context(), cfg.ADBPath, "kill-server").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil && r.Context().Err() == nil {
		log.Printf("[MuMu] adb kill-server 失敗: %v: %s", err, msg)
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if msg != "" {
		log.Printf("[MuMu] adb kill-server: %s", msg)
	}
	writeOK(w)
}

// handleADBConnect は adb connect <serial> で指定シリアルのデバイスを追加接続する。
func (s *Server) handleADBConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "serial required", http.StatusBadRequest)
		return
	}
	// host:port 形式のみ受け付ける（コマンドインジェクション防止）
	if strings.ContainsAny(req.Serial, " \t\r\n/\\;|&`$<>") {
		http.Error(w, "invalid serial format", http.StatusBadRequest)
		return
	}
	cfg := s.patroller.Config()
	out, err := exec.CommandContext(r.Context(), cfg.ADBPath, "connect", req.Serial).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil && r.Context().Err() == nil {
		log.Printf("[MuMu] adb connect %s 失敗: %v: %s", req.Serial, err, msg)
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("[MuMu] adb connect %s: %s", req.Serial, msg)
	writeJSON(w, map[string]interface{}{"ok": true, "message": msg})
}

// handleTapLoopStart はタップループを開始する。
func (s *Server) handleTapLoopStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		TapX       int      `json:"tap_x"`
		TapY       int      `json:"tap_y"`
		IntervalMs int      `json:"interval_ms"`
		Serials    []string `json:"serials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	cfg := s.patroller.Config()
	serials := req.Serials
	if len(serials) == 0 {
		var err error
		serials, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil || len(serials) == 0 {
			writeJSON(w, map[string]any{"ok": false, "error": "デバイスが見つかりません"})
			return
		}
	}
	if req.IntervalMs <= 0 {
		req.IntervalMs = 1000
	}
	s.tapLooper.Start(cfg, serials, req.TapX, req.TapY, req.IntervalMs)
	writeOK(w)
}

// handleTapLoopStop はタップループを停止する。
func (s *Server) handleTapLoopStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.tapLooper.Stop()
	writeOK(w)
}

// handleTapLoopStatus はタップループの現在状態をJSONで返す。
func (s *Server) handleTapLoopStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.tapLooper.Status())
}

// handleWebhookTest は指定URLにDiscordテスト通知を送る
func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "URL必須"})
		return
	}
	if err := notifier.SendTest(req.URL); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeOK(w)
}

// handleSpawnLog は出現ログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleSpawnLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, spawnLogHTML)
}

// handleChatReportNotify はフロントエンドのスコアリングで候補確定したチャットをDiscordに通知する。
func (s *Server) handleChatReportNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Channel  uint32 `json:"channel"`
		Message  string `json:"message"`
		Location string `json:"location"`
		Monster  string `json:"monster"`
		Sender   string `json:"sender"`
		Score    int    `json:"score"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if s.chatReportFn == nil {
		writeOK(w)
		return
	}
	// 5分間のdedup（複数ブラウザタブや再描画での二重送信を防ぐ）
	key := fmt.Sprintf("%d|%s", req.Channel, req.Message)
	now := time.Now()
	s.chatReportDedupMu.Lock()
	if s.chatReportDedup == nil {
		s.chatReportDedup = make(map[string]time.Time)
	}
	if t, seen := s.chatReportDedup[key]; seen && now.Sub(t) < 5*time.Minute {
		s.chatReportDedupMu.Unlock()
		writeOK(w)
		return
	}
	s.chatReportDedup[key] = now
	for k, t := range s.chatReportDedup {
		if now.Sub(t) > 10*time.Minute {
			delete(s.chatReportDedup, k)
		}
	}
	s.chatReportDedupMu.Unlock()

	det := notifier.Detection{
		Source:      notifier.SourceChat,
		ChatLineID:  req.Channel,
		LineID:      req.Channel,
		Location:    req.Location,
		MonsterName: req.Monster,
		Message:     req.Message,
		PlayerName:  req.Sender,
		Score:       req.Score,
		Time:        now,
	}
	go s.chatReportFn(det)
	writeOK(w)
}

// handleChatLog はチャットログ履歴をJSONで返す
func (s *Server) handleChatLog(w http.ResponseWriter, r *http.Request) {
	s.chatMu.RLock()
	events := make([]ChatEvent, len(s.chatLog))
	copy(events, s.chatLog)
	s.chatMu.RUnlock()
	writeJSON(w, events)
}

// handleChatEvents はチャットをServer-Sent Eventsでリアルタイム配信する
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	s.sseStream(w, r,
		func(ch chan string) {
			s.chatMu.Lock()
			s.chatClients = append(s.chatClients, ch)
			s.chatMu.Unlock()
		},
		func(ch chan string) {
			s.chatMu.Lock()
			for i, c := range s.chatClients {
				if c == ch {
					s.chatClients = append(s.chatClients[:i], s.chatClients[i+1:]...)
					break
				}
			}
			s.chatMu.Unlock()
		},
		func(msg string) string { return msg },
	)
}

// handlePortMapPending は確認待ち portMap 変更の一覧を返す。
func (s *Server) handlePortMapPending(w http.ResponseWriter, r *http.Request) {
	s.pendingPortMapMu.Lock()
	list := make([]PendingPortMapChange, len(s.pendingPortMaps))
	copy(list, s.pendingPortMaps)
	s.pendingPortMapMu.Unlock()
	writeJSON(w, list)
}

// handlePortMapConfirm は portMap 変更の適用・却下を処理する。
// リクエスト: {"ids":["pm-1","pm-2"],"action":"apply"|"reject"}
func (s *Server) handlePortMapConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	idSet := make(map[string]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		idSet[id] = struct{}{}
	}

	s.pendingPortMapMu.Lock()
	var toApply []PendingPortMapChange
	remaining := s.pendingPortMaps[:0]
	for _, p := range s.pendingPortMaps {
		if _, matched := idSet[p.ID]; matched {
			if req.Action == "apply" {
				toApply = append(toApply, p)
			}
		} else {
			remaining = append(remaining, p)
		}
	}
	s.pendingPortMaps = remaining
	applyFn := s.portMapApplyFn
	s.pendingPortMapMu.Unlock()

	if applyFn != nil {
		for _, p := range toApply {
			applyFn(p.Ch, p.NewIP)
		}
	}
	writeOK(w)
}

// handlePortMapEntries はポートマップの全エントリを返す。
func (s *Server) handlePortMapEntries(w http.ResponseWriter, r *http.Request) {
	fn := s.getPortMapFn
	if fn == nil {
		writeJSON(w, []PortMapEntry{})
		return
	}
	writeJSON(w, fn())
}

// handlePortMapMapCh は現在セッションを指定chにマッピングする。
// リクエスト: {"ch": 5}
func (s *Server) handlePortMapMapCh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Ch uint32 `json:"ch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ch == 0 {
		http.Error(w, "ch must be a positive integer", http.StatusBadRequest)
		return
	}
	fn := s.mapChFn
	if fn != nil {
		fn(req.Ch)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "ch": req.Ch})
}

// handlePortMapMapAll は全セッションをLineIDでマッピングする。
func (s *Server) handlePortMapMapAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fn := s.mapAllFn
	count := 0
	if fn != nil {
		count = fn()
	}
	writeJSON(w, map[string]interface{}{"ok": true, "mapped": count})
}

// handlePortMapManual は ch/IP/ポートを手動でポートマップに登録する。
func (s *Server) handlePortMapManual(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Ch   uint32 `json:"ch"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Ch == 0 || req.IP == "" || req.Port <= 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "ch / ip / port が不正です"})
		return
	}
	serverIP := fmt.Sprintf("%s:%d", req.IP, req.Port)
	if fn := s.portMapApplyFn; fn != nil {
		fn(req.Ch, serverIP)
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleDevicesIdentified は全巡回対象デバイスが追跡可能状態かどうかを返す。
// 各シリアルに対応するセッションが Confirmed && LineID > 0 を満たすか確認する。
func (s *Server) handleDevicesIdentified(w http.ResponseWriter, r *http.Request) {
	serials := s.patroller.Status().Serials
	if len(serials) == 0 {
		serials = s.patroller.Config().ConnectSerials
	}
	total := len(serials)

	var sessions []DeviceSessionInfo
	if s.getSessions != nil {
		sessions = s.getSessions()
	}
	uidToSess := make(map[uint64]DeviceSessionInfo)
	for _, sess := range sessions {
		if sess.UserUID != 0 {
			uidToSess[sess.UserUID] = sess
		}
	}

	ready := 0
	var missing []string
	for _, serial := range serials {
		uid := s.patroller.GetSerialUID(serial)
		if uid == 0 {
			missing = append(missing, serial)
			continue
		}
		sess, ok := uidToSess[uid]
		if !ok || !sess.Confirmed || sess.LineID == 0 {
			missing = append(missing, serial)
			continue
		}
		ready++
	}
	writeJSON(w, map[string]interface{}{
		"identified": total > 0 && ready >= total,
		"total":      total,
		"ready":      ready,
		"missing":    missing,
	})
}

// handleDevicesMemory は Patroller が記憶している serial→uid/label マップを返す。
func (s *Server) handleDevicesMemory(w http.ResponseWriter, r *http.Request) {
	uidMap, labelMap := s.patroller.GetSerialMaps()

	allSerials := make(map[string]struct{})
	for serial := range uidMap {
		allSerials[serial] = struct{}{}
	}
	for serial := range labelMap {
		allSerials[serial] = struct{}{}
	}

	uidToSess := make(map[uint64]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.UserUID != 0 {
				uidToSess[sess.UserUID] = sess
			}
		}
	}

	type DeviceMemoryEntry struct {
		Serial    string `json:"serial"`
		UID       uint64 `json:"uid"`
		Label     string `json:"label"`
		CurrentCh uint32 `json:"current_ch"`
		Confirmed bool   `json:"confirmed"`
	}

	entries := make([]DeviceMemoryEntry, 0, len(allSerials))
	for serial := range allSerials {
		e := DeviceMemoryEntry{
			Serial: serial,
			UID:    uidMap[serial],
			Label:  labelMap[serial],
		}
		if e.UID != 0 {
			if sess, ok := uidToSess[e.UID]; ok {
				e.CurrentCh = sess.CurrentCh
				e.Confirmed = sess.Confirmed
			}
		}
		entries = append(entries, e)
	}
	writeJSON(w, entries)
}

// handleDeleteSerialUID は serial に紐付いた UID バインドを削除する。
func (s *Server) handleDeleteSerialUID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	s.patroller.DeleteSerialUIDBinding(req.Serial)
	writeOK(w)
}

// handleComputeAssignments は現在のデバイス台数からch分担を計算してファイルに保存し、Patrollerへ反映する。
func (s *Server) handleComputeAssignments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serials := s.patroller.Status().Serials
	if len(serials) == 0 {
		serials = s.patroller.Config().ConnectSerials
	}
	deviceCount := len(serials)
	if deviceCount == 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "デバイス未検出"})
		return
	}
	assignments := mumu.ComputeDeviceAssignments(deviceCount, 100)
	s.mu.RLock()
	file := s.assignmentsFile
	s.mu.RUnlock()
	if file == "" {
		file = "config/device_assignments.json"
	}
	if err := mumu.SaveDeviceAssignments(file, assignments); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	s.patroller.SetDeviceAssignments(assignments)
	log.Printf("[GUI] デバイス分担計算: %d台 → %dグループ保存 (%s)", deviceCount, len(assignments), file)
	writeJSON(w, map[string]interface{}{"ok": true, "device_count": deviceCount, "groups": len(assignments)})
}

// handleChatLogPage はチャットログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleChatLogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, chatLogHTML)
}

// handlePatrolStatus は現在の巡回状態を返す
func (s *Server) handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		writeJSON(w, map[string]interface{}{"running": false, "last_channel": 0})
		return
	}
	writeJSON(w, s.patroller.Status())
}

// handlePatrolDeviceStatuses はデバイスごとの巡回状態を返す
func (s *Server) handlePatrolDeviceStatuses(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, s.patroller.GetDeviceStatuses())
}

// handlePatrolRecover は指定シリアルのゲームクラッシュ復帰を手動トリガーする
func (s *Server) handlePatrolRecover(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "serial required", http.StatusBadRequest)
		return
	}
	go func() {
		if err := s.patroller.RecoverDevice(r.Context(), req.Serial); err != nil {
			log.Printf("[GUI] recover device %s: %v", req.Serial, err)
		}
	}()
	writeJSON(w, map[string]interface{}{"ok": true, "serial": req.Serial})
}

// sseStream はSSEエンドポイントの共通ループ処理。
// add/remove でクライアントチャネルの登録・解除を行い、format でメッセージを整形する。
func (s *Server) sseStream(w http.ResponseWriter, r *http.Request,
	add func(chan string),
	remove func(chan string),
	format func(string) string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	add(ch)
	defer remove(ch)

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", format(msg))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// handleSSE はServer-Sent Eventsで検知ログをリアルタイム配信する
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	s.sseStream(w, r,
		func(ch chan string) {
			s.mu.Lock()
			s.clients = append(s.clients, ch)
			s.mu.Unlock()
		},
		func(ch chan string) {
			s.mu.Lock()
			for i, c := range s.clients {
				if c == ch {
					s.clients = append(s.clients[:i], s.clients[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
		},
		func(msg string) string { return strings.ReplaceAll(msg, "\n", "\\n") },
	)
}

// processedIndexHTML は indexHTML に対して起動時に一度だけ適用した加工済みHTML。
// handleIndex が呼ばれるたびに同じ置換処理を繰り返さないようにする。
var processedIndexHTML = func() string {
	html := indexHTML
	html = strings.ReplaceAll(html, "setTimeout(function(){switchView('dashboard'", "")
	html = strings.ReplaceAll(html, "setInterval(function(){switchView('dashboard'", "")
	html = strings.ReplaceAll(html, "function switchView(id,navEl){", "function switchView(id,navEl){if(window.__disableAutoSwitchView)return;")
	return html
}()

const patrolDisabledScript = `<script>
(function(){
	window.__patrolDisabled=true;
	function hide(id){var el=document.getElementById(id);if(el)el.remove();}
	hide('nav-patrol');
	hide('view-patrol');
	hide('card-dash-patrol');
	hide('nav-data-management');
	hide('view-data-management');
	if(typeof loadPatrolChannels==='function'){loadPatrolChannels=async function(){window.patrolChannels=[];};}
	if(typeof pollPatrolStatus==='function'){pollPatrolStatus=function(){};}
	if(typeof patrolStart==='function'){patrolStart=async function(){alert('このアプリでは巡回機能は無効です');};}
	if(typeof patrolStop==='function'){patrolStop=async function(){};}
	if(typeof saveChannels==='function'){saveChannels=async function(){};}
	if(typeof clearMoveFailedChannels==='function'){clearMoveFailedChannels=async function(){};}
})();
</script>`

//go:embed assets/index.html
var indexHTML string

// spawnLogHTML は出現ログ専用の分離ウィンドウ用ページ
//go:embed assets/spawn_log.html
var spawnLogHTML string

// chatLogHTML はチャットログ専用の分離ウィンドウ用ページ
//go:embed assets/chat_log.html
var chatLogHTML string
