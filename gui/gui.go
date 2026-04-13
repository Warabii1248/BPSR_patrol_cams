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
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	webview "github.com/jchv/go-webview2"

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
	patrolChannels     []uint32             // 起動時に設定から読み込んだチャンネルリスト
	patrolChannelsFile string               // channels.txt パス（ホットリロード用）
	getSessions        func() []DeviceSessionInfo
	testDetectFn       func()
	monsterScanFn      func(bool)             // モンスタースキャン有効/無効コールバック
	saveChannelsFn     func([]uint32) error
	channelNotifyFn    func(uint32)           // Patrollerのチャンネル切替時コールバック
	getConfigFn        func() ([]byte, error)
	saveConfigFn       func([]byte) error

	mu              sync.RWMutex
	logLines        []string           // 検知ログ（最大200件）
	clients         []chan string       // SSEクライアント
	goldBoarHistory []GoldBoarEvent    // 金ウリボ検知履歴（最大50件）
	cooldownChs     map[uint32]time.Time // 発見済みch → 除外期限（発見から30分）

	chatMu      sync.RWMutex
	chatLog     []ChatEvent    // チャットログ（最大500件）
	chatClients []chan string  // チャットSSEクライアント
}

// GoldBoarEvent は金ウリボ検知の1件分の記録
type GoldBoarEvent struct {
	Time        string `json:"time"`
	Channel     uint32 `json:"channel"`
	Location    string `json:"location"`
	MonsterName string `json:"monster_name"`
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

// New はGUIサーバーを作成する
func New(port int, mumuCfg mumu.Config, patrolChannels []uint32, patrolChannelsFile string) *Server {
	s := &Server{
		port:               port,
		patroller:          mumu.NewPatroller(mumuCfg),
		patrolChannels:     patrolChannels,
		patrolChannelsFile: patrolChannelsFile,
		cooldownChs:        make(map[uint32]time.Time),
	}
	// Patroller のチャンネル切替を channelNotifyFn に転送する
	s.patroller.SetOnChannelSwitch(func(ch uint32) {
		s.mu.RLock()
		fn := s.channelNotifyFn
		s.mu.RUnlock()
		if fn != nil {
			fn(ch)
		}
	})
	return s
}

// SetSessionProvider はADD ↔ UID 対応に使うセッション情報提供関数を設定する。
func (s *Server) SetSessionProvider(fn func() []DeviceSessionInfo) {
	s.getSessions = fn
}

// SetTestDetectFn はテスト通知ボタンから呼ばれるコールバックを設定する。
func (s *Server) SetTestDetectFn(fn func()) {
	s.testDetectFn = fn
}

// SetMonsterScanFn はGUIからモンスタースキャンを切り替えるコールバックを設定する。
func (s *Server) SetMonsterScanFn(fn func(bool)) {
	s.monsterScanFn = fn
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

// NotifyChMovePacket は ncap が [0x2E] パケットを受信したとき main.go から呼ぶ。
// label は "Instance-N" 形式のインスタンスラベル。巡回中であれば Patroller に転送する。
func (s *Server) NotifyChMovePacket(label string) {
	s.patroller.NotifyChMovePacket(label)
}

// UpdatePatrollerCfg は Patroller の設定をリアルタイムで更新する。
func (s *Server) UpdatePatrollerCfg(cfg mumu.Config) {
	s.patroller.UpdateConfig(cfg)
}

// OnDetect は検知イベントをGUIのログに追加するコールバック
func (s *Server) OnDetect(det notifier.Detection) {
	line := fmt.Sprintf("[%s] %s", det.Time.Format("15:04:05"), notifier.Format(det))
	detCh := det.LineID // 検知されたチャンネル番号

	s.mu.Lock()
	// 通常ログに追記
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	// 金ウリボ履歴に追記（最大50件）
	monName := det.MonsterName
	if monName == "" {
		monName = "ゴールドウリボ"
	}
	loc := det.Location
	if loc == "" {
		loc = "不明"
	}
	event := GoldBoarEvent{
		Time:        det.Time.Format("01/02 15:04:05"),
		Channel:     detCh,
		Location:    loc,
		MonsterName: monName,
	}
	s.goldBoarHistory = append(s.goldBoarHistory, event)
	if len(s.goldBoarHistory) > 50 {
		s.goldBoarHistory = s.goldBoarHistory[len(s.goldBoarHistory)-50:]
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
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
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
	for _, ch := range clients {
		select {
		case ch <- line:
		default:
		}
	}
}

// AddLog は1行のログをGUIのSSEストリームとlogLinesに追加する
func (s *Server) AddLog(line string) {
	s.mu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	s.mu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- line:
		default:
		}
	}
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
	clients := make([]chan string, len(s.chatClients))
	copy(clients, s.chatClients)
	s.chatMu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- jsonStr:
		default:
		}
	}
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
			if strings.Contains(line, "[0x2E] UUID=") {
				w.srv.patroller.NotifyChMovePacket(extractInstanceLabel(line))
			}
		}
	}
	return n, err
}

// extractInstanceLabel は "[Instance-7][0x2E] UUID=..." 形式のログ行から
// インスタンスラベル（"Instance-7"）を抽出する。
func extractInstanceLabel(line string) string {
	end := strings.Index(line, "][0x2E]")
	if end <= 0 {
		return ""
	}
	start := strings.LastIndex(line[:end], "[")
	if start < 0 {
		return ""
	}
	return line[start+1 : end]
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
	mux.HandleFunc("/api/test-detect", s.handleTestDetect)
	mux.HandleFunc("/api/monster-scan", s.handleMonsterScan)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/gold-history", s.handleGoldHistory)
	mux.HandleFunc("/api/adb/restart", s.handleADBRestart)
	mux.HandleFunc("/api/chat-log", s.handleChatLog)
	mux.HandleFunc("/api/chat-events", s.handleChatEvents)
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

	// 起動時にデバイス一覧を最大3回取得し、全て失敗したら ADB を自動再起動する
	go func() {
		cfg := s.patroller.Config()
		for attempt := 1; attempt <= 3; attempt++ {
			time.Sleep(1 * time.Second)
			log.Printf("[MuMu] 起動時デバイス確認 (%d/3)...", attempt)
			devices, err := mumu.ListDevices(cfg)
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
		// 3回全て失敗 → ADB サーバーを再起動して再確認
		log.Println("[MuMu] デバイスが見つからないため ADB サーバーを自動再起動します...")
		if err := mumu.RestartServer(cfg); err != nil {
			log.Printf("[MuMu] ADB 再起動失敗: %v", err)
			return
		}
		time.Sleep(1 * time.Second)
		devices, err := mumu.ListDevices(cfg)
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

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	return url, nil
}

// RunWindow はHTTPサーバーを起動しEdge WebView2の専用ウィンドウを開く。
func (s *Server) RunWindow(ctx context.Context) error {
	url, err := s.startHTTP(ctx)
	if err != nil {
		return err
	}
	log.Printf("[GUI] opening window: %s", url)

	w := webview.NewWithOptions(webview.WebViewOptions{
		Debug: false,
		WindowOptions: webview.WindowOptions{
			Title:  "LoyalBoarlet Monitor",
			Width:  1000,
			Height: 720,
			Center: true,
		},
	})
	if w == nil {
		log.Println("[GUI] WebView2 unavailable, falling back to browser")
		openBrowser(url)
		<-ctx.Done()
		return nil
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	return nil
}

// Start はHTTPサーバーをバックグラウンド起動してブラウザで開く
func (s *Server) Start(ctx context.Context) error {
	url, err := s.startHTTP(ctx)
	if err != nil {
		return err
	}
	log.Printf("[GUI] http server: %s", url)
	openBrowser(url)
	<-ctx.Done()
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
	fmt.Fprint(w, indexHTML)
}

// handleDeviceMap はADBデバイスのエミュレータIPを取得し、
// キャプチャセッション（UID等）と紐付けた一覧をJSONで返す。
func (s *Server) handleDeviceMap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	adbDevices, err := mumu.ListDevices(s.patroller.Config())
	if err != nil {
		log.Printf("[MuMu] device-map: ListDevices error: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	ipToSess := make(map[string]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
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
		Confirmed bool   `json:"confirmed"`
	}

	entries := make([]DeviceEntry, len(adbDevices))
	var wg sync.WaitGroup
	for i, serial := range adbDevices {
		entries[i] = DeviceEntry{Serial: serial}
		wg.Add(1)
		go func(idx int, ser string) {
			defer wg.Done()
			ipCh := make(chan string, 1)
			go func() {
				ip, ipErr := mumu.GetDeviceIP(ser, s.patroller.Config())
				if ipErr != nil {
					log.Printf("[MuMu] GetDeviceIP %s: %v", ser, ipErr)
					ip = ""
				}
				ipCh <- ip
			}()
			var devIP string
			select {
			case devIP = <-ipCh:
			case <-ctx.Done():
			}
			entries[idx].DeviceIP = devIP
			if devIP != "" {
				if sess, ok := ipToSess[devIP]; ok {
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"devices": entries})
}

// handleDevices はADB接続デバイス一覧をJSONで返す
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := mumu.ListDevices(s.patroller.Config())
	if err != nil {
		log.Printf("[MuMu] adb devices エラー: %v", err)
	}
	if devices == nil {
		devices = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": devices,
	})
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
		serials, err = mumu.ListDevices(cfg)
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
			mumu.SwitchGroup(serials, start, end, req.Channel, cfg, switchResults, &switchMu)
		}
		for serial, err := range switchResults {
			r := result{Serial: serial, OK: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			results = append(results, r)
		}
	} else {
		// 単体切替
		err := mumu.SwitchChannel(req.Serial, req.Channel, cfg)
		r := result{Serial: req.Serial, OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"results": results,
	})
}

// handleLogs は既存ログ一覧をJSONで返す
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": lines,
	})
}

