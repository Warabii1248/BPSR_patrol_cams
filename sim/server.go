package sim

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// adbRequest は fake-adb から受け取る JSON メッセージ。
type adbRequest struct {
	Args []string `json:"args"`
	Ts   int64    `json:"ts"`
}

// adbResponse は fake-adb へ返す JSON メッセージ。
type adbResponse struct {
	Exit    int    `json:"exit"`
	Stdout  string `json:"stdout"`
	DelayMs int    `json:"delay_ms"`
	Screen  string `json:"screen"`
}

// SimServer は fake-adb からの TCP 接続を受け付けるシミュレーターサーバー。
type SimServer struct {
	listener net.Listener
	Addr     string

	mu      sync.Mutex
	rng     *rand.Rand
	scenario *Scenario

	// serial → デバイス状態
	deviceStates map[string]*deviceState

	// serial → 直前に送られた digit keycodes（KEYCODE_ENTER の前に送られる数字列）
	pendingDigits map[string]string

	// シグナル注入コールバック（GameServer が設定する）
	onEnter func(serial string, ch uint32)

	// 全受信コマンドのログ（デバッグ用）
	cmdLog   []string
	cmdLogMu sync.Mutex
}

type deviceState struct {
	mu        sync.Mutex
	currentCh uint32
	screen    string // "black" | "normal"
	silent    bool   // true=このサイクルは無応答
}

// NewSimServer はシミュレーターサーバーを作成してリッスンを開始する。
func NewSimServer(scenario *Scenario) (*SimServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sim listen: %w", err)
	}

	src := rand.NewSource(scenario.Seed)
	if scenario.Seed == 0 {
		src = rand.NewSource(time.Now().UnixNano())
	}

	s := &SimServer{
		listener:      ln,
		Addr:          ln.Addr().String(),
		rng:           rand.New(src),
		scenario:      scenario,
		deviceStates:  make(map[string]*deviceState),
		pendingDigits: make(map[string]string),
	}

	for _, dev := range scenario.Devices {
		s.deviceStates[dev.Serial] = &deviceState{
			currentCh: dev.InitialCh,
			screen:    "normal",
		}
	}

	go s.acceptLoop()
	return s, nil
}

// SetOnEnter は KEYCODE_ENTER 受信時のコールバックを設定する。
func (s *SimServer) SetOnEnter(fn func(serial string, ch uint32)) {
	s.mu.Lock()
	s.onEnter = fn
	s.mu.Unlock()
}

// SetScreenState は指定 serial の screencap 応答を変更する。
func (s *SimServer) SetScreenState(serial, screen string) {
	s.mu.Lock()
	ds := s.deviceStates[serial]
	s.mu.Unlock()
	if ds != nil {
		ds.mu.Lock()
		ds.screen = screen
		ds.mu.Unlock()
	}
}

// SetSilent は指定 serial の応答をサイレント（遅延なし即応答・シグナル注入なし）に切り替える。
func (s *SimServer) SetSilent(serial string, silent bool) {
	s.mu.Lock()
	ds := s.deviceStates[serial]
	s.mu.Unlock()
	if ds != nil {
		ds.mu.Lock()
		ds.silent = silent
		ds.mu.Unlock()
	}
}

// RandDelay は DelayRange に従って乱数遅延を返す。
func (s *SimServer) RandDelay(r DelayRange) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.MaxMs <= r.MinMs {
		return time.Duration(r.MinMs) * time.Millisecond
	}
	ms := r.MinMs + s.rng.Intn(r.MaxMs-r.MinMs+1)
	return time.Duration(ms) * time.Millisecond
}

// CmdLog は受信したコマンド一覧のコピーを返す。
func (s *SimServer) CmdLog() []string {
	s.cmdLogMu.Lock()
	defer s.cmdLogMu.Unlock()
	out := make([]string, len(s.cmdLog))
	copy(out, s.cmdLog)
	return out
}

func (s *SimServer) Close() {
	s.listener.Close()
}

func (s *SimServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *SimServer) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := scanner.Text()

	var req adbRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		log.Printf("[SimServer] JSON parse error: %v / raw=%q", err, line)
		return
	}

	s.cmdLogMu.Lock()
	s.cmdLog = append(s.cmdLog, strings.Join(req.Args, " "))
	s.cmdLogMu.Unlock()

	resp := s.dispatch(req)

	enc := json.NewEncoder(conn)
	if err := enc.Encode(resp); err != nil {
		log.Printf("[SimServer] send response error: %v", err)
	}
}

