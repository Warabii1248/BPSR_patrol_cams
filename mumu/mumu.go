// Package mumu はMuMu Playerエミュレーターに対してADB経由でチャンネル切替を行う。
// uribo-discord-watcher/src/mumu.rs をGo移植したもの。
package mumu

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config はADB操作の設定
type Config struct {
	ADBPath     string
	TapX        int
	TapY        int
	ClearLength int
	PreKeycode  string
	GlobalDelay time.Duration
	// ParallelLimit は同時切替の最大台数。0=無制限（グループディレイも無効）。
	ParallelLimit int
	// ParallelGroupDelay はグループ間の待機時間。ParallelLimit>0のとき有効。
	ParallelGroupDelay time.Duration
	// MoveTimeout は1台目の[0x2E]パケットを待つ最大時間。0=無効（ADB完了即dwell開始）
	MoveTimeout time.Duration
	// MergeTimeout は1台目受信後、残り台数を待つ最大時間。0=MoveTimeoutと同じ従来動作
	MergeTimeout time.Duration
	// DwellDuration はch移動完了後〜次ch移動開始までの待機時間
	DwellDuration time.Duration
}

// DefaultConfig はデフォルト値を返す
func DefaultConfig() Config {
	return Config{
		ADBPath:            "adb",
		TapX:               975,
		TapY:               664,
		ClearLength:        3,
		PreKeycode:         "KEYCODE_P",
		GlobalDelay:        800 * time.Millisecond,
		ParallelLimit:      0,
		ParallelGroupDelay: 0,
	}
}

// newCmd は HideWindow: true でコマンドを作成する（GUIモード時のコンソール点滅防止）
func newCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func runAdb(cfg Config, args ...string) (string, error) {
	cmd := newCmd(cfg.ADBPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("adb %v: %w\n%s", args, err, string(out))
	}
	if cfg.GlobalDelay > 0 {
		time.Sleep(cfg.GlobalDelay)
	}
	return strings.TrimSpace(string(out)), nil
}

func adb(serial string, cfg Config, args ...string) (string, error) {
	full := append([]string{"-s", serial}, args...)
	return runAdb(cfg, full...)
}

// EnsureServer は ADB サーバーが起動していることを確認し、未起動なら起動する。
// RestartServer と異なり kill-server は行わないため既存接続を維持できる。
func EnsureServer(cfg Config) error {
	log.Println("[MuMu] adb start-server...")
	out, err := newCmd(cfg.ADBPath, "start-server").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb start-server: %w\n%s", err, string(out))
	}
	log.Println("[MuMu] ADB サーバー起動確認完了")
	time.Sleep(500 * time.Millisecond)
	return nil
}

// RestartServer は adb kill-server → adb start-server でADBサーバーを再起動する。
// ADB接続が切れた場合の復旧に使用する。
func RestartServer(cfg Config) error {
	log.Println("[MuMu] adb kill-server...")
	// kill-server は失敗しても無視（既に停止済みの場合あり）
	_ = newCmd(cfg.ADBPath, "kill-server").Run()
	time.Sleep(500 * time.Millisecond)

	log.Println("[MuMu] adb start-server...")
	out, err := newCmd(cfg.ADBPath, "start-server").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb start-server: %w\n%s", err, string(out))
	}
	log.Println("[MuMu] ADBサーバー再起動完了")
	time.Sleep(500 * time.Millisecond)
	return nil
}

// ListDevices は接続中のADBデバイス一覧を返す。
// ADBサーバーの再起動は行わない（通常の取得に使用）。
func ListDevices(cfg Config) ([]string, error) {
	log.Println("[MuMu] デバイス一覧を取得中...")
	devices, err := listDevicesOnce(cfg)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		log.Println("[MuMu] デバイスが見つかりません。ADBでエミュレーターが認識されているか確認してください")
	}
	return devices, nil
}

// ListDevicesWithRestart は adb kill-server/start-server でADBサーバーを再起動してから
// デバイス一覧を返す。接続が切れた場合の復旧用。
func ListDevicesWithRestart(cfg Config) ([]string, error) {
	if restartErr := RestartServer(cfg); restartErr != nil {
		log.Printf("[MuMu] ADB再起動失敗: %v", restartErr)
	}
	return ListDevices(cfg)
}