// handlePatrolChannels はconfig読み込み済みのチャンネルリストを返す（GET）または保存する（POST）
func (s *Server) handlePatrolChannels(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"channels": s.patrolChannels,
	})
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
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
					ADBPath                string  `json:"adb_path"`
					MumuTapX               int     `json:"mumu_tap_x"`
					MumuTapY               int     `json:"mumu_tap_y"`
					MumuClearLength        int     `json:"mumu_clear_length"`
					MumuPreKeycode         string  `json:"mumu_pre_keycode"`
					MumuDelayMs            int     `json:"mumu_delay_ms"`
					ParallelLimit          int     `json:"parallel_limit"`
					ParallelGroupDelaySecs float64 `json:"parallel_group_delay_secs"`
					PatrolMoveTimeoutSecs  float64 `json:"patrol_move_timeout_secs"`
					PatrolMergeTimeoutSecs float64 `json:"patrol_merge_timeout_secs"`
					PatrolDwellSecs        float64 `json:"patrol_dwell_secs"`
				}
				if json.Unmarshal(savedData, &appCfg) == nil {
					newCfg := mumu.Config{
						ADBPath:            appCfg.ADBPath,
						TapX:               appCfg.MumuTapX,
						TapY:               appCfg.MumuTapY,
						ClearLength:        appCfg.MumuClearLength,
						PreKeycode:         appCfg.MumuPreKeycode,
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

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "チャンネルリストが空です"})
		return
	}
	opts := mumu.PatrolOptions{
		Reversed:     req.Reversed,
		LoopMode:     req.LoopMode,
		StartChannel: req.StartChannel,
	}
	s.patroller.Start(req.Serials, channels, s.patrolChannelsFile, opts)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleMonsterScan はモンスタースキャンモードを切り替える
func (s *Server) handleMonsterScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.RLock()
	fn := s.monsterScanFn
	s.mu.RUnlock()
	if fn != nil {
		fn(body.Enabled)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "enabled": body.Enabled})
}

// handleTestDetect はテスト用ゴールドウリボ検知を発火する
func (s *Server) handleTestDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.testDetectFn == nil {
		http.Error(w, "test detect not configured", 503)
		return
	}
	go s.testDetectFn()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handlePatrolClearFull は満員判定リストをクリアする
func (s *Server) handlePatrolClearFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.ClearFullChannels()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handlePatrolStop は巡回を停止する
func (s *Server) handlePatrolStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.Stop()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleADBRestart は ADB サーバーを kill-server → start-server で再起動する。
// 再起動完了を待ってからレスポンスを返すことで、呼び出し元がデバイス取得前に再起動が完了することを保証する。
func (s *Server) handleADBRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := mumu.RestartServer(s.patroller.Config()); err != nil {
		log.Printf("[MuMu] ADB再起動失敗: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleSpawnLog は出現ログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleSpawnLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, spawnLogHTML)
}

// handleChatLog はチャットログ履歴をJSONで返す
func (s *Server) handleChatLog(w http.ResponseWriter, r *http.Request) {
	s.chatMu.RLock()
	events := make([]ChatEvent, len(s.chatLog))
	copy(events, s.chatLog)
	s.chatMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(events)
}

// handleChatEvents はチャットをServer-Sent Eventsでリアルタイム配信する
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	s.chatMu.Lock()
	s.chatClients = append(s.chatClients, ch)
	s.chatMu.Unlock()

	defer func() {
		s.chatMu.Lock()
		for i, c := range s.chatClients {
			if c == ch {
				s.chatClients = append(s.chatClients[:i], s.chatClients[i+1:]...)
				break
			}
		}
		s.chatMu.Unlock()
	}()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// handleChatLogPage はチャットログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleChatLogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, chatLogHTML)
}

// handlePatrolStatus は現在の巡回状態を返す
func (s *Server) handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.patroller.Status())
}