// dispatch は ADB コマンド引数に応じて応答を生成する。
func (s *SimServer) dispatch(req adbRequest) adbResponse {
	args := req.Args
	delayMs := s.scenario.Server.AdbCmdDelayMs

	// -s <serial> を除いた実コマンドを抽出する
	serial := ""
	cmdArgs := args
	if len(args) >= 2 && args[0] == "-s" {
		serial = args[1]
		cmdArgs = args[2:]
	}

	// devices コマンド
	if len(cmdArgs) == 1 && cmdArgs[0] == "devices" {
		var lines []string
		lines = append(lines, "List of devices attached")
		for _, dev := range s.scenario.Devices {
			lines = append(lines, dev.Serial+"\tdevice")
		}
		return adbResponse{
			Exit:    0,
			Stdout:  strings.Join(lines, "\n"),
			DelayMs: delayMs,
		}
	}

	// start-server / kill-server / reconnect
	if len(cmdArgs) == 1 && (cmdArgs[0] == "start-server" || cmdArgs[0] == "kill-server" || cmdArgs[0] == "reconnect") {
		return adbResponse{Exit: 0, DelayMs: delayMs}
	}

	// connect <addr>
	if len(cmdArgs) == 2 && cmdArgs[0] == "connect" {
		return adbResponse{Exit: 0, Stdout: "connected to " + cmdArgs[1], DelayMs: delayMs}
	}

	// exec-out screencap -p
	if len(cmdArgs) >= 2 && cmdArgs[0] == "exec-out" && strings.Contains(strings.Join(cmdArgs, " "), "screencap") {
		screen := "black"
		if serial != "" {
			s.mu.Lock()
			ds := s.deviceStates[serial]
			s.mu.Unlock()
			if ds != nil {
				ds.mu.Lock()
				screen = ds.screen
				ds.mu.Unlock()
			}
		}
		return adbResponse{Exit: 0, Screen: screen, DelayMs: delayMs}
	}

	// shell input keyevent ...
	if len(cmdArgs) >= 3 && cmdArgs[0] == "shell" && cmdArgs[1] == "input" && cmdArgs[2] == "keyevent" {
		keycodes := cmdArgs[3:]
		s.processKeycodes(serial, keycodes)
		return adbResponse{Exit: 0, DelayMs: delayMs}
	}

	// shell input tap X Y
	if len(cmdArgs) >= 5 && cmdArgs[0] == "shell" && cmdArgs[1] == "input" && cmdArgs[2] == "tap" {
		return adbResponse{Exit: 0, DelayMs: delayMs}
	}

	// shell pidof ...
	if len(cmdArgs) >= 2 && cmdArgs[0] == "shell" && cmdArgs[1] == "pidof" {
		return adbResponse{Exit: 0, Stdout: "12345", DelayMs: delayMs}
	}

	// shell ip route get 1
	if len(cmdArgs) >= 5 && cmdArgs[0] == "shell" && cmdArgs[1] == "ip" {
		return adbResponse{Exit: 0, Stdout: "1.0.0.0 via 10.0.2.2 dev eth0 src 192.168.9.1 uid 0", DelayMs: delayMs}
	}

	// その他は空応答
	return adbResponse{Exit: 0, DelayMs: delayMs}
}

// processKeycodes は keyevent コマンドのキーコード列を処理し、
// KEYCODE_ENTER を検出したらチャンネル番号を復元して onEnter を呼ぶ。
//
// switchChannelOnce は DEL×N + 数字列 を1コマンドで送り、その後 KEYCODE_ENTER を
// 別コマンドで送る（mumu.go:540-545 参照）。
// そのため数字をコマンド間で serial ごとに蓄積し、ENTER コマンド受信時に取り出す。
func (s *SimServer) processKeycodes(serial string, keycodes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hasEnter := false
	for _, kc := range keycodes {
		switch {
		case kc == "KEYCODE_ENTER":
			hasEnter = true
		case kc == "KEYCODE_DEL":
			// DEL = pendingDigits を全クリア。実機は末尾1文字削除だが、
			// switchChannelOnce は必ず DEL×ClearLength → 数字列の順で送るため等価
			s.pendingDigits[serial] = ""
		case strings.HasPrefix(kc, "KEYCODE_") && len(kc) == 9:
			// KEYCODE_0 〜 KEYCODE_9
			digit := kc[8]
			if digit >= '0' && digit <= '9' {
				s.pendingDigits[serial] += string(digit)
			}
		}
	}

	if !hasEnter {
		return
	}

	chStr := s.pendingDigits[serial]
	s.pendingDigits[serial] = "" // バッファクリア

	if chStr == "" {
		return
	}

	var ch uint32
	for _, c := range chStr {
		ch = ch*10 + uint32(c-'0')
	}
	if ch == 0 {
		return
	}

	cb := s.onEnter
	// ロックを保持したまま goroutine 起動（cb は参照のみ）
	if cb != nil {
		go cb(serial, ch)
	}
}
