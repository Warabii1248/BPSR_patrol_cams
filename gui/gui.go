// Package gui はローカルHTTPサーバーとして動作するWebベースGUIを提供する。
// Edge WebView2を使った専用ウィンドウで表示する。
package gui

import (
	"bytes"
	"context"
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

	gasTargetEnemy string // Chrome拡張から受信したchのフィルタ対象エネミー名

	pendingPortMapMu  sync.Mutex
	pendingPortMaps   []PendingPortMapChange
	pendingPortMapSeq int
	portMapApplyFn    func(ch uint32, serverIP string)

	chatReportFn      func(notifier.Detection)
	chatReportDedupMu sync.Mutex
	chatReportDedup   map[string]time.Time
}

// PortMapEntry はポートマップの1エントリ（GUI向け）
type PortMapEntry struct {
	Ch        uint32    `json:"ch"`
	ServerIP  string    `json:"server_ip"`
	UpdatedAt time.Time `json:"updated_at"`
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

// UpdatePatrollerCfg は Patroller の設定をリアルタイムで更新する。
func (s *Server) UpdatePatrollerCfg(cfg mumu.Config) {
	if !s.patrolEnabled {
		return
	}
	s.patroller.UpdateConfig(cfg)
}

// OnDetect は検知イベントをGUIのログに追加するコールバック
func (s *Server) OnDetect(det notifier.Detection) {
	line := fmt.Sprintf("[%s] %s [DETECTION]", det.Time.Format("15:04:05"), notifier.Format(det))
	detCh := det.LineID // 検知されたチャンネル番号

	s.mu.Lock()
	clients := s.appendLogLocked(line)
	// 金ウリボ履歴に追記（最大50件）
	monName := det.MonsterName
	if monName == "" {
		monName = "ウリボ・ゴールド"
	}
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
	s.pendingPortMapMu.Unlock()

	s.mu.RLock()
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	s.mu.RUnlock()

	broadcastSSE(clients, "[PORTMAP_PENDING]")
}

// OnChat はチャット受信イベントをログに追加しSSEで配信する
func (s *Server) OnChat(clientIP, sender, message string, channel uint32, hasCh bool) {
	ev := ChatEvent{
		Time:     time.Now().Format("15:04:05"),
		ClientIP: clientIP,
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
				w.srv.patroller.NotifyChMovePacket(extractUID(line))
			}
		}
	}
	return n, err
}