func listDevicesOnce(cfg Config) ([]string, error) {
	out, err := runAdb(cfg, "devices")
	if err != nil {
		log.Printf("[MuMu] adb devices 失敗: %v", err)
		return nil, err
	}
	// 生出力を全行ログ（\n 展開して見やすく）
	log.Printf("[MuMu] adb devices 出力:\n%s", out)

	var devices []string
	var offline []string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		switch parts[1] {
		case "device":
			devices = append(devices, parts[0])
		case "offline", "unauthorized":
			offline = append(offline, parts[0]+"("+parts[1]+")")
		}
	}
	if len(offline) > 0 {
		log.Printf("[MuMu] 接続不可デバイス: %v", offline)
	}
	log.Printf("[MuMu] 有効デバイス: %v (%d台)", devices, len(devices))
	return devices, nil
}

// SwitchChannel は指定デバイスを指定チャンネルに切り替える。
// 失敗した場合は adb kill-server/start-server で復旧してリトライする。
func SwitchChannel(serial string, channel uint32, cfg Config) error {
	err := switchChannelOnce(serial, channel, cfg)
	if err != nil {
		log.Printf("[MuMu] switch_channel失敗(%v)、ADBサーバーを再起動してリトライ...", err)
		if restartErr := RestartServer(cfg); restartErr != nil {
			log.Printf("[MuMu] ADB再起動失敗: %v", restartErr)
			return err // 再起動失敗なら元のエラーを返す
		}
		return switchChannelOnce(serial, channel, cfg)
	}
	return nil
}

func switchChannelOnce(serial string, channel uint32, cfg Config) error {
	log.Printf("[MuMu] switch_channel: serial=%s channel=%d", serial, channel)

	// Pキーでチャンネル入力を開く
	if cfg.PreKeycode != "" {
		if _, err := adb(serial, cfg, "shell", "input", "keyevent", cfg.PreKeycode); err != nil {
			return fmt.Errorf("pre_keycode: %w", err)
		}
	}

	// タップで入力欄をフォーカス
	if cfg.TapX > 0 && cfg.TapY > 0 {
		tapArgs := []string{"shell", "input", "tap",
			fmt.Sprintf("%d", cfg.TapX),
			fmt.Sprintf("%d", cfg.TapY),
		}
		if _, err := adb(serial, cfg, tapArgs...); err != nil {
			return fmt.Errorf("tap: %w", err)
		}
	}

	// DEL × ClearLength + 数字入力を1コマンドにまとめて送信（ディレイ削減）
	keycodes := make([]string, 0, cfg.ClearLength+len(fmt.Sprintf("%d", channel)))
	for i := 0; i < cfg.ClearLength; i++ {
		keycodes = append(keycodes, "KEYCODE_DEL")
	}
	for _, c := range fmt.Sprintf("%d", channel) {
		keycodes = append(keycodes, fmt.Sprintf("KEYCODE_%c", c))
	}
	args := append([]string{"shell", "input", "keyevent"}, keycodes...)
	if _, err := adb(serial, cfg, args...); err != nil {
		return fmt.Errorf("clear+input: %w", err)
	}
	// Enterで確定
	if _, err := adb(serial, cfg, "shell", "input", "keyevent", "KEYCODE_ENTER"); err != nil {
		return fmt.Errorf("enter: %w", err)
	}

	// Pキーでチャンネル入力を閉じる（満員時のダイアログも閉じる）
	if cfg.PreKeycode != "" {
		if _, err := adb(serial, cfg, "shell", "input", "keyevent", cfg.PreKeycode); err != nil {
			return fmt.Errorf("pre_keycode: %w", err)
		}
	}

	log.Printf("[MuMu] switch_channel done: serial=%s channel=%d", serial, channel)
	return nil
}

