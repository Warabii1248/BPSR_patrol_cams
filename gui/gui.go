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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"

	"github.com/balrogsxt/StarResonanceAPI/appconfig"
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

	patrolRemoveDedupMu sync.Mutex
	patrolRemoveDedup   map[uint32]time.Time

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
func (s *Server) NotifyChMovePacket(label string, uid uint64) {
	if !s.patrolEnabled {
		return
	}
	s.patroller.NotifyChMovePacket(label, uid)
}

// NotifyLineIDChange は ncap が 0x15/0x16 で LineID を観測したとき main.go から呼ぶ。
// per-device CH 追跡のため Patroller に転送する（巡回中・停止中を問わない）。
func (s *Server) NotifyLineIDChange(uid uint64, lineID uint32, changedAt time.Time) {
	if s.patrolEnabled {
		s.patroller.NotifyLineIDChange(uid, lineID, changedAt)
	}
}

// NotifyPostLoadReady は ncap が 0x2E UUID パケットを受信したとき main.go から呼ぶ。
// ロード 75% シグナルとして Patroller に転送する。巡回中のみ有効。
func (s *Server) NotifyPostLoadReady(uid uint64, lineID uint32, t time.Time) {
	if s.patrolEnabled {
		s.patroller.NotifyPostLoadReady(uid, lineID, t)
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
				w.srv.patroller.NotifyChMovePacket(extractInstanceLabel(line), extractUIDFromLine(line))
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

// extractUIDFromLine は "[0x2E] UUID=... (UID=12345)" 形式のログ行から UID を抽出する。
// 見つからない・解析失敗時は 0 を返す（呼び出し側で従来動作にフォールバック）。
func extractUIDFromLine(line string) uint64 {
	idx := strings.Index(line, "(UID=")
	if idx < 0 {
		return 0
	}
	rest := line[idx+5:]
	end := strings.IndexByte(rest, ')')
	if end <= 0 {
		return 0
	}
	v, err := strconv.ParseUint(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return v
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
	mux.HandleFunc("/api/patrol/screenshot", s.handlePatrolScreenshot)
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
	mux.HandleFunc("/api/patrol/remove-ch", s.handlePatrolRemoveCh)
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



//go:embed assets/index.html
var indexHTMLRaw string

//go:embed assets/styles.css
var stylesCSS string

//go:embed assets/app.js
var appJS string

// indexHTML は index.html のテンプレ内 <link>/<script src> を
// styles.css / app.js の内容で inline 化した完全 HTML。
// IDE シンタックス支援のため CSS/JS を別ファイル化しつつ、
// 配信は単一 HTML のままにする。
var indexHTML = func() string {
	html := indexHTMLRaw
	html = strings.Replace(html, `<link rel="stylesheet" href="styles.css">`, "<style>\n"+stylesCSS+"\n</style>", 1)
	html = strings.Replace(html, `<script src="app.js"></script>`, "<script>\n"+appJS+"\n</script>", 1)
	return html
}()

// spawnLogHTML は出現ログ専用の分離ウィンドウ用ページ
//go:embed assets/spawn_log.html
var spawnLogHTML string

// chatLogHTML はチャットログ専用の分離ウィンドウ用ページ
//go:embed assets/chat_log.html
var chatLogHTML string