// extractUID は "[Instance-7][0x2E] UUID=1234567890 (UID=9876543)" 形式のログ行から
// UID文字列（"9876543"）を抽出する。UID が見つからない場合は空文字列を返す。
func extractUID(line string) string {
	const prefix = "(UID="
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(prefix):]
	end := strings.Index(rest, ")")
	if end <= 0 {
		return ""
	}
	uid := rest[:end]
	if uid == "0" {
		return ""
	}
	return uid
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
	mux.HandleFunc("/api/patrol/status", s.handlePatrolStatus)
	mux.HandleFunc("/api/patrol/clear-full", s.handlePatrolClearFull)
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

	// 巡回モード時のみ、起動後15秒間デバイスを1回だけ待機する
	if s.patrolEnabled {
		go func() {
			cfg := s.patroller.Config()
			for attempt := 1; attempt <= 3; attempt++ {
				time.Sleep(1 * time.Second)
				log.Printf("[MuMu] 起動時デバイス確認 (%d/3)...", attempt)
				mumu.ConnectConfiguredSerials(context.Background(), cfg)
				devices, err := mumu.ListDevices(context.Background(), cfg)
				if err != nil {
					log.Printf("[MuMu] 起動時デバイス取得失敗 (%d/3): %v", attempt, err)
					continue
				}
				if len(devices) == 0 {
					log.Printf("[MuMu] 起動時デバイスが見つかりません (%d/3)。MuMu Playerを起動してadb connectで接続してください", attempt)
					continue
				}
				log.Printf("[MuMu] 起動時デバイス: %v", devices)
				return // デバイス確認完了
			}
			// 3回全て失敗 → まず非破壊の ADB 復旧を試す
			log.Println("[MuMu] デバイスが見つからないため ADB 復旧を試みます...")
			if err := mumu.RecoverServer(context.Background(), cfg); err != nil {
				log.Printf("[MuMu] ADB 復旧失敗: %v", err)
				return
			}
			time.Sleep(1 * time.Second)
			devices, err := mumu.ListDevices(context.Background(), cfg)
			if err != nil {
				log.Printf("[MuMu] ADB 再起動後のデバイス取得失敗: %v", err)
				return
			}
			if len(devices) == 0 {
				log.Println("[MuMu] ADB 再起動後もデバイスが見つかりません。MuMu Player が起動しているか確認してください")
			} else {
				log.Printf("[MuMu] ADB 再起動後にデバイスを検出: %v", devices)
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

	// 同一 IP を持つセッションが複数ある場合（NAT環境）に備えてスライスで保持
	ipToSess := make(map[string][]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.ClientIP != "" {
				ipToSess[sess.ClientIP] = append(ipToSess[sess.ClientIP], sess)
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
			devIP, ipErr := mumu.GetDeviceIP(ctx, ser, cfg)
			if ipErr != nil {
				log.Printf("[MuMu] GetDeviceIP %s: %v", ser, ipErr)
			}
			entries[idx].DeviceIP = devIP
			if devIP != "" {
				if sessions, ok := ipToSess[devIP]; ok && len(sessions) > 0 {
					if len(sessions) > 1 {
						log.Printf("[MuMu] device-map: IP %s が %d セッションと衝突 (NAT環境?) — 先頭セッションを使用", devIP, len(sessions))
					}
					sess := sessions[0]
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].Confirmed = sess.Confirmed
				}
			}
		}(i, serial)
	}
	wg.Wait()

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

	// 手動切替後も capDevice の currentChannel を更新する
	if req.Channel > 0 {
		s.mu.RLock()
		fn := s.channelNotifyFn
		s.mu.RUnlock()
		if fn != nil {
			fn(req.Channel)
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
	for i, ev := range s.goldBoarHistory {
		if ev.Timestamp == req.Timestamp {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s.goldBoarHistory = append(s.goldBoarHistory[:idx], s.goldBoarHistory[idx+1:]...)
		if err := s.persistGoldHistoryLocked(); err != nil {
			s.mu.Unlock()
			log.Printf("[GUI] gold history save failed: %v", err)
			http.Error(w, "save failed", 500)
			return
		}
	}
	s.mu.Unlock()

	if idx < 0 {
		http.Error(w, "not found", 404)
		return
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
					PatrolMoveTimeoutSecs  float64  `json:"patrol_move_timeout_secs"`
					PatrolMergeTimeoutSecs float64  `json:"patrol_merge_timeout_secs"`
					PatrolDwellSecs        float64  `json:"patrol_dwell_secs"`
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
						MoveTimeout:        time.Duration(appCfg.PatrolMoveTimeoutSecs * float64(time.Second)),
						MergeTimeout:       time.Duration(appCfg.PatrolMergeTimeoutSecs * float64(time.Second)),
						DwellDuration:      time.Duration(appCfg.PatrolDwellSecs * float64(time.Second)),
					}
					s.UpdatePatrollerCfg(newCfg)
					log.Printf("[GUI] 巡回設定を即時反映: 滞在=%.0fs, 初回待ち=%.0fs, マージ待ち=%.0fs, グループ間=%.0fs, 並列=%d",
						appCfg.PatrolDwellSecs, appCfg.PatrolMoveTimeoutSecs, appCfg.PatrolMergeTimeoutSecs,
						appCfg.ParallelGroupDelaySecs, appCfg.ParallelLimit)
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

// handlePatrolClearFull は満員判定リストをクリアする
func (s *Server) handlePatrolClearFull(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.ClearFullChannels()
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
	// 巡回停止後に capDevice の currentChannel をリセットし stale 値での lineID 解決を防ぐ
	s.mu.RLock()
	fn := s.channelNotifyFn
	s.mu.RUnlock()
	if fn != nil {
		fn(0)
	}
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
	if(typeof clearFullChannels==='function'){clearFullChannels=async function(){};}
})();
</script>`

const indexHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>BPSR patrol cams</title>
<style>
/* === BPSR patrol cams — Design Refresh === */
*{box-sizing:border-box;margin:0;padding:0}

/* ── Themes ── */
:root{
  /* Dark Blue — GitHub-inspired navy */
  --bg0:#0d1117;--bg1:#161b22;--bg2:#1c2128;--bg3:#262d3a;
  --accent:#58a6ff;--accent2:#3fb950;--warn:#d29922;--danger:#f85149;
  --text1:#e6edf3;--text2:#8b949e;--text3:#6e7781;
  --border:rgba(48,54,61,1);
  --radius:6px;--radius-lg:10px;
  /* Font size scale — XL is default (center). 5 steps: S M XL 2X 3X */
  --fs-base:17px;
  --fs-xs:11px;
  --fs-sm:13px;
  --fs-md:17px;
  --fs-lg:20px;
  --fs-xl:23px;
  --fs-2xl:26px;
}
/* Black — OLED pure black + electric cyan, high contrast */
[data-theme="black"]{
  --bg0:#000000;--bg1:#0c0c0c;--bg2:#181818;--bg3:#242424;
  --accent:#00e5ff;--accent2:#00e676;--warn:#ffca28;--danger:#ff5252;
  --text1:#ffffff;--text2:#cccccc;--text3:#888888;
  --border:rgba(62,62,62,1);
}
/* Light — GitHub-light style */
[data-theme="light"]{
  --bg0:#f6f8fa;--bg1:#ffffff;--bg2:#f0f2f5;--bg3:#e1e4e8;
  --accent:#0969da;--accent2:#1a7f37;--warn:#7a4500;--danger:#cf222e;
  --text1:#1f2328;--text2:#57606a;--text3:#8c959f;
  --border:rgba(208,215,222,1);
}
/* Gold + Black — luxury dark with gold accents */
[data-theme="warm"]{
  --bg0:#000000;--bg1:#0d0b00;--bg2:#1a1600;--bg3:#262000;
  --accent:#ffd700;--accent2:#4caf50;--warn:#ffa000;--danger:#ff4444;
  --text1:#ffffff;--text2:#d4bc6a;--text3:#8a7538;
  --border:rgba(100,78,0,1);
}
[data-fs="s"] {--fs-base:11px;--fs-xs:7px; --fs-sm:9px; --fs-md:11px;--fs-lg:14px;--fs-xl:17px;--fs-2xl:20px}
[data-fs="m"] {--fs-base:14px;--fs-xs:9px; --fs-sm:11px;--fs-md:14px;--fs-lg:17px;--fs-xl:20px;--fs-2xl:23px}
[data-fs="xl"]{--fs-base:17px;--fs-xs:11px;--fs-sm:13px;--fs-md:17px;--fs-lg:20px;--fs-xl:23px;--fs-2xl:26px}
[data-fs="2x"]{--fs-base:20px;--fs-xs:14px;--fs-sm:16px;--fs-md:20px;--fs-lg:23px;--fs-xl:26px;--fs-2xl:29px}
[data-fs="3x"]{--fs-base:23px;--fs-xs:17px;--fs-sm:19px;--fs-md:23px;--fs-lg:26px;--fs-xl:29px;--fs-2xl:32px}

body{background:var(--bg0);color:var(--text1);font-family:'Segoe UI',system-ui,sans-serif;font-size:var(--fs-base);line-height:1.5;height:100vh;overflow:hidden}
.app{display:grid;grid-template-rows:3px 1fr;grid-template-columns:220px 1fr;height:100vh}

/* ── Titlebar ── */
.titlebar{grid-column:1/-1;background:var(--border);transition:background .4s}
@keyframes status-flow{0%{background-position:0% 50%}100%{background-position:200% 50%}}
.titlebar.running{background:linear-gradient(90deg,#1a5c30,#3fb950,#7ee898,#3fb950,#1a5c30);background-size:200% 100%;animation:status-flow 3s linear infinite}

/* ── Sidebar ── */
.sidebar{background:var(--bg1);border-right:1px solid var(--border);overflow-y:auto;display:flex;flex-direction:column}

/* Brand header */
.sidebar-brand{padding:14px 16px 12px;border-bottom:1px solid var(--border);flex-shrink:0}
.brand-row{display:flex;align-items:center;gap:8px}
.brand-icon{width:26px;height:26px;border-radius:7px;background:var(--bg3);border:1px solid var(--border);display:flex;align-items:center;justify-content:center;flex-shrink:0;font-size:var(--fs-sm);font-weight:700;color:var(--accent);letter-spacing:-.01em;font-family:monospace}
.brand-text{flex:1;min-width:0}
.brand-name{font-size:var(--fs-md);font-weight:600;color:var(--text1);letter-spacing:.01em;line-height:1.2}
.brand-sub{font-size:var(--fs-xs);color:var(--text3);letter-spacing:.02em;margin-top:1px}
.brand-status{display:flex;align-items:center;gap:5px;margin-top:8px;padding:4px 8px;background:var(--bg2);border:1px solid var(--border);border-radius:var(--radius);font-size:var(--fs-xs);color:var(--text3)}
.brand-dot{width:6px;height:6px;border-radius:50%;background:var(--text3);flex-shrink:0;transition:background .3s}
.brand-dot.running{background:var(--accent2);animation:pulse-dot 2s ease-in-out infinite}
@keyframes pulse-dot{0%,100%{box-shadow:0 0 0 0 rgba(63,185,80,.5)}50%{box-shadow:0 0 0 5px rgba(63,185,80,0)}}
#brand-status-text{flex:1}

/* Nav */
.sidebar-nav{flex:1;padding:8px 0 4px;display:flex;flex-direction:column}
.nav-section{padding:10px 14px 3px;font-size:var(--fs-xs);font-weight:600;color:var(--text3);letter-spacing:.08em;text-transform:uppercase;user-select:none}
.nav-item{display:flex;align-items:center;gap:9px;padding:6px 14px;cursor:pointer;color:var(--text2);border-left:2px solid transparent;transition:color .12s,background .12s,border-color .12s;font-size:calc(var(--fs-base) - 1px);user-select:none}
.nav-item:hover{background:rgba(255,255,255,.04);color:var(--text1)}
.nav-item.active{background:rgba(88,166,255,.07);color:var(--accent);border-left-color:var(--accent);font-weight:500}
.nav-item.layout-edit-disabled{color:var(--text3);opacity:.4;cursor:not-allowed;pointer-events:none}
.nav-item.layout-edit-disabled:hover{background:transparent;color:var(--text3)}
.nav-icon{width:14px;height:14px;flex-shrink:0;opacity:.75}
.nav-item.active .nav-icon{opacity:1}
.nav-badge{margin-left:auto;background:var(--danger);color:#fff;font-size:var(--fs-xs);padding:1px 5px;border-radius:9px;font-weight:600}
.nav-badge.ok{background:var(--accent2)}

/* PortMap modal */
.pm-overlay{position:fixed;inset:0;background:rgba(0,0,0,.72);z-index:9999;display:flex;align-items:center;justify-content:center}
.pm-modal{background:var(--bg1);border:1px solid var(--border);border-radius:var(--radius-lg);padding:20px 22px;min-width:340px;max-width:480px;max-height:80vh;display:flex;flex-direction:column;overflow:hidden;box-shadow:0 8px 32px rgba(0,0,0,.5)}
.pm-modal-title{font-size:var(--fs-md);font-weight:600;color:var(--warn);margin-bottom:12px;display:flex;align-items:center;gap:7px;flex-shrink:0}
.pm-body{overflow-y:auto;flex:1;min-height:0}
.pm-change-row{background:var(--bg2);border:1px solid var(--border);border-radius:var(--radius);padding:8px 10px;margin-bottom:6px;font-size:var(--fs-sm);line-height:1.6}
.pm-ch{color:var(--accent);font-weight:700}
.pm-ip{font-family:monospace;color:var(--text2);font-size:var(--fs-sm);word-break:break-all}
.pm-arrow{color:var(--text3);margin:0 5px}
.pm-votes{font-size:var(--fs-xs);color:var(--text3);margin-top:2px}
.pm-btns{display:flex;gap:8px;margin-top:14px;justify-content:flex-end;flex-shrink:0}

/* Main */
.main{background:var(--bg0);overflow:hidden;padding:12px;display:flex;flex-direction:column;min-height:0}
.view{display:none;flex-direction:column;gap:10px;flex:1;min-height:0}
.view.active{display:flex}
#view-dashboard{overflow-y:auto}
#view-patrol{overflow-y:auto;padding-right:4px}
#view-settings{overflow-y:auto;padding-right:4px}
#view-data-management{overflow-y:auto;padding-right:4px}

/* Card */
.card{background:var(--bg1);border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px;position:relative}
.card-title{font-size:var(--fs-xs);font-weight:600;color:var(--text2);letter-spacing:.06em;text-transform:uppercase;margin-bottom:12px;display:flex;align-items:center;gap:7px;padding-bottom:8px;border-bottom:1px solid var(--border);cursor:default}
.card.dragging{opacity:.5;transform:scale(.98);box-shadow:0 8px 24px rgba(0,0,0,.4)}
.card-placeholder{border-color:rgba(88,166,255,.4);background:rgba(88,166,255,.05)}
.card-title .title-actions{display:flex;gap:6px;align-items:center;margin-left:auto}
.panel-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:10px}

/* Stats */
.status-grid{display:grid;gap:8px}
.stat{background:var(--bg2);border-radius:var(--radius);padding:10px 12px;border:1px solid var(--border)}
.stat-label{font-size:var(--fs-xs);color:var(--text3);margin-bottom:3px;text-transform:uppercase;letter-spacing:.06em;font-weight:500}
.stat-value{font-size:var(--fs-2xl);font-weight:600;color:var(--text1);letter-spacing:-.01em}
.stat-sub{font-size:var(--fs-xs);color:var(--text3);margin-top:2px}
.stat-value.ok{color:var(--accent2)}
.stat-value.warn{color:var(--warn)}

/* Layout helpers */
.row2{display:grid;grid-template-columns:1fr 1fr;gap:10px}
.col{display:flex;flex-direction:column;gap:10px}
.flex-row{display:flex;flex-wrap:wrap;gap:6px;align-items:center}

/* Device row */
.device-row{display:flex;align-items:center;gap:10px;padding:8px 10px;background:var(--bg2);border-radius:var(--radius);border:1px solid var(--border);margin-bottom:5px}
.device-icon{width:28px;height:28px;border-radius:6px;background:var(--bg3);display:flex;align-items:center;justify-content:center;font-size:var(--fs-md);flex-shrink:0}
.device-info{flex:1;min-width:0}
.device-name{font-size:var(--fs-sm);font-weight:500;color:var(--text1);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.device-sub{font-size:var(--fs-xs);color:var(--text2)}
.device-badge{font-size:var(--fs-xs);padding:2px 8px;border-radius:9px;font-weight:500;flex-shrink:0}
.device-badge.connected{background:rgba(63,185,80,.1);color:var(--accent2);border:1px solid rgba(63,185,80,.25)}
.device-badge.offline{background:rgba(248,81,73,.08);color:var(--danger);border:1px solid rgba(248,81,73,.2)}

/* Device list */
.device-list{display:flex;flex-direction:column;gap:5px;margin-top:6px}
.device-entry{background:var(--bg2);border:1px solid var(--border);border-radius:var(--radius);padding:7px 9px;transition:border-color .12s}
.device-entry:hover{border-color:var(--text3)}
.device-entry .serial{color:var(--accent);font-family:monospace;font-size:.88em}
.device-entry .uid{color:var(--text3);font-size:.82em}
.device-entry.matched .uid{color:var(--warn)}
.no-devices{color:var(--text2);font-size:.85em;padding:4px 0}

/* Buttons */
button,.btn{background:var(--bg2);border:1px solid var(--border);color:var(--text1);border-radius:var(--radius);padding:5px 11px;font-size:var(--fs-sm);cursor:pointer;display:inline-flex;align-items:center;gap:5px;transition:background .12s,border-color .12s,color .12s;white-space:nowrap;font-family:inherit;font-weight:450;line-height:1.4}
button:hover,.btn:hover{background:var(--bg3);border-color:var(--text3);color:var(--text1)}
button:disabled,.btn:disabled{opacity:.3;cursor:default}
button.green,.btn.success{background:rgba(63,185,80,.08);border-color:rgba(63,185,80,.25);color:var(--accent2)}
button.green:hover,.btn.success:hover{background:rgba(63,185,80,.16);border-color:rgba(63,185,80,.4)}
.btn.primary{background:rgba(88,166,255,.08);border-color:rgba(88,166,255,.3);color:var(--accent)}
.btn.primary:hover{background:rgba(88,166,255,.16);border-color:rgba(88,166,255,.5)}
.btn.danger{background:rgba(248,81,73,.06);border-color:rgba(248,81,73,.22);color:var(--danger)}
.btn.danger:hover{background:rgba(248,81,73,.14)}
button.toggle-btn,.btn.toggle-btn{padding:4px 10px;font-size:var(--fs-sm)}
button.toggle-btn.active,.btn.toggle-btn.active,.btn.active{background:rgba(88,166,255,.1);border-color:rgba(88,166,255,.35);color:var(--accent)}
.btn-row{display:flex;gap:6px;flex-wrap:wrap;margin-top:8px}
input[type=text],input[type=number],textarea,select{background:var(--bg2);color:var(--text1);border:1px solid var(--border);border-radius:var(--radius);padding:4px 8px;font-size:var(--fs-sm);font-family:inherit;outline:none;transition:border-color .12s,box-shadow .12s}
input[type=text]:focus,input[type=number]:focus,textarea:focus,select:focus{border-color:rgba(88,166,255,.5);box-shadow:0 0 0 2px rgba(88,166,255,.1)}
input[type=checkbox]{accent-color:var(--accent);width:14px;height:14px}

/* Log */
.log-area{background:var(--bg0);border:1px solid var(--border);border-radius:var(--radius);overflow-y:auto;padding:6px 8px;font-family:'Consolas','Cascadia Code',monospace;font-size:var(--fs-sm);display:flex;flex-direction:column;gap:1px;min-height:0}
.log-area.full{flex:1;max-height:none}
.log-line{display:flex;gap:7px;align-items:flex-start;padding:2px 3px;border-radius:4px}
.log-line:hover{background:rgba(255,255,255,.04)}
.log-time{color:var(--text3);flex-shrink:0;min-width:52px;font-size:var(--fs-xs);padding-top:2px}
.log-tag{flex-shrink:0;font-size:var(--fs-xs);padding:1px 5px;border-radius:3px;font-weight:600;min-width:42px;text-align:center;margin-top:1px}
.log-tag.info{background:rgba(88,166,255,.12);color:var(--accent)}
.log-tag.ok{background:rgba(63,185,80,.12);color:var(--accent2)}
.log-tag.warn{background:rgba(210,153,34,.12);color:var(--warn)}
.log-tag.err{background:rgba(248,81,73,.12);color:var(--danger)}
.log-msg{color:var(--text1);flex:1;word-break:break-all;line-height:1.5;white-space:pre-wrap}
.log-line.detect .log-msg{color:var(--warn);font-weight:500}

/* Chips */
.chip-row{display:flex;gap:5px;flex-wrap:wrap;margin-bottom:8px}
.chip{background:var(--bg2);border:1px solid var(--border);border-radius:99px;padding:3px 10px;font-size:var(--fs-xs);color:var(--text2);cursor:pointer;transition:all .12s}
.chip.active{background:rgba(88,166,255,.1);border-color:rgba(88,166,255,.35);color:var(--accent)}
.chip:hover{color:var(--text1);border-color:var(--text3)}

/* Patrol */
.patrol-status{background:var(--bg2);border:1px solid var(--border);border-radius:var(--radius);padding:6px 10px;font-size:var(--fs-sm);margin-bottom:8px}
.patrol-status span{margin-right:10px}
.patrol-status .running{color:var(--accent2);font-weight:600}
.patrol-status .stopped{color:var(--text3)}
.ch-editor{display:flex;flex-direction:column;gap:3px;max-height:180px;overflow-y:auto;margin:6px 0}
.ch-row{display:flex;gap:5px;align-items:center;background:var(--bg2);border:1px solid var(--border);border-radius:var(--radius);padding:4px 8px;transition:border-color .12s}
.ch-row:hover{border-color:var(--text3)}
.ch-row .ch-num{color:var(--accent);font-family:monospace;font-size:.9em;min-width:24px;text-align:right}

/* Gold table */
.gold-table{width:100%;border-collapse:collapse;font-size:.82em;table-layout:fixed}
.gold-table th{color:var(--text2);text-align:left;padding:4px 8px;border-bottom:1px solid var(--border);white-space:nowrap;position:sticky;top:0;background:var(--bg1);z-index:1;font-size:.88em;font-weight:600;text-transform:uppercase;letter-spacing:.04em}
.gold-table col.col-time{width:90px}.gold-table col.col-ch{width:46px}.gold-table col.col-name{width:110px}.gold-table col.col-action{width:38px}
.gold-table td{padding:5px 8px;border-bottom:1px solid var(--border);vertical-align:middle;line-height:1.4;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.gold-table .action-cell{padding:0;white-space:normal;text-align:center}
.gold-table .action-cell button{background:transparent;border:none;color:var(--danger);font-size:1em;cursor:pointer;padding:0 4px;line-height:1.5}
.gold-table .action-cell button:hover{color:#ff8a8a}
.gold-table tr:hover td{background:rgba(255,255,255,.03)}
.gold-table .ch-cell{color:var(--accent);font-family:monospace;font-weight:600}
.gold-table .time-cell{color:var(--text3);font-size:.85em}
.gold-table .name-cell{color:var(--warn);font-weight:500}
.gold-table .name-cell.silver{color:var(--text2)}
.no-history{color:var(--text2);font-size:.85em;padding:8px 0}
#gold-history-container{flex:1;min-height:0;max-height:none;overflow-y:auto}

/* Config */
.cfg-grid{display:grid;grid-template-columns:1fr 1fr;gap:10px 20px}
.cfg-stack{display:flex;flex-direction:column;gap:10px}
.cfg-card{border:1px solid var(--border);border-radius:var(--radius-lg);padding:12px;background:var(--bg1)}
.cfg-card.chat-filter-card{min-height:0}
.chat-filter-card #cfg-chat-form{grid-template-columns:1fr}
.cfg-rule-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:12px}
.chat-filter-card .cfg-rule-grid{grid-template-columns:1fr}
.cfg-rule-box{border:1px solid var(--border);border-radius:var(--radius);padding:10px;background:var(--bg0)}
.cfg-rule-box-title{font-size:.8em;color:var(--text1);font-weight:600;margin-bottom:8px}
.cfg-rule-actions{display:flex;gap:6px;align-items:center;flex-wrap:wrap;margin-bottom:8px}
.cfg-rule-actions select{min-width:150px;flex:1}
.cfg-rule-actions input{flex:1;min-width:120px}
.cfg-rule-table-wrap{border:1px solid var(--border);border-radius:var(--radius);overflow:auto;background:var(--bg1);max-height:255px}
.cfg-rule-table{width:100%;border-collapse:collapse;font-size:.8em;table-layout:fixed}
.cfg-rule-table th,.cfg-rule-table td{border-bottom:1px solid var(--border);border-right:1px solid var(--border);padding:6px 8px;vertical-align:top;line-height:1.4;word-break:break-word}
.cfg-rule-table th:last-child,.cfg-rule-table td:last-child{border-right:none}
.cfg-rule-table th{background:var(--bg2);color:var(--text2);font-weight:600;position:sticky;top:0;z-index:1}
.cfg-rule-table tr:last-child td{border-bottom:none}
.cfg-rule-table td.cell-mono{font-family:Consolas,monospace;color:var(--accent)}
.cfg-rule-table td.filter-include{color:var(--accent2);font-weight:600}
.cfg-rule-table td.filter-exclude{color:var(--danger);font-weight:600}
.cfg-rule-empty{padding:10px;color:var(--text3);font-size:.8em}
.cfg-hidden-field{display:none}
.cfg-rule-header{display:flex;align-items:center;gap:8px;margin-bottom:8px}
.cfg-rule-header .cfg-rule-box-title{margin-bottom:0}
.cfg-rule-window-body{margin:0;background:#0b0e15;color:#e6edf6;font-family:'Segoe UI',sans-serif;padding:16px}
.cfg-rule-window-section{margin-bottom:18px}
.cfg-rule-window-title{font-size:14px;font-weight:600;color:#e6edf6;margin-bottom:10px}
.cfg-rule-window-note{font-size:12px;color:#98a3b8;margin-bottom:10px}
.cfg-field{display:flex;flex-direction:column;gap:3px}
.cfg-field label{color:var(--text1);font-size:.82em;font-weight:500}
.cfg-field input{width:100%}
.cfg-field textarea{width:100%;min-height:calc(1.5em * 10 + 18px);max-height:calc(1.5em * 10 + 18px);resize:vertical;overflow-y:auto;line-height:1.5}
.cfg-save-bar{display:flex;gap:8px;align-items:center;margin-top:10px}
.cfg-note{font-size:.75em;color:var(--text3);margin-top:3px}
.section-title{font-size:.78em;color:var(--accent);font-weight:500;margin:10px 0 5px;text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid var(--border);padding-bottom:4px}
.cfg-card-title{font-size:.86em;color:var(--text1);font-weight:600;margin-bottom:10px}
.cfg-card-title-collapsible{display:flex;align-items:center;gap:8px}
.cfg-card-title-collapsible .btn{margin-left:auto;padding:2px 8px;font-size:var(--fs-xs)}
.chat-filter-card.minimized #chat-filter-content{display:none}
.chat-score-threshold-box{padding:10px 12px;border:1px solid var(--border);border-radius:10px;background:var(--bg2);margin-bottom:10px}
.chat-score-threshold-title{font-size:var(--fs-sm);font-weight:600;color:var(--text1);margin-bottom:8px}
.chat-score-threshold-controls{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.chat-score-threshold-controls input[type=range]{flex:1;min-width:180px}
.chat-score-threshold-controls input[type=number]{width:72px}
.chat-score-threshold-value{font-size:var(--fs-sm);color:var(--text2);min-width:120px}
.check-label{display:flex;align-items:center;gap:5px;cursor:pointer;color:var(--text1)}

/* Chat */
.chat-toolbar{padding:5px 10px;border-bottom:1px solid var(--border);background:var(--bg1);display:flex;align-items:center;gap:8px;font-size:var(--fs-sm);flex-shrink:0}
.chat-msg{padding:5px 10px;border-bottom:1px solid var(--border);line-height:1.5;transition:background .12s;font-size:.82em}
.chat-msg:hover{background:rgba(255,255,255,.03)}
.chat-msg.report{border-left:3px solid rgba(210,153,34,.4);padding-left:7px}
.chat-msg-main{display:flex;gap:8px;align-items:flex-start}
.chat-msg-body{flex:1;min-width:0;word-break:break-word}
.chat-msg-text{color:var(--text1);user-select:text}
.chat-msg-actions{display:flex;gap:4px;flex-wrap:wrap;opacity:0;transition:opacity .12s;align-items:center;pointer-events:none}
.chat-msg:hover .chat-msg-actions{opacity:1;pointer-events:auto}
.chat-action-btn{padding:2px 6px;font-size:var(--fs-xs);border-radius:999px;background:rgba(88,166,255,.08);border:1px solid rgba(88,166,255,.22);color:var(--accent)}
.chat-action-btn.exclude{background:rgba(248,81,73,.06);border-color:rgba(248,81,73,.18);color:var(--danger)}
.chat-split{display:flex;flex-direction:column;gap:10px;min-height:0;flex:1}
.chat-report-list{display:flex;flex-direction:column;gap:0;background:var(--bg0);border:1px solid var(--border);border-radius:var(--radius);overflow-y:auto;flex:1;min-height:0}
.chat-report-empty{color:var(--text3);padding:10px;font-size:.82em}
.chat-report-summary{color:var(--text3);font-size:.76em;line-height:1.5;margin-bottom:8px}
.chat-report-score{display:inline-block;margin-left:6px;padding:1px 6px;border-radius:999px;background:rgba(210,153,34,.1);border:1px solid rgba(210,153,34,.2);color:var(--warn);font-size:var(--fs-xs);vertical-align:middle}

/* Crash warning */
.crash-warning{display:none;background:rgba(248,81,73,.08);border:1px solid rgba(248,81,73,.22);border-radius:var(--radius);padding:6px 10px;font-size:.8em;color:var(--danger);margin-bottom:6px}
.patrol-progress{margin-top:10px}
.patrol-progress .progress-line{height:4px;border-radius:999px;background:var(--bg3);overflow:hidden}
.patrol-progress .progress-fill{height:100%;width:0%;background:linear-gradient(90deg,var(--accent),var(--accent2));transition:width .3s ease}
.patrol-progress .progress-text{display:flex;justify-content:space-between;gap:10px;font-size:.82em;color:var(--text3);margin-top:5px}

/* Scrollbar */
::-webkit-scrollbar{width:5px;height:5px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:var(--bg3);border-radius:3px}
::-webkit-scrollbar-thumb:hover{background:var(--text3)}

/* Dashboard grid */
.dashboard-grid{--dash-grid-row-unit:8px;--dash-grid-gap:10px;display:grid;grid-template-columns:repeat(2,minmax(0,1fr));grid-auto-rows:var(--dash-grid-row-unit);gap:var(--dash-grid-gap)}
.dashboard-grid > .card{position:relative;min-width:0;min-height:0;overflow:auto;grid-row:span var(--panel-rows,28)}
.panel-size-1x1,.panel-size-1x2{grid-column:span 1}
.panel-size-2x1,.panel-size-2x2,.panel-size-2x3,.panel-size-2x4{grid-column:span 2}
.panel-col-1.panel-size-1x1,.panel-col-1.panel-size-1x2{grid-column:1 / span 1}
.panel-col-2.panel-size-1x1,.panel-col-2.panel-size-1x2{grid-column:2 / span 1}
.panel-size-1x1,.panel-size-2x1{--panel-rows:28}
.panel-size-1x2,.panel-size-2x2{--panel-rows:52}
.panel-size-2x3{--panel-rows:76}
.panel-size-2x4{--panel-rows:100}
#card-dash-gold{display:flex;flex-direction:column;min-height:0}
#dash-chat-area{height:420px;overflow-y:auto;flex-shrink:0}
#dash-chat-report-area{flex:1;min-height:0;overflow-y:auto}

/* Edit mode */
.layout-edit-surface.edit-mode .card{outline:1px dashed rgba(210,153,34,.4);outline-offset:1px}
.layout-edit-surface.edit-mode .card-title{cursor:grab}
.layout-edit-surface.edit-mode .card.dragging{cursor:grabbing}
.dashboard-grid.edit-mode .card{outline:1px dashed rgba(210,153,34,.35);outline-offset:1px;cursor:grab}
.dashboard-grid.edit-mode .card > :not(.panel-resize-handle-x):not(.panel-resize-handle-y){pointer-events:none}
.dashboard-grid:not(.edit-mode) .card .card-title{cursor:default}
.panel-resize-handle-y,.panel-resize-handle-x{display:none;position:absolute;opacity:.6;box-shadow:0 0 0 1px rgba(210,153,34,.2)}
.panel-resize-handle-y{left:50%;bottom:4px;transform:translateX(-50%);width:52px;height:10px;border-radius:999px;cursor:ns-resize;background:repeating-linear-gradient(90deg,rgba(210,153,34,.7) 0 6px,transparent 6px 10px)}
.panel-resize-handle-x{top:50%;width:10px;height:52px;border-radius:999px;cursor:ew-resize;background:repeating-linear-gradient(180deg,rgba(210,153,34,.7) 0 6px,transparent 6px 10px)}
.panel-resize-handle-x.handle-right{right:4px;transform:translateY(-50%)}
.panel-resize-handle-x.handle-left{left:4px;transform:translateY(-50%)}
.dashboard-grid.edit-mode .panel-resize-handle-y,.dashboard-grid.edit-mode .panel-resize-handle-x{display:block}
.dashboard-grid.edit-mode .panel-resize-handle-x.is-hidden{display:none}
.dashboard-grid.edit-mode .card.resizing-y{user-select:none;cursor:ns-resize}
.dashboard-grid.edit-mode .card.resizing-x{user-select:none;cursor:ew-resize}
.dashboard-grid.edit-mode .card.dragging{cursor:grabbing}

/* Sidebar bottom */
.sidebar-bottom{margin-top:auto;border-top:1px solid var(--border);padding:0}
.nav-item.layout-edit-active{background:rgba(210,153,34,.08);color:var(--warn);border-left-color:var(--warn)}

/* Appearance panel */
.appearance-panel{padding:10px 14px;border-bottom:1px solid var(--border)}
.appearance-row{display:flex;align-items:center;gap:6px;margin-bottom:7px}
.appearance-row:last-child{margin-bottom:0}
.ap-label{font-size:var(--fs-xs);font-weight:600;color:var(--text3);letter-spacing:.08em;text-transform:uppercase;min-width:38px;flex-shrink:0}
.theme-swatches{display:flex;gap:4px;flex:1}
.theme-swatch{
  width:18px;height:18px;border-radius:4px;cursor:pointer;
  border:2px solid transparent;transition:border-color .12s,transform .12s;
  position:relative;flex-shrink:0;
}
.theme-swatch:hover{transform:scale(1.15)}
.theme-swatch.active{border-color:var(--accent)}
.theme-swatch[data-t="dark-blue"]{background:linear-gradient(135deg,#161b22 50%,#58a6ff 50%);border:2px solid transparent}
.theme-swatch[data-t="black"]{background:linear-gradient(135deg,#0c0c0c 50%,#00e5ff 50%)}
.theme-swatch[data-t="light"]{background:linear-gradient(135deg,#ffffff 50%,#0969da 50%);border:1px solid var(--border)}
.theme-swatch[data-t="warm"]{background:linear-gradient(135deg,#0d0b00 50%,#ffd700 50%)}
.fs-btns{display:flex;gap:3px;flex:1}
.fs-btn{background:var(--bg2);border:1px solid var(--border);color:var(--text3);border-radius:4px;padding:2px 0;font-size:var(--fs-xs);cursor:pointer;transition:all .12s;font-family:inherit;flex:1;text-align:center}
.fs-btn:hover{color:var(--text1);background:var(--bg3)}
.fs-btn.active{background:rgba(88,166,255,.1);border-color:rgba(88,166,255,.35);color:var(--accent)}

/* Chat log */
#card-chat-log{flex:1;min-width:0;min-height:0}
#card-chat-report-col{flex:1;min-width:0;min-height:0}
.chat-swap-handle{display:none;align-items:center;justify-content:center;height:28px;flex:none;cursor:pointer;border-radius:var(--radius);border:1px dashed rgba(210,153,34,.4);color:var(--warn);font-size:var(--fs-sm);gap:6px;background:rgba(210,153,34,.04)}
.chat-swap-handle:hover{background:rgba(210,153,34,.1)}
#view-chat-log.edit-mode .card{outline:1px dashed rgba(210,153,34,.4);outline-offset:1px}
#view-chat-log.edit-mode .chat-swap-handle{display:flex}
.grid-drop-indicator{background:rgba(88,166,255,.05);border:1px dashed rgba(88,166,255,.3);border-radius:var(--radius-lg);pointer-events:none;min-height:60px}

/* ── Light theme overrides ── */
[data-theme="light"] .nav-item:hover{background:rgba(0,0,0,.05)}
[data-theme="light"] .nav-item.active{background:rgba(9,105,218,.08);color:#0969da;border-left-color:#0969da}
[data-theme="light"] .log-line:hover{background:rgba(0,0,0,.04)}
[data-theme="light"] .chat-msg:hover{background:rgba(0,0,0,.03)}
[data-theme="light"] .gold-table tr:hover td{background:rgba(0,0,0,.03)}
[data-theme="light"] .device-entry:hover{border-color:#8c959f}
[data-theme="light"] .card{box-shadow:0 1px 3px rgba(0,0,0,.08)}
[data-theme="light"] .titlebar{background:rgba(208,215,222,1)}
[data-theme="light"] button:hover,[data-theme="light"] .btn:hover{background:rgba(0,0,0,.06);border-color:#8c959f}
[data-theme="light"] .brand-icon{background:#e1e4e8;color:#0969da}
[data-theme="light"] .btn.success{background:rgba(26,127,55,.08);border-color:rgba(26,127,55,.25);color:#1a7f37}
[data-theme="light"] .btn.primary{background:rgba(9,105,218,.08);border-color:rgba(9,105,218,.25);color:#0969da}
[data-theme="light"] .btn.danger{background:rgba(207,34,46,.06);border-color:rgba(207,34,46,.22);color:#cf222e}
[data-theme="light"] .chip.active{background:rgba(9,105,218,.1);border-color:rgba(9,105,218,.35);color:#0969da}
[data-theme="light"] .nav-item.layout-edit-active{background:rgba(122,69,0,.08);color:#7a4500;border-left-color:#7a4500}
</style>
</head>
<body data-theme="dark-blue" data-fs="xl">
<div class="app">
<!-- Titlebar -->
<div class="titlebar" id="hdr-bar"></div>
<!-- Sidebar -->
<div class="sidebar">
  
<div class="sidebar-brand">
  <div class="brand-row">
    <div class="brand-icon">BP</div>
    <div class="brand-text">
      <div class="brand-name">BPSR patrol cams</div>
      <div class="brand-sub">Blue Protocol Star Resonance</div>
    </div>
  </div>
	<div class="brand-status" id="brand-status-bar">
		<span class="brand-dot" id="brand-dot"></span>
		<span id="brand-status-text">待機中</span>
	</div>
	<div class="brand-status" id="brand-uptime-box" style="margin-top:6px; display:flex; align-items:center; gap:5px; background:var(--bg2); border:1px solid var(--border); border-radius:var(--radius); font-size:var(--fs-xs); color:var(--text3);">
		<span style="font-weight:600; color:var(--accent2);">稼働:</span>
		<span id="brand-uptime-small" style="font-family:monospace; font-size:var(--fs-xs); color:var(--text3);"></span>
	</div>
	<div class="brand-status" id="brand-time-box" style="margin-top:4px; display:flex; align-items:center; gap:5px; background:var(--bg2); border:1px solid var(--border); border-radius:var(--radius); font-size:var(--fs-xs); color:var(--text3);">
		<span style="font-weight:600; color:var(--accent);">時刻:</span>
		<span id="brand-current-time-small" style="font-family:monospace; font-size:var(--fs-xs); color:var(--text3);"></span>
	</div>
</div>
<div class="sidebar-nav">
<div class="nav-item active" id="nav-dashboard" onclick="switchView('dashboard',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="currentColor"><rect x="1" y="1" width="6" height="6" rx="1.5"/><rect x="9" y="1" width="6" height="6" rx="1.5"/><rect x="1" y="9" width="6" height="6" rx="1.5"/><rect x="9" y="9" width="6" height="6" rx="1.5"/></svg>
    ダッシュボード
  </div>
  <div class="nav-item" id="nav-detect-log" onclick="switchView('detect-log',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="8" r="6.5"/><path d="M8 4.5v4l2.5 1.5" stroke-linecap="round"/></svg>
    検知ログ
  </div>
  <div class="nav-item" id="nav-chat-log" onclick="switchView('chat-log',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 3h12a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1H5l-3 2V4a1 1 0 0 1 1-1z"/></svg>
    チャットログ
  </div>
  <div class="nav-item" id="nav-devices" onclick="switchView('devices',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M5 7h6M5 10h4"/></svg>
    デバイス管理
  </div>
  <div class="nav-item" id="nav-patrol" onclick="switchView('patrol',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M8 2v4M8 10v4M2 8h4M10 8h4" stroke-linecap="round"/></svg>
    チャンネル巡回
  </div>
  <div class="nav-item" id="nav-settings" onclick="switchView('settings',this)">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="8" r="2.5"/><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4" stroke-linecap="round"/></svg>
    設定
  </div>
  <div class="nav-item" id="nav-data-management" onclick="switchView('data-management',this);loadPortMapEntries()">
    <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><ellipse cx="8" cy="4.5" rx="5.5" ry="2"/><path d="M2.5 4.5v3c0 1.1 2.46 2 5.5 2s5.5-.9 5.5-2v-3"/><path d="M2.5 7.5v3c0 1.1 2.46 2 5.5 2s5.5-.9 5.5-2v-3"/></svg>
    データ管理
  </div>
  </div><!-- /sidebar-nav -->
<div class="sidebar-bottom"><div class="appearance-panel">
  <div class="appearance-row">
    <span class="ap-label">テーマ</span>
    <div class="theme-swatches">
      <div class="theme-swatch active" data-t="dark-blue" title="Dark Blue" onclick="setTheme('dark-blue',this)"></div>
      <div class="theme-swatch" data-t="black" title="Black" onclick="setTheme('black',this)"></div>
      <div class="theme-swatch" data-t="light" title="Light" onclick="setTheme('light',this)"></div>
      <div class="theme-swatch" data-t="warm" title="Warm" onclick="setTheme('warm',this)"></div>
    </div>
  </div>
  <div class="appearance-row">
    <span class="ap-label">文字</span>
    <div class="fs-btns">
      <button class="fs-btn" data-fs="s"  onclick="setFontSize('s', this)">SS</button>
      <button class="fs-btn" data-fs="m"  onclick="setFontSize('m', this)">S</button>
      <button class="fs-btn active" data-fs="xl" onclick="setFontSize('xl',this)">M</button>
      <button class="fs-btn" data-fs="2x" onclick="setFontSize('2x',this)">L</button>
      <button class="fs-btn" data-fs="3x" onclick="setFontSize('3x',this)">XL</button>
    </div>
  </div>
</div>

    <div class="nav-item" id="nav-layout-edit" onclick="toggleLayoutEdit()">
      <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="1" y="1" width="6" height="6" rx="1"/><rect x="9" y="1" width="6" height="6" rx="1"/><rect x="1" y="9" width="6" height="6" rx="1"/><rect x="9" y="9" width="6" height="6" rx="1"/></svg>
      レイアウト編集
    </div>
  </div>
</div>
<!-- Main content -->
<div class="main">
<!-- ===== DASHBOARD ===== -->
<div class="view active" id="view-dashboard">
  <div class="status-grid" style="grid-template-columns:repeat(2,1fr)">
    <div class="stat"><div class="stat-label">検知イベント</div><div class="stat-value warn" id="dash-detect-count">0</div><div class="stat-sub" id="dash-detect-meta">0/hour · --</div></div>
    <div class="stat" style="position:relative">
      <div class="stat-label" style="display:flex;align-items:center">稼働時間<button onclick="resetUptime()" title="リセット" style="margin-left:auto;background:transparent;border:none;color:var(--text3);cursor:pointer;padding:0 2px;font-size:var(--fs-sm);line-height:1;transition:color .12s" onmouseover="this.style.color='var(--text1)'" onmouseout="this.style.color='var(--text3)'">&#8635;</button></div>
      <div class="stat-value ok" id="dash-uptime">00:00:00</div>
      <div class="stat-sub" id="dash-current-time" style="font-family:monospace;letter-spacing:.04em">--:--:--</div>
    </div>
  </div>
  <div class="dashboard-grid" id="dashboard-grid">
    <!-- 接続デバイス -->
		<div class="card panel-size-1x1" id="card-dash-devices">
      <div class="card-title"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M5 7h6M5 10h4"/></svg>接続デバイス</div>
      <div id="dash-device-list"><div class="no-devices">読み込み中...</div></div>
      <div class="btn-row">
        <button class="btn primary" onclick="scanDevices();switchView('devices',document.getElementById('nav-devices'))">🔍 再スキャン</button>
        <button class="btn" onclick="switchView('devices',document.getElementById('nav-devices'))">管理 →</button>
      </div>
    </div>
		<!-- 巡回制御 -->
		<div class="card panel-size-1x1" id="card-dash-patrol">
			<div class="card-title"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M8 2v4M8 10v4M2 8h4M10 8h4" stroke-linecap="round"/></svg>巡回制御</div>
      <div class="patrol-status"><span id="dash-ps-state" class="stopped">■ 停止中</span><span id="dash-ps-ch" style="color:var(--accent)"></span><span id="dash-ps-prog" style="color:var(--text3)"></span><span id="dash-ps-parallel"></span></div>
      <div class="patrol-progress" id="dash-patrol-progress">
        <div class="progress-line"><div class="progress-fill" id="dash-patrol-fill"></div></div>
        <div class="progress-text"><span id="dash-patrol-label">--</span><span id="dash-patrol-percent"></span></div>
      </div>
      <div id="dash-crash-warning" class="crash-warning"></div>
      <div style="display:flex;align-items:center;gap:8px;min-height:1.2em;margin-bottom:6px">
        <div id="dash-ps-full" style="font-size:.78em;color:#fca5a5;flex:1"></div>
        <button id="btn-dash-clear-full" class="btn" style="font-size:.75em;padding:2px 8px;display:none" onclick="clearFullChannels()">✕ クリア</button>
      </div>
			<div class="flex-row" style="margin-bottom:8px">
				<label style="font-size:var(--fs-sm)">開始Ch:</label>
				<select id="dash-patrol-start-ch-select" style="width:80px"></select>
				<input type="number" id="dash-patrol-start-ch" min="0" max="9999" value="0" style="width:65px" title="0=前回位置から再開" oninput="syncPatrolStartCh(this.value)">
				<button class="btn toggle-btn" id="dash-btn-reversed" onclick="toggleReversed()">⬆ 正順</button>
				<button class="btn toggle-btn" id="dash-btn-loop" onclick="toggleLoop()">🔁 ループ</button>
			</div>
      <div class="flex-row">
        <button class="btn success" id="dash-btn-patrol-start" onclick="patrolStart()" disabled>▶ 巡回開始</button>
        <button class="btn" id="dash-btn-patrol-stop" onclick="patrolStop()" disabled>■ 停止</button>
        <button class="btn" onclick="switchView('patrol',document.getElementById('nav-patrol'))">詳細 →</button>
      </div>
      <div class="status-grid" style="grid-template-columns:repeat(2,minmax(0,1fr));margin-top:10px">
        <div class="stat">
          <div class="stat-label">1サイクル時間</div>
          <div class="stat-value" id="dash-ps-cycle-time">--</div>
        </div>
        <div class="stat">
          <div class="stat-label">巡回速度</div>
          <div class="stat-value ok" id="dash-ps-cycle-rate">--</div>
        </div>
      </div>
    </div>
    <!-- レアエネミー検知履歴 -->
		<div class="card panel-size-1x1" id="card-dash-gold">
		<div class="card-title">🌟 レアエネミー検知履歴<button class="btn" style="margin-left:auto;padding:2px 8px;font-size:var(--fs-xs)" onclick="clearAllGoldHistory()">✕ 一括クリア</button><button class="btn" style="padding:2px 8px;font-size:var(--fs-xs)" onclick="window.open('/spawn-log','spawn-log','width=600,height=400')">⧉</button></div>
      <div id="gold-history-container"><div class="no-history">検知履歴なし</div></div>
    </div>
    <!-- チャットログ -->
		<div class="card panel-size-1x1" id="card-dash-chat" style="display:flex;flex-direction:column;padding:0;overflow:hidden;--panel-rows:58">
      <div class="card-title" style="padding:10px 14px 8px;margin-bottom:0;border-bottom:1px solid var(--border)">
        <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 3h12a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1H5l-3 2V4a1 1 0 0 1 1-1z"/></svg>
        チャットログ
        <button class="btn" style="margin-left:auto;padding:2px 8px;font-size:var(--fs-xs)" onclick="switchView('chat-log',document.getElementById('nav-chat-log'))">展開 →</button>
      </div>
	<div id="dash-chat-area"></div>
    </div>
    <!-- 発見報告候補 -->
		<div class="card panel-size-1x1" id="card-dash-report" style="display:flex;flex-direction:column;padding:0;overflow:hidden">
      <div class="card-title" style="padding:10px 14px 8px;margin-bottom:0;border-bottom:1px solid var(--border)">
        発見報告候補
        <span style="margin-left:auto;font-size:var(--fs-xs);color:var(--text3)">自動抽出</span>
      </div>
      <div id="dash-chat-report-area" class="chat-report-list"></div>
    </div>
  </div>
</div>
<!-- ===== 検知ログ ===== -->
<div class="view" id="view-detect-log">
  <div class="card" style="flex:1;display:flex;flex-direction:column;min-height:0">
    <div class="card-title" style="margin-bottom:6px"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 4h12M2 8h8M2 12h10" stroke-linecap="round"/></svg>検知ログ</div>
    <div class="chip-row" id="log-filter-bar"></div>
    <div class="log-area full" id="log-area" style="flex:1;max-height:calc(100vh - 200px)"></div>
    <div style="display:flex;gap:6px;margin-top:8px">
      <button class="btn" onclick="document.getElementById('log-area').innerHTML=''">クリア</button>
      <button class="btn primary" onclick="testDetect()">🔔 テスト通知</button>
      <button class="btn" onclick="testDetect('ウリボゴールド')">🔔 ウリボゴールド</button>
      <button class="btn" onclick="testDetect('金ナッポ')">🔔 金ナッポ</button>
      <button class="btn" onclick="testDetect('銀ナッポ')">🔔 銀ナッポ</button>
      <button class="btn" style="margin-left:auto" onclick="window.open('/spawn-log','spawn-log','width=600,height=400')">⧉ 別ウィンドウ</button>
    </div>
  </div>
</div>
<!-- ===== チャットログ ===== -->
<div class="view" id="view-chat-log">
	<div class="chat-split" id="chat-split-container">
		<div class="card" id="card-chat-log" style="display:flex;flex-direction:column;padding:0;overflow:hidden;min-height:0">
			<div class="chat-toolbar">
				<svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 3h12a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1H5l-3 2V4a1 1 0 0 1 1-1z"/></svg>
				<span style="font-weight:500;color:var(--text2);text-transform:uppercase;font-size:var(--fs-xs);letter-spacing:.6px">チャットログ</span>
				<select id="chat-device-select" onchange="renderChatPanel()" style="font-size:var(--fs-sm);padding:2px 6px;max-width:140px"><option value="">すべて</option></select>
				<input type="text" id="chat-search" placeholder="キーワード検索..." style="width:130px;font-size:var(--fs-sm)" oninput="renderChatPanel()">
				<button class="btn" style="margin-left:auto" onclick="window.open('/chat-log','chat-log','width=700,height=500')">⧉ 別ウィンドウ</button>
				<button class="btn" onclick="clearChatPanel()">クリア</button>
			</div>
			<div class="log-area full" id="chat-area" style="flex:1;border-radius:0;border:none;border-top:1px solid var(--border)"></div>
		</div>
		<div class="chat-swap-handle" id="chat-swap-handle" onclick="swapChatPanels()" title="上下を入れ替え">⇅ 上下入れ替え</div>
		<div class="card" id="card-chat-report-col" style="display:flex;flex-direction:column;padding:14px;overflow:hidden;min-height:0">
			<div class="card-title" style="display:flex;align-items:center;gap:8px"><span>発見報告候補</span><button id="notify-sound-toggle" class="btn" style="margin-left:auto;font-size:var(--fs-sm);padding:2px 8px" onclick="toggleNotifySound()" title="通知音ON/OFF">🔔</button><input type="range" id="notify-sound-volume" min="0" max="1" step="0.05" style="width:70px" title="通知音量" oninput="setNotifyVolume(this.value)"></div>
			<div class="chat-report-summary" id="chat-report-summary">発見・出現・湧き・チャンネル番号を含む短文を優先して表示します。</div>
			<div id="chat-report-area" class="chat-report-list"></div>
		</div>
	</div>
</div>
<!-- ===== デバイス管理 ===== -->
<div class="view" id="view-devices">
  <div class="card" style="margin-bottom:10px">
    <div class="card-title">デバイス操作</div>
    <div class="flex-row" style="margin-bottom:8px;flex-wrap:wrap;gap:6px">
      <button class="btn primary" id="btn-adb-scan" onclick="scanDevices()">🔍 再スキャン</button>
      <button class="btn" id="btn-adb-restart" onclick="restartADB()">🔄 ADB再起動</button>
      <button class="btn danger" id="btn-adb-kill" onclick="killADB()" style="margin-left:auto">⏹ ADB停止</button>
    </div>
    <div class="flex-row" style="gap:6px">
      <input type="text" id="adb-add-serial" placeholder="host:port を入力（例: 127.0.0.1:16384）" style="flex:1;font-size:.85em">
      <button class="btn" onclick="addADBDevice()" style="white-space:nowrap">＋ 追加接続</button>
    </div>
    <div id="adb-op-status" style="font-size:.82em;color:var(--accent2);margin-top:6px;min-height:1.2em"></div>
  </div>
  <div class="card">
    <div class="card-title">接続デバイス一覧</div>
    <div class="flex-row" style="margin-bottom:6px">
      <button class="btn" onclick="selectAllDevices()">☑ 全選択</button>
      <button class="btn" onclick="deselectAllDevices()">☐ 全解除</button>
      <button class="btn" style="margin-left:auto" onclick="scanDevices()">🔍 再スキャン</button>
    </div>
    <div class="flex-row" style="margin-bottom:10px">
      <label style="font-size:var(--fs-sm);color:var(--text2)">一括 Ch:</label>
      <input type="number" id="allch" min="1" max="999" value="1" style="width:65px">
      <button class="btn primary" onclick="switchAll()">▶ 全切替</button>
      <span id="status-bar" style="font-size:.82em;color:var(--accent2)"></span>
    </div>
    <div class="device-list" id="device-list"><div class="no-devices">読み込み中...</div></div>
  </div>
  <div class="card">
    <div class="card-title">タップループ</div>
    <div style="display:flex;gap:12px;align-items:center;flex-wrap:wrap;margin-bottom:8px">
      <label style="font-size:var(--fs-sm)">X: <input type="number" id="tap-x" value="0" min="0" style="width:70px"></label>
      <label style="font-size:var(--fs-sm)">Y: <input type="number" id="tap-y" value="0" min="0" style="width:70px"></label>
      <label style="font-size:var(--fs-sm)">間隔: <input type="number" id="tap-interval" value="1000" min="100" step="100" style="width:80px"> ms</label>
    </div>
    <p style="font-size:var(--fs-xs);color:var(--text3);margin:0 0 8px">デバイス一覧でチェックしたシリアルが対象（未選択時は全台）</p>
    <div class="flex-row">
      <button class="btn success" id="btn-tap-start" onclick="tapLoopStart()">▶ 開始</button>
      <button class="btn danger" id="btn-tap-stop" onclick="tapLoopStop()" disabled>■ 停止</button>
      <span id="tap-status" style="font-size:.82em;color:var(--text2)"></span>
    </div>
  </div>
</div>
<!-- ===== チャンネル巡回 ===== -->
<div class="view" id="view-patrol">
	<div class="dashboard-grid layout-edit-surface" id="patrol-layout-root">
		<div class="card panel-size-1x2 panel-col-1" id="card-patrol-control">
        <div class="card-title">巡回制御</div>
        <div class="patrol-status"><span class="stopped" id="ps-state">■ 停止中</span><span id="ps-ch"></span><span id="ps-prog"></span><span id="ps-parallel"></span></div>
        <div class="patrol-progress" id="ps-progress">
          <div class="progress-line"><div class="progress-fill" id="ps-progress-fill"></div></div>
          <div class="progress-text"><span id="ps-patrol-label">--</span><span id="ps-patrol-percent"></span></div>
        </div>
        <div id="crash-warning" class="crash-warning">⚠ ゲームクライアントがch移動できない状態です（クラッシュの可能性）。ADBサーバーを再起動してください。</div>
        <div style="display:flex;align-items:center;gap:8px;min-height:1.2em;margin-bottom:6px">
          <div id="ps-full" style="font-size:.78em;color:#fca5a5;flex:1"></div>
          <button id="btn-clear-full" class="btn" style="font-size:.75em;padding:2px 8px;display:none" onclick="clearFullChannels()">✕ クリア</button>
        </div>
        <div class="flex-row" style="margin-bottom:8px">
          <label style="font-size:var(--fs-sm)">開始Ch:</label>
          <input type="number" id="patrol-start-ch" min="0" max="9999" value="0" style="width:65px" title="0=前回位置から再開" oninput="syncPatrolStartCh(this.value)">
          <button class="btn toggle-btn" id="btn-reversed" onclick="toggleReversed()">⬆ 正順</button>
          <button class="btn toggle-btn" id="btn-loop" onclick="toggleLoop()">🔁 ループ</button>
        </div>
        <div class="flex-row">
          <button class="btn success" id="btn-patrol-start" onclick="patrolStart()">▶ 巡回開始</button>
          <button class="btn" id="btn-patrol-stop" onclick="patrolStop()" disabled>■ 停止</button>
        </div>
        <hr style="border:none;border-top:1px solid var(--border);margin:10px 0 8px">
        <div class="flex-row" style="margin-bottom:6px;flex-wrap:wrap;gap:6px">
          <label style="font-size:var(--fs-sm)">開始ch:</label>
          <input type="number" id="patrol-all-start-ch" min="1" max="999" value="1" style="width:58px">
          <label style="font-size:var(--fs-sm)">終了ch:</label>
          <input type="number" id="patrol-all-end-ch" min="1" max="999" value="100" style="width:58px">
          <button class="btn primary" onclick="patrolAllOnce()">⏵ 全Ch1巡</button>
        </div>
				<div class="status-grid" style="grid-template-columns:repeat(2,minmax(0,1fr));margin-top:10px">
					<div class="stat">
						<div class="stat-label">1サイクル時間</div>
						<div class="stat-value" id="ps-cycle-time">--</div>
					</div>
					<div class="stat">
						<div class="stat-label">巡回速度</div>
						<div class="stat-value ok" id="ps-cycle-rate">--</div>
					</div>
				</div>
				</div>
			<div class="card panel-size-1x2 panel-col-1" id="card-patrol-channels">
        <div class="card-title">巡回チャンネル</div>
        <div class="flex-row" style="margin-bottom:6px">
          <button class="btn" style="padding:3px 8px;font-size:.8em" onclick="addChannel()">＋ 追加</button>
          <button class="btn" style="padding:3px 8px;font-size:.8em" onclick="sortChannels('asc')">↑ 昇順</button>
          <button class="btn" style="padding:3px 8px;font-size:.8em" onclick="sortChannels('desc')">↓ 降順</button>
          <button class="btn primary" style="padding:3px 8px;font-size:.8em" id="btn-ch-save" onclick="saveChannels()" disabled>💾 保存</button>
          <span id="ch-save-status" style="font-size:.8em;color:var(--text2)"></span>
        </div>
        <div class="ch-editor" id="ch-editor"><div class="no-devices">読み込み中...</div></div>
        <div class="flex-row" style="margin-top:6px">
          <input type="text" id="ch-bulk-input" placeholder="例: 6,13,23,35,41..." style="flex:1;font-size:.85em">
          <button class="btn" style="padding:3px 10px;font-size:.8em" onclick="bulkImportChannels()">上書き</button>
        </div>
      </div>
		<div class="card panel-size-1x2 panel-col-2" id="card-patrol-gold">
		<div class="card-title">🌟 レアエネミー検知履歴<button class="btn" style="margin-left:auto;padding:2px 8px;font-size:var(--fs-xs)" onclick="clearAllGoldHistory()">✕ 一括クリア</button><button class="btn" style="padding:2px 8px;font-size:var(--fs-xs)" onclick="window.open('/spawn-log','spawn-log','width=600,height=400')">⧉</button></div>
        <div id="gold-history-container-patrol"><div class="no-history">検知履歴なし</div></div>
      </div>
  </div>
</div>
<!-- ===== 設定 ===== -->
<div class="view" id="view-settings">
	<div class="cfg-stack">
		<div class="card cfg-card chat-filter-card">
			<div class="cfg-card-title cfg-card-title-collapsible"><span>チャットフィルター</span><button id="chat-filter-toggle-btn" type="button" class="btn" onclick="toggleChatFilterMinimize()">－ 最小化</button></div>
			<div id="chat-filter-content"><div id="cfg-chat-rule-managers" class="cfg-rule-grid"></div></div>
		</div>
		<div class="card cfg-card">
			<div class="cfg-card-title">システム設定</div>
			<div id="cfg-form" class="cfg-grid"></div>
			<div style="margin:12px 0 0 0">
				<label style="font-size:var(--fs-md);font-weight:500;">通知するエネミー</label>
				<div id="cfg-enemy-checkboxes" style="margin-top:6px;display:flex;gap:18px;font-size:var(--fs-md)">
					<label><input type="checkbox" class="cfg-enemy" value="ウリボ・ゴールド">ウリボ・ゴールド</label>
					<label><input type="checkbox" class="cfg-enemy" value="金ナッポ">金ナッポ</label>
					<label><input type="checkbox" class="cfg-enemy" value="銀ナッポ">銀ナッポ</label>
				</div>
			</div>
			<div class="cfg-save-bar">
				<button class="btn primary" onclick="saveConfig()">💾 保存・反映</button>
				<span id="cfg-status" style="font-size:.82em;color:var(--text2)"></span>
			</div>
			<p class="cfg-note" style="margin-top:6px">* 滞在時間・タイムアウト・並列設定・通知エネミー・Webhook・デバウンス・チャットフィルターは保存後すぐ反映されます。NIC・GUIポート・ロケーションファイルは再起動が必要です。</p>
		</div>
	</div>
</div>
<!-- ===== データ管理 ===== -->
<div class="view" id="view-data-management">
  <div class="cfg-stack">
    <div class="card cfg-card">
      <div class="cfg-card-title">chマッピング管理</div>
      <!-- 操作パネル -->
      <div style="display:flex;gap:12px;align-items:flex-end;flex-wrap:wrap;margin-bottom:14px">
        <div>
          <div style="font-size:var(--fs-sm);color:var(--text2);margin-bottom:4px">全chマッピング（現在セッションのLineIDを自動登録）</div>
          <button class="btn primary" onclick="portmapMapAll()">⚡ 全chマッピング</button>
        </div>
        <div style="border-left:1px solid var(--border);padding-left:12px">
          <div style="font-size:var(--fs-sm);color:var(--text2);margin-bottom:4px">指定chマッピング（現在セッションを指定chに登録）</div>
          <div style="display:flex;gap:6px;align-items:center">
            <label style="font-size:var(--fs-sm)">Ch:</label>
            <input type="number" id="portmap-ch-input" min="1" max="9999" placeholder="例: 40" style="width:80px">
            <button class="btn success" onclick="portmapMapCh()">📌 マッピング</button>
          </div>
        </div>
        <div style="margin-left:auto;display:flex;gap:6px;align-items:flex-end">
          <button class="btn" onclick="portmapSetAsPatrol()" title="マッピング済みchを巡回チャンネルリストに反映">📡 巡回リストへ反映</button>
          <button class="btn" onclick="loadPortMapEntries()">🔄 更新</button>
        </div>
      </div>
      <div id="portmap-status" style="font-size:var(--fs-sm);color:var(--text2);margin-bottom:8px;min-height:1.4em"></div>
      <!-- エントリ一覧テーブル -->
      <div style="overflow-x:auto">
        <table id="portmap-table" style="width:100%;border-collapse:collapse;font-size:var(--fs-md)">
          <thead>
            <tr style="border-bottom:1px solid var(--border)">
              <th style="text-align:left;padding:6px 10px;color:var(--text2);font-weight:500;white-space:nowrap">Ch</th>
              <th style="text-align:left;padding:6px 10px;color:var(--text2);font-weight:500;white-space:nowrap">サーバーIP:ポート</th>
              <th style="text-align:left;padding:6px 10px;color:var(--text2);font-weight:500;white-space:nowrap">更新日時</th>
            </tr>
          </thead>
          <tbody id="portmap-tbody">
            <tr><td colspan="3" style="padding:12px 10px;color:var(--text3);text-align:center">読み込み中...</td></tr>
          </tbody>
        </table>
      </div>
      <div id="portmap-count" style="font-size:var(--fs-sm);color:var(--text3);margin-top:6px"></div>
    </div>
  </div>
</div>
</div><!-- /main -->
</div><!-- /app -->
<script>
// ===== データ管理 / chマッピング =====
function loadPortMapEntries() {
  fetch('/api/portmap/entries').then(r=>r.json()).then(entries=>{
    const tbody = document.getElementById('portmap-tbody');
    const count = document.getElementById('portmap-count');
    if (!tbody) return;
    if (!entries || entries.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="padding:12px 10px;color:var(--text3);text-align:center">マッピングデータなし</td></tr>';
      if (count) count.textContent = '';
      return;
    }
    entries.sort((a,b) => a.ch - b.ch);
    tbody.innerHTML = entries.map(e => {
      const dt = e.updated_at ? new Date(e.updated_at).toLocaleString('ja-JP') : '--';
      return '<tr style="border-bottom:1px solid var(--border)">' +
        '<td style="padding:5px 10px;font-weight:600;color:var(--accent)">'+e.ch+'</td>' +
        '<td style="padding:5px 10px;font-family:monospace;font-size:var(--fs-sm)">'+e.server_ip+'</td>' +
        '<td style="padding:5px 10px;color:var(--text2);font-size:var(--fs-sm)">'+dt+'</td>' +
        '</tr>';
    }).join('');
    if (count) count.textContent = '合計 '+entries.length+' 件';
  }).catch(()=>{
    const tbody = document.getElementById('portmap-tbody');
    if (tbody) tbody.innerHTML = '<tr><td colspan="3" style="padding:12px 10px;color:#f87171;text-align:center">取得失敗</td></tr>';
  });
}
function portmapSetStatus(msg, isErr) {
  const el = document.getElementById('portmap-status');
  if (!el) return;
  el.textContent = msg;
  el.style.color = isErr ? '#f87171' : 'var(--text2)';
}
function portmapMapAll() {
  portmapSetStatus('全chマッピング実行中...', false);
  fetch('/api/portmap/map-all', {method:'POST'}).then(r=>r.json()).then(d=>{
    portmapSetStatus('完了: '+d.mapped+' 件をマッピングしました', false);
    loadPortMapEntries();
  }).catch(()=>portmapSetStatus('エラーが発生しました', true));
}
function portmapMapCh() {
  const inp = document.getElementById('portmap-ch-input');
  const ch = inp ? parseInt(inp.value) : 0;
  if (!ch || ch <= 0) { portmapSetStatus('ch番号を入力してください', true); return; }
  portmapSetStatus('ch'+ch+' にマッピング中...', false);
  fetch('/api/portmap/map-ch', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({ch:ch})})
    .then(r=>r.json()).then(d=>{
      portmapSetStatus('ch'+ch+' にマッピングしました', false);
      if (inp) inp.value = '';
      loadPortMapEntries();
    }).catch(()=>portmapSetStatus('エラーが発生しました', true));
}
async function portmapSetAsPatrol() {
  portmapSetStatus('ポートマップからch一覧を取得中...', false);
  let entries;
  try { entries = await fetch('/api/portmap/entries').then(r=>r.json()); } catch(e) { portmapSetStatus('取得失敗', true); return; }
  if (!entries || entries.length === 0) { portmapSetStatus('マッピングデータがありません', true); return; }
  const chs = [...new Set(entries.map(e=>e.ch).filter(c=>c>0 && c<=100))].sort((a,b)=>a-b);
  if (chs.length === 0) { portmapSetStatus('有効なch（1〜100）がありません', true); return; }
  const preview = chs.slice(0,5).join(',') + (chs.length>5 ? '...' : '');
  const ok = confirm('ポートマップの '+chs.length+' ch ['+preview+'] を巡回リストに設定しますか？\n現在のリストは上書きされます。');
  if (!ok) return;
  try {
    const r = await fetch('/api/patrol/channels', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({channels:chs})});
    const d = await r.json();
    if (d.ok) {
      portmapSetStatus('✓ '+chs.length+' chを巡回リストに反映しました', false);
      if (typeof loadPatrolChannels === 'function') loadPatrolChannels();
    } else {
      portmapSetStatus('✗ 保存失敗', true);
    }
  } catch(e) { portmapSetStatus('✗ エラー: '+e.message, true); }
}
// 巡回チャンネルリストから開始Chプルダウン生成・同期
function updateStartChDropdown() {
  fetch('/api/patrol/channels').then(r=>r.json()).then(data=>{
    const sel = document.getElementById('dash-patrol-start-ch-select');
    if (!sel) return;
    sel.innerHTML = '<option value="0">(前回位置)</option>';
    (data.channels||[]).forEach(function(ch){
      sel.innerHTML += '<option value="'+ch+'">'+ch+'</option>';
    });
    // 入力値と同期
    var inp = document.getElementById('dash-patrol-start-ch');
    sel.value = inp.value;
  });
}
document.addEventListener('DOMContentLoaded',function(){
  updateStartChDropdown();
  var sel = document.getElementById('dash-patrol-start-ch-select');
  var inp = document.getElementById('dash-patrol-start-ch');
  if(sel&&inp){
    sel.addEventListener('change',function(){syncPatrolStartCh(sel.value);});
    inp.addEventListener('input',function(){syncPatrolStartCh(inp.value);});
  }
});
// ...既存コード...
const EDITABLE_LAYOUT_VIEWS=['dashboard','patrol','chat-log'];
let currentViewId='dashboard';
function isLayoutEditableView(id){
	return EDITABLE_LAYOUT_VIEWS.includes(id);
}
function syncLayoutEditState(){
	const dashGrid=document.getElementById('dashboard-grid');
	const patrolRoot=document.getElementById('patrol-layout-root');
	if(dashGrid)dashGrid.classList.toggle('edit-mode',layoutEditMode&&currentViewId==='dashboard');
	if(patrolRoot)patrolRoot.classList.toggle('edit-mode',layoutEditMode&&currentViewId==='patrol');
	const chatView=document.getElementById('view-chat-log');
	if(chatView)chatView.classList.toggle('edit-mode',layoutEditMode&&currentViewId==='chat-log');
	const btn=document.getElementById('nav-layout-edit');
	if(btn){
		const editable=isLayoutEditableView(currentViewId);
		btn.classList.toggle('layout-edit-active',editable&&layoutEditMode);
		btn.classList.toggle('layout-edit-disabled',!editable);
		btn.setAttribute('aria-disabled',editable?'false':'true');
		btn.title=editable?'レイアウト編集':'このページではレイアウト編集できません';
	}
}
function switchView(id,navEl){
  document.querySelectorAll('.view').forEach(v=>v.classList.remove('active'));
  document.querySelectorAll('.nav-item').forEach(n=>n.classList.remove('active'));
  const v=document.getElementById('view-'+id);if(v)v.classList.add('active');
  if(navEl)navEl.classList.add('active');
	currentViewId=id;
	syncLayoutEditState();
}
function initPanelDragAndCollapse(){
  const cards=document.querySelectorAll('.card');
  let dragSrc=null;
	function placeDraggedCardInContainer(container, clientY){
		if(!dragSrc||!container)return;
		const siblings=[...container.querySelectorAll(':scope > .card')].filter(card=>card!==dragSrc);
		const target=siblings.find(card=>{
			const rect=card.getBoundingClientRect();
			return clientY < rect.top + rect.height/2;
		});
		if(target)container.insertBefore(dragSrc, target);
		else container.appendChild(dragSrc);
	}
  cards.forEach(card=>{
    card.draggable=true;
	// dashboard-grid系カードのドラッグは専用処理で個別管理
	if(card.closest('.dashboard-grid'))return;
    card.addEventListener('dragstart',e=>{
			const patrolRoot=card.closest('#patrol-layout-root');
			if(!patrolRoot||!patrolRoot.classList.contains('edit-mode')){e.preventDefault();return;}
      const titleEl=card.querySelector('.card-title');
      if(titleEl && !titleEl.contains(e.target)){e.preventDefault();return;}
      dragSrc=card;
      card.classList.add('dragging');
      e.dataTransfer.effectAllowed='move';
      e.dataTransfer.setData('text/plain','');
    });
    card.addEventListener('dragend',()=>{
      card.classList.remove('dragging');
      dragSrc=null;
      document.querySelectorAll('.card-placeholder').forEach(el=>el.classList.remove('card-placeholder'));
    });
    card.addEventListener('dragover',e=>{
      if(!dragSrc || dragSrc===card) return;
      e.preventDefault();
			placeDraggedCardInContainer(card.parentElement, e.clientY);
    });
    card.addEventListener('drop',e=>{
      if(!dragSrc||dragSrc===card) return;
      e.preventDefault();
			placeDraggedCardInContainer(card.parentElement, e.clientY);
			if(dragSrc.closest('#patrol-layout-root'))savePatrolLayout();
      dragSrc.classList.remove('dragging');
      dragSrc=null;
    });
  });
  document.querySelectorAll('.col, .panel-grid').forEach(container=>{
    container.addEventListener('dragover',e=>{
      if(!dragSrc) return;
      e.preventDefault();
			placeDraggedCardInContainer(container, e.clientY);
    });
    container.addEventListener('drop',e=>{
      if(!dragSrc) return;
			e.preventDefault();
			placeDraggedCardInContainer(container, e.clientY);
			if(dragSrc.closest('#patrol-layout-root'))savePatrolLayout();
			dragSrc.classList.remove('dragging');
			dragSrc=null;
    });
  });
}
// ── Grid drag & drop ──
function initGridDragDrop(){
  const grid=document.getElementById('dashboard-grid');if(!grid)return;
  let activeDrag=null;
  let placeholder=null;
  function getOrCreatePlaceholder(){
    if(!placeholder){
      placeholder=document.createElement('div');
      placeholder.className='card grid-drop-indicator';
      placeholder.dataset.placeholder='1';
    }
    return placeholder;
  }
  function syncPlaceholderSize(dragCard){
    const ph=getOrCreatePlaceholder();
    DASH_SIZE_CLASSES.forEach(c=>ph.classList.remove(c));
    const sc=DASH_SIZE_CLASSES.find(c=>dragCard.classList.contains(c))||'panel-size-1x1';
    ph.classList.add(sc);
    return ph;
  }
  function removePlaceholder(){
    if(placeholder&&placeholder.parentNode)placeholder.remove();
  }
  // 2D位置から挿入先兄弟カードを決定する
  function getInsertBefore(clientX,clientY){
    const gridRect=grid.getBoundingClientRect();
    const siblings=[...grid.querySelectorAll(':scope > .card')].filter(c=>c!==activeDrag&&!c.dataset.placeholder);
    for(const s of siblings){
      const r=s.getBoundingClientRect();
      const midY=r.top+r.height/2;
      const midX=r.left+r.width/2;
      // 全幅カード(2列span)はY中心のみで判定
      if(r.width>gridRect.width*0.8){
        if(clientY<midY)return s;
      } else {
        // 1列カード: Yで行を判定しつつXで左右を判定
        if(clientY<r.top)return s; // このカードの行より上
        if(clientY<midY&&clientX<midX)return s; // 同行左半
      }
    }
    return null;
  }
  function updatePlaceholder(clientX,clientY){
    const ph=syncPlaceholderSize(activeDrag);
    const before=getInsertBefore(clientX,clientY);
    if(before){if(ph.nextSibling!==before)grid.insertBefore(ph,before);}
    else{if(grid.lastChild!==ph)grid.appendChild(ph);}
  }
  function attachCard(card){
    card.draggable=true;
    card.addEventListener('dragstart',e=>{
      if(!grid.classList.contains('edit-mode')){e.preventDefault();return;}
			if(e.target&&e.target.closest('.panel-resize-handle-x, .panel-resize-handle-y')){e.preventDefault();return;}
      activeDrag=card;
      card.classList.add('dragging');
      e.dataTransfer.effectAllowed='move';
      e.dataTransfer.setData('text/plain','');
    });
    card.addEventListener('dragend',()=>{
      removePlaceholder();
      card.classList.remove('dragging');
      activeDrag=null;
    });
  }
  grid.querySelectorAll(':scope > .card').forEach(attachCard);
  grid.addEventListener('dragover',e=>{
    if(!activeDrag||!grid.classList.contains('edit-mode'))return;
    e.preventDefault();
    updatePlaceholder(e.clientX,e.clientY);
  });
  grid.addEventListener('drop',e=>{
    if(!activeDrag||!grid.classList.contains('edit-mode'))return;
    e.preventDefault();
    // プレースホルダーの描画カラム（左右）をドロップ前に読み取る
    let targetColumn = getPanelColumn(activeDrag); // フォールバック: 元のカラム
    if(placeholder && placeholder.parentNode === grid) {
      const gridRect = grid.getBoundingClientRect();
      const phRect = placeholder.getBoundingClientRect();
      targetColumn = (phRect.left + phRect.width/2 > gridRect.left + gridRect.width/2) ? 2 : 1;
    }
    if(placeholder&&placeholder.parentNode===grid){
      grid.insertBefore(activeDrag,placeholder);
    }
    removePlaceholder();
    activeDrag.classList.remove('dragging');
    // ドラッグしたカードのカラムクラスのみ更新（他のカードは変更しない）
    if(getPanelWidthUnits(activeDrag) === 1) {
      activeDrag.classList.remove('panel-col-1','panel-col-2');
      activeDrag.classList.add(targetColumn === 2 ? 'panel-col-2' : 'panel-col-1');
    }
    activeDrag=null;
		updateDashboardResizeHandlePositions();
    saveDashboardLayout();
  });
  grid.addEventListener('dragleave',e=>{
    if(!grid.contains(e.relatedTarget))removePlaceholder();
  });
}
function initPatrolGridDragDrop(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	let activeDrag=null;
	let placeholder=null;
	function getOrCreatePlaceholder(){
		if(!placeholder){
			placeholder=document.createElement('div');
			placeholder.className='card grid-drop-indicator';
			placeholder.dataset.placeholder='1';
		}
		return placeholder;
	}
	function syncPlaceholderSize(dragCard){
		const ph=getOrCreatePlaceholder();
		DASH_SIZE_CLASSES.forEach(c=>ph.classList.remove(c));
		const sc=DASH_SIZE_CLASSES.find(c=>dragCard.classList.contains(c))||'panel-size-1x1';
		ph.classList.add(sc);
		return ph;
	}
	function removePlaceholder(){
		if(placeholder&&placeholder.parentNode)placeholder.remove();
	}
	function getInsertBefore(clientX,clientY){
		const gridRect=grid.getBoundingClientRect();
		const siblings=[...grid.querySelectorAll(':scope > .card')].filter(c=>c!==activeDrag&&!c.dataset.placeholder);
		for(const s of siblings){
			const r=s.getBoundingClientRect();
			const midY=r.top+r.height/2;
			const midX=r.left+r.width/2;
			if(r.width>gridRect.width*0.8){
				if(clientY<midY)return s;
			}else{
				if(clientY<r.top)return s;
				if(clientY<midY&&clientX<midX)return s;
			}
		}
		return null;
	}
	function updatePlaceholder(clientX,clientY){
		const ph=syncPlaceholderSize(activeDrag);
		const before=getInsertBefore(clientX,clientY);
		if(before){if(ph.nextSibling!==before)grid.insertBefore(ph,before);}
		else{if(grid.lastChild!==ph)grid.appendChild(ph);}
	}
	function attachCard(card){
		card.draggable=true;
		card.addEventListener('dragstart',e=>{
			if(!grid.classList.contains('edit-mode')){e.preventDefault();return;}
			if(e.target&&e.target.closest('.panel-resize-handle-x, .panel-resize-handle-y')){e.preventDefault();return;}
			activeDrag=card;
			card.classList.add('dragging');
			e.dataTransfer.effectAllowed='move';
			e.dataTransfer.setData('text/plain','');
		});
		card.addEventListener('dragend',()=>{
			removePlaceholder();
			card.classList.remove('dragging');
			activeDrag=null;
		});
	}
	grid.querySelectorAll(':scope > .card').forEach(attachCard);
	grid.addEventListener('dragover',e=>{
		if(!activeDrag||!grid.classList.contains('edit-mode'))return;
		e.preventDefault();
		updatePlaceholder(e.clientX,e.clientY);
	});
	grid.addEventListener('drop',e=>{
		if(!activeDrag||!grid.classList.contains('edit-mode'))return;
		e.preventDefault();
		let targetColumn=getPanelColumn(activeDrag);
		if(placeholder&&placeholder.parentNode===grid){
			const gridRect=grid.getBoundingClientRect();
			const phRect=placeholder.getBoundingClientRect();
			targetColumn=(phRect.left + phRect.width/2 > gridRect.left + gridRect.width/2) ? 2 : 1;
		}
		if(placeholder&&placeholder.parentNode===grid)grid.insertBefore(activeDrag,placeholder);
		removePlaceholder();
		activeDrag.classList.remove('dragging');
		if(getPanelWidthUnits(activeDrag)===1){
			activeDrag.classList.remove('panel-col-1','panel-col-2');
			activeDrag.classList.add(targetColumn===2?'panel-col-2':'panel-col-1');
		}
		activeDrag=null;
		updatePatrolResizeHandlePositions();
		savePatrolLayout();
	});
	grid.addEventListener('dragleave',e=>{
		if(!grid.contains(e.relatedTarget))removePlaceholder();
	});
}
// ── Log filter chips ──
const LOG_CATS=[
  {id:'mumu',  label:'[MuMu]', test:l=>l.includes('[MuMu]')},
  {id:'det',   label:'検知',    test:l=>l.includes('[DETECTION]')||l.includes('[検知]')},
  {id:'chat',  label:'チャット',test:l=>l.includes('[チャット')},
  {id:'pkt',   label:'パケット',test:l=>/\[0x[0-9a-fA-F]+\]/.test(l)||/\[Instance-/.test(l)},
  {id:'gas',   label:'GAS',    test:l=>l.includes('[GASFetch]')},
  {id:'gui',   label:'GUI',    test:l=>l.includes('[GUI]')},
  {id:'other', label:'その他', test:_=>true},
];
const logFilter={mumu:true,det:true,chat:true,pkt:false,gas:true,gui:true,other:true};
function getCat(line){for(const c of LOG_CATS){if(c.test(line))return c.id;}return 'other';}
function isVisible(line){return logFilter[getCat(line)]!==false;}
function buildFilterBar(){
  const bar=document.getElementById('log-filter-bar');if(!bar)return;
  bar.innerHTML=LOG_CATS.map(c=>'<div id="fc-'+c.id+'" class="chip'+(logFilter[c.id]?' active':'')+'" onclick="toggleCat(\''+c.id+'\')">'+c.label+'</div>').join('');
}
function toggleCat(id){
  logFilter[id]=!logFilter[id];
  const chip=document.getElementById('fc-'+id);if(chip)chip.className='chip'+(logFilter[id]?' active':'');
  document.querySelectorAll('#log-area .log-line').forEach(div=>{div.style.display=isVisible(div.textContent)?'':'none';});
}
// ── Escape ──
function escHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function escAttrJs(s){return JSON.stringify(String(s)).replace(/"/g,'&quot;');}
// ── Log / SSE ──
(function(){
  const la=document.getElementById('log-area');let userScrolling=false;
  if(la){la.addEventListener('scroll',()=>{userScrolling=(la.scrollHeight-la.scrollTop-la.clientHeight)>20;});}
  window._logUserScrolling=()=>userScrolling;
})();
function appendLog(line){
  const la=document.getElementById('log-area');if(!la)return;
  const div=document.createElement('div');
  const isDetect=line.includes('[DETECTION]')||line.includes('金');
  div.className='log-line'+(isDetect?' detect':'');
  let tagCls='info',tagTxt='INFO';
  if(line.toLowerCase().includes('error')||line.includes('失敗')||line.toLowerCase().includes('fatal')){tagCls='err';tagTxt='ERR';}
  if(line.includes('完了')||line.includes('確立')||line.includes('起動完了')||line.includes(' ok')||line.includes('[OK]')){tagCls='ok';tagTxt='OK';}
  if(isDetect||line.toLowerCase().includes('warn')||line.includes('[DETECTION]')){tagCls='warn';tagTxt='WARN';}
  const tm=line.match(/\d{4}\/\d{2}\/\d{2} (\d{2}:\d{2}:\d{2})/);
  const timeStr=tm?tm[1]:'';const rest=tm?line.slice(tm[0].length).trim():line;
  div.innerHTML='<span class="log-time">'+escHtml(timeStr)+'</span>'
    +'<span class="log-tag '+tagCls+'">'+tagTxt+'</span>'
    +'<span class="log-msg">'+escHtml(rest)+'</span>';
  if(!isVisible(line))div.style.display='none';
  la.appendChild(div);
	if(la.children.length>1500)la.removeChild(la.firstChild);
	if(!window._logUserScrolling())la.scrollTop=la.scrollHeight;
}
async function testDetect(monster){
  const url = monster ? '/api/test-detect?monster=' + encodeURIComponent(monster) : '/api/test-detect';
  await fetch(url,{method:'POST'});
}
(function(){
  const src=new EventSource('/events');
  src.onmessage=e=>{
    appendLog(e.data);
    if(e.data.includes('[GUI] 金ウリボ')||e.data.includes('[DETECTION]')){loadGoldHistory();loadPatrolChannels();playNotifyBeep();}
    if(e.data.includes('channels.txt')){loadPatrolChannels();}
    if(e.data.includes('[PORTMAP_PENDING]')){pmCheckPending();}
  };
  fetch('/api/logs').then(r=>r.json()).then(lines=>(lines||[]).forEach(appendLog));
})();
// ── PortMap 変更確認モーダル ──
let _pmChanges=[];
async function pmCheckPending(){
  const data=await fetch('/api/portmap/pending').then(r=>r.json()).catch(()=>[]);
  _pmChanges=data||[];
  const ov=document.getElementById('pm-overlay');
  if(!ov)return;
  if(_pmChanges.length>0){pmRender();ov.style.display='flex';}
  // 変更が0件なら既に処理済みなので閉じたまま
}
function pmRender(){
  const body=document.getElementById('pm-body');
  if(!body)return;
  body.innerHTML=_pmChanges.map(c=>{
    const oldPart=c.old_ip?('<span class="pm-ip">'+c.old_ip+'</span><span class="pm-arrow">→</span>'):'<span class="pm-arrow">新規</span>';
    return '<div class="pm-change-row">'
      +'<div><span class="pm-ch">Ch'+c.ch+'</span></div>'
      +'<div>'+oldPart+'<span class="pm-ip">'+c.new_ip+'</span></div>'
      +'<div class="pm-votes">'+c.vote_count+'台が同一ポートを検知</div>'
      +'</div>';
  }).join('');
}
async function pmApplyAll(){
  const ids=_pmChanges.map(c=>c.id);
  await fetch('/api/portmap/confirm',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids,action:'apply'})});
  document.getElementById('pm-overlay').style.display='none';
  _pmChanges=[];
}
async function pmRejectAll(){
  const ids=_pmChanges.map(c=>c.id);
  await fetch('/api/portmap/confirm',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids,action:'reject'})});
  document.getElementById('pm-overlay').style.display='none';
  _pmChanges=[];
}
// ── Gold History ──
function formatAgo(seconds){
  if(seconds < 60) return 'just now';
  const mins = Math.floor(seconds / 60);
  if(mins < 60) return mins + 'min ago';
  const hrs = Math.floor(mins / 60);
  return hrs + 'h ago';
}
async function removeGoldHistory(timestamp){
  try{
    const res = await fetch('/api/gold-history/delete', {
      method: 'DELETE',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({timestamp})
    });
    if(!res.ok) throw new Error('delete failed');
    await loadGoldHistory();
  }catch(_){ }
}
async function clearAllGoldHistory(){
	try{
		const res = await fetch('/api/gold-history/clear', {
			method: 'DELETE'
		});
		if(!res.ok) throw new Error('clear failed');
		await loadGoldHistory();
	}catch(_){ }
}
async function loadGoldHistory(){
  try{
    const h=await fetch('/api/gold-history').then(r=>r.json());
    const detEl=document.getElementById('dash-detect-count');
    const metaEl=document.getElementById('dash-detect-meta');
    const nowSec=Math.floor(Date.now()/1000);
    let perHour=0;
    let latestAgo='--';
    if(Array.isArray(h) && h.length > 0){
      h.forEach(e=>{
        if(e && typeof e.timestamp === 'number' && nowSec - e.timestamp <= 3600){
          perHour++;
        }
      });
      const latest = h[0];
      if(latest && typeof latest.timestamp === 'number'){
        latestAgo = formatAgo(Math.max(0, nowSec - latest.timestamp));
      }
    }
    if(detEl) detEl.textContent = Array.isArray(h) ? String(h.length) : '0';
    if(metaEl) metaEl.textContent = String(perHour) + '/hour · ' + latestAgo;
    const tbl = (!h || h.length === 0) ? '<div class="no-history">検知履歴なし</div>'
      : '<table class="gold-table"><colgroup><col class="col-time"><col class="col-name"><col class="col-ch"><col><col class="col-action"></colgroup>'
      + '<thead><tr><th>時刻</th><th>名前</th><th>Ch</th><th>場所</th><th></th></tr></thead><tbody>'
      + h.map(e=>{const nm=e.monster_name||'ウリボ・ゴールド';const cls=nm.includes('銀ナッポ')?'name-cell silver':'name-cell';return '<tr><td class="time-cell">'+escHtml(e.time||'')+'</td><td class="'+cls+'">'+escHtml(nm)+'</td><td class="ch-cell">Ch'+Number(e.channel)+'</td><td>'+escHtml(e.location||'')+'</td><td class="action-cell"><button onclick="removeGoldHistory('+Number(e.timestamp)+')">×</button></td></tr>'}).join('')
      + '</tbody></table>';
    const c1=document.getElementById('gold-history-container');if(c1)c1.innerHTML=tbl;
    const c2=document.getElementById('gold-history-container-patrol');if(c2)c2.innerHTML=tbl;
  }catch(_){}
}
// ── Dashboard device summary ──
function renderDashDevices(devs,deviceMap){
  const el=document.getElementById('dash-device-list');if(!el)return;
  if(!devs||devs.length===0){el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>';return;}
  el.innerHTML=devs.map(d=>{
    const info=deviceMap[d]||{};
    const uid=info.user_uid||'',ch=info.line_id||'',confirmed=info.confirmed||false;
    const sub=uid?((confirmed?'🔗 ':'')+' UID:'+uid+(ch?' Ch'+ch:'')):'--';
    return '<div class="device-row"><div class="device-icon">📱</div>'
      +'<div class="device-info"><div class="device-name">'+escHtml(d)+'</div><div class="device-sub">'+escHtml(sub)+'</div></div>'
      +'<div class="device-badge '+(confirmed?'connected':'offline')+'">'+escHtml(d)+'</div></div>';
  }).join('');
}
// ── Devices ──
let selectedDevices=new Set();
let currentDevices=[];
function selectedSerials(){return[...selectedDevices];}
function selectAllDevices(){currentDevices.forEach(d=>selectedDevices.add(d));renderDeviceList();}
function deselectAllDevices(){selectedDevices.clear();renderDeviceList();}
function renderDeviceList(){
  const el=document.getElementById('device-list');if(!el)return;
  if(!currentDevices||currentDevices.length===0){el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>';return;}
  el.innerHTML=currentDevices.map(d=>{
    const info=currentDeviceMap[d]||{};const uid=info.user_uid||'',ch=info.line_id||'',confirmed=info.confirmed||false;
    const checked=selectedDevices.has(d)?'checked':'';
    const uidHtml=uid?('<span class="uid">'+(confirmed?'🔗':'')+' UID:'+uid+(ch?' Ch'+ch:'')+'</span>'):'';
    const eid='ch-'+encodeURIComponent(d);
    return '<div class="device-entry'+(confirmed?' matched':'')+'">'
      +'<label class="check-label"><input type="checkbox" '+checked+' onchange="toggleDevice('+escAttrJs(d)+',this.checked)"><span class="serial">'+escHtml(d)+'</span>'+uidHtml+'</label>'
      +'<div style="display:flex;gap:6px;margin-top:4px"><input type="number" id="'+escHtml(eid)+'" min="1" max="999" value="1" style="width:65px"><button style="padding:3px 8px;font-size:.8em" onclick="switchOne('+escAttrJs(d)+')">切替</button></div></div>';
  }).join('');
}
let currentDeviceMap={};
async function refreshDevices(){
  const bar=document.getElementById('status-bar');if(bar)bar.textContent='ADB再起動中...';
  await fetch('/api/adb/restart',{method:'POST'});
  let devs=[];
  let deviceMap={};
  const r=await fetch('/api/devices');const res=await r.json();
  devs=Array.isArray(res)?res:(res.devices||[]);
  const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  deviceMap={};if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.serial)deviceMap[e.serial]=e;if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
  renderDashDevices(devs,deviceMap);
  if(bar)bar.textContent='';
  currentDevices=devs||[];currentDeviceMap=deviceMap;
  renderDeviceList();
}
async function scanDevices(){
  const st=document.getElementById('adb-op-status');if(st)st.textContent='スキャン中...';
  const r=await fetch('/api/devices');const res=await r.json();
  const devs=Array.isArray(res)?res:(res.devices||[]);
  const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  const deviceMap={};if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.serial)deviceMap[e.serial]=e;if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
  renderDashDevices(devs,deviceMap);
  currentDevices=devs||[];currentDeviceMap=deviceMap;
  renderDeviceList();
  if(st)st.textContent=devs.length>0?'✓ '+devs.length+'台検出':'デバイスが見つかりません';
  setTimeout(()=>{if(st)st.textContent='';},3000);
}
async function restartADB(){
  const st=document.getElementById('adb-op-status');if(st)st.textContent='ADB再起動中...';
  await fetch('/api/adb/restart',{method:'POST'});
  await scanDevices();
}
async function killADB(){
  if(!confirm('ADBサーバーを停止しますか？'))return;
  const st=document.getElementById('adb-op-status');if(st)st.textContent='ADB停止中...';
  const res=await fetch('/api/adb/kill-server',{method:'POST'}).then(r=>r.json()).catch(()=>({ok:false}));
  if(st){st.textContent=res.ok?'✓ ADB停止完了':'✗ 停止失敗';setTimeout(()=>st.textContent='',3000);}
}
async function addADBDevice(){
  const inp=document.getElementById('adb-add-serial');
  const serial=(inp?inp.value:'').trim();
  const st=document.getElementById('adb-op-status');
  if(!serial){if(st){st.textContent='host:portを入力してください';setTimeout(()=>st.textContent='',3000);}return;}
  if(st)st.textContent='接続中...';
  const res=await fetch('/api/adb/connect',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({serial})}).then(r=>r.json()).catch(()=>({ok:false}));
  if(st){st.textContent=res.ok?'✓ '+(res.message||'接続完了'):'✗ '+(res.error||'接続失敗');setTimeout(()=>st.textContent='',4000);}
  if(inp&&res.ok)inp.value='';
  if(res.ok)await scanDevices();
}
function toggleDevice(s,c){c?selectedDevices.add(s):selectedDevices.delete(s);}
// ── Continuous Tap ──
// ── Chat Panel ──
let chatEvents=[],chatIPToSerial={},chatKnownSerials=[];
let notifySoundEnabled=localStorage.getItem('notifySoundEnabled')!=='false';
let notifySoundVolume=parseFloat(localStorage.getItem('notifySoundVolume')||'0.5');
const DEFAULT_CHAT_LOCATION_RULES=[];
const CHAT_MONSTER_ALIASES=[];
const CHAT_REPORT_VERBS=['発見','出現','いた','居た','います','あり','あります','湧','わき','沸','出た','でた','見つけ','みつけ','確認'];
const CHAT_MONSTER_HINT_WORDS=['金','きん','gold','銀','ぎん','silver','うり','ウリ','豚','猪','boar','なぽ','なっぽ','ナッポ','nappo'];
function chatMessageLength(text){return Array.from(String(text||'')).length;}
function normalizeCsvList(items){return [...new Set((Array.isArray(items)?items:[]).map(v=>String(v||'').trim()).filter(Boolean))];}
function normalizeChatCandidateText(text){
	return String(text||'').toLowerCase().replace(/[\s\u3000・._\-\/]/g,'').replace(/[０-９]/g, ch=>String.fromCharCode(ch.charCodeAt(0)-0xFEE0));
}
function removeKnownChatTokens(baseText, tokenList){
	let rest=String(baseText||'');
	normalizeCsvList(tokenList).map(v=>normalizeChatCandidateText(v)).filter(Boolean).sort((a,b)=>b.length-a.length).forEach(tok=>{
		rest=rest.split(tok).join('');
	});
	return rest;
}
function estimateChatMessageNoiseCount(facts){
	let rest=String(facts&&facts.compactMessage||'');
	if(!rest)return 0;
	if(facts.channel>0){
		const ch=String(facts.channel);
		rest=rest.replace(new RegExp('(?:^|[^0-9])'+ch+'ch','g'),'');
		rest=rest.replace(new RegExp('ch'+ch,'g'),'');
		rest=rest.replace(new RegExp('^'+ch+'(?=[^0-9]|$)','g'),'');
	}
	rest=rest.replace(/[0-9]{2,4}[,、.．][0-9]{2,4}/g,'');
	const tokens=[];
	if(facts.locationRule&&Array.isArray(facts.locationRule.aliases))tokens.push(...facts.locationRule.aliases);
	if(facts.monster){
		const monsterRule=getChatMonsterAliases().find(rule=>rule.name===facts.monster);
		if(monsterRule&&Array.isArray(monsterRule.aliases))tokens.push(...monsterRule.aliases);
		tokens.push(facts.monster);
	}
	tokens.push(...CHAT_REPORT_VERBS);
	tokens.push(...CHAT_MONSTER_HINT_WORDS);
	rest=removeKnownChatTokens(rest,tokens);
	rest=rest.replace(/[%％!！?？,、。．.~-]/g,'');
	rest=rest.replace(/[0-9]/g,'');
	return Array.from(rest).length;
}
function cloneChatLocationRule(rule){
	return {name:String(rule.name||''),aliases:normalizeCsvList(rule.aliases||[]),monsters:normalizeCsvList(rule.monsters||[])};
}
function parseChatLocationRuleLine(line){
	const raw=String(line||'').trim();
	if(!raw)return null;
	const parts=raw.split('|').map(v=>v.trim());
	const name=parts[0]||'';
	if(!name)return null;
	const aliases=normalizeCsvList([name].concat((parts[1]||'').split(',').map(v=>v.trim()).filter(Boolean)));
	const monsters=normalizeCsvList((parts[2]||'').split(',').map(v=>v.trim()).filter(Boolean));
	return {name,aliases,monsters};
}
function parseChatMonsterAliasRuleLine(line){
	const raw=String(line||'').trim();
	if(!raw)return null;
	const parts=raw.split('|').map(v=>v.trim());
	const name=parts[0]||'';
	if(!name)return null;
	const aliases=normalizeCsvList((parts[1]||'').split(',').map(v=>v.trim()).filter(Boolean));
	return {name,aliases};
}
function getChatLocationRules(){
	const merged=new Map();
	DEFAULT_CHAT_LOCATION_RULES.forEach(rule=>{
		const copy=cloneChatLocationRule(rule);
		merged.set(copy.name,copy);
	});
	normalizeCsvList(cfgData.chat_report_location_rules).forEach(line=>{
		const parsed=parseChatLocationRuleLine(line);
		if(!parsed)return;
		const existing=merged.get(parsed.name);
		if(existing){
			existing.aliases=normalizeCsvList(existing.aliases.concat(parsed.aliases));
			existing.monsters=normalizeCsvList(existing.monsters.concat(parsed.monsters));
		}else{
			merged.set(parsed.name,parsed);
		}
	});
	return [...merged.values()];
}
function getChatMonsterAliases(){
	const merged=new Map();
	CHAT_MONSTER_ALIASES.forEach(rule=>{
		merged.set(rule.name,{name:rule.name,aliases:normalizeCsvList(rule.aliases||[])});
	});
	normalizeCsvList(cfgData.chat_report_monster_alias_rules).forEach(line=>{
		const parsed=parseChatMonsterAliasRuleLine(line);
		if(!parsed)return;
		const existing=merged.get(parsed.name);
		if(existing)existing.aliases=normalizeCsvList(existing.aliases.concat(parsed.aliases));
		else merged.set(parsed.name,{name:parsed.name,aliases:parsed.aliases});
	});
	return [...merged.values()];
}
function findAliasGroup(compactText, groups){
	for(const group of groups){
		if(group.aliases.some(alias=>compactText.includes(normalizeChatCandidateText(alias))))return group.name;
	}
	return '';
}
function findAliasRule(compactText, groups){
	for(const group of groups){
		if(group.aliases.some(alias=>compactText.includes(normalizeChatCandidateText(alias))))return group;
	}
	return null;
}
function extractChatCandidateFacts(ev){
	const rawMessage=String(ev&&ev.message||'');
	const rawSender=String(ev&&ev.sender||'');
	const compactMessage=normalizeChatCandidateText(rawMessage);
	const asciiMessage=compactMessage;
	let channel=0;
	let match=asciiMessage.match(/(?:^|[^0-9])([0-9]{1,3})ch/);
	if(!match)match=asciiMessage.match(/ch([0-9]{1,3})/);
	if(!match)match=asciiMessage.match(/^([0-9]{1,3})(?=[^0-9]|$)/);
	if(match)channel=parseInt(match[1],10)||0;
	const locationRule=findAliasRule(compactMessage, getChatLocationRules());
	const location=locationRule?locationRule.name:'';
	const explicitMonster=findAliasGroup(compactMessage, getChatMonsterAliases());
	const hasGoldWord=['金','きん','gold'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasSilverWord=['銀','ぎん','silver'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasBoarWord=['うり','ウリ','豚','猪','boar'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasNappoWord=['なぽ','なっぽ','ナッポ','ナッポ','nappo'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const spawnMonsters=locationRule&&Array.isArray(locationRule.monsters)?locationRule.monsters:[];
	let inferredMonster='';
	if(!explicitMonster){
		if(spawnMonsters.length===1){
			inferredMonster=spawnMonsters[0];
		}else if(spawnMonsters.includes('金ナッポ') && spawnMonsters.includes('銀ナッポ')){
			if(hasGoldWord && !hasBoarWord)inferredMonster='金ナッポ';
			else if(hasSilverWord && !hasBoarWord)inferredMonster='銀ナッポ';
			else if(hasNappoWord && hasGoldWord)inferredMonster='金ナッポ';
			else if(hasNappoWord && hasSilverWord)inferredMonster='銀ナッポ';
		}
		if(!inferredMonster){
			if(hasGoldWord && !hasBoarWord && (hasNappoWord || location || channel>0))inferredMonster='金ナッポ';
			else if(hasSilverWord && !hasBoarWord && (hasNappoWord || location || channel>0))inferredMonster='銀ナッポ';
			else if((channel>0 && location) || (channel>0 && hasBoarWord) || (location && hasBoarWord))inferredMonster='ウリボ・ゴールド';
		}
	}
	const monster=explicitMonster||inferredMonster;
	const hasCoords=/[0-9]{2,4}[,、.．][0-9]{2,4}/.test(asciiMessage);
	const hasReportVerb=CHAT_REPORT_VERBS.some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const sender=rawSender.toLowerCase();
	return {rawMessage, compactMessage, sender, channel, location, locationRule, spawnMonsters, monster, explicitMonster, inferredMonster, hasGoldWord, hasSilverWord, hasBoarWord, hasNappoWord, hasCoords, hasReportVerb};
}
function getChatCandidateConfig(){
	return {
		senders: normalizeCsvList(cfgData.chat_report_senders),
		excludedSenders: normalizeCsvList(cfgData.chat_report_excluded_senders),
		minLength: parseInt(cfgData.chat_report_min_length)||4,
		maxLength: parseInt(cfgData.chat_report_max_length)||80,
	};
}
function getChatCandidateScore(ev){
	const facts=extractChatCandidateFacts(ev);
	const message=facts.rawMessage.toLowerCase();
	const sender=facts.sender;
	const length=chatMessageLength(facts.rawMessage);
	const rules=getChatCandidateConfig();
	if(!message||length<rules.minLength||length>rules.maxLength)return 0;
	const excludeKeywords=['ありがとう','ありがと','よろしく','こん','こんばんは','おつ','了解','りょ','募集','売り','買い','null'];
	if(rules.excludedSenders.some(v=>sender.includes(v.toLowerCase())))return 0;
	if(excludeKeywords.some(v=>message.includes(v)))return 0;
	if(!facts.channel && !facts.location && !facts.monster && !facts.hasCoords && !facts.hasReportVerb)return 0;
	let score=0;
	if(length>=6&&length<=36)score+=1;
	if(facts.channel>0)score+=2;
	if(facts.location)score+=2;
	if(facts.explicitMonster)score+=3;
	else if(facts.inferredMonster)score+=2;
	if(facts.spawnMonsters.length===1 && facts.location)score+=2;
	if(facts.spawnMonsters.length>1 && facts.location && facts.inferredMonster)score+=1;
	if(facts.hasReportVerb)score+=1;
	if(facts.hasCoords)score+=1;
	if(facts.channel>0 && facts.location)score+=2;
	if(facts.monster && (facts.channel>0 || facts.location))score+=2;
	if(facts.inferredMonster==='ウリボ・ゴールド' && facts.channel>0 && facts.location)score+=2;
	if((facts.inferredMonster==='金ナッポ' || facts.inferredMonster==='銀ナッポ') && facts.channel>0 && facts.location)score+=2;
	if((facts.hasGoldWord || facts.hasSilverWord) && !facts.hasBoarWord)score+=1;
	if(rules.senders.length && (facts.channel>0 || facts.location || facts.monster))score+=1;
	const noiseCount=estimateChatMessageNoiseCount(facts);
	if(noiseCount>0){
		score-=Math.min(8, noiseCount*2);
		if(facts.channel>0 && !facts.location && !facts.monster && noiseCount>=2)score-=2;
	}
	return Math.max(0, score);
}
function isChatCandidate(ev){
	const facts=extractChatCandidateFacts(ev);
	if(!(facts.channel>0) || !facts.location)return false;
	const score=getChatCandidateScore(ev);
	return score>=getChatNotifyMinScore();
}
function dedupeChatEvents(source){
	const seen=new Set();
	return (source||[]).filter(ev=>{const k=ev.channel+'|'+ev.sender+'|'+ev.message;if(seen.has(k))return false;seen.add(k);return true;});
}
function getPickedChatEvents(source){
	return dedupeChatEvents((source||[]).filter(isChatCandidate));
}
function chatCandidateMetaHtml(ev){
	const facts=extractChatCandidateFacts(ev);
	const parts=[];
	if(facts.channel>0)parts.push('Ch'+facts.channel);
	if(facts.location)parts.push(facts.location);
	if(facts.monster)parts.push(facts.monster);
	if(!parts.length)return '';
	const label=facts.explicitMonster?'判定':'推定';
	return '<div style="margin-top:4px;font-size:var(--fs-xs);color:var(--text3)">'+label+': '+escHtml(parts.join(' / '))+'</div>';
}
function chatMsgHtml(ev,opts){
	opts=opts||{};
  const serial=chatIPToSerial[ev.client_ip]||ev.client_ip;
  const ch=ev.has_ch?'<span style="color:#4f8ef7;margin-right:3px;font-size:.88em">Ch'+ev.channel+'</span>':'';
	const scoreBadge=opts.report?'<span class="chat-report-score">score '+getChatCandidateScore(ev)+'</span>':'';
	const rowClass='chat-msg'+(opts.report?' report':'');
	const actionHtml=opts.withActions===false?'':'<span class="chat-msg-actions">'
		+'<button type="button" class="chat-action-btn exclude" data-action="sender-exclude" data-sender="'+escHtml(ev.sender)+'" onclick="applyChatFilterAction(this)">発言者-</button>'
		+'</span>';
	return '<div class="'+rowClass+'" onmouseover="this.style.background=\'var(--bg2)\'" onmouseout="this.style.background=\'\'">'
		+'<div class="chat-msg-main">'
		+'<div class="chat-msg-body">'
		+'<span style="color:var(--text3);margin-right:5px">'+escHtml(ev.time)+'</span>'
		+'<span style="color:var(--text2);font-size:.88em;margin-right:3px">['+escHtml(serial)+']</span>'
		+ch+'<span style="color:var(--warn);font-weight:600;margin-right:3px">'+escHtml(ev.sender)+'</span>'
		+'<span class="chat-msg-text">'+escHtml(ev.message)+'</span>'
		+(opts.report?chatCandidateMetaHtml(ev):'')
		+scoreBadge
		+'</div>'
		+actionHtml
		+'</div></div>';
}
function getChatRowSelection(row){
	const selection=window.getSelection?window.getSelection():null;
	if(!selection||selection.rangeCount===0)return '';
	const text=String(selection).trim();
	if(!text)return '';
	const range=selection.getRangeAt(0);
	const startNode=range.startContainer;
	const endNode=range.endContainer;
	if(row && row.contains(startNode) && row.contains(endNode))return text;
	return '';
}
function getChatActionValue(action,btn){
	if(action==='sender-include' || action==='sender-exclude')return String(btn.dataset.sender||'').trim();
	const row=btn.closest('.chat-msg');
	const selected=getChatRowSelection(row);
	if(selected)return selected;
	return String(btn.dataset.message||'').trim();
}
function updateChatFilterTextarea(key, values){
	const normalized=normalizeCsvList(values);
	cfgData[key]=normalized;
	const el=document.getElementById('cfg-'+key);
	if(el)el.value=normalized.join('\n');
}
async function appendChatFilterValue(key, value){
	if(!value)return;
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.concat([value]));
	renderChatCandidatePanels();
	await saveConfig(true);
}
async function applyChatFilterAction(btn){
	const action=btn&&btn.dataset?btn.dataset.action:'';
	const value=getChatActionValue(action, btn);
	if(!action||!value)return;
	if(action==='sender-include')await appendChatFilterValue('chat_report_senders', value);
	else if(action==='sender-exclude')await appendChatFilterValue('chat_report_excluded_senders', value);
}
function refreshChatDeviceDropdown(){
  const sel=document.getElementById('chat-device-select');if(!sel)return;
  const current=sel.value;
  const serials=chatKnownSerials.length?[...chatKnownSerials]:[...new Set(Object.values(chatIPToSerial))].sort();
  sel.innerHTML='<option value="">すべて</option>'+serials.map(s=>'<option value="'+escHtml(s)+'">'+escHtml(s)+'</option>').join('');
  if(serials.includes(current))sel.value=current;
}
function chatMatchFilter(ev){
  const sel=document.getElementById('chat-device-select');const filterSerial=sel?sel.value:'';
  if(filterSerial){const serial=chatIPToSerial[ev.client_ip];if(!serial||serial!==filterSerial)return false;}
  const q=document.getElementById('chat-search')?document.getElementById('chat-search').value.toLowerCase():'';
  if(q&&!(ev.sender.toLowerCase().includes(q)||ev.message.toLowerCase().includes(q)))return false;
  return true;
}
function renderDashChat(evs){
  const el=document.getElementById('dash-chat-area');if(!el)return;
  if(!evs||!evs.length){el.innerHTML='<div style="color:var(--text3);padding:8px;font-size:.82em">チャットなし</div>';return;}
	el.innerHTML=evs.map(ev=>chatMsgHtml(ev,{withActions:false})).join('');el.scrollTop=el.scrollHeight;
}
function renderChatCandidatePanels(source){
	const picked=getPickedChatEvents(source||chatEvents);
	const full=document.getElementById('chat-report-area');
	const dash=document.getElementById('dash-chat-report-area');
	const summary=document.getElementById('chat-report-summary');
	const rules=getChatCandidateConfig();
	const emptyHtml='<div class="chat-report-empty">候補はまだありません</div>';
	if(summary){
		summary.textContent='発見・出現・湧き・チャンネル番号を含む短文を優先して表示します。';
	}
	if(full)full.innerHTML=picked.length?picked.slice().reverse().map(ev=>chatMsgHtml(ev,{report:true})).join(''):emptyHtml;
	if(dash)dash.innerHTML=picked.length?picked.slice(-6).reverse().map(ev=>chatMsgHtml(ev,{report:true,withActions:false})).join(''):emptyHtml;
}
function isChatAtBottom(){const el=document.getElementById('chat-area');return !el||el.scrollHeight-el.scrollTop-el.clientHeight<50;}
function renderChatPanel(){
  const el=document.getElementById('chat-area');if(!el)return;
  const atBottom=isChatAtBottom();
  const prevScrollTop=el.scrollTop;
  const prevScrollHeight=el.scrollHeight;
  const filtered=chatEvents.filter(chatMatchFilter);
	const deduped=dedupeChatEvents(filtered);
	if(!deduped.length){el.innerHTML='<div style="color:var(--text3);padding:8px;font-size:.82em">チャットなし</div>';renderDashChat([]);renderChatCandidatePanels([]);return;}
  el.innerHTML=deduped.map(chatMsgHtml).join('');
  if(atBottom){el.scrollTop=el.scrollHeight;}
  else{el.scrollTop=prevScrollTop+(el.scrollHeight-prevScrollHeight);}
  renderDashChat(deduped.slice(-15));
	renderChatCandidatePanels(filtered);
}
function playNotifyBeep(){
	if(!notifySoundEnabled)return;
	try{
		const ctx=new(window.AudioContext||window.webkitAudioContext)();
		const osc=ctx.createOscillator();
		const gain=ctx.createGain();
		osc.connect(gain);gain.connect(ctx.destination);
		osc.type='sine';osc.frequency.value=880;
		gain.gain.setValueAtTime(notifySoundVolume,ctx.currentTime);
		gain.gain.exponentialRampToValueAtTime(0.001,ctx.currentTime+0.3);
		osc.start();osc.stop(ctx.currentTime+0.3);
		osc.onended=()=>ctx.close();
	}catch(_){}
}
function toggleNotifySound(){
	notifySoundEnabled=!notifySoundEnabled;
	localStorage.setItem('notifySoundEnabled',notifySoundEnabled);
	applyNotifySoundUI();
	if(notifySoundEnabled)playNotifyBeep();
}
function setNotifyVolume(v){
	notifySoundVolume=parseFloat(v);
	localStorage.setItem('notifySoundVolume',notifySoundVolume);
}
function applyNotifySoundUI(){
	const btn=document.getElementById('notify-sound-toggle');
	const vol=document.getElementById('notify-sound-volume');
	if(btn){btn.textContent=notifySoundEnabled?'🔔':'🔕';btn.classList.toggle('active',notifySoundEnabled);}
	if(vol){vol.value=notifySoundVolume;vol.disabled=!notifySoundEnabled;}
}
function appendChatToPanel(ev){
  const isDup=chatEvents.slice(-50).some(e=>e.channel===ev.channel&&e.sender===ev.sender&&e.message===ev.message);
  if(isDup)return;
  chatEvents.push(ev);if(chatEvents.length>500)chatEvents=chatEvents.slice(-500);
  if(isChatCandidate(ev)){
    const facts=extractChatCandidateFacts(ev);
		fetch('/api/chat-report/notify',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:facts.channel||0,message:ev.message,location:facts.location,monster:facts.monster||'',sender:ev.sender||'',score:getChatCandidateScore(ev)})}).catch(()=>{});
  }
	renderChatPanel();
}
function clearChatPanel(){chatEvents=[];const el=document.getElementById('chat-area');if(el)el.innerHTML='';renderDashChat([]);renderChatCandidatePanels([]);}
async function initChat(){
  const dm=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  if(dm.devices)dm.devices.forEach(e=>{if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  refreshChatDeviceDropdown();
  const h=await fetch('/api/chat-log').then(r=>r.json()).catch(()=>[]);
  chatEvents=h||[];renderChatPanel();applyNotifySoundUI();
  const es=new EventSource('/api/chat-events');
  es.onmessage=e=>{try{appendChatToPanel(JSON.parse(e.data));}catch(_){}};
}
async function switchAll(){
  const ch=document.getElementById('allch').value;
  const bar=document.getElementById('status-bar');if(bar)bar.textContent='切替中...';
  const serials=selectedSerials();
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:parseInt(ch),serials})});
  const d=await r.json();
  if(d.results){const failed=d.results.filter(x=>!x.ok);if(bar)bar.textContent=failed.length===0?'✓ 完了':'✗ '+failed.length+'台失敗';}
  else{if(bar)bar.textContent=d.error?'✗ '+d.error:'✗ 失敗';}
  setTimeout(()=>{if(bar)bar.textContent='';},3000);
}
async function switchOne(serial){
  const ch=parseInt(document.getElementById('ch-'+encodeURIComponent(serial)).value);
  const bar=document.getElementById('status-bar');if(bar)bar.textContent='切替中...';
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:ch,serial})});
  const d=await r.json();const res=d.results&&d.results[0];
  if(bar)bar.textContent=res&&res.ok?'✓ 完了':'✗ '+(res&&res.error||d.error||'失敗');
  setTimeout(()=>{if(bar)bar.textContent='';},3000);
}
// ── Tap Loop ──
let tapLoopPollTimer=null;
async function tapLoopStart(){
  const x=parseInt(document.getElementById('tap-x').value)||0;
  const y=parseInt(document.getElementById('tap-y').value)||0;
  const interval=parseInt(document.getElementById('tap-interval').value)||1000;
  const serials=selectedSerials();
  const r=await fetch('/api/adb/tap-loop/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({tap_x:x,tap_y:y,interval_ms:interval,serials})});
  const d=await r.json();
  const st=document.getElementById('tap-status');
  if(d.ok){
    document.getElementById('btn-tap-start').disabled=true;
    document.getElementById('btn-tap-stop').disabled=false;
    if(st)st.textContent='実行中...';
    startTapLoopPoll();
  } else {
    if(st)st.textContent='✗ '+(d.error||'失敗');
  }
}
async function tapLoopStop(){
  await fetch('/api/adb/tap-loop/stop',{method:'POST'});
  stopTapLoopPoll();
  document.getElementById('btn-tap-start').disabled=false;
  document.getElementById('btn-tap-stop').disabled=true;
  const st=document.getElementById('tap-status');if(st)st.textContent='停止';
}
function startTapLoopPoll(){
  if(tapLoopPollTimer)return;
  tapLoopPollTimer=setInterval(async()=>{
    const d=await fetch('/api/adb/tap-loop/status').then(r=>r.json()).catch(()=>null);
    if(!d)return;
    const st=document.getElementById('tap-status');
    if(d.running){
      if(st)st.textContent='実行中 | タップ数: '+d.tick_count;
    } else {
      if(st)st.textContent='停止';
      document.getElementById('btn-tap-start').disabled=false;
      document.getElementById('btn-tap-stop').disabled=true;
      stopTapLoopPoll();
    }
  },1000);
}
function stopTapLoopPoll(){
  if(tapLoopPollTimer){clearInterval(tapLoopPollTimer);tapLoopPollTimer=null;}
}
(async function(){
  const d=await fetch('/api/adb/tap-loop/status').then(r=>r.json()).catch(()=>null);
  if(d&&d.running){
    document.getElementById('btn-tap-start').disabled=true;
    document.getElementById('btn-tap-stop').disabled=false;
    const xi=document.getElementById('tap-x'),yi=document.getElementById('tap-y'),ii=document.getElementById('tap-interval');
    if(xi)xi.value=d.tap_x;if(yi)yi.value=d.tap_y;if(ii)ii.value=d.interval_ms;
    const st=document.getElementById('tap-status');if(st)st.textContent='実行中 | タップ数: '+d.tick_count;
    startTapLoopPoll();
  }
})();
// ── Patrol ──
let patrolChannels=[],patrolReversed=localStorage.getItem('patrolReversed')==='true',patrolLoopMode=localStorage.getItem('patrolLoopMode')!=='false';
function applyReversedUI(){['btn-reversed','dash-btn-reversed'].forEach(id=>{const b=document.getElementById(id);if(!b)return;b.textContent=patrolReversed?'⬇ 逆順':'⬆ 正順';b.classList.toggle('active',patrolReversed);});}
function applyLoopUI(){['btn-loop','dash-btn-loop'].forEach(id=>{const b=document.getElementById(id);if(!b)return;b.textContent=patrolLoopMode?'🔁 ループ':'1️⃣ 一巡';b.classList.toggle('active',!patrolLoopMode);});}
async function loadPatrolChannels(){
  const d=await fetch('/api/patrol/channels').then(r=>r.json());
  patrolChannels=d.channels||[];renderChannelEditor();
  const sel=document.getElementById('dash-patrol-start-ch-select');
  if(sel){const cur=sel.value;sel.innerHTML='<option value="0">(前回位置)</option>'+patrolChannels.map(ch=>'<option value="'+ch+'">'+ch+'</option>').join('');sel.value=cur;}
}
function renderChannelEditor(){
  const el=document.getElementById('ch-editor');
  if(patrolChannels.length===0){el.innerHTML='<div class="no-devices">チャンネルなし</div>';document.getElementById('btn-ch-save').disabled=true;return;}
  el.innerHTML=patrolChannels.map((ch,i)=>'<div class="ch-row"><span class="ch-num">'+(i+1)+'.</span>'
    +'<input type="number" value="'+ch+'" min="1" max="9999" style="width:75px" onchange="patrolChannels['+i+']=parseInt(this.value)||1;document.getElementById(\'btn-ch-save\').disabled=false">'
    +'<button class="btn" style="padding:2px 8px;font-size:.8em" onclick="removeChannel('+i+')">✕</button></div>').join('');
  document.getElementById('btn-ch-save').disabled=false;
}
function addChannel(){const v=parseInt(prompt('追加するチャンネル番号:',''))||0;if(v>0){patrolChannels.push(v);renderChannelEditor();}}
function removeChannel(i){patrolChannels.splice(i,1);renderChannelEditor();document.getElementById('btn-ch-save').disabled=false;}
function sortChannels(dir){patrolChannels.sort((a,b)=>dir==='asc'?a-b:b-a);renderChannelEditor();document.getElementById('btn-ch-save').disabled=false;}
function bulkImportChannels(){
  const nums=document.getElementById('ch-bulk-input').value.split(/[,\s]+/).map(s=>parseInt(s)).filter(n=>n>0);
  if(!nums.length)return;
  patrolChannels=nums;renderChannelEditor();document.getElementById('ch-bulk-input').value='';document.getElementById('btn-ch-save').disabled=false;
}
async function saveChannels(){
  const r=await fetch('/api/patrol/channels',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channels:patrolChannels})});
  const d=await r.json();const st=document.getElementById('ch-save-status');
  st.textContent=d.ok?'✓ 保存済':'✗ 失敗';
  if(d.ok){document.getElementById('btn-ch-save').disabled=true;loadPatrolChannels();}
  setTimeout(()=>st.textContent='',3000);
}
function toggleReversed(){patrolReversed=!patrolReversed;localStorage.setItem('patrolReversed',patrolReversed);applyReversedUI();}
function toggleLoop(){patrolLoopMode=!patrolLoopMode;localStorage.setItem('patrolLoopMode',patrolLoopMode);applyLoopUI();}
function syncPatrolStartCh(v){['patrol-start-ch','dash-patrol-start-ch'].forEach(id=>{const el=document.getElementById(id);if(el&&el.value!==String(v))el.value=v;});const sel=document.getElementById('dash-patrol-start-ch-select');if(sel&&sel.value!==String(v))sel.value=v;}
async function patrolStart(){
  const chs=patrolChannels.length>0?patrolChannels:[];
  const startChEl=document.getElementById('patrol-start-ch')||document.getElementById('dash-patrol-start-ch');
  const body={serials:selectedSerials(),reversed:patrolReversed,loop_mode:patrolLoopMode,start_channel:parseInt(startChEl?.value)||0};
  if(chs.length>0)body.channels=chs;
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  const d=await r.json();if(!d.ok)alert('巡回開始失敗: '+(d.error||''));
}
async function patrolStop(){await fetch('/api/patrol/stop',{method:'POST'});}
async function patrolAllOnce(){
  const startCh=parseInt(document.getElementById('patrol-all-start-ch')?.value)||1;
  const endCh=parseInt(document.getElementById('patrol-all-end-ch')?.value)||100;
  if(startCh>endCh){alert('開始chが終了chより大きいです');return;}
  const channels=[];for(let i=startCh;i<=endCh;i++)channels.push(i);
  const body={serials:selectedSerials(),reversed:false,loop_mode:false,channels};
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  const d=await r.json();if(!d.ok)alert('巡回開始失敗: '+(d.error||''));
}
async function clearFullChannels(){await fetch('/api/patrol/clear-full',{method:'POST'});}
const patrolCycleStats={lastMoveStartAt:0,cycleMsHistory:[],avgCycleMs:0};
function formatPatrolCycleDuration(ms){
	if(!(ms>0))return '--';
	const totalSeconds=Math.max(1,Math.round(ms/1000));
	const minutes=Math.floor(totalSeconds/60);
	const seconds=totalSeconds%60;
	if(minutes<=0)return seconds+'s';
	return minutes+'m '+(seconds<10?'0':'')+seconds+'s';
}
function formatPatrolCycleRate(ms){
	if(!(ms>0))return '--';
	const perHour=3600000/ms;
	const rounded=Math.round(perHour*10)/10;
	return (Number.isInteger(rounded)?String(Math.round(rounded)):rounded.toFixed(1))+'ch/hour';
}
function renderPatrolCycleStats(running){
	const pairs=[
		['ps-cycle-time','ps-cycle-rate'],
		['dash-ps-cycle-time','dash-ps-cycle-rate'],
	];
	pairs.forEach(([timeId,rateId])=>{
		const timeEl=document.getElementById(timeId);
		const rateEl=document.getElementById(rateId);
		if(!timeEl)return;
		timeEl.textContent=formatPatrolCycleDuration(patrolCycleStats.avgCycleMs);
		if(rateEl)rateEl.textContent=formatPatrolCycleRate(patrolCycleStats.avgCycleMs);
	});
}
function updatePatrolCycleStats(d,currentPhase){
	if(!d.running){
		patrolCycleStats.lastMoveStartAt=0;
		renderPatrolCycleStats(false);
		return;
	}
	if(currentPhase==='move_start'){
		const startedAt=Number(d.phase_started_at_unix_ms||0);
		if(startedAt>0){
			if(patrolCycleStats.lastMoveStartAt>0 && startedAt!==patrolCycleStats.lastMoveStartAt){
				const cycleMs=startedAt-patrolCycleStats.lastMoveStartAt;
				if(cycleMs>0 && cycleMs<86400000){
					patrolCycleStats.cycleMsHistory.push(cycleMs);
					if(patrolCycleStats.cycleMsHistory.length>10)patrolCycleStats.cycleMsHistory.shift();
					const sum=patrolCycleStats.cycleMsHistory.reduce((a,b)=>a+b,0);
					patrolCycleStats.avgCycleMs=sum/patrolCycleStats.cycleMsHistory.length;
				}
			}
			patrolCycleStats.lastMoveStartAt=startedAt;
		}
	}
	renderPatrolCycleStats(true);
}
function updatePatrolUI(running){
  ['btn-patrol-start','dash-btn-patrol-start'].forEach(id=>{const b=document.getElementById(id);if(b)b.disabled=running;});
  ['btn-patrol-stop','dash-btn-patrol-stop'].forEach(id=>{const b=document.getElementById(id);if(b)b.disabled=!running;});
  const bar=document.getElementById('hdr-bar');
  if(bar){bar.className=running?'titlebar running':'titlebar';}
}
async function pollPatrolStatus(){
  try{
    const d=await fetch('/api/patrol/status').then(r=>r.json());
    const els=(id)=>document.getElementById(id);
    if(d.running){
			const phaseMap = {
				move_start: {label:'移動開始', start:0, end:20},
				loading: {label:'ロード中', start:20, end:68},
				dwell_wait: {label:'滞在待機', start:68, end:100}
			};
      const currentPhase = d.phase && phaseMap[d.phase] ? d.phase : (d.waiting_move ? 'loading' : 'move_start');
			const phaseState = phaseMap[currentPhase] || {label:'巡回中', start:0, end:0};
			const now = Date.now();
			const startedAt = Number(d.phase_started_at_unix_ms || 0);
			const totalMs = Math.max(0, Number(d.phase_total_secs || 0) * 1000);
			const elapsedMs = startedAt > 0 ? Math.max(0, now - startedAt) : 0;
			let progressPct = phaseState.end;
			if(totalMs > 0 && phaseState.end > phaseState.start){
				const ratio = Math.min(elapsedMs / totalMs, 1);
				progressPct = phaseState.start + ((phaseState.end - phaseState.start) * ratio);
			}else if(phaseState.end > phaseState.start){
				progressPct = phaseState.start + ((phaseState.end - phaseState.start) * 0.6);
			}
			const remainingSecs = totalMs > 0 ? Math.max(0, Math.ceil((totalMs - elapsedMs) / 1000)) : 0;
			const phaseLabel = phaseState.label + (remainingSecs > 0 ? ' (' + remainingSecs + 's)' : '');
			const progressText = Math.round(progressPct) + '%';
      updatePatrolCycleStats(d, currentPhase);
      ['ps-state','dash-ps-state'].forEach(id=>{const e=els(id);if(e){e.className='running';e.textContent='▶ '+phaseState.label+(id==='ps-state'&&d.waiting_move?' ⏳':'');}});
      ['ps-ch','dash-ps-ch'].forEach(id=>{const e=els(id);if(e)e.textContent='Ch'+d.current_channel;});
      ['ps-prog','dash-ps-prog'].forEach(id=>{const e=els(id);if(e)e.textContent=(d.current_index+1)+'/'+d.total_channels;});
      ['dash-patrol-label','ps-patrol-label'].forEach(id=>{const e=els(id);if(e)e.textContent=phaseLabel;});
      ['dash-patrol-percent','ps-patrol-percent'].forEach(id=>{const e=els(id);if(e)e.textContent=progressText;});
			['dash-patrol-fill','ps-progress-fill'].forEach(id=>{const e=els(id);if(e)e.style.width=Math.round(progressPct)+'%';});
      ['ps-parallel','dash-ps-parallel'].forEach(id=>{
        const par=els(id);if(par){
          const delay=d.parallel_group_delay>0?'(+'+d.parallel_group_delay+'s)':'';
          par.textContent=(d.parallel_limit===0?'並列:無制限':'並列:'+d.parallel_limit+'台'+delay)+(d.move_timeout_secs>0?' | timeout:'+d.move_timeout_secs+'s':'')+' | 滞在:'+Math.round(d.dwell_secs)+'s';
        }
      });
      updatePatrolUI(true);
    }else{
      ['ps-state','dash-ps-state'].forEach(id=>{const e=els(id);if(e){e.className='stopped';e.textContent='■ 停止中';}});
      ['ps-ch','dash-ps-ch','ps-prog','dash-ps-prog'].forEach(id=>{const e=els(id);if(e)e.textContent=id==='ps-ch'&&d.last_channel>0?'前回: Ch'+d.last_channel:'';});
			['dash-patrol-label','ps-patrol-label'].forEach(id=>{const e=els(id);if(e)e.textContent='--';});
			['dash-patrol-percent','ps-patrol-percent'].forEach(id=>{const e=els(id);if(e)e.textContent='';});
			['dash-patrol-fill','ps-progress-fill'].forEach(id=>{const e=els(id);if(e)e.style.width='0%';});
			updatePatrolCycleStats(d, '');
      ['ps-parallel','dash-ps-parallel'].forEach(id=>{const par=els(id);if(par)par.textContent='';});
      updatePatrolUI(false);
    }
    [['ps-full','btn-clear-full'],['dash-ps-full','btn-dash-clear-full']].forEach(([fullId,clearId])=>{
      const fullEl=els(fullId),clearBtn=els(clearId);
      if(d.full_channels&&d.full_channels.length){if(fullEl)fullEl.textContent='🚫 満員スキップ: Ch'+d.full_channels.join(', Ch');if(clearBtn)clearBtn.style.display='';}
      else{if(fullEl)fullEl.textContent='';if(clearBtn)clearBtn.style.display='none';}
    });
    const crashed=d.crashed_instances&&d.crashed_instances.length?d.crashed_instances:null;
    const showWarn=!crashed&&d.running&&(d.consecutive_full_count||0)>=3;
    const warnMsg=crashed?'⚠ クラッシュ判定: '+crashed.join(', ')+' (3回連続未応答)':'⚠ ゲームクライアントがch移動できない状態です（クラッシュの可能性）。ADBサーバーを再起動してください。';
    ['crash-warning','dash-crash-warning'].forEach(id=>{
      const e=els(id);if(!e)return;
      const show=crashed||showWarn;
      e.style.display=show?'':'none';if(show)e.textContent=warnMsg;
    });
  }catch(e){console.warn('patrol status error:',e);}
  finally{setTimeout(pollPatrolStatus,2000);}
}
// ── Config ──
const CHAT_FILTER_FIELDS=[
	{k:'chat_report_senders',label:'候補発言者(含む)',type:'multiline-list',desc:'hidden'},
	{k:'chat_report_excluded_senders',label:'候補発言者(除外)',type:'multiline-list',desc:'hidden'},
];
const CHAT_RULE_FIELDS=[
	{k:'chat_report_location_rules',label:'地点別名ルール',type:'multiline-list',desc:'1行形式: 地点名|別名|モンスター1,モンスター2'},
	{k:'chat_report_monster_alias_rules',label:'モンスター別名ルール',type:'multiline-list',desc:'1行形式: モンスター名|別名'},
];
const CHAT_FILTER_MINIMIZED_KEY='settingsChatFilterMinimized';
let chatFilterMinimized=localStorage.getItem(CHAT_FILTER_MINIMIZED_KEY)==='true';
const CHAT_NOTIFY_SCORE_MIN=0;
const CHAT_NOTIFY_SCORE_MAX=20;
const CHAT_NOTIFY_SCORE_DEFAULT=6;
const CHAT_NOTIFY_SCORE_KEY='chatNotifyMinScore';
let chatNotifyMinScore=CHAT_NOTIFY_SCORE_DEFAULT;
try{
	const saved=parseInt(localStorage.getItem(CHAT_NOTIFY_SCORE_KEY)||'',10);
	if(Number.isFinite(saved))chatNotifyMinScore=saved;
}catch(_){ }
const CFG_FIELDS=[
  {k:'discord_webhook',label:'Discord Webhook URL (検知報告)',type:'text',desc:'空にするとDiscord通知無効',testBtn:true},
  {k:'discord_chat_report_webhook',label:'Discord Webhook URL (ワルチャ報告)',type:'text',desc:'チャット報告候補を別チャンネルに通知。空で無効',testBtn:true},
  {k:'chat_exclude',label:'チャット除外キーワード',type:'csv',desc:'カンマ区切り。例: いない,終わった'},
  {k:'chat_report_min_length',label:'報告候補 最小文字数',type:'number',desc:'0でデフォルト(4)。これ未満のメッセージを除外'},
  {k:'chat_report_max_length',label:'報告候補 最大文字数',type:'number',desc:'0でデフォルト(80)。これ超のメッセージを除外'},
  {k:'patrol_dwell_secs',label:'滞在時間 (秒)',type:'number',desc:'ch移動完了後〜次ch移動開始までの待機秒数'},
  {k:'patrol_move_timeout_secs',label:'初回マージ待ちタイムアウト (秒)',type:'number',desc:'1台目のマージを待つ最大秒数。0=無効'},
  {k:'patrol_merge_timeout_secs',label:'残りマージ待ちタイムアウト (秒)',type:'number',desc:'1台目受信後、残り台数を待つ最大秒数'},
  {k:'parallel_limit',label:'並列切替台数',type:'number',desc:'0=全台同時（ディレイ無効）'},
  {k:'parallel_group_delay_secs',label:'グループ間ディレイ (秒)',type:'number',desc:'並列台数>0のとき有効'},
  {k:'adb_path',label:'ADBパス',type:'text',desc:'adb.exeのフルパスまたは「adb」'},
  {k:'mumu_delay_ms',label:'ADBコマンド間隔 (ms)',type:'number',desc:'各ADBコマンド間の待機時間'},
  {k:'mumu_tap_x',label:'タップX座標',type:'number',desc:'チャンネル入力欄のタップX'},
  {k:'mumu_tap_y',label:'タップY座標',type:'number',desc:'チャンネル入力欄のタップY'},
  {k:'mumu_pre_keycode',label:'プリキーコード',type:'text',desc:'チャンネル入力欄を開くキーコード'},
  {k:'gas_target_enemy',label:'GAS 対象エネミー',type:'select',options:['金ウリボ','金ナッポ'],desc:'Chrome拡張から受信するエネミー種別'},
];
let cfgData={};
function renderConfigFields(containerId, fields){
	const root=document.getElementById(containerId);if(!root)return;
	root.innerHTML=fields.map(function(f){
		var val=cfgData[f.k]!==undefined?cfgData[f.k]:'';
		var noteHtml=f.desc?('<span class="cfg-note">'+escHtml(f.desc)+'</span>'):'';
		if(f.type==='csv'&&Array.isArray(val))val=val.join(',');
		if(f.type==='multiline-list'&&Array.isArray(val))val=val.join('\n');
		if(f.type==='multiline-list'){
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><textarea id="cfg-'+f.k+'" rows="10" spellcheck="false" placeholder="'+escHtml(f.desc||'')+'">'+escHtml(String(val))+'</textarea>'+noteHtml+'</div>';
		}
		if(f.type==='bool'){
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><label style="display:flex;align-items:center;gap:6px;cursor:pointer"><input type="checkbox" id="cfg-'+f.k+'"'+(val?' checked':'')+'>'+escHtml(f.desc||'')+'</label></div>';
		}
		if(f.type==='select'){
			const opts=(f.options||[]).map(o=>'<option value="'+escHtml(o)+'"'+(val===o?' selected':'')+'>'+escHtml(o)+'</option>').join('');
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><select id="cfg-'+f.k+'">'+opts+'</select>'+noteHtml+'</div>';
		}
		var inputType=f.type==='csv'?'text':f.type;
		var testBtnHtml=f.testBtn?('<div style="margin-top:4px"><button type="button" class="btn" onclick="testWebhook(\'cfg-'+f.k+'\')">📨 テスト送信</button><span id="cfg-'+f.k+'-test-result" style="margin-left:8px;font-size:var(--fs-sm);opacity:.8"></span></div>'):'';
		return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><input type="'+inputType+'" id="cfg-'+f.k+'" value="'+escHtml(String(val))+'" placeholder="'+escHtml(f.desc||'')+'">'+noteHtml+testBtnHtml+'</div>';
	}).join('');
}
function renderChatRuleTable(headers, rows, emptyText){
	if(!rows.length)return '<div class="cfg-rule-empty">'+escHtml(emptyText)+'</div>';
	return '<div class="cfg-rule-table-wrap"><table class="cfg-rule-table"><thead><tr>'
		+headers.map(h=>'<th>'+escHtml(h)+'</th>').join('')
		+'</tr></thead><tbody>'
		+rows.join('')
		+'</tbody></table></div>';
}
function renderChatRuleTableMarkup(headers, rows, emptyText){
	if(!rows.length)return '<div class="cfg-rule-empty">'+escHtml(emptyText)+'</div>';
	return '<div class="cfg-rule-table-wrap"><table class="cfg-rule-table"><thead><tr>'
		+headers.map(h=>'<th>'+escHtml(h)+'</th>').join('')
		+'</tr></thead><tbody>'
		+rows.join('')
		+'</tbody></table></div>';
}
function applyChatFilterMinimizeState(){
	const card=document.querySelector('.chat-filter-card');
	const btn=document.getElementById('chat-filter-toggle-btn');
	if(card)card.classList.toggle('minimized',chatFilterMinimized);
	if(btn)btn.textContent=chatFilterMinimized?'＋ 展開':'－ 最小化';
}
function toggleChatFilterMinimize(){
	chatFilterMinimized=!chatFilterMinimized;
	localStorage.setItem(CHAT_FILTER_MINIMIZED_KEY,String(chatFilterMinimized));
	applyChatFilterMinimizeState();
}
function clampChatNotifyScore(value){
	const n=parseInt(value,10);
	if(!Number.isFinite(n))return CHAT_NOTIFY_SCORE_DEFAULT;
	return Math.max(CHAT_NOTIFY_SCORE_MIN,Math.min(CHAT_NOTIFY_SCORE_MAX,n));
}
function getChatNotifyMinScore(){
	chatNotifyMinScore=clampChatNotifyScore(chatNotifyMinScore);
	return chatNotifyMinScore;
}
function syncChatNotifyScoreInputs(){
	const score=getChatNotifyMinScore();
	const slider=document.getElementById('chat-notify-score-slider');
	const number=document.getElementById('chat-notify-score-number');
	const label=document.getElementById('chat-notify-score-value');
	if(slider && String(slider.value)!==String(score))slider.value=String(score);
	if(number && String(number.value)!==String(score))number.value=String(score);
	if(label)label.textContent='score '+score+' 以上で通知';
}
function setChatNotifyMinScore(value){
	const next=clampChatNotifyScore(value);
	if(next===chatNotifyMinScore){
		syncChatNotifyScoreInputs();
		return;
	}
	chatNotifyMinScore=next;
	localStorage.setItem(CHAT_NOTIFY_SCORE_KEY,String(chatNotifyMinScore));
	syncChatNotifyScoreInputs();
	renderChatCandidatePanels();
}
function onChatNotifyScoreSliderInput(value){
	setChatNotifyMinScore(value);
}
function onChatNotifyScoreNumberChange(value){
	setChatNotifyMinScore(value);
}
function getCustomLocationRuleLines(){
	return normalizeCsvList(cfgData.chat_report_location_rules);
}
function getCustomMonsterAliasRuleLines(){
	return normalizeCsvList(cfgData.chat_report_monster_alias_rules);
}
function getChatFilterLines(key){
	return normalizeCsvList(cfgData[key]);
}
function renderSimpleFilterRows(key){
	return getChatFilterLines(key).map(value=>'<tr>'
		+'<td>'+escHtml(value)+'</td>'
		+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="removeChatFilterValue(\''+key+'\','+escAttrJs(value)+')">削除</button></td>'
		+'</tr>');
}
function renderCustomLocationRuleRows(usePopupHandler){
	return getCustomLocationRuleLines().map(line=>{
		const parsed=parseChatLocationRuleLine(line);
		if(!parsed)return '';
		const deleteCall=(usePopupHandler?'removeRule':'removeChatRuleLine')+'(\'chat_report_location_rules\','+escAttrJs(line)+')';
		return '<tr>'
			+'<td class="cell-mono">'+escHtml(parsed.name)+'</td>'
			+'<td>'+escHtml(parsed.aliases.filter(v=>v!==parsed.name).join(', '))+'</td>'
			+'<td>'+escHtml(parsed.monsters.join(', '))+'</td>'
			+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="'+deleteCall+'">削除</button></td>'
			+'</tr>';
	}).filter(Boolean);
}
function renderCustomMonsterAliasRuleRows(usePopupHandler){
	return getCustomMonsterAliasRuleLines().map(line=>{
		const parsed=parseChatMonsterAliasRuleLine(line);
		if(!parsed)return '';
		const deleteCall=(usePopupHandler?'removeRule':'removeChatRuleLine')+'(\'chat_report_monster_alias_rules\','+escAttrJs(line)+')';
		return '<tr>'
			+'<td class="cell-mono">'+escHtml(parsed.name)+'</td>'
			+'<td>'+escHtml(parsed.aliases.join(', '))+'</td>'
			+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="'+deleteCall+'">削除</button></td>'
			+'</tr>';
	}).filter(Boolean);
}
let chatRuleWindowRef=null;
function buildChatRuleWindowHtml(){
	const locationRows=renderCustomLocationRuleRows(true);
	const monsterRows=renderCustomMonsterAliasRuleRows(true);
	const locationOptions=getChatLocationRules().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const monsterOptions=getChatMonsterAliases().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	return '<!doctype html><html><head><meta charset="utf-8"><title>チャット候補ルール一覧</title><style>'
		+'body{margin:0;background:#0b0e15;color:#e6edf6;font-family:Segoe UI,sans-serif;padding:16px}'
		+'.cfg-rule-window-section{margin-bottom:18px}'
		+'.cfg-rule-window-title{font-size:14px;font-weight:600;color:#e6edf6;margin-bottom:10px}'
		+'.cfg-rule-window-note{font-size:12px;color:#98a3b8;margin-bottom:10px}'
		+'.cfg-rule-actions{display:flex;gap:6px;align-items:center;flex-wrap:wrap;margin-bottom:10px}'
		+'.cfg-rule-actions select,.cfg-rule-actions input{background:#121826;color:#e6edf6;border:1px solid #2b3448;border-radius:8px;padding:7px 10px;font-size:12px}'
		+'.cfg-rule-actions select{min-width:180px;flex:1}.cfg-rule-actions input{flex:1;min-width:160px}'
		+'.btn{background:#1a2233;border:1px solid #2b3448;color:#e6edf6;border-radius:8px;padding:7px 10px;font-size:12px;cursor:pointer}'
		+'.btn:hover{background:#202b42}.btn.danger{color:#ff8a8a}'
		+'.cfg-rule-table-wrap{border:1px solid #2b3448;border-radius:10px;overflow:auto;background:#121826;max-height:none}'
		+'.cfg-rule-table{width:100%;border-collapse:collapse;font-size:12px;table-layout:fixed}'
		+'.cfg-rule-table th,.cfg-rule-table td{border-bottom:1px solid #2b3448;border-right:1px solid #2b3448;padding:8px 10px;vertical-align:top;line-height:1.5;word-break:break-word}'
		+'.cfg-rule-table th:last-child,.cfg-rule-table td:last-child{border-right:none}'
		+'.cfg-rule-table th{background:#1a2233;color:#98a3b8;font-weight:600;position:sticky;top:0}'
		+'.cfg-rule-table tr:last-child td{border-bottom:none}'
		+'.cell-mono{font-family:Consolas,monospace;color:#6ea8ff}'
		+'.cfg-rule-empty{padding:12px;color:#98a3b8;font-size:12px}'
		+'</style></head><body>'
		+'<div class="cfg-rule-window-section"><div class="cfg-rule-window-title">場所別名ルール一覧</div><div class="cfg-rule-window-note">このウィンドウから追加・削除できます。</div>'
		+'<div class="cfg-rule-actions"><select id="popup-location-rule-target"><option value="">場所を選択</option>'+locationOptions+'</select><input type="text" id="popup-location-rule-alias" placeholder="別名を入力。例: tnt"><button type="button" class="btn" onclick="addLocationRule()">追加</button></div>'
		+renderChatRuleTableMarkup(['場所','追加した別名','出現候補',''],locationRows,'追加した場所別名はありません')+'</div>'
		+'<div class="cfg-rule-window-section"><div class="cfg-rule-window-title">モンスター別名ルール一覧</div>'
		+'<div class="cfg-rule-actions"><select id="popup-monster-rule-target"><option value="">モンスターを選択</option>'+monsterOptions+'</select><input type="text" id="popup-monster-rule-alias" placeholder="別名を入力。例: 金ウリ"><button type="button" class="btn" onclick="addMonsterRule()">追加</button></div>'
		+renderChatRuleTableMarkup(['モンスター','追加した別名',''],monsterRows,'追加したモンスター別名はありません')+'</div>'
		+'<script>'
		+'function addLocationRule(){var target=document.getElementById("popup-location-rule-target");var input=document.getElementById("popup-location-rule-alias");if(!window.opener||!target||!input)return;window.opener.addChatLocationRuleValue(String(target.value||""),String(input.value||""));}'
		+'function addMonsterRule(){var target=document.getElementById("popup-monster-rule-target");var input=document.getElementById("popup-monster-rule-alias");if(!window.opener||!target||!input)return;window.opener.addChatMonsterAliasRuleValue(String(target.value||""),String(input.value||""));}'
		+'function removeRule(key,line){if(window.opener)window.opener.removeChatRuleLine(key,line);}'
		+'<\/script>'
		+'</body></html>';
}
function refreshChatRuleWindow(){
	if(!chatRuleWindowRef || chatRuleWindowRef.closed)return;
	const doc=chatRuleWindowRef.document;
	doc.open();
	doc.write(buildChatRuleWindowHtml());
	doc.close();
}
function openChatRuleWindow(){
	chatRuleWindowRef=window.open('','chat-rule-window','width=1200,height=780');
	if(!chatRuleWindowRef)return;
	refreshChatRuleWindow();
	chatRuleWindowRef.focus();
}
function renderChatRuleManagers(){
	const root=document.getElementById('cfg-chat-rule-managers');if(!root)return;
	const scroller=document.getElementById('view-settings');
	const savedScroll=scroller?scroller.scrollTop:0;
	if(document.activeElement && root.contains(document.activeElement))document.activeElement.blur();
	const locationOptions=getChatLocationRules().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const monsterOptions=getChatMonsterAliases().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const excludeRows=renderSimpleFilterRows('chat_report_excluded_senders');
	const senderExcludeRules=normalizeCsvList(cfgData.chat_report_excluded_senders).join('\n');
	const locationRules=normalizeCsvList(cfgData.chat_report_location_rules).join('\n');
	const monsterRules=normalizeCsvList(cfgData.chat_report_monster_alias_rules).join('\n');
	const notifyScore=getChatNotifyMinScore();
	const locationRows=renderCustomLocationRuleRows(false);
	const monsterRows=renderCustomMonsterAliasRuleRows(false);
	root.innerHTML=''
		+'<div class="chat-score-threshold-box">'
		+'<div class="chat-score-threshold-title">通知しきい値</div>'
		+'<div class="chat-score-threshold-controls">'
		+'<input type="range" id="chat-notify-score-slider" min="'+CHAT_NOTIFY_SCORE_MIN+'" max="'+CHAT_NOTIFY_SCORE_MAX+'" step="1" value="'+notifyScore+'" oninput="onChatNotifyScoreSliderInput(this.value)">'
		+'<input type="number" id="chat-notify-score-number" min="'+CHAT_NOTIFY_SCORE_MIN+'" max="'+CHAT_NOTIFY_SCORE_MAX+'" step="1" value="'+notifyScore+'" onchange="onChatNotifyScoreNumberChange(this.value)">'
		+'<span id="chat-notify-score-value" class="chat-score-threshold-value">score '+notifyScore+' 以上で通知</span>'
		+'</div>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-box-title" style="color:var(--danger)">検知不要プレイヤー</div>'
		+'<div class="cfg-rule-actions">'
		+'<input type="text" id="cfg-sender-exclude-input" placeholder="発言者名を入力">'
		+'<button type="button" class="btn danger" onclick="addChatFilterManagerValue(\'chat_report_excluded_senders\',\'cfg-sender-exclude-input\')">+ 除外に追加</button>'
		+'</div>'
		+renderChatRuleTable(['発言者',''],excludeRows,'除外発言者なし')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_excluded_senders">'+escHtml(senderExcludeRules)+'</textarea>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-header"><div class="cfg-rule-box-title">場所別名を追加</div><button type="button" class="btn" style="margin-left:auto" onclick="openChatRuleWindow()">⧉ 一覧を別ウィンドウ</button></div>'
		+'<div class="cfg-rule-actions">'
		+'<select id="cfg-location-rule-target"><option value="">場所を選択</option>'+locationOptions+'</select>'
		+'<input type="text" id="cfg-location-rule-alias" placeholder="別名を入力。例: tnt">'
		+'<button type="button" class="btn" onclick="addChatLocationRule()">追加</button>'
		+'</div>'
		+renderChatRuleTableMarkup(['場所','追加した別名','出現候補',''],locationRows,'追加した場所別名はありません')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_location_rules" rows="10" spellcheck="false" placeholder="地点名|別名|モンスター1,モンスター2">'+escHtml(locationRules)+'</textarea>'
		+'<span class="cfg-note">追加内容はセル形式で表示しています。保存はこの一覧から行われます。</span>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-box-title">モンスター別名を追加</div>'
		+'<div class="cfg-rule-actions">'
		+'<select id="cfg-monster-rule-target"><option value="">モンスターを選択</option>'+monsterOptions+'</select>'
		+'<input type="text" id="cfg-monster-rule-alias" placeholder="別名を入力。例: 金ウリ"><button type="button" class="btn" onclick="addChatMonsterAliasRule()">追加</button>'
		+'</div>'
		+renderChatRuleTableMarkup(['モンスター','追加した別名',''],monsterRows,'追加したモンスター別名はありません')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_monster_alias_rules" rows="10" spellcheck="false" placeholder="モンスター名|別名">'+escHtml(monsterRules)+'</textarea>'
		+'<span class="cfg-note">追加内容はセル形式で表示しています。保存はこの一覧から行われます。</span>'
		+'</div>';
	syncChatNotifyScoreInputs();
	if(scroller)requestAnimationFrame(()=>{ scroller.scrollTop=savedScroll; });
}
async function removeChatFilterValue(key, value){
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.filter(v=>v!==value));
	renderChatRuleManagers();
	refreshChatRuleWindow();
	await saveConfig(true);
}
async function addChatFilterManagerValue(key, inputId){
	const input=document.getElementById(inputId);
	if(!input)return;
	const value=String(input.value||'').trim();
	if(!value)return;
	await appendChatFilterValue(key, value);
	input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function removeChatRuleLine(key, line){
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.filter(v=>v!==line));
	renderChatRuleManagers();
	refreshChatRuleWindow();
	await saveConfig(true);
}
async function addChatLocationRuleValue(nameValue, aliasValue){
	const name=String(nameValue||'').trim();
	const alias=String(aliasValue||'').trim();
	if(!name||!alias)return;
	const current=Array.isArray(cfgData.chat_report_location_rules)?cfgData.chat_report_location_rules:[];
	const idx=current.findIndex(line=>{const p=parseChatLocationRuleLine(line);return p&&p.name===name;});
	if(idx>=0){
		const parsed=parseChatLocationRuleLine(current[idx]);
		const newAliases=normalizeCsvList(parsed.aliases.concat([alias]));
		const newLine=[name,newAliases.join(','),parsed.monsters.join(',')].join('|');
		const updated=current.slice();
		updated[idx]=newLine;
		updateChatFilterTextarea('chat_report_location_rules',updated);
		renderChatCandidatePanels();
		await saveConfig(true);
	}else{
		const rule=getChatLocationRules().find(v=>v.name===name);
		const monsters=rule&&Array.isArray(rule.monsters)?rule.monsters.join(','):'';
		await appendChatFilterValue('chat_report_location_rules',[name,alias,monsters].join('|'));
	}
	const input=document.getElementById('cfg-location-rule-alias');
	if(input)input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function addChatLocationRule(){
	const target=document.getElementById('cfg-location-rule-target');
	const input=document.getElementById('cfg-location-rule-alias');
	if(!target||!input)return;
	await addChatLocationRuleValue(target.value,input.value);
	input.value='';
}
async function addChatMonsterAliasRuleValue(nameValue, aliasValue){
	const name=String(nameValue||'').trim();
	const alias=String(aliasValue||'').trim();
	if(!name||!alias)return;
	await appendChatFilterValue('chat_report_monster_alias_rules',[name,alias].join('|'));
	const input=document.getElementById('cfg-monster-rule-alias');
	if(input)input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function addChatMonsterAliasRule(){
	const target=document.getElementById('cfg-monster-rule-target');
	const input=document.getElementById('cfg-monster-rule-alias');
	if(!target||!input)return;
	await addChatMonsterAliasRuleValue(target.value,input.value);
	input.value='';
}
async function loadConfig(){
  cfgData=await fetch('/api/config').then(r=>r.json());
	renderConfigFields('cfg-chat-form',CHAT_FILTER_FIELDS);
	renderChatRuleManagers();
	renderConfigFields('cfg-form',CFG_FIELDS);
	renderChatCandidatePanels();
	renderConfigForm(cfgData);
	applyChatFilterMinimizeState();
}
async function saveConfig(silent){
	const updated={...cfgData};
		[...CHAT_FILTER_FIELDS,...CHAT_RULE_FIELDS,...CFG_FIELDS].forEach(f=>{const el=document.getElementById('cfg-'+f.k);if(!el)return;
		if(f.type==='number')updated[f.k]=parseFloat(el.value)||0;
		else if(f.type==='multiline-list')updated[f.k]=el.value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean);
		else if(f.type==='csv')updated[f.k]=el.value.split(',').map(s=>s.trim()).filter(Boolean);
		else if(f.type==='bool')updated[f.k]=el.checked;
		else updated[f.k]=el.value;});
	// 通知エネミー（{name, enabled} 形式で全件保存）
	updated.notify_enemies=[];
	document.querySelectorAll('.cfg-enemy').forEach(chk=>{updated.notify_enemies.push({name:chk.value,enabled:chk.checked});});
	const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(updated)});
	const d=await r.json();const st=document.getElementById('cfg-status');
		if(st && !silent){
				st.textContent=d.ok?'✓ 保存・反映済':'✗ 失敗: '+(d.error||'');
				setTimeout(()=>st.textContent='',4000);
		}
	cfgData=updated;renderChatRuleManagers();renderChatCandidatePanels();
}
async function testWebhook(inputId){
	const url=(document.getElementById(inputId)||{}).value||'';
	const resultEl=document.getElementById(inputId+'-test-result');
	if(!url.trim()){if(resultEl)resultEl.textContent='URL未入力';return;}
	if(resultEl)resultEl.textContent='送信中...';
	try{
		const r=await fetch('/api/webhook/test',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url:url.trim()})});
		const d=await r.json();
		if(resultEl)resultEl.textContent=d.ok?'✓ 送信成功':'✗ 失敗: '+(d.error||'');
	}catch(e){if(resultEl)resultEl.textContent='✗ エラー: '+e.message;}
	setTimeout(()=>{if(resultEl)resultEl.textContent='';},5000);
}
function renderConfigForm(cfg){
	// 通知エネミー チェックボックス反映（{name,enabled} 形式）
	const notifyEnemies=cfg.notify_enemies||[
		{name:"ウリボ・ゴールド",enabled:true},
		{name:"金ナッポ",enabled:true},
		{name:"銀ナッポ",enabled:true}
	];
	document.querySelectorAll('.cfg-enemy').forEach(chk=>{
		const entry=notifyEnemies.find(e=>e.name===chk.value);
		chk.checked=entry?entry.enabled:true;
	});
}
// ── Uptime counter + current time ──
let _startTime=parseInt(localStorage.getItem('uptimeStart')||'0')||Date.now();
localStorage.setItem('uptimeStart',_startTime);
function resetUptime(){_startTime=Date.now();localStorage.setItem('uptimeStart',_startTime);}
setInterval(()=>{
	const now=Date.now();
	const s=Math.floor((now-_startTime)/1000);
	const h=Math.floor(s/3600),m=Math.floor((s%3600)/60),ss=s%60;
	const el=document.getElementById('dash-uptime');
	if(el)el.textContent=(h<10?'0':'')+h+':'+(m<10?'0':'')+m+':'+(ss<10?'0':'')+ss;
	const ct=document.getElementById('dash-current-time');
	if(ct){const d=new Date(now),hh=d.getHours(),mm=d.getMinutes(),sc=d.getSeconds();ct.textContent=(hh<10?'0':'')+hh+':'+(mm<10?'0':'')+mm+':'+(sc<10?'0':'')+sc;}
	// Sidebar uptime
	const sm=document.getElementById('brand-uptime-small');
	if(sm)sm.textContent=(h<10?'0':'')+h+':'+(m<10?'0':'')+m+':'+(ss<10?'0':'')+ss;
	// Sidebar current time
	const cts=document.getElementById('brand-current-time-small');
	if(cts){const d=new Date(now),hh=d.getHours(),mm=d.getMinutes(),sc=d.getSeconds();cts.textContent=(hh<10?'0':'')+hh+':'+(mm<10?'0':'')+mm+':'+(sc<10?'0':'')+sc;}
},1000);
// ── Dashboard layout ──
const DASH_PANEL_IDS=['card-dash-devices','card-dash-patrol','card-dash-gold','card-dash-chat','card-dash-report'];
const DASH_SIZE_CLASSES=['panel-size-1x1','panel-size-1x2','panel-size-2x1','panel-size-2x2','panel-size-2x3','panel-size-2x4'];
const PATROL_LAYOUT_CARD_IDS=['card-patrol-control','card-patrol-channels','card-patrol-gold'];
const PATROL_LAYOUT_DEFAULT_COLUMNS={
	'card-patrol-control':'left',
	'card-patrol-channels':'left',
	'card-patrol-gold':'right',
};
const DASH_GRID_ROW_UNIT=8;
const DASH_GRID_GAP=10;
const DASH_MIN_PANEL_ROWS=18;
const DASH_MAX_PANEL_ROWS=220;
let layoutEditMode=false;
function getDefaultPanelRows(size){
	switch(size){
		case '1x2':
		case '2x2': return 52;
		case '2x3': return 76;
		case '2x4': return 100;
		default: return 28;
	}
}
function getPanelRows(card){
	if(!card)return DASH_MIN_PANEL_ROWS;
	const inlineRows=parseInt(card.style.getPropertyValue('--panel-rows'),10);
	if(inlineRows>0)return inlineRows;
	const computedRows=parseInt(getComputedStyle(card).getPropertyValue('--panel-rows'),10);
	if(computedRows>0)return computedRows;
	const sc=DASH_SIZE_CLASSES.find(c=>card.classList.contains(c));
	return getDefaultPanelRows(sc?sc.replace('panel-size-',''):'1x1');
}
function getPanelWidthUnits(card){
	if(!card)return 1;
	return DASH_SIZE_CLASSES.some(c=>c.indexOf('panel-size-2x')===0&&card.classList.contains(c))?2:1;
}
function clampPanelRows(rows){
	return Math.max(DASH_MIN_PANEL_ROWS,Math.min(DASH_MAX_PANEL_ROWS,rows));
}
function pixelsToPanelRows(heightPx){
	return clampPanelRows(Math.round((heightPx + DASH_GRID_GAP) / (DASH_GRID_ROW_UNIT + DASH_GRID_GAP)));
}
function panelRowsToPixels(rows){
	const safeRows=clampPanelRows(rows);
	return safeRows * DASH_GRID_ROW_UNIT + Math.max(0,safeRows - 1) * DASH_GRID_GAP;
}
function setPanelRows(card,rows,save){
	if(!card)return;
	card.style.setProperty('--panel-rows',String(clampPanelRows(rows)));
	if(save)saveDashboardLayout();
}
function getPanelColumn(card){
	if(!card)return 1;
	if(card.classList.contains('panel-col-2'))return 2;
	return 1;
}
function setPanelColumn(card,column){
	if(!card)return;
	card.classList.remove('panel-col-1','panel-col-2');
	if(column===2)card.classList.add('panel-col-2');
	else card.classList.add('panel-col-1');
}
function setPanelWidthUnits(card,width,save,column){
	if(!card)return;
	const currentRows=getPanelRows(card);
	DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
	if(width===2){
		card.classList.remove('panel-col-1','panel-col-2');
		card.classList.add('panel-size-2x1');
	}else{
		card.classList.add('panel-size-1x1');
		setPanelColumn(card,column===2?2:1);
	}
	setPanelRows(card,currentRows,false);
	updateDashboardResizeHandlePositions();
	if(save)saveDashboardLayout();
}
function updateDashboardResizeHandlePositions(){
	const grid=document.getElementById('dashboard-grid');if(!grid)return;
	const gridRect=grid.getBoundingClientRect();
	const gridMidX=gridRect.left + gridRect.width/2;
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		const leftHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-left');
		const rightHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-right');
		if(!leftHandle||!rightHandle)return;
		const rect=card.getBoundingClientRect();
		const isWide=getPanelWidthUnits(card)===2 || rect.width >= gridRect.width*0.8;
		const column=isWide?(rect.left < gridMidX?1:2):getPanelColumn(card);
		leftHandle.classList.toggle('is-hidden',!isWide && column===1);
		rightHandle.classList.toggle('is-hidden',!isWide && column===2);
	});
}
function setPatrolPanelWidthUnits(card,width,save,column){
	if(!card)return;
	const currentRows=getPanelRows(card);
	DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
	if(width===2){
		card.classList.remove('panel-col-1','panel-col-2');
		card.classList.add('panel-size-2x1');
	}else{
		card.classList.add('panel-size-1x1');
		setPanelColumn(card,column===2?2:1);
	}
	setPanelRows(card,currentRows,false);
	updatePatrolResizeHandlePositions();
	if(save)savePatrolLayout();
}
function updatePatrolResizeHandlePositions(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	const gridRect=grid.getBoundingClientRect();
	const gridMidX=gridRect.left + gridRect.width/2;
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		const leftHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-left');
		const rightHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-right');
		if(!leftHandle||!rightHandle)return;
		const rect=card.getBoundingClientRect();
		const isWide=getPanelWidthUnits(card)===2 || rect.width >= gridRect.width*0.8;
		const column=isWide?(rect.left < gridMidX?1:2):getPanelColumn(card);
		leftHandle.classList.toggle('is-hidden',!isWide && column===1);
		rightHandle.classList.toggle('is-hidden',!isWide && column===2);
	});
}
function applyPanelSizeInternal(panelId,size,save){
  const card=document.getElementById(panelId);if(!card)return;
  DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
  card.classList.add('panel-size-'+size);
	setPanelRows(card,getDefaultPanelRows(size),false);
  if(save)saveDashboardLayout();
}
function applyPanelSize(panelId,size){applyPanelSizeInternal(panelId,size,true);}
function initDashboardResizeHandles(){
	const grid=document.getElementById('dashboard-grid');if(!grid)return;
	let activeResize=null;
	function stopResize(){
		if(!activeResize)return;
		activeResize.card.classList.remove('resizing-x','resizing-y');
		document.body.style.userSelect='';
		updateDashboardResizeHandlePositions();
		saveDashboardLayout();
		activeResize=null;
	}
	function onPointerMove(e){
		if(!activeResize)return;
		if(activeResize.type==='height'){
			const deltaY=e.clientY-activeResize.startY;
			const targetHeight=Math.max(120,activeResize.startHeight+deltaY);
			setPanelRows(activeResize.card,pixelsToPanelRows(targetHeight),false);
			return;
		}
		const deltaX=e.clientX-activeResize.startX;
		const startUnits=activeResize.startUnits;
		if(activeResize.side==='right'){
			if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?2:1,false,1);
			else setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?1:2,false,1);
			return;
		}
		if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?2:1,false,2);
		else setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?1:2,false,2);
	}
	function onPointerUp(){
		stopResize();
	}
	document.addEventListener('pointermove',onPointerMove);
	document.addEventListener('pointerup',onPointerUp);
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		if(card.querySelector(':scope > .panel-resize-handle-y'))return;
		const heightHandle=document.createElement('div');
		heightHandle.className='panel-resize-handle-y';
		heightHandle.title='上下にドラッグして高さ変更';
		heightHandle.addEventListener('pointerdown',e=>{
			if(!layoutEditMode)return;
			e.preventDefault();
			e.stopPropagation();
			activeResize={type:'height',card,startY:e.clientY,startHeight:panelRowsToPixels(getPanelRows(card))};
			card.classList.add('resizing-y');
			document.body.style.userSelect='none';
		});
		function createWidthHandle(side){
			const handle=document.createElement('div');
			handle.className='panel-resize-handle-x handle-'+side;
			handle.title='左右にドラッグして横幅変更';
			handle.addEventListener('pointerdown',e=>{
				if(!layoutEditMode)return;
				e.preventDefault();
				e.stopPropagation();
				const gridRect=grid.getBoundingClientRect();
				const columnWidth=(gridRect.width-DASH_GRID_GAP)/2;
				activeResize={type:'width',side:side,card,startX:e.clientX,startUnits:getPanelWidthUnits(card),threshold:Math.max(24,columnWidth*0.22)};
				card.classList.add('resizing-x');
				document.body.style.userSelect='none';
			});
			return handle;
		}
		const leftWidthHandle=createWidthHandle('left');
		const rightWidthHandle=createWidthHandle('right');
		card.appendChild(heightHandle);
		card.appendChild(leftWidthHandle);
		card.appendChild(rightWidthHandle);
	});
	updateDashboardResizeHandlePositions();
	window.addEventListener('resize',updateDashboardResizeHandlePositions);
}
function initPatrolResizeHandles(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	let activeResize=null;
	function stopResize(){
		if(!activeResize)return;
		activeResize.card.classList.remove('resizing-x','resizing-y');
		document.body.style.userSelect='';
		updatePatrolResizeHandlePositions();
		savePatrolLayout();
		activeResize=null;
	}
	function onPointerMove(e){
		if(!activeResize)return;
		if(activeResize.type==='height'){
			const deltaY=e.clientY-activeResize.startY;
			const targetHeight=Math.max(120,activeResize.startHeight+deltaY);
			setPanelRows(activeResize.card,pixelsToPanelRows(targetHeight),false);
			return;
		}
		const deltaX=e.clientX-activeResize.startX;
		const startUnits=activeResize.startUnits;
		if(activeResize.side==='right'){
			if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?2:1,false,1);
			else setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?1:2,false,1);
			return;
		}
		if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?2:1,false,2);
		else setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?1:2,false,2);
	}
	function onPointerUp(){
		stopResize();
	}
	document.addEventListener('pointermove',onPointerMove);
	document.addEventListener('pointerup',onPointerUp);
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		if(card.querySelector(':scope > .panel-resize-handle-y'))return;
		const heightHandle=document.createElement('div');
		heightHandle.className='panel-resize-handle-y';
		heightHandle.title='上下にドラッグして高さ変更';
		heightHandle.addEventListener('pointerdown',e=>{
			if(!layoutEditMode||currentViewId!=='patrol')return;
			e.preventDefault();
			e.stopPropagation();
			activeResize={type:'height',card,startY:e.clientY,startHeight:panelRowsToPixels(getPanelRows(card))};
			card.classList.add('resizing-y');
			document.body.style.userSelect='none';
		});
		function createWidthHandle(side){
			const handle=document.createElement('div');
			handle.className='panel-resize-handle-x handle-'+side;
			handle.title='左右にドラッグして横幅変更';
			handle.addEventListener('pointerdown',e=>{
				if(!layoutEditMode||currentViewId!=='patrol')return;
				e.preventDefault();
				e.stopPropagation();
				const gridRect=grid.getBoundingClientRect();
				const columnWidth=(gridRect.width-DASH_GRID_GAP)/2;
				activeResize={type:'width',side:side,card,startX:e.clientX,startUnits:getPanelWidthUnits(card),threshold:Math.max(24,columnWidth*0.22)};
				card.classList.add('resizing-x');
				document.body.style.userSelect='none';
			});
			return handle;
		}
		card.appendChild(heightHandle);
		card.appendChild(createWidthHandle('left'));
		card.appendChild(createWidthHandle('right'));
	});
	updatePatrolResizeHandlePositions();
	window.addEventListener('resize',updatePatrolResizeHandlePositions);
}
function saveChatLayout(){
	const container=document.getElementById('chat-split-container');
	const logCard=document.getElementById('card-chat-log');
	const data={};
	if(container&&logCard){data.reportOnTop=container.firstElementChild!==logCard;}
	localStorage.setItem('chatLayout',JSON.stringify(data));
}
function loadChatLayout(){
	try{
		const saved=JSON.parse(localStorage.getItem('chatLayout')||'{}');
		if(saved.reportOnTop)swapChatPanels();
	}catch(e){}
}
function swapChatPanels(){
	const container=document.getElementById('chat-split-container');
	const logCard=document.getElementById('card-chat-log');
	const reportCard=document.getElementById('card-chat-report-col');
	const handle=document.getElementById('chat-swap-handle');
	if(!container||!logCard||!reportCard||!handle)return;
	if(container.firstElementChild===logCard){
		container.appendChild(handle);
		container.appendChild(logCard);
	} else {
		container.appendChild(handle);
		container.appendChild(reportCard);
	}
	saveChatLayout();
}
function initChatLayoutEdit(){}
function saveDashboardLayout(){
  const grid=document.getElementById('dashboard-grid');if(!grid)return;
  const order=[...grid.querySelectorAll(':scope > .card')].map(c=>c.id);
  const sizes={};
	const heights={};
	const columns={};
  DASH_PANEL_IDS.forEach(id=>{
    const card=document.getElementById(id);if(!card)return;
    const sc=DASH_SIZE_CLASSES.find(c=>card.classList.contains(c));
    sizes[id]=sc?sc.replace('panel-size-',''):'1x1';
		heights[id]=getPanelRows(card);
		columns[id]=getPanelColumn(card);
  });
	localStorage.setItem('dashLayout',JSON.stringify({order,sizes,heights,columns}));
}
function loadDashboardLayout(){
  const grid=document.getElementById('dashboard-grid');if(!grid)return;
  try{
    const saved=JSON.parse(localStorage.getItem('dashLayout')||'{}');
    if(saved.sizes){Object.entries(saved.sizes).forEach(([id,size])=>applyPanelSizeInternal(id,size,false));}
		if(saved.columns){Object.entries(saved.columns).forEach(([id,column])=>{const card=document.getElementById(id);if(card&&getPanelWidthUnits(card)===1)setPanelColumn(card,parseInt(column,10)===2?2:1);});}
		if(saved.heights){Object.entries(saved.heights).forEach(([id,rows])=>{const card=document.getElementById(id);if(card)setPanelRows(card,parseInt(rows,10)||getPanelRows(card),false);});}
    if(saved.order&&saved.order.length){
      saved.order.forEach(id=>{const el=document.getElementById(id);if(el&&el.parentNode===grid)grid.appendChild(el);});
    }
		updateDashboardResizeHandlePositions();
  }catch(e){}
}
function savePatrolLayout(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	const order=[...grid.querySelectorAll(':scope > .card')].map(c=>c.id);
	const sizes={};
	const heights={};
	const columns={};
	PATROL_LAYOUT_CARD_IDS.forEach(id=>{
		const card=document.getElementById(id);if(!card)return;
		const sc=DASH_SIZE_CLASSES.find(c=>card.classList.contains(c));
		sizes[id]=sc?sc.replace('panel-size-',''):'1x1';
		heights[id]=getPanelRows(card);
		columns[id]=getPanelColumn(card);
	});
	localStorage.setItem('patrolLayout',JSON.stringify({order,sizes,heights,columns}));
}
function loadPatrolLayout(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	try{
		const saved=JSON.parse(localStorage.getItem('patrolLayout')||'{}');
		if(saved.sizes){Object.entries(saved.sizes).forEach(([id,size])=>{const card=document.getElementById(id);if(!card)return;DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));card.classList.add('panel-size-'+size);});}
		if(saved.columns){Object.entries(saved.columns).forEach(([id,column])=>{const card=document.getElementById(id);if(card&&getPanelWidthUnits(card)===1)setPanelColumn(card,parseInt(column,10)===2?2:1);});}
		if(saved.heights){Object.entries(saved.heights).forEach(([id,rows])=>{const card=document.getElementById(id);if(card)setPanelRows(card,parseInt(rows,10)||getPanelRows(card),false);});}
		if(saved.order&&saved.order.length){
			saved.order.forEach(id=>{const el=document.getElementById(id);if(el&&el.parentNode===grid)grid.appendChild(el);});
		}
		updatePatrolResizeHandlePositions();
	}catch(e){}
}
function toggleLayoutEdit(){
	if(!isLayoutEditableView(currentViewId))return;
  layoutEditMode=!layoutEditMode;
	syncLayoutEditState();
}
// ── Init ──
buildFilterBar();
applyReversedUI();
applyLoopUI();
loadPatrolChannels();
pollPatrolStatus();
loadConfig();
loadGoldHistory();
setInterval(loadGoldHistory,30000);
initChat();
initPanelDragAndCollapse();
initGridDragDrop();
initPatrolGridDragDrop();
initDashboardResizeHandles();
initPatrolResizeHandles();
loadDashboardLayout();
loadPatrolLayout();
initChatLayoutEdit();
loadChatLayout();
syncLayoutEditState();
(async function startupDeviceCheck(){
  async function fetchDevicesOnly(){
    const r=await fetch('/api/devices');const res=await r.json();
    const devs=Array.isArray(res)?res:(res.devices||[]);
    const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
    const deviceMap={};if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.serial)deviceMap[e.serial]=e;if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
    if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
    renderDashDevices(devs,deviceMap);
    currentDevices=devs||[];currentDeviceMap=deviceMap;
    renderDeviceList();
    return devs&&devs.length>0;
  }
  for(let i=1;i<=3;i++){const found=await fetchDevicesOnly();if(found)return;if(i<3)await new Promise(r=>setTimeout(r,3000));}
})();

// ── Appearance: theme + font size ──
(function(){
  function setTheme(t,el){
    document.body.setAttribute('data-theme',t);
    localStorage.setItem('uiTheme',t);
    document.querySelectorAll('.theme-swatch').forEach(s=>s.classList.toggle('active',s.dataset.t===t));
  }
  function setFontSize(fs,el){
    document.body.setAttribute('data-fs',fs);
    localStorage.setItem('uiFontSize',fs);
    document.querySelectorAll('.fs-btn').forEach(b=>b.classList.toggle('active',b.dataset.fs===fs));
  }
  window.setTheme=setTheme;
  window.setFontSize=setFontSize;
  // restore persisted prefs
  const savedTheme=localStorage.getItem('uiTheme')||'dark-blue'; localStorage.getItem('uiTheme')==='grey'&&localStorage.setItem('uiTheme','light');

  // Migrate old font size keys
  (function(){const m={'sm':'s','md':'m','lg':'m'};const v=localStorage.getItem('uiFontSize');if(v&&m[v])localStorage.setItem('uiFontSize',m[v]);})();
  const savedFs=localStorage.getItem('uiFontSize')||'xl';
  document.body.setAttribute('data-theme',savedTheme);
  document.body.setAttribute('data-fs',savedFs);
  document.querySelectorAll('.theme-swatch').forEach(s=>s.classList.toggle('active',s.dataset.t===savedTheme));
  document.querySelectorAll('.fs-btn').forEach(b=>b.classList.toggle('active',b.dataset.fs===savedFs));

  // brand status sync
  function syncBrandStatus(){
    const bar=document.getElementById('hdr-bar');
    const dot=document.getElementById('brand-dot');
    const txt=document.getElementById('brand-status-text');
    const upt=document.getElementById('brand-uptime-small');
    const running=bar&&bar.classList.contains('running');
    if(dot)dot.classList.toggle('running',running);
    if(txt){
      const psState=document.getElementById('ps-state')||document.getElementById('dash-ps-state');
      if(running&&psState)txt.textContent='巡回中';
      else if(running)txt.textContent='稼働中';
      else txt.textContent='待機中';
    }
    const uptimeEl=document.getElementById('dash-uptime');
    if(upt&&uptimeEl)upt.textContent=uptimeEl.textContent||'';
  }
  setInterval(syncBrandStatus,1000);
})();
</script>

<!-- PortMap 変更確認モーダル -->
<div id="pm-overlay" class="pm-overlay" style="display:none">
  <div class="pm-modal">
    <div class="pm-modal-title">⚠ PortMap 変更の確認</div>
    <div id="pm-body" class="pm-body"></div>
    <div class="pm-btns">
      <button class="btn danger" onclick="pmRejectAll()">すべて却下</button>
      <button class="btn success" onclick="pmApplyAll()">すべて適用</button>
    </div>
  </div>
</div>
</body>
</html>`

// spawnLogHTML は出現ログ専用の分離ウィンドウ用ページ
const spawnLogHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>出現ログ - BPSR Patrol Cams</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
:root{--bg0:#0f1117;--bg1:#161b27;--bg2:#1e2535;--bg3:#252d40;--accent:#4f8ef7;--warn:#f5a623;--text1:#e8eaf0;--text2:#9aa3b8;--text3:#5c6680;--border:#2a3348;--radius:8px;}
body{background:var(--bg0);color:var(--text1);font-family:'Segoe UI',sans-serif;font-size:13px;display:flex;flex-direction:column;height:100vh}
h1{font-size:13px;padding:0 16px;height:44px;background:var(--bg1);border-bottom:1px solid var(--border);display:flex;align-items:center;gap:8px;flex-shrink:0;font-weight:500}
#container{flex:1;overflow-y:auto;padding:8px}
table{width:100%;border-collapse:collapse;font-size:0.85em}
th{color:var(--warn);text-align:left;padding:4px 10px;border-bottom:1px solid rgba(245,166,35,.2);position:sticky;top:0;background:var(--bg1);z-index:1;font-weight:500}
td{padding:4px 10px;border-bottom:1px solid var(--border);line-height:1.4;white-space:nowrap}
tr:hover td{background:var(--bg2)}
.ch{color:var(--accent);font-family:monospace;font-weight:bold}
.time{color:var(--text3);margin-right:6px}
.monster{color:var(--warn);font-weight:bold}
.monster.silver{color:#b8b8b8}
.no-data{color:var(--text3);padding:12px}
::-webkit-scrollbar{width:5px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{background:var(--bg3);border-radius:3px}
</style>
</head>
<body>
<h1>🌟 出現ログ</h1>
<div id="container"><div class="no-data">読み込み中...</div></div>
<script>
function eH(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
async function load(){
  const h=await fetch('/api/gold-history').then(r=>r.json()).catch(()=>[]);
  const c=document.getElementById('container');
  if(!h||h.length===0){c.innerHTML='<div class="no-data">出現履歴なし</div>';return;}
  c.innerHTML='<table><thead><tr><th>時刻</th><th>名前</th><th>Ch</th><th>場所</th></tr></thead><tbody>'
    +h.map(e=>{const nm=e.monster_name||'ウリボ・ゴールド';const cls=nm.includes('銀ナッポ')?'monster silver':'monster';return '<tr><td class="time">'+eH(e.time||'')+'</td><td class="'+cls+'">'+eH(nm)+'</td><td class="ch">Ch'+Number(e.channel)+'</td><td>'+eH(e.location||'')+'</td></tr>'}).join('')
    +'</tbody></table>';
}
load();
setInterval(load,5000);
const es=new EventSource('/events');
es.onmessage=e=>{if(e.data.includes('[DETECTION]')||e.data.includes('[GUI] 金ウリボ'))load();};
</script>
</body>
</html>`

// chatLogHTML はチャットログ専用の分離ウィンドウ用ページ
const chatLogHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>チャットログ - BPSR Patrol Cams</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
:root{--bg0:#0f1117;--bg1:#161b27;--bg2:#1e2535;--bg3:#252d40;--accent:#4f8ef7;--warn:#f5a623;--text1:#e8eaf0;--text2:#9aa3b8;--text3:#5c6680;--border:#2a3348;}
body{background:var(--bg0);color:var(--text1);font-family:'Segoe UI',sans-serif;font-size:13px;display:flex;flex-direction:column;height:100vh}
h1{font-size:13px;padding:0 16px;height:44px;background:var(--bg1);border-bottom:1px solid var(--border);display:flex;align-items:center;gap:8px;flex-shrink:0;font-weight:500}
#toolbar{padding:5px 12px;background:var(--bg1);border-bottom:1px solid var(--border);display:flex;align-items:center;gap:10px;flex-shrink:0;font-size:0.85em}
#toolbar input[type=text]{background:var(--bg2);border:1px solid var(--border);color:var(--text1);padding:3px 8px;border-radius:6px;width:180px;outline:none}
#toolbar input[type=text]:focus{border-color:rgba(79,142,247,.5);box-shadow:0 0 0 2px rgba(79,142,247,.12)}
#container{flex:1;overflow-y:auto;padding:0}
.msg{padding:5px 12px;border-bottom:1px solid var(--border);line-height:1.5;transition:background .15s}
.msg:hover{background:var(--bg2)}
.time{color:var(--text3);margin-right:6px}
.ip{color:var(--text2);font-size:0.85em;margin-right:4px}
.ch{color:var(--accent);font-size:0.85em;margin-right:4px}
.sender{color:var(--warn);font-weight:600;margin-right:4px}
.body{color:var(--text1)}
.no-data{color:var(--text3);padding:12px}
#count{color:var(--text2);margin-left:auto;font-size:0.85em}
::-webkit-scrollbar{width:5px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{background:var(--bg3);border-radius:3px}
</style>
</head>
<body>
<h1>💬 チャットログ</h1>
<div id="toolbar">
  <input type="text" id="search" placeholder="キーワード検索..." oninput="render()">
  <span id="count"></span>
</div>
<div id="container"><div class="no-data">読み込み中...</div></div>
<script>
let all=[],userScrolled=false;
function escH(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
function isAtBottom(el){return el.scrollHeight-el.scrollTop-el.clientHeight<50;}
function render(){
  const q=document.getElementById('search').value.toLowerCase();
  const filtered=q?all.filter(e=>e.sender.toLowerCase().includes(q)||e.message.toLowerCase().includes(q)):all;
  document.getElementById('count').textContent=filtered.length+'件';
  const c=document.getElementById('container');
  if(!filtered.length){c.innerHTML='<div class="no-data">チャットなし</div>';return;}
  c.innerHTML=filtered.map(e=>{
    const ch=e.has_ch?'<span class="ch">Ch'+e.channel+'</span>':'';
    return '<div class="msg"><span class="time">'+e.time+'</span><span class="ip">['+escH(e.client_ip)+']</span>'+ch+'<span class="sender">'+escH(e.sender)+'</span><span class="body">'+escH(e.message)+'</span></div>';
  }).join('');
  userScrolled=false;
  c.scrollTop=c.scrollHeight;
}
function appendOne(ev){
  all.push(ev);
  if(all.length>500)all=all.slice(-500);
  const q=document.getElementById('search').value.toLowerCase();
  if(q&&!(ev.sender.toLowerCase().includes(q)||ev.message.toLowerCase().includes(q)))return;
  const c=document.getElementById('container');
  if(c.querySelector('.no-data'))c.innerHTML='';
  const ch=ev.has_ch?'<span class="ch">Ch'+ev.channel+'</span>':'';
  const div=document.createElement('div');
  div.className='msg';
  div.innerHTML='<span class="time">'+ev.time+'</span><span class="ip">['+escH(ev.client_ip)+']</span>'+ch+'<span class="sender">'+escH(ev.sender)+'</span><span class="body">'+escH(ev.message)+'</span>';
  c.appendChild(div);
  document.getElementById('count').textContent=all.length+'件';
  if(!userScrolled)c.scrollTop=c.scrollHeight;
}
(async function(){
  const c=document.getElementById('container');
  c.addEventListener('scroll',()=>{userScrolled=!isAtBottom(c);});
  const h=await fetch('/api/chat-log').then(r=>r.json()).catch(()=>[]);
  all=h||[];
  render();
  const es=new EventSource('/api/chat-events');
  es.onmessage=e=>{try{appendOne(JSON.parse(e.data));}catch(_){}};
})();
</script>
</body>
</html>`