// handleSSE はServer-Sent Eventsで検知ログをリアルタイム配信する
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	s.mu.Lock()
	s.clients = append(s.clients, ch)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		for i, c := range s.clients {
			if c == ch {
				s.clients = append(s.clients[:i], s.clients[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			escaped := strings.ReplaceAll(msg, "\n", "\\n")
			fmt.Fprintf(w, "data: %s\n\n", escaped)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LoyalBoarlet Monitor</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0a0a1a;color:#eaeaea;font-family:'Segoe UI',sans-serif;font-size:14px;height:100vh;width:100vw;overflow:hidden;display:flex;flex-direction:column}
.panel-header,.panel-btn,.panel-resizer,.splitter-h{user-select:none}
h1{font-size:1.1em;padding:6px 12px;background:#0d1b33;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:8px;height:36px;flex-shrink:0}
button{background:#1a3a6a;color:#eaeaea;border:none;padding:6px 14px;border-radius:4px;cursor:pointer;font-size:0.88em;transition:background .15s}
button:hover{background:#2a5aa0}
button:disabled{opacity:.4;cursor:default}
button.green{background:#1b5e20}
button.green:hover{background:#2e7d32}
button.secondary{background:#1a2a3a}
button.secondary:hover{background:#2a3a4a}
button.toggle-btn{padding:5px 12px;font-size:0.85em}
button.toggle-btn.active{background:#1565c0}
button.toggle-btn.active:hover{background:#1976d2}
input[type=text],input[type=number],textarea,select{background:#0f3460;color:#eaeaea;border:1px solid #334466;border-radius:4px;padding:5px 8px;font-size:0.88em}
input[type=checkbox]{accent-color:#e94560;width:16px;height:16px}
.flex-row{display:flex;flex-wrap:wrap;gap:8px;align-items:center}
/* ─── Layout ─── */
#workspace{display:flex;flex-direction:row;flex:1;overflow:hidden;min-height:0;width:100%}
.panel-col{display:flex;flex-direction:column;overflow-y:auto;overflow-x:hidden;min-width:200px;min-height:0}
.splitter-h{width:5px;background:#1a3a6a;cursor:col-resize;flex-shrink:0}
.splitter-h:hover,.splitter-h.active{background:#2a7ae0}
/* ─── Panel ─── */
.panel{display:flex;flex-direction:column;background:#0d1b33;border:1px solid #1a3a6a;border-radius:6px;margin:3px;overflow:hidden;transition:none;flex-shrink:0}
.panel.flexible{flex-shrink:1;min-height:120px}
.panel.minimized{flex:none!important;flex-shrink:0!important}
.panel-header{display:flex;align-items:center;gap:6px;padding:5px 10px;background:#111e38;border-bottom:1px solid #1a3a6a;cursor:grab;flex-shrink:0;height:32px;border-radius:5px 5px 0 0}
.panel-header:active{cursor:grabbing}
.panel.minimized .panel-header{border-bottom:none;border-radius:5px}
.panel-title{font-size:0.85em;font-weight:bold;flex:1;pointer-events:none;white-space:nowrap}
.panel-btn{background:none;border:none;color:#aaa;cursor:pointer;padding:2px 6px;font-size:1em;border-radius:3px;line-height:1}
.panel-btn:hover{background:#2a3a5a;color:#fff}
.panel-body{flex:1;overflow-y:auto;padding:10px;min-height:0;flex-shrink:1}
.panel.minimized .panel-body{display:none!important}
.panel-resizer{height:5px;background:transparent;cursor:row-resize;flex-shrink:0;transition:background .15s}
.panel-resizer:hover,.panel-resizer.active{background:#2a7ae0}
/* Drop indicator */
.drop-indicator{display:none;position:fixed;background:rgba(42,122,224,.3);border:2px dashed #2a7ae0;border-radius:4px;pointer-events:none;z-index:999}
.drop-indicator.visible{display:block}
/* Content */
.device-list{display:flex;flex-direction:column;gap:6px;margin-top:8px}
.device-entry{background:#0d0d1a;border-radius:6px;padding:8px 10px}
.device-entry .serial{color:#7ec8e3;font-family:monospace;font-size:0.85em}
.device-entry .uid{color:#a0a0b0;font-size:0.8em}
.device-entry.matched .uid{color:#ffd700}
.no-devices{color:#606080;font-size:0.85em;padding:4px 0}
.log-area{flex:1;background:#0a0a14;overflow-y:auto;padding:8px;font-family:monospace;font-size:0.8em;min-height:0}
.log-line{color:#b0b0c0;padding:1px 0;white-space:pre-wrap;word-break:break-all}
.log-line.detect{color:#ffd700;font-weight:bold}
#status-bar{color:#4caf50;font-size:0.82em}
.patrol-status{background:#090f20;border-radius:4px;padding:6px 10px;font-size:0.82em;margin-bottom:6px}
.patrol-status span{margin-right:12px}
.patrol-status .running{color:#4caf50;font-weight:bold}
.patrol-status .stopped{color:#888}
.ch-editor{display:flex;flex-direction:column;gap:4px;max-height:150px;overflow-y:auto;margin-bottom:6px}
.ch-row{display:flex;gap:6px;align-items:center;background:#0d0d1a;border-radius:4px;padding:4px 8px}
.ch-row .ch-num{color:#7ec8e3;font-family:monospace;font-size:0.9em;min-width:24px;text-align:right}
.cfg-grid{display:grid;grid-template-columns:1fr 1fr;gap:10px 20px}
.cfg-field{display:flex;flex-direction:column;gap:3px}
.cfg-field label{color:#a0a0b0;font-size:0.82em}
.cfg-field input{width:100%}
.cfg-save-bar{display:flex;gap:8px;align-items:center;margin-top:10px}
.cfg-note{font-size:0.75em;color:#606080;margin-top:4px}
.check-label{display:flex;align-items:center;gap:6px;cursor:pointer;color:#eaeaea}
.section-title{font-size:0.8em;color:#7ec8e3;font-weight:bold;margin:10px 0 4px;border-bottom:1px solid #1a3a6a;padding-bottom:3px}
.gold-table{width:100%;border-collapse:collapse;font-size:0.82em;table-layout:fixed}
.gold-table th{color:#ffd700;text-align:left;padding:3px 8px;border-bottom:1px solid #1a3a6a;white-space:nowrap;position:sticky;top:0;background:#0d1b33;z-index:1}
.gold-table col.col-time{width:90px}
.gold-table col.col-ch{width:46px}
.gold-table col.col-name{width:110px}
.gold-table td{padding:3px 8px;border-bottom:1px solid #0d1530;vertical-align:middle;line-height:1.4;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.gold-table tr:hover td{background:#111e38}
.gold-table .ch-cell{color:#7ec8e3;font-family:monospace;font-weight:bold}
.gold-table .time-cell{color:#a0a0b0;font-size:0.85em}
.gold-table .name-cell{color:#ffd700}
.no-history{color:#606080;font-size:0.85em;padding:8px 0}
#gold-history-container{max-height:110px;overflow-y:auto}
/* ログフィルター */
.log-filter-bar{display:flex;flex-wrap:wrap;gap:4px;padding:5px 8px;border-bottom:1px solid #1a3a6a;flex-shrink:0;background:#090f20}
.log-filter-bar button{padding:2px 8px;font-size:0.75em;border-radius:10px;background:#1a2a3a;color:#a0a0b0;border:1px solid #334466}
.log-filter-bar button.on{background:#1565c0;color:#fff;border-color:#2a7ae0}
/* クラッシュ警告 */
.crash-warning{display:none;background:#4a0000;border:1px solid #e57373;border-radius:4px;padding:5px 10px;font-size:0.8em;color:#ef9a9a;margin-bottom:6px}
</style>
</head>
<body>
<h1>🐗 LoyalBoarlet Monitor</h1>
<div id="workspace">
  <!-- 左カラム -->
  <div class="panel-col" id="col-left" style="flex:1.3">
    <div class="panel" id="panel-devices">
      <div class="panel-header" draggable="true" data-panel="panel-devices">
        <span class="panel-title">📱 デバイス一覧 &amp; 手動切替</span>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-devices')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div class="flex-row">
          <button onclick="refreshDevices()">🔄 再取得</button>
          <label>一括 Ch:</label>
          <input type="number" id="allch" min="1" max="999" value="1" style="width:65px">
          <button onclick="switchAll()">▶ 全切替</button>
          <span id="status-bar"></span>
        </div>
        <div class="device-list" id="device-list"><div class="no-devices">読み込み中...</div></div>
      </div>
      <div class="panel-resizer" data-panel="panel-devices"></div>
    </div>
    <div class="panel flexible" id="panel-patrol" style="flex:2">
      <div class="panel-header" draggable="true" data-panel="panel-patrol">
        <span class="panel-title">🔁 チャンネル巡回</span>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-patrol')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div class="patrol-status">
          <span class="stopped" id="ps-state">■ 停止中</span>
          <span id="ps-ch"></span>
          <span id="ps-prog"></span>
          <span id="ps-parallel"></span>
        </div>
        <div id="crash-warning" class="crash-warning">⚠ ゲームクライアントがch移動できない状態です（クラッシュの可能性）。ADBサーバーを再起動してください。</div>
        <div style="display:flex;align-items:center;gap:8px;min-height:1.2em;margin-bottom:6px">
          <div id="ps-full" style="font-size:0.78em;color:#e57373;flex:1"></div>
          <button id="btn-clear-full" class="secondary" style="font-size:0.75em;padding:2px 8px;display:none" onclick="clearFullChannels()">✕ クリア</button>
        </div>
        <div class="flex-row" style="margin-bottom:8px">
          <label>開始Ch:</label>
          <input type="number" id="patrol-start-ch" min="0" max="9999" value="0" style="width:65px" title="0=前回位置から再開">
          <button class="secondary toggle-btn" id="btn-reversed" onclick="toggleReversed()">⬆ 正順</button>
          <button class="secondary toggle-btn" id="btn-loop" onclick="toggleLoop()">🔁 ループ</button>
        </div>
        <div class="flex-row" style="margin-bottom:8px">
          <button class="green" id="btn-patrol-start" onclick="patrolStart()">▶ 巡回開始</button>
          <button class="secondary" id="btn-patrol-stop" onclick="patrolStop()" disabled>■ 停止</button>
        </div>
        <div class="section-title">巡回チャンネル</div>
        <div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:6px">
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="addChannel()">＋ 追加</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="sortChannels('asc')">↑ 昇順</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="sortChannels('desc')">↓ 降順</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" id="btn-ch-save" onclick="saveChannels()" disabled>💾 保存</button>
          <span id="ch-save-status" style="font-size:0.8em;color:#a0a0b0"></span>
        </div>
        <div class="ch-editor" id="ch-editor"><div class="no-devices">読み込み中...</div></div>
        <div style="display:flex;gap:6px;align-items:center;margin-top:6px">
          <input type="text" id="ch-bulk-input" placeholder="例: 6,13,23,35,41..." style="flex:1;width:auto;font-size:0.85em">
          <button class="secondary" style="padding:3px 10px;font-size:0.8em;white-space:nowrap" onclick="bulkImportChannels()">上書き</button>
        </div>
      </div>
      <div class="panel-resizer" data-panel="panel-patrol"></div>
    </div>
  </div>

  <div class="splitter-h" id="splitter-main"></div>

  <!-- 右カラム -->
  <div class="panel-col" id="col-right" style="flex:1">
    <div class="panel" id="panel-gold">
      <div class="panel-header" draggable="true" data-panel="panel-gold">
        <span class="panel-title">🌟 金ウリボ検知履歴</span>
        <button class="panel-btn" onclick="window.open('/spawn-log','spawn-log','width=600,height=400')" title="ウィンドウ分離">⧉</button>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-gold')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div id="gold-history-container"><div class="no-history">検知履歴なし</div></div>
      </div>
      <div class="panel-resizer" data-panel="panel-gold"></div>
    </div>
    <div class="panel flexible" id="panel-chat" style="flex:1.5">
      <div class="panel-header" draggable="true" data-panel="panel-chat">
        <span class="panel-title">💬 チャットログ</span>
        <button class="panel-btn" onclick="window.open('/chat-log','chat-log','width=700,height=500')" title="別ウィンドウ">⧉</button>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-chat')" title="最小化">─</button>
      </div>
      <div class="panel-body" style="padding:0;display:flex;flex-direction:column">
        <div style="padding:4px 8px;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:8px;flex-shrink:0;font-size:0.82em">
          <select id="chat-device-select" onchange="renderChatPanel()" style="background:#0d1b33;border:1px solid #1a3a6a;color:#eaeafa;padding:2px 4px;border-radius:3px;font-size:0.9em;max-width:140px">
            <option value="">すべて</option>
          </select>
          <input type="text" id="chat-search" placeholder="検索..." style="background:#0d1b33;border:1px solid #1a3a6a;color:#eaeafa;padding:2px 5px;border-radius:3px;width:90px;font-size:0.9em" oninput="renderChatPanel()">
          <button class="secondary" style="padding:2px 7px;font-size:0.85em;margin-left:auto" onclick="clearChatPanel()">クリア</button>
        </div>
        <div class="log-area" id="chat-area" style="flex:1"></div>
      </div>
      <div class="panel-resizer" data-panel="panel-chat"></div>
    </div>
    <div class="panel flexible" id="panel-log" style="flex:2">
      <div class="panel-header" draggable="true" data-panel="panel-log">
        <span class="panel-title">📋 検知ログ</span>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-log')" title="最小化">─</button>
      </div>
      <div class="panel-body" style="padding:0;display:flex;flex-direction:column">
        <div class="log-filter-bar" id="log-filter-bar"></div>
        <div class="log-area" id="log-area"></div>
        <div style="padding:6px 10px;border-top:1px solid #1a3a6a;display:flex;gap:8px;flex-shrink:0">
          <button class="secondary" style="font-size:0.8em;padding:3px 10px" onclick="document.getElementById('log-area').innerHTML=''">クリア</button>
          <button style="font-size:0.8em;padding:3px 10px" onclick="testDetect()">🔔 テスト通知</button>
          <button class="secondary toggle-btn" id="btn-monster-scan" style="font-size:0.8em;padding:3px 10px" onclick="toggleMonsterScan()" title="出現した全モンスターのtmplID・名前・座標をログに出力します">🔍 怪物スキャン</button>
        </div>
      </div>
      <div class="panel-resizer" data-panel="panel-log"></div>
    </div>
    <div class="panel" id="panel-config">
      <div class="panel-header" draggable="true" data-panel="panel-config">
        <span class="panel-title">⚙ 設定</span>
        <button class="panel-btn panel-min-btn" onclick="minimizePanel('panel-config')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div id="cfg-form" class="cfg-grid"></div>
        <div class="cfg-save-bar">
          <button onclick="saveConfig()">💾 保存</button>
          <span id="cfg-status" style="font-size:0.82em;color:#a0a0b0"></span>
        </div>
        <p class="cfg-note">* 滞在時間・タイムアウト・並列設定は保存後すぐ反映されます。その他は再起動が必要です。</p>
      </div>
      <div class="panel-resizer" data-panel="panel-config"></div>
    </div>
  </div>
</div>
<div class="drop-indicator" id="drop-indicator"></div>

<script>
// ── Minimize ──
function minimizePanel(id) {
  const p = document.getElementById(id);
  const btn = p.querySelector('.panel-min-btn');
  if (p.classList.toggle('minimized')) { btn.textContent='＋'; btn.title='展開'; }
  else { btn.textContent='─'; btn.title='最小化'; }
  saveLayout();
}

// ── Layout persistence ──
const LAYOUT_KEY = 'loyalboarlet_layout';
function saveLayout() {
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  const totalW = L.getBoundingClientRect().width + R.getBoundingClientRect().width;
  const leftRatio = totalW > 0 ? L.getBoundingClientRect().width / totalW : 0.57;
  const panelState = p => ({
    id: p.id,
    minimized: p.classList.contains('minimized'),
    height: p.style.height || null
  });
  const layout = {
    leftRatio,
    left:  [...L.querySelectorAll('.panel')].map(panelState),
    right: [...R.querySelectorAll('.panel')].map(panelState),
  };
  try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)); } catch(_){}
}
function restoreLayout() {
  let layout;
  try { layout = JSON.parse(localStorage.getItem(LAYOUT_KEY)||'null'); } catch(_){}
  if (!layout) return;
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  if (layout.leftRatio) {
    L.style.flex = layout.leftRatio + ' 1 0';
    R.style.flex = (1 - layout.leftRatio) + ' 1 0';
  }
  [[L, layout.left],[R, layout.right]].forEach(([col, entries])=>{
    if (!entries) return;
    entries.forEach(({id, minimized, height})=>{
      const panel = document.getElementById(id);
      if (!panel) return;
      col.appendChild(panel);
      const btn = panel.querySelector('.panel-min-btn');
      if (minimized) {
        panel.classList.add('minimized');
        if (btn) { btn.textContent='＋'; btn.title='展開'; }
      } else {
        panel.classList.remove('minimized');
        if (btn) { btn.textContent='─'; btn.title='最小化'; }
        if (height) { panel.style.flex='none'; panel.style.height=height; }
      }
    });
  });
}

// ── Splitter ──
(function(){
  const sp = document.getElementById('splitter-main');
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  let drag=false,sx=0,slw=0,srw=0;
  sp.addEventListener('mousedown', e=>{
    drag=true; sx=e.clientX;
    slw=L.getBoundingClientRect().width;
    srw=R.getBoundingClientRect().width;
    sp.classList.add('active');
    document.body.style.cursor='col-resize';
    e.preventDefault();
  });
  document.addEventListener('mousemove', e=>{
    if(!drag) return;
    const dx=e.clientX-sx, total=slw+srw;
    const nl=Math.max(180,Math.min(total-180,slw+dx));
    const nr=total-nl;
    const ratio=nl/total;
    L.style.flex=ratio+' 1 0';
    R.style.flex=(1-ratio)+' 1 0';
    L.style.width=''; R.style.width='';
  });
  document.addEventListener('mouseup', ()=>{
    if(!drag) return;
    drag=false; sp.classList.remove('active');
    document.body.style.cursor='';
    saveLayout();
  });
})();

// ── Panel vertical resize ──
(function(){
  let dragging=null, startY=0, startH=0;
  document.addEventListener('mousedown', e=>{
    const rz=e.target.closest('.panel-resizer');
    if(!rz) return;
    const panel=rz.closest('.panel');
    if(!panel||panel.classList.contains('minimized')) return;
    dragging=panel;
    startY=e.clientY;
    startH=panel.getBoundingClientRect().height;
    rz.classList.add('active');
    document.body.style.cursor='row-resize';
    // flex固定にして高さをpxで管理
    panel.style.flex='none';
    panel.style.height=startH+'px';
    e.preventDefault();
  });
  document.addEventListener('mousemove', e=>{
    if(!dragging) return;
    const dy=e.clientY-startY;
    const newH=Math.max(60, startH+dy);
    dragging.style.height=newH+'px';
  });
  document.addEventListener('mouseup', e=>{
    if(!dragging) return;
    dragging.querySelector('.panel-resizer').classList.remove('active');
    document.body.style.cursor='';
    saveLayout();
    dragging=null;
  });
})();
let dragPanel=null;
const dropInd=document.getElementById('drop-indicator');
document.querySelectorAll('.panel-header[draggable]').forEach(h=>{
  h.addEventListener('dragstart', e=>{
    dragPanel=document.getElementById(h.dataset.panel);
    e.dataTransfer.effectAllowed='move';
    e.dataTransfer.setData('text/plain',h.dataset.panel);
    setTimeout(()=>{ if(dragPanel) dragPanel.style.opacity='0.4'; },0);
  });
  h.addEventListener('dragend', ()=>{
    if(dragPanel) dragPanel.style.opacity='';
    dragPanel=null;
    dropInd.classList.remove('visible');
    saveLayout();
  });
});
document.querySelectorAll('.panel').forEach(p=>{
  p.addEventListener('dragover', e=>{
    if(!dragPanel||p===dragPanel) return;
    e.preventDefault();
    const r=p.getBoundingClientRect();
    const before=e.clientY<r.top+r.height/2;
    dropInd.style.left=r.left+'px'; dropInd.style.width=r.width+'px';
    dropInd.style.height='3px'; dropInd.style.top=((before?r.top:r.bottom)-2)+'px';
    dropInd.classList.add('visible');
    p._dropBefore=before;
  });
  p.addEventListener('dragleave',()=>dropInd.classList.remove('visible'));
  p.addEventListener('drop', e=>{
    e.preventDefault();
    dropInd.classList.remove('visible');
    if(!dragPanel||p===dragPanel) return;
    p.parentNode.insertBefore(dragPanel, p._dropBefore?p:p.nextSibling);
  });
});
['col-left','col-right'].forEach(id=>{
  const col=document.getElementById(id);
  col.addEventListener('dragover', e=>{
    if(!dragPanel||dragPanel.closest('.panel-col')===col) return;
    e.preventDefault();
    const r=col.getBoundingClientRect();
    dropInd.style.left=r.left+'px'; dropInd.style.width=r.width+'px';
    dropInd.style.height=r.height+'px'; dropInd.style.top=r.top+'px';
    dropInd.classList.add('visible');
  });
  col.addEventListener('drop', e=>{
    e.preventDefault();
    dropInd.classList.remove('visible');
    if(!dragPanel||dragPanel.closest('.panel-col')===col) return;
    col.appendChild(dragPanel);
  });
});

// ── Devices ──
let selectedDevices=new Set();
function selectedSerials(){ return [...selectedDevices]; }
async function refreshDevices(){
  const bar=document.getElementById('status-bar');
  bar.textContent='ADB再起動中...';
  await fetch('/api/adb/restart',{method:'POST'});
  bar.textContent='デバイス取得中...';
  const r=await fetch('/api/devices');
  const res=await r.json();
  const devs=Array.isArray(res)?res:(res.devices||[]);
  const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
  const el=document.getElementById('device-list');
  bar.textContent='';
  if(!devs||devs.length===0){ el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>'; return; }
  el.innerHTML=devs.map(d=>{
    const info=mapRes[d]||{};
    const uid=info.user_uid||'', ch=info.line_id||'', confirmed=info.confirmed||false;
    const checked=selectedDevices.has(d)?'checked':'';
    const uidHtml=uid?('<span class="uid">'+(confirmed?'🔗':'')+' UID:'+uid+(ch?' Ch'+ch:'')+'</span>'):'';
    const eid='ch-'+encodeURIComponent(d);
    return '<div class="device-entry'+(confirmed?' matched':'')+'">'
      +'<label class="check-label">'
      +'<input type="checkbox" '+checked+' onchange="toggleDevice('+escAttrJs(d)+',this.checked)">'
      +'<span class="serial">'+escHtml(d)+'</span>'+uidHtml
      +'</label>'
      +'<div style="display:flex;gap:6px;margin-top:4px">'
      +'<input type="number" id="'+escHtml(eid)+'" min="1" max="999" value="1" style="width:65px">'
      +'<button style="padding:3px 8px;font-size:0.8em" onclick="switchOne('+escAttrJs(d)+')">切替</button>'
      +'</div></div>';
  }).join('');
}
function toggleDevice(s,c){ c?selectedDevices.add(s):selectedDevices.delete(s); }

// ── Chat Panel ──
let chatEvents=[];
let chatIPToSerial={};
let chatKnownSerials=[];
function escHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function escAttrJs(s){return JSON.stringify(String(s)).replace(/"/g,'&quot;');}
function chatMsgHtml(ev){
  const serial=chatIPToSerial[ev.client_ip]||ev.client_ip;
  const ch=ev.has_ch?'<span style="color:#7ec8e3;margin-right:3px;font-size:0.88em">Ch'+ev.channel+'</span>':'';
  return '<div style="padding:3px 8px;border-bottom:1px solid #0d1530;font-size:0.82em;line-height:1.5">'
    +'<span style="color:#606080;margin-right:5px">'+ev.time+'</span>'
    +'<span style="color:#7a6030;font-size:0.88em;margin-right:3px">['+escHtml(serial)+']</span>'
    +ch
    +'<span style="color:#ffd700;font-weight:bold;margin-right:3px">'+escHtml(ev.sender)+'</span>'
    +'<span style="color:#d0d0e0">'+escHtml(ev.message)+'</span>'
    +'</div>';
}
function refreshChatDeviceDropdown(){
  const sel=document.getElementById('chat-device-select');
  if(!sel)return;
  const current=sel.value;
  const serials=chatKnownSerials.length?[...chatKnownSerials]:[...new Set(Object.values(chatIPToSerial))].sort();
  sel.innerHTML='<option value="">すべて</option>'
    +serials.map(s=>'<option value="'+escHtml(s)+'">'+escHtml(s)+'</option>').join('');
  if(serials.includes(current))sel.value=current;
}
function chatMatchFilter(ev){
  const sel=document.getElementById('chat-device-select');
  const filterSerial=sel?sel.value:'';
  if(filterSerial){
    const serial=chatIPToSerial[ev.client_ip];
    if(!serial||serial!==filterSerial)return false;
  }
  const q=document.getElementById('chat-search').value.toLowerCase();
  if(q&&!(ev.sender.toLowerCase().includes(q)||ev.message.toLowerCase().includes(q)))return false;
  return true;
}
function renderChatPanel(){
  const el=document.getElementById('chat-area');
  if(!el)return;
  const filtered=chatEvents.filter(chatMatchFilter);
  // 同一メッセージ（送信者+内容+チャンネル）の重複を除去（最初の出現を残す）
  const seen=new Set();
  const deduped=filtered.filter(ev=>{const k=ev.channel+'|'+ev.sender+'|'+ev.message;if(seen.has(k))return false;seen.add(k);return true;});
  if(!deduped.length){el.innerHTML='<div style="color:#606080;padding:8px;font-size:0.82em">チャットなし</div>';return;}
  el.innerHTML=deduped.map(chatMsgHtml).join('');
  el.scrollTop=el.scrollHeight;
}
function appendChatToPanel(ev){
  // 直近50件に同じ送信者+メッセージ+チャンネルがあれば重複として無視
  const isDup=chatEvents.slice(-50).some(e=>e.channel===ev.channel&&e.sender===ev.sender&&e.message===ev.message);
  if(isDup)return;
  chatEvents.push(ev);
  if(chatEvents.length>500)chatEvents=chatEvents.slice(-500);
  if(!chatMatchFilter(ev))return;
  const el=document.getElementById('chat-area');
  if(!el)return;
  if(el.querySelector('div[style*="606080"]')&&el.children.length===1)el.innerHTML='';
  const tmp=document.createElement('div');
  tmp.innerHTML=chatMsgHtml(ev);
  el.appendChild(tmp.firstChild);
  if(el.children.length>500)el.removeChild(el.firstChild);
  el.scrollTop=el.scrollHeight;
}
function clearChatPanel(){chatEvents=[];const el=document.getElementById('chat-area');if(el)el.innerHTML='';}
async function initChat(){
  const dm=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  if(dm.devices)dm.devices.forEach(e=>{if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  refreshChatDeviceDropdown();
  const h=await fetch('/api/chat-log').then(r=>r.json()).catch(()=>[]);
  chatEvents=h||[];
  renderChatPanel();
  const es=new EventSource('/api/chat-events');
  es.onmessage=e=>{try{appendChatToPanel(JSON.parse(e.data));}catch(_){}};
}
async function switchAll(){
  const ch=document.getElementById('allch').value;
  const bar=document.getElementById('status-bar');
  bar.textContent='切替中...';
  const serials=selectedSerials(); // 空 = 全台
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:parseInt(ch),serials})});
  const d=await r.json();
  if(d.results){
    const failed=d.results.filter(x=>!x.ok);
    bar.textContent=failed.length===0?'✓ 完了':'✗ '+failed.length+'台失敗';
  } else {
    bar.textContent=d.error?'✗ '+d.error:'✗ 失敗';
  }
  setTimeout(()=>bar.textContent='',3000);
}
async function switchOne(serial){
  const ch=parseInt(document.getElementById('ch-'+encodeURIComponent(serial)).value);
  const bar=document.getElementById('status-bar');
  bar.textContent='切替中...';
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:ch,serial})});
  const d=await r.json();
  const res=d.results&&d.results[0];
  bar.textContent=res&&res.ok?'✓ 完了':'✗ '+(res&&res.error||d.error||'失敗');
  setTimeout(()=>bar.textContent='',3000);
}

// ── Log Filter ──
const LOG_CATS=[
  {id:'mumu',  label:'[MuMu]',  test:l=>l.includes('[MuMu]')},
  {id:'det',   label:'検知',     test:l=>l.includes('[DETECTION]')||l.includes('[検知]')},
  {id:'chat',  label:'チャット', test:l=>l.includes('[チャット')},
  {id:'pkt',   label:'パケット', test:l=>/\[0x[0-9a-fA-F]+\]/.test(l)||/\[Instance-/.test(l)},
  {id:'gas',   label:'GAS',     test:l=>l.includes('[GASFetch]')},
  {id:'gui',   label:'GUI',     test:l=>l.includes('[GUI]')},
  {id:'other', label:'その他',  test:_=>true},
];
// デフォルト: パケット非表示
const logFilter={mumu:true,det:true,chat:true,pkt:false,gas:true,gui:true,other:true};
function getCat(line){ for(const c of LOG_CATS){ if(c.test(line)) return c.id; } return 'other'; }
function isVisible(line){ return logFilter[getCat(line)]!==false; }
function buildFilterBar(){
  const bar=document.getElementById('log-filter-bar');
  if(!bar) return;
  bar.innerHTML=LOG_CATS.map(c=>'<button id="fc-'+c.id+'" class="'+(logFilter[c.id]?'on':'')+'" onclick="toggleCat(\''+c.id+'\')">'+c.label+'</button>').join('');
}
function toggleCat(id){
  logFilter[id]=!logFilter[id];
  const btn=document.getElementById('fc-'+id);
  if(btn) btn.className=logFilter[id]?'on':'';
  document.querySelectorAll('#log-area .log-line').forEach(div=>{
    div.style.display=isVisible(div.textContent)?'':'none';
  });
}

// ── Log / SSE ──
(function(){
  const la=document.getElementById('log-area');
  let userScrolling=false;
  if(la){
    la.addEventListener('scroll',()=>{
      userScrolling=(la.scrollHeight-la.scrollTop-la.clientHeight)>20;
    });
  }
  window._logUserScrolling=()=>userScrolling;
})();
function appendLog(line){
  const la=document.getElementById('log-area');
  const div=document.createElement('div');
  div.className='log-line'+(line.includes('[DETECTION]')||line.includes('金')?' detect':'');
  div.textContent=line;
  if(!isVisible(line)) div.style.display='none';
  la.appendChild(div);
  if(!window._logUserScrolling||!window._logUserScrolling()){
    la.scrollTop=la.scrollHeight;
  }
}
async function testDetect(){ await fetch('/api/test-detect',{method:'POST'}); }
let monsterScanEnabled=false;
async function toggleMonsterScan(){
  monsterScanEnabled=!monsterScanEnabled;
  await fetch('/api/monster-scan',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:monsterScanEnabled})});
  const btn=document.getElementById('btn-monster-scan');
  btn.classList.toggle('active',monsterScanEnabled);
  btn.textContent=monsterScanEnabled?'🔍 スキャン中...':'🔍 怪物スキャン';
}
(function(){
  const src=new EventSource('/events');
  src.onmessage=e=>{
    appendLog(e.data);
    if(e.data.includes('[GUI] 金ウリボ')||e.data.includes('[DETECTION]')){
      loadGoldHistory(); loadPatrolChannels();
    }
    if(e.data.includes('channels.txt')){
      loadPatrolChannels();
    }
  };
  fetch('/api/logs').then(r=>r.json()).then(lines=>(lines||[]).forEach(appendLog));
})();

// ── Gold History ──
async function loadGoldHistory(){
  try{
    const h=await fetch('/api/gold-history').then(r=>r.json());
    const c=document.getElementById('gold-history-container');
    if(!h||h.length===0){ c.innerHTML='<div class="no-history">検知履歴なし</div>'; return; }
    c.innerHTML='<table class="gold-table"><colgroup><col class="col-time"><col class="col-name"><col class="col-ch"><col></colgroup>'
      +'<thead><tr><th>時刻</th><th>名前</th><th>Ch</th><th>場所</th></tr></thead><tbody>'
      +h.map(e=>'<tr>'
        +'<td class="time-cell">'+escHtml(e.time||'')+'</td>'
        +'<td class="name-cell">'+escHtml(e.monster_name||'ゴールドウリボ')+'</td>'
        +'<td class="ch-cell">Ch'+Number(e.channel)+'</td>'
        +'<td>'+escHtml(e.location||'')+'</td>'
        +'</tr>').join('')
      +'</tbody></table>';
  }catch(_){}
}

// ── Patrol ──
let patrolChannels=[],patrolReversed=localStorage.getItem('patrolReversed')==='true',patrolLoopMode=localStorage.getItem('patrolLoopMode')!=='false';
function applyReversedUI(){ const b=document.getElementById('btn-reversed'); b.textContent=patrolReversed?'⬇ 逆順':'⬆ 正順'; b.classList.toggle('active',patrolReversed); }
function applyLoopUI(){ const b=document.getElementById('btn-loop'); b.textContent=patrolLoopMode?'🔁 ループ':'1️⃣ 一巡'; b.classList.toggle('active',!patrolLoopMode); }
async function loadPatrolChannels(){
  const d=await fetch('/api/patrol/channels').then(r=>r.json());
  patrolChannels=d.channels||[];
  renderChannelEditor();
}
function renderChannelEditor(){
  const el=document.getElementById('ch-editor');
  if(patrolChannels.length===0){ el.innerHTML='<div class="no-devices">チャンネルなし</div>'; document.getElementById('btn-ch-save').disabled=true; return; }
  el.innerHTML=patrolChannels.map((ch,i)=>
    '<div class="ch-row">'
    +'<span class="ch-num">'+(i+1)+'.</span>'
    +'<input type="number" value="'+ch+'" min="1" max="9999" style="width:75px"'
    +' onchange="patrolChannels['+i+']=parseInt(this.value)||1;document.getElementById(\'btn-ch-save\').disabled=false">'
    +'<button class="secondary" style="padding:2px 8px;font-size:0.8em" onclick="removeChannel('+i+')">✕</button>'
    +'</div>'
  ).join('');
  document.getElementById('btn-ch-save').disabled=false;
}
function addChannel(){ const v=parseInt(prompt('追加するチャンネル番号:',''))||0; if(v>0){patrolChannels.push(v);renderChannelEditor();} }
function removeChannel(i){ patrolChannels.splice(i,1); renderChannelEditor(); document.getElementById('btn-ch-save').disabled=false; }
function sortChannels(dir){ patrolChannels.sort((a,b)=>dir==='asc'?a-b:b-a); renderChannelEditor(); document.getElementById('btn-ch-save').disabled=false; }
function bulkImportChannels(){
  const nums=document.getElementById('ch-bulk-input').value.split(/[,\s]+/).map(s=>parseInt(s)).filter(n=>n>0);
  if(!nums.length) return;
  patrolChannels=nums; renderChannelEditor(); document.getElementById('ch-bulk-input').value=''; document.getElementById('btn-ch-save').disabled=false;
}
async function saveChannels(){
  const r=await fetch('/api/patrol/channels',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channels:patrolChannels})});
  const d=await r.json();
  const st=document.getElementById('ch-save-status');
  st.textContent=d.ok?'✓ 保存済':'✗ 失敗';
  if(d.ok){document.getElementById('btn-ch-save').disabled=true;loadPatrolChannels();}
  setTimeout(()=>st.textContent='',3000);
}
function toggleReversed(){ patrolReversed=!patrolReversed; localStorage.setItem('patrolReversed',patrolReversed); applyReversedUI(); }
function toggleLoop(){ patrolLoopMode=!patrolLoopMode; localStorage.setItem('patrolLoopMode',patrolLoopMode); applyLoopUI(); }
async function patrolStart(){
  // channels が空なら送らない（Go側が s.patrolChannels を使う）
  const chs = patrolChannels.length > 0 ? patrolChannels : [];
  const body = {
    serials: selectedSerials(),
    reversed: patrolReversed,
    loop_mode: patrolLoopMode,
    start_channel: parseInt(document.getElementById('patrol-start-ch').value)||0
  };
  if(chs.length > 0) body.channels = chs;
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  const d=await r.json();
  if(!d.ok) alert('巡回開始失敗: '+(d.error||''));
}
async function patrolStop(){ await fetch('/api/patrol/stop',{method:'POST'}); }
async function clearFullChannels(){
  await fetch('/api/patrol/clear-full',{method:'POST'});
}
function updatePatrolUI(running){ document.getElementById('btn-patrol-start').disabled=running; document.getElementById('btn-patrol-stop').disabled=!running; }
async function pollPatrolStatus(){
  try{
    const d=await fetch('/api/patrol/status').then(r=>r.json());
    const stateEl=document.getElementById('ps-state');
    const chEl=document.getElementById('ps-ch');
    const progEl=document.getElementById('ps-prog');
    const parEl=document.getElementById('ps-parallel');
    if(d.running){
      stateEl.className='running';
      stateEl.textContent='▶ 巡回中'+(d.waiting_move?' ⏳':'');
      chEl.textContent='Ch'+d.current_channel;
      progEl.textContent=(d.current_index+1)+'/'+d.total_channels;
      const delay=d.parallel_group_delay>0?'(+'+d.parallel_group_delay+'s)':'';
      parEl.textContent=(d.parallel_limit===0?'並列:無制限':'並列:'+d.parallel_limit+'台'+delay)
        +(d.move_timeout_secs>0?' | timeout:'+d.move_timeout_secs+'s':'')
        +' | 滞在:'+Math.round(d.dwell_secs)+'s';
      updatePatrolUI(true);
    }else{
      stateEl.className='stopped'; stateEl.textContent='■ 停止中';
      chEl.textContent=d.last_channel>0?'前回: Ch'+d.last_channel:'';
      progEl.textContent=''; parEl.textContent='';
      updatePatrolUI(false);
    }
    const fullEl=document.getElementById('ps-full');
    const clearBtn=document.getElementById('btn-clear-full');
    if(d.full_channels&&d.full_channels.length){
      fullEl.textContent='🚫 満員スキップ: Ch'+d.full_channels.join(', Ch');
      clearBtn.style.display='';
    }else{
      fullEl.textContent='';
      clearBtn.style.display='none';
    }
    const warnEl=document.getElementById('crash-warning');
    if(warnEl){
      const crashed=d.crashed_instances&&d.crashed_instances.length?d.crashed_instances:null;
      if(crashed){
        warnEl.style.display='';
        warnEl.textContent='⚠ クラッシュ判定: '+crashed.join(', ')+' (3回連続未応答)';
      }else{
        warnEl.style.display=(d.running&&(d.consecutive_full_count||0)>=3)?'':'none';
        if(warnEl.style.display!='none') warnEl.textContent='⚠ ゲームクライアントがch移動できない状態です（クラッシュの可能性）。ADBサーバーを再起動してください。';
      }
    }
  }catch(e){
    console.warn('patrol status error:', e);
  }finally{
    setTimeout(pollPatrolStatus,2000);
  }
}

// ── Config ──
const CFG_FIELDS=[
  {k:'discord_webhook',label:'Discord Webhook URL',type:'text',desc:'空にするとDiscord通知無効'},
  {k:'chat_exclude',label:'チャット除外キーワード',type:'csv',desc:'カンマ区切り。例: いない,終わった'},
  {k:'patrol_dwell_secs',label:'滞在時間 (秒)',type:'number',desc:'ch移動完了後〜次ch移動開始までの待機秒数'},
  {k:'patrol_move_timeout_secs',label:'初回マージ待ちタイムアウト (秒)',type:'number',desc:'1台目のマージを待つ最大秒数。0=無効（時間内に1台も来なければ満員と判定）'},
  {k:'patrol_merge_timeout_secs',label:'残りマージ待ちタイムアウト (秒)',type:'number',desc:'1台目受信後、残り台数を待つ最大秒数。3回タイムアウトごとに稼働台数を1台削減。0=初回待ちと同じ'},
  {k:'parallel_limit',label:'並列切替台数',type:'number',desc:'0=全台同時（ディレイ無効）'},
  {k:'parallel_group_delay_secs',label:'グループ間ディレイ (秒)',type:'number',desc:'並列台数>0のとき有効'},
  {k:'adb_path',label:'ADBパス',type:'text',desc:'adb.exeのフルパスまたは「adb」'},
  {k:'mumu_delay_ms',label:'ADBコマンド間隔 (ms)',type:'number',desc:'各ADBコマンド間の待機時間'},
  {k:'mumu_tap_x',label:'タップX座標',type:'number',desc:'チャンネル入力欄のタップX'},
  {k:'mumu_tap_y',label:'タップY座標',type:'number',desc:'チャンネル入力欄のタップY'},
  {k:'mumu_pre_keycode',label:'プリキーコード',type:'text',desc:'チャンネル入力欄を開くキーコード'},
];
let cfgData={};
async function loadConfig(){
  cfgData=await fetch('/api/config').then(r=>r.json());
  document.getElementById('cfg-form').innerHTML=CFG_FIELDS.map(function(f){
    var val=cfgData[f.k]!==undefined?cfgData[f.k]:'';
    if(f.type==='csv'&&Array.isArray(val)) val=val.join(',');
    var inputType=f.type==='csv'?'text':f.type;
    var noteHtml=f.desc?('<span class="cfg-note">'+escHtml(f.desc)+'</span>'):'';
    return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label>'
      +'<input type="'+inputType+'" id="cfg-'+f.k+'" value="'+escHtml(String(val))+'" placeholder="'+escHtml(f.desc||'')+'">'
      +noteHtml+'</div>';
  }).join('');
}
async function saveConfig(){
  const updated={...cfgData};
  CFG_FIELDS.forEach(f=>{
    const el=document.getElementById('cfg-'+f.k); if(!el) return;
    if(f.type==='number') updated[f.k]=parseFloat(el.value)||0;
    else if(f.type==='csv') updated[f.k]=el.value.split(',').map(s=>s.trim()).filter(Boolean);
    else updated[f.k]=el.value;
  });
  const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(updated)});
  const d=await r.json();
  const st=document.getElementById('cfg-status');
  st.textContent=d.ok?'✓ 保存・反映済':'✗ 失敗: '+(d.error||'');
  setTimeout(()=>st.textContent='',4000);
  cfgData=updated;
}

// ── Init ──
restoreLayout();
buildFilterBar();

// 起動時デバイス確認：ADB再起動なしで最大3回試行し、見つかれば終了
(async function startupDeviceCheck(){
  async function fetchDevicesOnly(){
    const r=await fetch('/api/devices');
    const res=await r.json();
    const devs=Array.isArray(res)?res:(res.devices||[]);
    const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
    if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
    if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
    const el=document.getElementById('device-list');
    if(!devs||devs.length===0){ el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>'; return; }
    el.innerHTML=devs.map(d=>{
      const info=mapRes[d]||{};
      const uid=info.user_uid||'', ch=info.line_id||'', confirmed=info.confirmed||false;
      const checked=selectedDevices.has(d)?'checked':'';
      const uidHtml=uid?('<span class="uid">'+(confirmed?'🔗':'')+' UID:'+uid+(ch?' Ch'+ch:'')+'</span>'):'';
      const eid='ch-'+encodeURIComponent(d);
      return '<div class="device-entry'+(confirmed?' matched':'')+'">'
        +'<label class="check-label">'
        +'<input type="checkbox" '+checked+' onchange="toggleDevice('+escAttrJs(d)+',this.checked)">'
        +'<span class="serial">'+escHtml(d)+'</span>'+uidHtml
        +'</label>'
        +'<div style="display:flex;gap:6px;margin-top:4px">'
        +'<input type="number" id="'+escHtml(eid)+'" min="1" max="999" value="1" style="width:65px">'
        +'<button style="padding:3px 8px;font-size:0.8em" onclick="switchOne('+escAttrJs(d)+')">切替</button>'
        +'</div></div>';
    }).join('');
  }
  for(let i=1;i<=3;i++){
    await fetchDevicesOnly();
    if(document.querySelectorAll('#device-list .device-entry').length>0) return;
    if(i<3) await new Promise(r=>setTimeout(r,3000));
  }
})();

applyReversedUI();
applyLoopUI();
loadPatrolChannels();
pollPatrolStatus();
loadConfig();
loadGoldHistory();
setInterval(loadGoldHistory,30000);
initChat();
</script>
</body>
</html>`

// spawnLogHTML は出現ログ専用の分離ウィンドウ用ページ
const spawnLogHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>出現ログ - LoyalBoarlet Monitor</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0a0a1a;color:#eaeaea;font-family:'Segoe UI',sans-serif;font-size:14px;display:flex;flex-direction:column;height:100vh}
h1{font-size:1em;padding:6px 12px;background:#0d1b33;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:8px;flex-shrink:0}
#container{flex:1;overflow-y:auto;padding:8px}
table{width:100%;border-collapse:collapse;font-size:0.85em}
th{color:#ffd700;text-align:left;padding:4px 10px;border-bottom:1px solid #1a3a6a;position:sticky;top:0;background:#0d1b33;z-index:1}
td{padding:4px 10px;border-bottom:1px solid #0d1530;line-height:1.4;white-space:nowrap}
tr:hover td{background:#111e38}
.ch{color:#7ec8e3;font-family:monospace;font-weight:bold}
.time{color:#a0a0b0}
.monster{color:#ffd700;font-weight:bold}
.no-data{color:#606080;padding:12px}
</style>
</head>
<body>
<h1>🌟 出現ログ</h1>
<div id="container"><div class="no-data">読み込み中...</div></div>
<script>
function eH(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
async function load(){
  const h=await fetch('/api/gold-history').then(r=>r.json()).catch(()=>[]);
  const c=document.getElementById('container');
  if(!h||h.length===0){c.innerHTML='<div class="no-data">出現履歴なし</div>';return;}
  c.innerHTML='<table><thead><tr><th>時刻</th><th>名前</th><th>Ch</th><th>場所</th></tr></thead><tbody>'
    +h.map(e=>'<tr><td class="time">'+eH(e.time||'')+'</td><td class="monster">'+eH(e.monster_name||'ゴールドウリボ')+'</td><td class="ch">Ch'+Number(e.channel)+'</td><td>'+eH(e.location||'')+'</td></tr>').join('')
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
<title>チャットログ - LoyalBoarlet Monitor</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0a0a1a;color:#eaeafa;font-family:'Segoe UI',sans-serif;font-size:13px;display:flex;flex-direction:column;height:100vh}
h1{font-size:1em;padding:6px 12px;background:#0d1b33;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:8px;flex-shrink:0}
#toolbar{padding:4px 10px;background:#0b0f20;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:10px;flex-shrink:0;font-size:0.85em}
#toolbar input[type=text]{background:#0d1b33;border:1px solid #1a3a6a;color:#eaeafa;padding:2px 6px;border-radius:3px;width:180px}
#container{flex:1;overflow-y:auto;padding:0}
.msg{padding:4px 10px;border-bottom:1px solid #0d1530;line-height:1.5}
.msg:hover{background:#0e1a30}
.time{color:#606080;margin-right:6px}
.ip{color:#7a6030;font-size:0.85em;margin-right:4px}
.ch{color:#7ec8e3;font-size:0.85em;margin-right:4px}
.sender{color:#ffd700;font-weight:bold;margin-right:4px}
.body{color:#d0d0e0}
.no-data{color:#606080;padding:12px}
#count{color:#a0a0b0;margin-left:auto;font-size:0.85em}
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
let all=[];
function escH(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
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
  c.scrollTop=c.scrollHeight;
}
(async function(){
  const h=await fetch('/api/chat-log').then(r=>r.json()).catch(()=>[]);
  all=h||[];
  render();
  const es=new EventSource('/api/chat-events');
  es.onmessage=e=>{try{appendOne(JSON.parse(e.data));}catch(_){}};
})();
</script>
</body>
</html>`