// parallelLimit は cfg.ParallelLimit が 0（無制限）のとき total を返すヘルパー。
// ParallelLimit > total の場合は total に丸める（無制限扱いにはしない）。
func ParallelLimit(cfg Config, total int) int {
	if cfg.ParallelLimit <= 0 {
		return total // 0 = 無制限
	}
	if cfg.ParallelLimit > total {
		return total // 上限がデバイス数より多い場合は全台
	}
	return cfg.ParallelLimit
}

// switchGroup は serials[start:end] を並列で切り替え、全完了を待つ。
func SwitchGroup(serials []string, start, end int, channel uint32, cfg Config, results map[string]error, mu *sync.Mutex) {
	var wg sync.WaitGroup
	for i := start; i < end; i++ {
		s := serials[i]
		wg.Add(1)
		go func(serial string) {
			defer wg.Done()
			err := SwitchChannel(serial, channel, cfg)
			mu.Lock()
			results[serial] = err
			mu.Unlock()
		}(s)
	}
	wg.Wait()
}

// GetDeviceIP は指定デバイスの仮想NWインターフェースIPを返す。
// "adb -s <serial> shell ip route get 1" の出力から src フィールドをパースする。
func GetDeviceIP(serial string, cfg Config) (string, error) {
	cmd := newCmd(cfg.ADBPath, "-s", serial, "shell", "ip", "route", "get", "1")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ip route get: %w", err)
	}
	// 例: "1.0.0.0 via 10.0.2.2 dev eth0 src 192.168.9.101 uid 0"
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "src" && i+1 < len(fields) {
				ip := fields[i+1]
				// ローカルループバックはスキップ
				if ip != "127.0.0.1" && ip != "::1" {
					return ip, nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not parse IP from adb output: %q", strings.TrimSpace(string(out)))
}

// ───── チャンネルリスト ─────

// LoadChannels はファイルからチャンネル番号リストを読み込む。
// カンマ区切りまたは1行1番号の形式に対応する。
func LoadChannels(path string) ([]uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var channels []uint32
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// カンマ区切り対応
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				continue
			}
			channels = append(channels, uint32(n))
		}
	}
	return channels, scanner.Err()
}

// SaveChannels は channels を 1行1番号のテキストとして path に上書き保存する。
func SaveChannels(path string, channels []uint32) error {
	var sb strings.Builder
	for _, ch := range channels {
		sb.WriteString(strconv.FormatUint(uint64(ch), 10))
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// ───── 巡回（パトロール） ─────

// moveSignalMsg は [0x2E] パケット受信時のシグナル（インスタンスラベル付き）
type moveSignalMsg struct {
	t     time.Time
	label string // "Instance-N"
}

// PatrolStatus は現在の巡回状態
type PatrolStatus struct {
	Running              bool     `json:"running"`
	CurrentChannel       uint32   `json:"current_channel"`
	CurrentIndex         int      `json:"current_index"`
	TotalChannels        int      `json:"total_channels"`
	Serials              []string `json:"serials"`
	DwellSecs            float64  `json:"dwell_secs"`
	Phase                string   `json:"phase"`
	PhaseTotalSecs       float64  `json:"phase_total_secs"`
	PhaseStartedAtUnixMs int64    `json:"phase_started_at_unix_ms"`
	ParallelLimit        int      `json:"parallel_limit"`
	ParallelGroupDelay   float64  `json:"parallel_group_delay"`
	LastChannel          uint32   `json:"last_channel"`           // 最後に巡回したチャンネル
	Reversed             bool     `json:"reversed"`               // 逆順巡回中
	LoopMode             bool     `json:"loop_mode"`              // true=ループ / false=一巡で停止
	WaitingMove          bool     `json:"waiting_move"`           // ch移動完了待ち中
	MoveTimeoutSecs      float64  `json:"move_timeout_secs"`      // 移動待ちタイムアウト(秒)
	FullChannels         []uint32 `json:"full_channels"`          // 満員と判定してスキップしたch一覧
	ConsecutiveFullCount int      `json:"consecutive_full_count"` // 連続して満員スキップしたch数（クラッシュ検知用）
	KnownInstances       []string `json:"known_instances"`        // 認識済みインスタンス一覧
	CrashedInstances     []string `json:"crashed_instances"`      // クラッシュ判定中のインスタンス
}

// PatrolOptions は巡回の追加オプション
type PatrolOptions struct {
	Reversed     bool   // true=逆順（末尾→先頭方向）
	LoopMode     bool   // true=ループ継続 / false=一巡で自動停止
	StartChannel uint32 // 0=前回位置から再開 / >0=指定チャンネルから開始
}

// Patroller はチャンネル巡回を管理する
type Patroller struct {
	cfg              Config
	mu               sync.RWMutex
	status           PatrolStatus
	cancel           context.CancelFunc
	lastChannel      uint32             // 最後に巡回したチャンネル（再開位置の計算に使用）
	moveSignal       chan moveSignalMsg // [0x2E]パケット受信シグナル（インスタンスラベル付き）
	onChannelSwitch  func(uint32)       // チャンネル切替完了時コールバック
	knownInstances   map[string]bool    // 一度でも応答したインスタンス
	missedCounts     map[string]int     // インスタンス別連続未応答カウント
	crashedInstances map[string]bool    // クラッシュ判定済みインスタンス（3回連続未応答）
}

// NotifyChMovePacket は ncap が [0x2E] パケットを受信したときに呼び出す。
// label は "Instance-N" 形式のインスタンスラベル。巡回中でない場合は何もしない。
func (p *Patroller) NotifyChMovePacket(label string) {
	p.mu.RLock()
	running := p.status.Running
	ch := p.moveSignal
	p.mu.RUnlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- moveSignalMsg{t: time.Now(), label: label}:
	default: // バッファ満杯なら捨てる（ブロックしない）
	}
}

// NewPatroller はPatrollerを作成する
func NewPatroller(cfg Config) *Patroller {
	return &Patroller{cfg: cfg}
}

// SetOnChannelSwitch はチャンネル切替完了時に呼ばれるコールバックを設定する。
// CapDevice への現在チャンネル通知に使用する。
func (p *Patroller) SetOnChannelSwitch(fn func(uint32)) {
	p.mu.Lock()
	p.onChannelSwitch = fn
	p.mu.Unlock()
}

// Config は現在の設定をスレッドセーフに返す。
func (p *Patroller) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// UpdateConfig は実行中の設定（ParallelLimit等）を動的に更新する。
// 巡回中の場合は次のチャンネルから新しい設定が反映される。
func (p *Patroller) UpdateConfig(cfg Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Status は現在の巡回状態を返す
func (p *Patroller) Status() PatrolStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

// ClearFullChannels は満員判定リストをクリアする
func (p *Patroller) ClearFullChannels() {
	p.mu.Lock()
	p.status.FullChannels = nil
	p.mu.Unlock()
	log.Println("[MuMu] 満員チャンネルリストをクリアしました")
}

// Stop は巡回を停止する
func (p *Patroller) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	if p.status.CurrentChannel > 0 {
		p.lastChannel = p.status.CurrentChannel
	}
	p.status.LastChannel = p.lastChannel
	p.status.Running = false
	p.status.WaitingMove = false
	p.moveSignal = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	log.Println("[MuMu] 巡回停止")
}

// findResumeIndex はチャンネルリストから再開インデックスを決定する。
// lastCh がリストに存在すればそのインデックスを返す。
// 存在しない場合は lastCh に最も近い値のインデックスを返す。
// lastCh が 0（初回起動）の場合は 0 を返す。
func findResumeIndex(channels []uint32, lastCh uint32) int {
	if lastCh == 0 || len(channels) == 0 {
		return 0
	}
	// 完全一致を優先
	for i, ch := range channels {
		if ch == lastCh {
			return i
		}
	}
	// 最近傍を探す
	bestIdx := 0
	bestDiff := absDiffUint32(channels[0], lastCh)
	for i, ch := range channels[1:] {
		if d := absDiffUint32(ch, lastCh); d < bestDiff {
			bestDiff = d
			bestIdx = i + 1
		}
	}
	return bestIdx
}

func absDiffUint32(a, b uint32) uint32 {
	if a >= b {
		return a - b
	}
	return b - a
}

// Start は指定デバイス・チャンネルリストで巡回を開始する。
// opts で巡回方向・ループモード・開始チャンネルを指定できる。
// すでに巡回中の場合は停止してから再起動する。
// serials が空の場合は adb devices で自動検出する。
// channels が空の場合は何もしない。
// channelsFile が指定されている場合、各チャンネル滞在後にファイル更新を確認し
// 変更があれば近傍チャンネルから巡回し直す。
func (p *Patroller) Start(serials []string, channels []uint32, channelsFile string, opts PatrolOptions) {
	if len(channels) == 0 {
		log.Println("[MuMu] 巡回: チャンネルリストが空のため開始しない")
		return
	}
	p.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	p.mu.Lock()
	cfg := p.cfg
	p.mu.Unlock()

	step := 1
	if opts.Reversed {
		step = -1
	}

	// 開始インデックスの決定
	var resumeIdx int
	if opts.StartChannel > 0 {
		// 指定チャンネルから開始
		resumeIdx = findResumeIndex(channels, opts.StartChannel)
	} else if p.lastChannel > 0 {
		// 最後に巡回したchの次のchから再開（正順/逆順を考慮）
		n := len(channels)
		lastIdx := findResumeIndex(channels, p.lastChannel)
		resumeIdx = ((lastIdx+step)%n + n) % n
	} else {
		// 初回起動：正順は先頭、逆順は末尾
		if opts.Reversed {
			resumeIdx = len(channels) - 1
		}
	}

	// dwell: cfg.DwellDuration が未設定なら最低5秒
	dwell := cfg.DwellDuration
	if dwell < 5*time.Second {
		dwell = 5 * time.Second
	}

	// moveSignal: [0x2E]パケット受信ごとにインスタンスラベル付きでバッファリング
	sig := make(chan moveSignalMsg, 64)

	p.mu.Lock()
	p.cancel = cancel
	p.moveSignal = sig
	// インスタンス追跡を巡回再開時にリセット
	p.knownInstances = make(map[string]bool)
	p.missedCounts = make(map[string]int)
	p.crashedInstances = make(map[string]bool)
	p.status = PatrolStatus{
		Running:            true,
		TotalChannels:      len(channels),
		Serials:            serials,
		DwellSecs:          dwell.Seconds(),
		ParallelLimit:      cfg.ParallelLimit,
		ParallelGroupDelay: cfg.ParallelGroupDelay.Seconds(),
		LastChannel:        p.lastChannel,
		Reversed:           opts.Reversed,
		LoopMode:           opts.LoopMode,
		MoveTimeoutSecs:    cfg.MoveTimeout.Seconds(),
	}
	p.mu.Unlock()

	// channels.txt の初回モッドタイムを記録
	var lastModTime time.Time
	if channelsFile != "" {
		if fi, err := os.Stat(channelsFile); err == nil {
			lastModTime = fi.ModTime()
		}
	}

	go func() {
		defer func() {
			p.mu.Lock()
			p.status.Running = false
			p.mu.Unlock()
			log.Println("[MuMu] 巡回終了")
		}()

		dirStr := "正順"
		if opts.Reversed {
			dirStr = "逆順"
		}
		modeStr := "ループ"
		if !opts.LoopMode {
			modeStr = "一巡"
		}
		log.Printf("[MuMu] 巡回開始: %d ch, 滞在=%.1fs, 方向=%s, モード=%s, 開始idx=%d",
			len(channels), dwell.Seconds(), dirStr, modeStr, resumeIdx)

		idx := resumeIdx
		visited := 0 // 一巡モード用カウンタ

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// インデックスを範囲内に正規化（負数対応）
			n := len(channels)
			normalIdx := ((idx % n) + n) % n
			ch := channels[normalIdx]

			// 一巡モード: 全チャンネルを回し終えたら停止
			if !opts.LoopMode && visited >= n {
				log.Println("[MuMu] 巡回: 一巡完了、自動停止")
				return
			}

			// cfg を毎ループ最新に取得（UpdateConfig による動的変更に対応）
			p.mu.Lock()
			currentCfg := p.cfg
			// dwellも毎回cfgから再取得
			dwell = currentCfg.DwellDuration
			if dwell < 5*time.Second {
				dwell = 5 * time.Second
			}
			p.mu.Unlock()

			// デバイス一覧を取得（空なら自動検出）
			targets := serials
			if len(targets) == 0 {
				var devErr error
				targets, devErr = ListDevices(currentCfg)
				if devErr != nil || len(targets) == 0 {
					// ADB認識不能の可能性 → kill-server/start-server で復旧試行
					log.Printf("[MuMu] 巡回: デバイス取得失敗または0台、ADB再起動を試みます...")
					if restartErr := RestartServer(currentCfg); restartErr != nil {
						log.Printf("[MuMu] ADB再起動失敗: %v", restartErr)
					}
					targets, devErr = ListDevices(currentCfg)
					if devErr != nil {
						log.Printf("[MuMu] 巡回: デバイス再取得失敗: %v", devErr)
						select {
						case <-ctx.Done():
							return
						case <-time.After(5 * time.Second):
						}
						continue
					}
				}
				if len(targets) == 0 {
					log.Println("[MuMu] 巡回: 対象デバイスが0台。MuMu Playerが起動しているか確認してください")
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
					continue
				}
			}

			p.mu.Lock()
			p.status.CurrentChannel = ch
			p.status.CurrentIndex = normalIdx
			p.status.Serials = targets
			p.status.ParallelLimit = currentCfg.ParallelLimit
			p.status.ParallelGroupDelay = currentCfg.ParallelGroupDelay.Seconds()
			p.status.DwellSecs = dwell.Seconds()
			p.status.Phase = "move_start"
			p.status.PhaseTotalSecs = 0
			p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
			p.lastChannel = ch
			p.status.LastChannel = ch
			p.mu.Unlock()

			limit := ParallelLimit(currentCfg, len(targets))
			// ParallelLimit=0（無制限）のとき グループ間ディレイは無効
			groupDelay := currentCfg.ParallelGroupDelay
			if currentCfg.ParallelLimit <= 0 {
				groupDelay = 0
			}
			log.Printf("[MuMu] 巡回: [%d/%d] Ch%d → %d台 (ParallelLimit=%d, グループ=%d台, ディレイ=%.1fs, 滞在=%.0fs)",
				normalIdx+1, n, ch, len(targets), currentCfg.ParallelLimit, limit,
				groupDelay.Seconds(), dwell.Seconds())

			// 切替開始時刻を記録（この時刻以降に届いた[0x2E]のみ有効）
			switchStartAt := time.Now()

			// デバイスをグループに分けて並列切替
			patrolResults := make(map[string]error, len(targets))
			var patrolMu sync.Mutex
			for start := 0; start < len(targets); start += limit {
				end := start + limit
				if end > len(targets) {
					end = len(targets)
				}
				if start > 0 && groupDelay > 0 {
					log.Printf("[MuMu] 巡回: グループ間ディレイ %.1fs 待機中... (グループ%d)", groupDelay.Seconds(), start/limit+1)
					select {
					case <-ctx.Done():
						return
					case <-time.After(groupDelay):
					}
				}
				SwitchGroup(targets, start, end, ch, currentCfg, patrolResults, &patrolMu)
			}
			// 全台切替完了時刻を記録（この時刻以降の[0x2E]のみカウント）
			switchDoneAt := time.Now()

			// チャンネル切替完了を CapDevice に通知（lineIDパケットが来ない場合のフォールバック用）
			p.mu.RLock()
			notifyFn := p.onChannelSwitch
			p.mu.RUnlock()
			if notifyFn != nil {
				go notifyFn(ch)
			}

			failCount := 0
			for serial, err := range patrolResults {
				if err != nil {
					log.Printf("[MuMu] 巡回: serial=%s ch=%d 失敗: %v", serial, ch, err)
					failCount++
				} else {
					log.Printf("[MuMu] 巡回: serial=%s ch=%d OK", serial, ch)
				}
			}
			if failCount > 0 {
				log.Printf("[MuMu] 巡回: Ch%d %d台失敗 → 次回ループでADB再試行", ch, failCount)
			}

			// ── ch移動完了待ち（[0x2E]シグナル） ──
			// ・1台目が届くまで MoveTimeout で待つ（届かなければ満員と判定）
			// ・1台目受信後は MergeTimeout（0 なら MoveTimeout を流用）で残りを待つ
			// ・インスタンスごとに応答を追跡し、3回連続未応答でクラッシュ判定
			isFull := false
			if currentCfg.MoveTimeout > 0 {
				// 期待台数 = ADBデバイス数（常に固定）
				// セッションマージによって複数デバイスが同じインスタンスへ統合される可能性があるため、
				// knownInstances数ではなくADB上の実デバイス数を期待値とする。
				need := len(targets)

				got := 0
				respondedSet := make(map[string]bool)

				mergeTimeout := currentCfg.MergeTimeout
				if mergeTimeout <= 0 {
					mergeTimeout = currentCfg.MoveTimeout // 未設定なら従来動作
				}
				p.mu.Lock()
				p.status.WaitingMove = true
				p.status.Phase = "loading"
				p.status.PhaseTotalSecs = mergeTimeout.Seconds()
				p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				p.mu.Unlock()
				log.Printf("[MuMu] 巡回: Ch%d 移動完了待ち (0/%d台, 初回待ち=%.0fs, マージ待ち=%.0fs, 切替所要=%.1fs)",
					ch, need, currentCfg.MoveTimeout.Seconds(), mergeTimeout.Seconds(),
					switchDoneAt.Sub(switchStartAt).Seconds())
				draining := true
				for draining && got < need {
					select {
					case msg := <-sig:
						if msg.t.After(switchStartAt) {
							got++
							respondedSet[msg.label] = true
							log.Printf("[MuMu] 巡回: Ch%d [0x2E] buffered %s (%d/%d台)", ch, msg.label, got, need)
						}
					default:
						draining = false
					}
				}

				// フェーズ1: 1台目のシグナルを MoveTimeout で待つ（バッファ消化で0台のとき）
				if got == 0 {
					deadline := time.NewTimer(currentCfg.MoveTimeout)
				waitFirst:
					for got == 0 {
						select {
						case <-ctx.Done():
							deadline.Stop()
							return
						case <-deadline.C:
							isFull = true
							log.Printf("[MuMu] 巡回: Ch%d 満員と判定（移動完了シグナルなし） → スキップ", ch)
							break waitFirst
						case msg := <-sig:
							if msg.t.After(switchStartAt) {
								got++
								respondedSet[msg.label] = true
								log.Printf("[MuMu] 巡回: Ch%d [0x2E] %s (%d/%d台) ← マージタイマー開始 (%.0fs)",
									ch, msg.label, got, need, mergeTimeout.Seconds())
							}
						}
					}
					deadline.Stop()
				}

				// フェーズ2: 残りのシグナルを MergeTimeout で待つ
				if !isFull && got < need {
					mergeDeadline := time.NewTimer(mergeTimeout)
				waitRest:
					for got < need {
						select {
						case <-ctx.Done():
							mergeDeadline.Stop()
							return
						case <-mergeDeadline.C:
							log.Printf("[MuMu] 巡回: Ch%d 移動待ちタイムアウト (%d/%d台) → 強制進行", ch, got, need)
							break waitRest
						case msg := <-sig:
							if msg.t.After(switchStartAt) {
								got++
								respondedSet[msg.label] = true
								log.Printf("[MuMu] 巡回: Ch%d [0x2E] %s (%d/%d台)", ch, msg.label, got, need)
							}
						}
					}
					mergeDeadline.Stop()
				}

				// インスタンス追跡更新（isFull の場合はスキップ: チャンネル切替未実施）
				if !isFull {
					p.mu.Lock()
					var newCrashed, recovered []string

					// 応答したインスタンスを登録・ミスカウントリセット
					for label := range respondedSet {
						p.knownInstances[label] = true
						p.missedCounts[label] = 0
						if p.crashedInstances[label] {
							delete(p.crashedInstances, label)
							recovered = append(recovered, label)
						}
					}
					// 既知インスタンスのうち未応答のものはミスカウント増加
					for label := range p.knownInstances {
						if !respondedSet[label] {
							p.missedCounts[label]++
							if p.missedCounts[label] >= 3 && !p.crashedInstances[label] {
								p.crashedInstances[label] = true
								newCrashed = append(newCrashed, label)
							}
						}
					}
					// PatrolStatus を更新
					crashed := make([]string, 0, len(p.crashedInstances))
					for label := range p.crashedInstances {
						crashed = append(crashed, label)
					}
					sort.Strings(crashed)
					known := make([]string, 0, len(p.knownInstances))
					for label := range p.knownInstances {
						known = append(known, label)
					}
					sort.Strings(known)
					p.status.CrashedInstances = crashed
					p.status.KnownInstances = known
					p.mu.Unlock()

					sort.Strings(newCrashed)
					sort.Strings(recovered)
					if len(newCrashed) > 0 {
						log.Printf("[MuMu] 巡回: %v クラッシュ判定（3回連続未応答）→ 稼働台数から除外", newCrashed)
					}
					if len(recovered) > 0 {
						log.Printf("[MuMu] 巡回: %v 復旧確認 → 稼働台数に復帰", recovered)
					}
				}

				p.mu.Lock()
				p.status.WaitingMove = false
				if isFull {
					alreadyIn := false
					for _, fc := range p.status.FullChannels {
						if fc == ch {
							alreadyIn = true
							break
						}
					}
					if !alreadyIn {
						p.status.FullChannels = append(p.status.FullChannels, ch)
					}
					p.status.Phase = "move_start"
					p.status.PhaseTotalSecs = 0
					p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				} else {
					p.status.Phase = "dwell_wait"
					p.status.PhaseTotalSecs = dwell.Seconds()
					p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				}
				p.mu.Unlock()
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(dwell):
			}

			// channels.txt が更新されていたら近傍から再巡回
			if channelsFile != "" {
				if fi, statErr := os.Stat(channelsFile); statErr == nil && fi.ModTime().After(lastModTime) {
					if newChs, loadErr := LoadChannels(channelsFile); loadErr == nil && len(newChs) > 0 {
						lastModTime = fi.ModTime()
						// 現在のchが削除されていたか確認
						currentChRemoved := true
						for _, c := range newChs {
							if c == ch {
								currentChRemoved = false
								break
							}
						}
						var newIdx int
						if currentChRemoved {
							// 削除済みなのでそのまま次のインデックスへ進める
							// 旧リストでの次インデックスに最も近いchを新リストから探す
							nextCh := uint32(0)
							nextNormalIdx := (((idx + step) % len(channels)) + len(channels)) % len(channels)
							nextCh = channels[nextNormalIdx]
							newIdx = findResumeIndex(newChs, nextCh)
							log.Printf("[MuMu] channels.txt 更新検知: Ch%d は削除済み → 次Ch%d(idx=%d)から継続",
								ch, nextCh, newIdx)
						} else {
							newIdx = findResumeIndex(newChs, ch)
							log.Printf("[MuMu] channels.txt 更新検知: %d → %d ch、Ch%d 近傍(idx=%d)から再巡回",
								len(channels), len(newChs), ch, newIdx)
						}
						channels = newChs
						idx = newIdx
						visited = 0
						p.mu.Lock()
						p.status.TotalChannels = len(channels)
						p.mu.Unlock()
						// 削除済みの場合は既に次chのidxを指しているのでそのまま continue
						// 現在chが残っている場合は通常通り idx+=step してから continue
						if !currentChRemoved {
							visited++
							idx += step
						}
						continue
					} else if loadErr != nil {
						log.Printf("[MuMu] channels.txt リロード失敗: %v", loadErr)
					}
				}
			}

			visited++
			idx += step
		}
	}()
}
