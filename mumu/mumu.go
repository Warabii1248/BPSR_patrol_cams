// Package mumu はMuMu Playerエミュレーターに対してADB経由でチャンネル切替を行う。
// uribo-discord-watcher/src/mumu.rs をGo移植したもの。
package mumu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/debuglog"
)

// Config はADB操作の設定
type Config struct {
	ADBPath     string
	TapX        int
	TapY        int
	ClearLength int
	PreKeycode  string
	// ConnectSerials は start-server 後に adb connect を試行するシリアル一覧（host:port）。
	ConnectSerials []string
	GlobalDelay    time.Duration
	// ParallelLimit は同時切替の最大台数。0=無制限（グループディレイも無効）。
	ParallelLimit int
	// ParallelGroupDelay はグループ間の待機時間。ParallelLimit>0のとき有効。
	ParallelGroupDelay time.Duration
	// MoveTimeout は1台目の完了シグナルを待つ最大時間。0=無効（ADB完了即dwell開始）
	MoveTimeout time.Duration
	// MergeTimeout は1台目受信後、残り台数を待つ最大時間。0=MoveTimeoutと同じ従来動作
	MergeTimeout time.Duration
	// LoadStabilizationDuration は 0x2E UUID 着信から完了シグナル発火までの遅延（手動値）。
	// 0 かつ LoadStabilizationAuto=false の場合は 6s を使用する。
	LoadStabilizationDuration time.Duration
	// LoadStabilizationAuto が true の場合は直近ロード時間の観測値から遅延を自動算出する。
	// false の場合は LoadStabilizationDuration を使用する。
	LoadStabilizationAuto bool
	// DwellDuration はch移動完了後〜次ch移動開始までの待機時間
	DwellDuration time.Duration
	// AdaptiveTimeout は実ロード時間を学習して MoveTimeout/MergeTimeout を自動調整する
	AdaptiveTimeout bool
	// AdaptiveTimeoutWindow は学習に使うサンプル数（0=デフォルト10）
	AdaptiveTimeoutWindow int

	// GetLabelByIP はIPアドレスからインスタンスラベル（"Instance-N"等）を解決するコールバック。
	// CapDevice.GetLabelByClientIP を注入する。省略可。
	GetLabelByIP func(ip string) string
	// SerialToLabel はADBシリアル→ラベルの手動マッピング（GetLabelByIP より優先）。
	SerialToLabel map[string]string
	// GamePackageName はゲームのAndroidパッケージ名（空=クラッシュ検知・復帰無効）。
	GamePackageName string
	// GameLaunchActivity はゲーム起動Activity（省略時は monkey -p を使用）。
	GameLaunchActivity string
	// CrashRecoveryEnabled はタイムアウト時にプロセス確認し自動復帰する。
	CrashRecoveryEnabled bool
	// CrashRecoveryDelaySecs は復帰後のゲーム起動待機秒数（デフォルト: 30）。
	CrashRecoveryDelaySecs float64

	// --- ロード完了判定方式（No.46） ---
	// LoadDetectMode は 0x2E UUID 受信後のシーケンス進行方式。
	// "time" (従来) / "screen" / "either"。空文字または未知値は "time" 扱い。
	LoadDetectMode string
	// ScreenPollInterval は画面判定のポーリング間隔。
	ScreenPollInterval time.Duration
	// ScreenRegionX / Y / W / H は監視矩形（px、画像座標系）。
	// 左上 (X, Y) から幅 W × 高さ H。画像範囲外にはみ出した場合は自動クリップ。
	ScreenRegionX int
	ScreenRegionY int
	ScreenRegionW int
	ScreenRegionH int
	// ScreenBlackLuma は黒判定の輝度閾値（0-255）。
	ScreenBlackLuma uint8
	// ScreenBlackPixelRatio は黒画素割合下限（0.0-1.0）。
	ScreenBlackPixelRatio float64
	// ScreenDetectTimeout は画面判定のフォールバックタイムアウト。
	ScreenDetectTimeout time.Duration
}

// newCmd は HideWindow: true でコマンドを作成する（GUIモード時のコンソール点滅防止）
// ctx がキャンセルされると実行中のプロセスが即座に強制終了される。
func newCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func runAdb(ctx context.Context, cfg Config, args ...string) (string, error) {
	cmd := newCmd(ctx, cfg.ADBPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("adb %v: %w\n%s", args, err, string(out))
	}
	if cfg.GlobalDelay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(cfg.GlobalDelay):
		}
	}
	return strings.TrimSpace(string(out)), nil
}

func adb(ctx context.Context, serial string, cfg Config, args ...string) (string, error) {
	full := append([]string{"-s", serial}, args...)
	return runAdb(ctx, cfg, full...)
}

func discoverLocalEmulatorADBTargets() []string {
	type scanRange struct {
		start, end, step int
	}
	// 偶数・奇数両方をスキャンして MuMu Player 12 / 標準エミュレーターに対応する。
	// localhost の場合、閉じているポートは ECONNREFUSED で即時返るため遅延は最小限。
	// 5554-5680: 標準エミュレーター最大64台分の全ポートをカバー。
	ranges := []scanRange{
		{5554, 5680, 1},
	}
	var targets []string
	for _, r := range ranges {
		for port := r.start; port <= r.end; port += r.step {
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			conn, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
			if err != nil {
				continue
			}
			_ = conn.Close()
			targets = append(targets, addr)
		}
	}
	return targets
}

func ConnectConfiguredSerials(ctx context.Context, cfg Config) {
	targets := make([]string, 0, len(cfg.ConnectSerials))
	for _, s := range cfg.ConnectSerials {
		t := strings.TrimSpace(s)
		if t != "" {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		targets = discoverLocalEmulatorADBTargets()
		if len(targets) > 0 {
			log.Printf("[MuMu] 自動検出したローカルADB候補: %v", targets)
		}
	}
	if len(targets) == 0 {
		return
	}
	workerCount := len(targets)
	if workerCount > 4 {
		workerCount = 4
	}

	jobs := make(chan string)
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range jobs {
				if ctx.Err() != nil {
					return
				}
				serial := strings.TrimSpace(raw)
				if serial == "" {
					continue
				}
				// adb connect は host:port 形式のみ対象にする。
				if !strings.Contains(serial, ":") {
					continue
				}
				out, err := newCmd(ctx, cfg.ADBPath, "connect", serial).CombinedOutput()
				msg := strings.TrimSpace(string(out))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if msg != "" {
						log.Printf("[MuMu] adb connect %s 失敗: %s", serial, msg)
					} else {
						log.Printf("[MuMu] adb connect %s 失敗: %v", serial, err)
					}
					continue
				}
				if msg != "" {
					log.Printf("[MuMu] adb connect %s: %s", serial, msg)
				}
			}
		}()
	}

	for _, raw := range targets {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- raw:
		}
	}
	close(jobs)
	wg.Wait()
}

// EnsureServer は ADB サーバーが起動していることを確認し、未起動なら起動する。
// RestartServer と異なり kill-server は行わないため既存接続を維持できる。
func EnsureServer(ctx context.Context, cfg Config) error {
	log.Println("[MuMu] adb start-server...")
	out, err := newCmd(ctx, cfg.ADBPath, "start-server").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("adb start-server: %w\n%s", err, string(out))
	}
	log.Println("[MuMu] ADB サーバー起動完了")
	ConnectConfiguredSerials(ctx, cfg)
	return nil
}

// RestartServer は adb kill-server → adb start-server でADBサーバーを再起動する。
// ADB接続が切れた場合の復旧に使用する。
func RestartServer(ctx context.Context, cfg Config) error {
	log.Println("[MuMu] adb kill-server...")
	// kill-server は失敗しても無視（既に停止済みの場合あり）
	_ = newCmd(ctx, cfg.ADBPath, "kill-server").Run()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}

	log.Println("[MuMu] adb start-server...")
	out, err := newCmd(ctx, cfg.ADBPath, "start-server").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("adb start-server: %w\n%s", err, string(out))
	}
	log.Println("[MuMu] ADBサーバー再起動完了")
	ConnectConfiguredSerials(ctx, cfg)
	// MuMu 独自 ADB では kill-server 後に全インスタンスが再登録するまで数秒かかる。
	// 再起動後もしばらくポーリングして 1 台以上を確認する。
	devices, waitErr := WaitForDevices(ctx, cfg, 15*time.Second)
	if waitErr != nil {
		return waitErr
	}
	if len(devices) == 0 {
		return fmt.Errorf("adb restart completed but no devices became available within 15s")
	}
	log.Printf("[MuMu] ADB再起動後デバイス確認: %d台", len(devices))
	return nil
}

// RecoverServer は既存接続をなるべく維持したまま ADB 復旧を試みる。
// まず start-server + reconnect + connect を実行し、改善しない場合のみ RestartServer にフォールバックする。
func RecoverServer(ctx context.Context, cfg Config) error {
	log.Println("[MuMu] ADB復旧開始（非破壊）...")
	if err := EnsureServer(ctx, cfg); err != nil {
		log.Printf("[MuMu] ADB復旧: start-server 失敗: %v", err)
	}

	// 既存トランスポートの再接続を試す（未対応環境では失敗しても継続）。
	out, err := newCmd(ctx, cfg.ADBPath, "reconnect").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := strings.TrimSpace(string(out)); msg != "" {
			log.Printf("[MuMu] adb reconnect: %s", msg)
		}
	} else {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			log.Printf("[MuMu] adb reconnect: %s", msg)
		}
	}

	ConnectConfiguredSerials(ctx, cfg)
	devices, waitErr := WaitForDevices(ctx, cfg, 20*time.Second)
	if waitErr == nil && len(devices) > 0 {
		log.Printf("[MuMu] ADB復旧成功（非破壊）: %d台", len(devices))
		return nil
	}

	log.Println("[MuMu] ADB復旧: 非破壊手順で改善せず、ハード再起動にフォールバックします")
	return RestartServer(ctx, cfg)
}

// ListDevices は接続中のADBデバイス一覧を返す。
// ADBサーバーの再起動は行わない（通常の取得に使用）。
func ListDevices(ctx context.Context, cfg Config) ([]string, error) {
	devices, err := listDevicesOnce(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(devices) > 0 {
		log.Printf("[MuMu] 有効デバイス: %v (%d台)", devices, len(devices))
	}
	return devices, nil
}

func listDevicesOnce(ctx context.Context, cfg Config) ([]string, error) {
	out, err := runAdb(ctx, cfg, "devices")
	if err != nil {
		log.Printf("[MuMu] adb devices 失敗: %v", err)
		return nil, err
	}
	debuglog.Vlogf("ADB", "adb devices raw:\n%s", out)

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

	// emulator-* エントリが存在するとき、対応する emulator-* を持たない
	// TCP デバイス（ゴースト接続）を除外する。
	// ただし MuMu Player 12 の標準ポートレンジ（奇数 5555–5679）は
	// emulator-* ペアが無くても本物として保持する。
	// MuMu 独自 ADB は台数によって emulator-* 形式で登録しないインスタンスがあるため。
	emulatorCanonicals := map[string]bool{}
	for _, d := range devices {
		if strings.HasPrefix(d, "emulator-") {
			emulatorCanonicals[canonicalADBSerial(d)] = true
		}
	}
	if len(emulatorCanonicals) > 0 {
		filtered := devices[:0]
		for _, d := range devices {
			keep := strings.HasPrefix(d, "emulator-") ||
				emulatorCanonicals[canonicalADBSerial(d)] ||
				isMuMuADBPort(d)
			if keep {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	normalized := normalizeDeviceList(devices)
	selected := selectDevicesForPatrol(normalized, cfg.ConnectSerials)

	if len(cfg.ConnectSerials) > 0 && len(selected) < len(cfg.ConnectSerials) {
		log.Printf("[MuMu] 設定シリアル %d台のうち %d台のみ検出", len(cfg.ConnectSerials), len(selected))
	}
	return selected, nil
}

// isMuMuADBPort は MuMu Player 12 の標準 ADB ポートレンジ（127.0.0.1 奇数 5555–5679）か返す。
func isMuMuADBPort(serial string) bool {
	host, portStr, err := net.SplitHostPort(serial)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return port >= 5555 && port <= 5679 && port%2 == 1
}

func canonicalADBSerial(serial string) string {
	trimmed := strings.TrimSpace(serial)
	if strings.HasPrefix(trimmed, "emulator-") {
		portStr := strings.TrimPrefix(trimmed, "emulator-")
		if port, err := strconv.Atoi(portStr); err == nil && port > 0 {
			// emulator-5580 と 127.0.0.1:5581 を同一実体として扱う。
			if port%2 == 0 {
				return fmt.Sprintf("127.0.0.1:%d", port+1)
			}
		}
	}
	return trimmed
}

func normalizeDeviceList(devices []string) []string {
	type entry struct {
		serial string
		score  int
	}
	chosen := map[string]entry{}
	for _, d := range devices {
		s := strings.TrimSpace(d)
		if s == "" {
			continue
		}
		canon := canonicalADBSerial(s)
		score := 0
		if strings.Contains(s, ":") {
			score = 1 // host:port を emulator-* より優先
		}
		prev, ok := chosen[canon]
		if !ok || score > prev.score {
			chosen[canon] = entry{serial: s, score: score}
		}
	}
	out := make([]string, 0, len(chosen))
	for _, v := range chosen {
		out = append(out, v.serial)
	}
	sort.Strings(out)
	return out
}

func selectDevicesForPatrol(devices []string, configured []string) []string {
	targets := make([]string, 0, len(configured))
	seen := map[string]struct{}{}
	for _, c := range configured {
		t := canonicalADBSerial(c)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return devices
	}

	available := map[string]string{}
	for _, d := range devices {
		canon := canonicalADBSerial(d)
		if canon == "" {
			continue
		}
		if _, ok := available[canon]; !ok {
			available[canon] = d
		}
	}

	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if d, ok := available[t]; ok {
			out = append(out, d)
		}
	}
	return out
}

// WaitForDevices は adb devices を一定時間ポーリングし、1台以上検出できたら返す。
// waitFor 内に検出できない場合は空スライスを返す。
func WaitForDevices(ctx context.Context, cfg Config, waitFor time.Duration) ([]string, error) {
	if waitFor <= 0 {
		return listDevicesOnce(ctx, cfg)
	}

	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		devices, err := listDevicesOnce(ctx, cfg)
		if err == nil && len(devices) > 0 {
			return devices, nil
		}
		if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return nil, lastErr
			}
			return []string{}, nil
		case <-ticker.C:
		}
	}
}

// SwitchChannel は指定デバイスを指定チャンネルに切り替える。
func SwitchChannel(ctx context.Context, serial string, channel uint32, cfg Config) error {
	return switchChannelOnce(ctx, serial, channel, cfg)
}

func switchChannelOnce(ctx context.Context, serial string, channel uint32, cfg Config) error {
	log.Printf("[MuMu] switch_channel: serial=%s channel=%d", serial, channel)

	// Pキーでチャンネル入力を開く
	if cfg.PreKeycode != "" {
		if _, err := adb(ctx, serial, cfg, "shell", "input", "keyevent", cfg.PreKeycode); err != nil {
			return fmt.Errorf("pre_keycode: %w", err)
		}
	}

	// タップで入力欄をフォーカス
	if cfg.TapX > 0 && cfg.TapY > 0 {
		tapArgs := []string{"shell", "input", "tap",
			fmt.Sprintf("%d", cfg.TapX),
			fmt.Sprintf("%d", cfg.TapY),
		}
		if _, err := adb(ctx, serial, cfg, tapArgs...); err != nil {
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
	if _, err := adb(ctx, serial, cfg, args...); err != nil {
		return fmt.Errorf("clear+input: %w", err)
	}
	// Enterで確定
	if _, err := adb(ctx, serial, cfg, "shell", "input", "keyevent", "KEYCODE_ENTER"); err != nil {
		return fmt.Errorf("enter: %w", err)
	}

	// Pキーでチャンネル入力を閉じる（満員時のダイアログも閉じる）
	if cfg.PreKeycode != "" {
		if _, err := adb(ctx, serial, cfg, "shell", "input", "keyevent", cfg.PreKeycode); err != nil {
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
func SwitchGroup(ctx context.Context, serials []string, start, end int, channel uint32, cfg Config, results map[string]error, mu *sync.Mutex) {
	var wg sync.WaitGroup
	for i := start; i < end; i++ {
		s := serials[i]
		wg.Add(1)
		go func(serial string) {
			defer wg.Done()
			err := SwitchChannel(ctx, serial, channel, cfg)
			mu.Lock()
			results[serial] = err
			mu.Unlock()
		}(s)
	}
	wg.Wait()
}

// GetDeviceIP は指定デバイスの仮想NWインターフェースIPを返す。
// "adb -s <serial> shell ip route get 1" の出力から src フィールドをパースする。
func GetDeviceIP(ctx context.Context, serial string, cfg Config) (string, error) {
	cmd := newCmd(ctx, cfg.ADBPath, "-s", serial, "shell", "ip", "route", "get", "1")
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

// ───── タップループ ─────

// TapLoopStatus はタップループの現在状態
type TapLoopStatus struct {
	Running    bool     `json:"running"`
	TapX       int      `json:"tap_x"`
	TapY       int      `json:"tap_y"`
	IntervalMs int      `json:"interval_ms"`
	Serials    []string `json:"serials"`
	TickCount  int64    `json:"tick_count"`
}

// TapLooper は指定座標への定期タップをADB経由でループ実行する。
type TapLooper struct {
	mu     sync.RWMutex
	status TapLoopStatus
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewTapLooper は TapLooper を生成する。
func NewTapLooper() *TapLooper {
	return &TapLooper{}
}

// Status は現在の状態をスレッドセーフに返す。
func (t *TapLooper) Status() TapLoopStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

// Stop はループを停止し、goroutine の終了を待つ。
func (t *TapLooper) Stop() {
	t.mu.Lock()
	cancel := t.cancel
	t.cancel = nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
}

// Start は指定シリアル群に対して tapX,tapY を intervalMs ミリ秒おきにタップし続ける。
// 既に実行中の場合は停止してから再起動する。intervalMs の最小値は 100ms。
func (t *TapLooper) Start(cfg Config, serials []string, tapX, tapY, intervalMs int) {
	t.Stop()
	if intervalMs < 100 {
		intervalMs = 100
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancel = cancel
	t.status = TapLoopStatus{
		Running:    true,
		TapX:       tapX,
		TapY:       tapY,
		IntervalMs: intervalMs,
		Serials:    append([]string(nil), serials...),
	}
	t.mu.Unlock()

	t.wg.Add(1)
	go func() {
		defer func() {
			t.mu.Lock()
			t.status.Running = false
			t.mu.Unlock()
			t.wg.Done()
			log.Println("[TapLoop] 終了")
		}()
		log.Printf("[TapLoop] 開始: serials=%v tap=(%d,%d) interval=%dms", serials, tapX, tapY, intervalMs)
		ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
		defer ticker.Stop()
		xStr := fmt.Sprintf("%d", tapX)
		yStr := fmt.Sprintf("%d", tapY)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var wg sync.WaitGroup
				for _, s := range serials {
					wg.Add(1)
					serial := s
					go func() {
						defer wg.Done()
						if _, err := adb(ctx, serial, cfg, "shell", "input", "tap", xStr, yStr); err != nil && ctx.Err() == nil {
							log.Printf("[TapLoop] adb tap %s (%d,%d): %v", serial, tapX, tapY, err)
						}
					}()
				}
				wg.Wait()
				t.mu.Lock()
				t.status.TickCount++
				t.mu.Unlock()
			}
		}
	}()
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

// DeviceStatus はADBシリアル単位のデバイス状態を保持する。
type DeviceStatus struct {
	Serial             string  `json:"serial"`
	Label              string  `json:"label"`                 // 紐づけ済みラベル（"Instance-N"等、空=未紐づけ）
	CurrentCh          uint32  `json:"current_ch"`            // 最後にロード完了したch
	LastLoadSecs       float64 `json:"last_load_secs"`        // 最後のロード完了時間（秒）
	TimedOutLast       bool    `json:"timed_out_last"`        // 直近サイクルでタイムアウト
	ConsecutiveTimeout int     `json:"consecutive_timeout"`   // 連続タイムアウト回数
	ADBFailed          bool    `json:"adb_failed"`            // ADB切替コマンド失敗
	GameCrashed        bool    `json:"game_crashed"`          // pidof でゲームプロセス死亡確認済み
	Recovering         bool    `json:"recovering"`            // 復帰処理実行中
	LastRecoveredAtMs  int64   `json:"last_recovered_at_ms"`  // 最後の復帰試行完了時刻
	ExpectedCh         uint32  `json:"expected_ch"`           // 直近 ADB 命令CH
	ActualCh           uint32  `json:"actual_ch"`             // パケット観測の実CH（LineId）
	Mismatch           bool    `json:"mismatch"`              // ExpectedCh != ActualCh かつ観測済み
	LastObservedAt     int64   `json:"last_observed_at_ms"`   // ActualCh 最終観測時刻(UnixMilli)
}

// chPendingProbe は ADB 切替コマンドと 0x15/0x16 LineID 観測を突合するための暫定記録
type chPendingProbe struct {
	serial   string
	targetCh uint32
	sentAt   time.Time
}

// moveSignalMsg は [0x2E] パケット受信時のシグナル（インスタンスラベル付き）
type moveSignalMsg struct {
	t      time.Time
	label  string // serial
	lineID uint32 // 信号発生時の lineID（0 = 不明・GUI 経由互換）
}

// PatrolStatus は現在の巡回状態
// GroupAssignment はデバイスペアとそれが担当するchリストを保持する。
type GroupAssignment struct {
	Group         int      `json:"group"`
	DeviceIndices []int    `json:"device_indices"`
	Channels      []uint32 `json:"channels"`
}

// DeviceAssignments は device_assignments.json のファイル形式。
type DeviceAssignments struct {
	Assignments []GroupAssignment `json:"assignments"`
}

// ComputeDeviceAssignments はデバイス台数に応じてペア単位のch分担を計算する。
// totalCh 本のチャンネルをペア数で等分する。奇数台の場合は最後の1台がアイドル。
func ComputeDeviceAssignments(deviceCount, totalCh int) []GroupAssignment {
	if deviceCount <= 0 || totalCh <= 0 {
		return nil
	}
	allChs := make([]uint32, totalCh)
	for i := range allChs {
		allChs[i] = uint32(i + 1)
	}
	if deviceCount == 1 {
		return []GroupAssignment{{Group: 0, DeviceIndices: []int{0}, Channels: allChs}}
	}
	pairs := deviceCount / 2
	chPerGroup := totalCh / pairs
	var assignments []GroupAssignment
	for g := 0; g < pairs; g++ {
		start := g * chPerGroup
		end := start + chPerGroup
		if g == pairs-1 {
			end = totalCh // 最終グループは余りも含む
		}
		assignments = append(assignments, GroupAssignment{
			Group:         g,
			DeviceIndices: []int{g * 2, g*2 + 1},
			Channels:      append([]uint32{}, allChs[start:end]...),
		})
	}
	return assignments
}

// SaveDeviceAssignments は分担設定をJSONファイルに保存する。
func SaveDeviceAssignments(file string, assignments []GroupAssignment) error {
	data, err := json.MarshalIndent(DeviceAssignments{Assignments: assignments}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0644)
}

// LoadDeviceAssignments はJSONファイルから分担設定を読み込む。
func LoadDeviceAssignments(file string) ([]GroupAssignment, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var da DeviceAssignments
	if err := json.Unmarshal(data, &da); err != nil {
		return nil, err
	}
	return da.Assignments, nil
}

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
	SwitchStartedAtUnixMs int64   `json:"switch_started_at_unix_ms"` // 現在スイッチ開始時刻
	AvgSignalLatencySecs  float64 `json:"avg_signal_latency_secs"`   // 0x2E到達レイテンシ平均(秒)
	MoveFailedChannels       []uint32 `json:"move_failed_channels"`          // 移動失敗（完了シグナルなし）でスキップしたch一覧
	ConsecutiveMoveFailCount int      `json:"consecutive_move_fail_count"` // 連続して移動失敗スキップしたch数（クラッシュ検知用）
	KnownInstances       []string       `json:"known_instances"`        // 認識済みUID一覧
	CrashedInstances     []string       `json:"crashed_instances"`      // クラッシュ判定中のUID
	DeviceStatuses       []DeviceStatus `json:"device_statuses"`        // デバイス別状態（シリアル単位）
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
	wg               sync.WaitGroup // 巡回goroutineの完了待機用
	status           PatrolStatus
	ctx              context.Context
	cancel           context.CancelFunc
	lastChannel      uint32             // 最後に巡回したチャンネル（再開位置の計算に使用）
	moveSignal       chan moveSignalMsg // [0x2E]パケット受信シグナル（UID付き）
	onChannelSwitch  func(uint32)       // チャンネル切替完了時コールバック
	onProbeMode      func(bool)         // Identify(runStaggerProbe) 開始/終了時コールバック
	onPatrolActive   func(bool)         // 巡回 Start/Stop 時コールバック
	knownInstances   map[string]bool    // 一度でも応答したUID
	missedCounts     map[string]int     // UID別連続未応答カウント
	crashedInstances map[string]bool    // クラッシュ判定済みUID（3回連続未応答）
	loadHistory      []float64          // 直近N回の実ロード完了時間（秒）
	loadHistMu       sync.Mutex

	deviceStatuses   map[string]*DeviceStatus // serial → デバイス状態
	deviceStatusesMu sync.RWMutex
	serialIPCache    map[string]string        // serial → IP（TTLキャッシュ）
	serialIPCacheAt  map[string]time.Time
	serialIPCacheMu  sync.Mutex

	// serial ↔ userUID 動的バインドマップ（Probe-and-Match で確立）
	serialToUID     map[string]uint64
	uidToSerial     map[uint64]string
	serialUIDMu     sync.RWMutex
	pendingProbes   map[string]chPendingProbe // serial → 直近の切替予約
	pendingProbesMu sync.Mutex

	// serial_to_uid 永続化コールバック
	saveSerialUIDFn func(map[string]uint64)

	// excludeUIDs はバインド候補から除外する UID セット（誤バインド防止）
	excludeUIDs   map[uint64]bool
	excludeUIDsMu sync.RWMutex

	// serial_to_label 自動確立マップと永続化コールバック
	serialToLabel    map[string]string
	labelToSerial    map[string]string // Instance label → serial（serialToLabel の逆引き）
	serialLabelMu    sync.RWMutex
	saveSerialLabelFn func(map[string]string)

	// deviceAssignments はデバイスペア別のch分担設定。空の場合は全デバイスが全chを巡回。
	deviceAssignments   []GroupAssignment
	deviceAssignmentsMu sync.RWMutex

	// postLoadLatencies は 0x2E UUID 受信に基づくロード時間サンプル（T_0x2E - T_switchStart 秒）
	postLoadLatencies []float64
	postLoadMu        sync.Mutex
	// switchStartTimes は serial → 直近の switch_channel 発行時刻（autoDelay 計算用）
	switchStartTimes   map[string]time.Time
	switchStartTimesMu sync.Mutex

	// postLoadCancels は NotifyPostLoadReady で起動した待機 goroutine の cancel 関数。
	// 同 serial の前回 goroutine をキャンセルして重複起動・リークを防ぐ。
	postLoadCancels  map[string]context.CancelFunc
	postLoadCancelMu sync.Mutex
}

// NotifyChMovePacket は guiWriter が "[0x2E] UUID=" ログ行を検出したとき呼ばれる fallback 経路。
// uid が判明している場合は除外UID・バインドを検証し、phantom シグナル（同一PC上の
// ネイティブクライアント等）を弾く。uid==0（解析不能な行）は従来動作を維持する。
func (p *Patroller) NotifyChMovePacket(instanceLabel string, uid uint64) {
	// No.80: screen モードでは 0x2E ログ起点の即時 moveSignal を無効化する
	// （ch切替起点の画面判定のみで完了を判定するため）。
	p.mu.RLock()
	cmMode := strings.ToLower(p.cfg.LoadDetectMode)
	p.mu.RUnlock()
	if cmMode == "screen" {
		return
	}

	if uid != 0 {
		// 除外 UID → 破棄
		p.excludeUIDsMu.RLock()
		excluded := p.excludeUIDs[uid]
		p.excludeUIDsMu.RUnlock()
		if excluded {
			return
		}
		// バインド済み uid → 確実な serial で送信
		p.serialUIDMu.RLock()
		serial := p.uidToSerial[uid]
		p.serialUIDMu.RUnlock()
		if serial != "" {
			p.notifyMoveSignal(serial, 0, time.Now())
			return
		}
		// uid 判明かつ未バインド → 巡回デバイスに紐付けられないため破棄（phantom 防止）
		return
	}
	// uid 不明: 従来どおり label → serial 解決で送信（既存コードをそのまま残す）
	if instanceLabel == "" {
		return
	}
	serial := instanceLabel
	p.serialLabelMu.RLock()
	if p.labelToSerial != nil {
		if s := p.labelToSerial[instanceLabel]; s != "" {
			serial = s
		}
	}
	p.serialLabelMu.RUnlock()
	p.notifyMoveSignal(serial, 0, time.Now())
}

// loadStabilizationDelay は 0x2E UUID 着信から完了シグナルを発火するまでの遅延を返す。
// LoadStabilizationAuto=true の場合は直近観測値から自動算出する。
func (p *Patroller) loadStabilizationDelay() time.Duration {
	p.mu.RLock()
	auto := p.cfg.LoadStabilizationAuto
	d := p.cfg.LoadStabilizationDuration
	p.mu.RUnlock()
	if auto {
		return p.autoStabilizationDelay()
	}
	if d > 0 {
		return d
	}
	return 6 * time.Second
}

// autoStabilizationDelay は直近の postLoadLatencies（T_0x2E - T_switchStart）サンプルから
// 遅延を自動算出する。
// 0x2E UUID はロード 75% 到達時に届く想定。残り 25% + 操作可能までの余裕として
// P90 の 40% を使う（クランプ: 2s – 15s）。サンプル不足時は 6s。
func (p *Patroller) autoStabilizationDelay() time.Duration {
	p.postLoadMu.Lock()
	samples := make([]float64, len(p.postLoadLatencies))
	copy(samples, p.postLoadLatencies)
	p.postLoadMu.Unlock()

	if len(samples) == 0 {
		return 6 * time.Second
	}
	// 昇順ソートして P90 を取得
	sort.Float64s(samples)
	p90idx := int(float64(len(samples)) * 0.9)
	if p90idx >= len(samples) {
		p90idx = len(samples) - 1
	}
	p90 := samples[p90idx]
	delaySecs := p90 * 0.4
	const minDelay, maxDelay = 2.0, 15.0
	if delaySecs < minDelay {
		delaySecs = minDelay
	}
	if delaySecs > maxDelay {
		delaySecs = maxDelay
	}
	return time.Duration(delaySecs * float64(time.Second))
}

// postLoadLatencyP90 は直近の 0x2E 到達レイテンシ（switchStartAt からの秒数）の P90 を返す。
// サンプルなし時は 0 を返す。dwell 延長の推定最大ロード時間計算に使用する。
func (p *Patroller) postLoadLatencyP90() float64 {
	p.postLoadMu.Lock()
	samples := make([]float64, len(p.postLoadLatencies))
	copy(samples, p.postLoadLatencies)
	p.postLoadMu.Unlock()
	if len(samples) == 0 {
		return 0
	}
	sort.Float64s(samples)
	idx := int(float64(len(samples)) * 0.9)
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}

// recordPostLoadLatency は 0x2E UUID 受信時刻と serial の switchStartTime の差分を記録する。
func (p *Patroller) recordPostLoadLatency(serial string, t time.Time, window int) {
	p.switchStartTimesMu.Lock()
	startAt, ok := p.switchStartTimes[serial]
	p.switchStartTimesMu.Unlock()
	if !ok || startAt.IsZero() {
		return
	}
	secs := t.Sub(startAt).Seconds()
	if secs < 0 || secs > 120 {
		return // 異常値除外
	}
	if window <= 0 {
		window = 10
	}
	p.postLoadMu.Lock()
	p.postLoadLatencies = append(p.postLoadLatencies, secs)
	if len(p.postLoadLatencies) > window {
		p.postLoadLatencies = p.postLoadLatencies[len(p.postLoadLatencies)-window:]
	}
	p.postLoadMu.Unlock()
}

// NotifyPostLoadReady は 0x2E UUID 受信時に ncap から呼ばれる（ロード 75% シグナル）。
// バインド済み serial に対し、loadStabilizationDelay 後に moveSignal を発火する。
func (p *Patroller) NotifyPostLoadReady(uid uint64, lineID uint32, t time.Time) {
	if uid == 0 || lineID == 0 {
		return
	}
	// 除外 UID チェック
	p.excludeUIDsMu.RLock()
	excluded := p.excludeUIDs[uid]
	p.excludeUIDsMu.RUnlock()
	if excluded {
		return
	}
	// serial 解決
	p.serialUIDMu.RLock()
	serial := p.uidToSerial[uid]
	p.serialUIDMu.RUnlock()
	if serial == "" {
		return // バインド前は対象外
	}
	// ラテンシ記録
	p.mu.RLock()
	window := p.cfg.AdaptiveTimeoutWindow
	p.mu.RUnlock()
	p.recordPostLoadLatency(serial, t, window)

	delay := p.loadStabilizationDelay()
	fireAt := t.Add(delay)

	p.mu.RLock()
	cfg := p.cfg
	parentCtx := p.ctx
	p.mu.RUnlock()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	mode := strings.ToLower(cfg.LoadDetectMode)
	debuglog.Vlogf("0x2E", "[PostLoadReady] serial=%s uid=%d lineID=%d mode=%s delay=%.1fs",
		serial, uid, lineID, mode, delay.Seconds())

	// 同 serial の前回 goroutine をキャンセルし、新規 ctx を発行する。
	ctx, cancel := context.WithCancel(parentCtx)
	p.postLoadCancelMu.Lock()
	if p.postLoadCancels == nil {
		p.postLoadCancels = make(map[string]context.CancelFunc)
	}
	if prev := p.postLoadCancels[serial]; prev != nil {
		prev()
	}
	p.postLoadCancels[serial] = cancel
	p.postLoadCancelMu.Unlock()

	go func() {
		// 戦略終了時に自分の ctx を必ず cancel する（CancelFunc は冪等なので
		// 上書き後の前任 cancel ハンドラから呼ばれていても無害）。
		defer cancel()
		switch mode {
		case "screen":
			// No.80: screen は ch切替起点（runScreenStrategyAtSwitch）で起動するため
			// 0x2E 起点では何もしない（二重 moveSignal 防止）。
			return
		case "either":
			p.runEitherStrategy(ctx, serial, lineID, fireAt, cfg)
		default: // "time" または未知値
			p.runTimeStrategy(ctx, serial, lineID, fireAt)
		}
	}()
}

// notifyMoveSignal は serial を label として moveSignal チャネルに t 付きで送信する。
// 巡回中でない場合・バッファ満杯の場合は何もしない（ブロックしない）。
func (p *Patroller) notifyMoveSignal(serial string, lineID uint32, t time.Time) {
	if serial == "" {
		return
	}
	p.mu.RLock()
	running := p.status.Running
	ch := p.moveSignal
	p.mu.RUnlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- moveSignalMsg{t: t, label: serial, lineID: lineID}:
	default: // バッファ満杯なら捨てる（ブロックしない）
	}
}

// NewPatroller はPatrollerを作成する
func NewPatroller(cfg Config) *Patroller {
	return &Patroller{cfg: cfg}
}

// SetDeviceAssignments はデバイス分担設定を動的に更新する。次回巡回開始時から有効。
func (p *Patroller) SetDeviceAssignments(assignments []GroupAssignment) {
	p.deviceAssignmentsMu.Lock()
	p.deviceAssignments = assignments
	p.deviceAssignmentsMu.Unlock()
}

// SetOnChannelSwitch は巡回が次chへ切り替える際（ADB発行前・switchStartAt 時点）に呼ばれる
// コールバックを設定する。CapDevice への現在chの通知に使用する（No.74: portMap 投票が
// 再接続時に正しいchを参照できるよう、SwitchGroup より前に通知する）。
func (p *Patroller) SetOnChannelSwitch(fn func(uint32)) {
	p.mu.Lock()
	p.onChannelSwitch = fn
	p.mu.Unlock()
}

// SetOnProbeMode は Identify(runStaggerProbe) の開始・終了時に呼ばれるコールバックを設定する。
// true=開始, false=終了。CapDevice の probeMode 切替（SetProbeMode）に使用する。
func (p *Patroller) SetOnProbeMode(fn func(bool)) {
	p.mu.Lock()
	p.onProbeMode = fn
	p.mu.Unlock()
}

// SetOnPatrolActive は巡回の開始・停止時に呼ばれるコールバックを設定する。
// true=開始, false=停止。CapDevice の patrolActive 切替（SetPatrolActive）に使用する。
func (p *Patroller) SetOnPatrolActive(fn func(bool)) {
	p.mu.Lock()
	p.onPatrolActive = fn
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

// Status は現在の巡回状態を返す。DeviceStatuses は別ロックから live マージする。
func (p *Patroller) Status() PatrolStatus {
	p.mu.RLock()
	s := p.status
	p.mu.RUnlock()
	s.DeviceStatuses = p.GetDeviceStatuses()
	p.postLoadMu.Lock()
	if n := len(p.postLoadLatencies); n > 0 {
		var sum float64
		for _, v := range p.postLoadLatencies {
			sum += v
		}
		s.AvgSignalLatencySecs = sum / float64(n)
	}
	p.postLoadMu.Unlock()
	return s
}

// ClearMoveFailedChannels は移動失敗リストをクリアする
func (p *Patroller) ClearMoveFailedChannels() {
	p.mu.Lock()
	p.status.MoveFailedChannels = nil
	p.mu.Unlock()
	log.Println("[MuMu] 移動失敗チャンネルリストをクリアしました")
}

// recordLoadTime は全台ロード完了時間（秒）を履歴に追記する。
func (p *Patroller) recordLoadTime(secs float64, window int) {
	if window <= 0 {
		window = 10
	}
	p.loadHistMu.Lock()
	defer p.loadHistMu.Unlock()
	p.loadHistory = append(p.loadHistory, secs)
	if len(p.loadHistory) > window {
		p.loadHistory = p.loadHistory[len(p.loadHistory)-window:]
	}
}

// adaptiveMaxLoad は直近ロード履歴の最大値×1.5 と base の大きい方を返す。
// 履歴がない場合は base をそのまま返す。
func (p *Patroller) adaptiveMaxLoad(base time.Duration) time.Duration {
	p.loadHistMu.Lock()
	defer p.loadHistMu.Unlock()
	if len(p.loadHistory) == 0 {
		return base
	}
	var maxT float64
	for _, t := range p.loadHistory {
		if t > maxT {
			maxT = t
		}
	}
	adaptive := time.Duration(maxT * 1.5 * float64(time.Second))
	if adaptive > base {
		return adaptive
	}
	return base
}

// GetDeviceStatuses は現在のデバイス別状態のスナップショットを返す。
func (p *Patroller) GetDeviceStatuses() []DeviceStatus {
	p.deviceStatusesMu.RLock()
	defer p.deviceStatusesMu.RUnlock()
	out := make([]DeviceStatus, 0, len(p.deviceStatuses))
	for _, ds := range p.deviceStatuses {
		out = append(out, *ds)
	}
	return out
}

// SetDeviceCh は手動切替完了時にシリアル単位の CurrentCh を即時更新する。
func (p *Patroller) SetDeviceCh(serial string, ch uint32) {
	p.deviceStatusesMu.Lock()
	defer p.deviceStatusesMu.Unlock()
	if ds, ok := p.deviceStatuses[serial]; ok {
		ds.CurrentCh = ch
		ds.ExpectedCh = ch
	}
}

// GetSerialUID は serial に紐付いた userUID を返す（未バインドは 0）。
func (p *Patroller) GetSerialUID(serial string) uint64 {
	p.serialUIDMu.RLock()
	defer p.serialUIDMu.RUnlock()
	return p.serialToUID[serial]
}

// GetUIDSerial は userUID に紐付いた serial を返す（未バインドは ""）。
func (p *Patroller) GetUIDSerial(uid uint64) string {
	p.serialUIDMu.RLock()
	defer p.serialUIDMu.RUnlock()
	return p.uidToSerial[uid]
}

// HasBinding は少なくとも 1 件の serial↔UID バインドが確立しているか返す。
func (p *Patroller) HasBinding() bool {
	p.serialUIDMu.RLock()
	defer p.serialUIDMu.RUnlock()
	return len(p.serialToUID) > 0
}

// DeleteSerialUIDBinding は serial に紐付いた UID バインドを削除し config.json に書き戻す。
func (p *Patroller) DeleteSerialUIDBinding(serial string) {
	p.serialUIDMu.Lock()
	uid := p.serialToUID[serial]
	delete(p.serialToUID, serial)
	if uid != 0 {
		delete(p.uidToSerial, uid)
	}
	saveFn := p.saveSerialUIDFn
	snapshot := make(map[string]uint64, len(p.serialToUID))
	for k, v := range p.serialToUID {
		snapshot[k] = v
	}
	p.serialUIDMu.Unlock()
	log.Printf("[Patroller] serial_to_uid 削除: serial=%s uid=%d", serial, uid)
	if saveFn != nil {
		go saveFn(snapshot)
	}
}

// SetExcludeUIDs はバインド候補から除外する UID リストを設定する。
// MatchLineChange の冒頭で該当 uid を弾き、誤バインドを防ぐ。
func (p *Patroller) SetExcludeUIDs(uids []uint64) {
	p.excludeUIDsMu.Lock()
	defer p.excludeUIDsMu.Unlock()
	p.excludeUIDs = make(map[uint64]bool, len(uids))
	for _, uid := range uids {
		p.excludeUIDs[uid] = true
	}
}

// LoadSerialUIDMap は config.json から読み込んだ serial_to_uid マップを初期設定する。
func (p *Patroller) LoadSerialUIDMap(m map[string]uint64) {
	if len(m) == 0 {
		return
	}
	p.serialUIDMu.Lock()
	defer p.serialUIDMu.Unlock()
	if p.serialToUID == nil {
		p.serialToUID = make(map[string]uint64)
	}
	if p.uidToSerial == nil {
		p.uidToSerial = make(map[uint64]string)
	}
	for serial, uid := range m {
		p.serialToUID[serial] = uid
		p.uidToSerial[uid] = serial
	}
	log.Printf("[Patroller] serial_to_uid: %d 件ロード済み", len(m))
}

// SetSaveSerialUIDFn は serial↔UID バインド成立時に config.json へ書き戻す関数を設定する。
func (p *Patroller) SetSaveSerialUIDFn(fn func(map[string]uint64)) {
	p.serialUIDMu.Lock()
	p.saveSerialUIDFn = fn
	p.serialUIDMu.Unlock()
}

// LoadSerialLabelMap は config.json から読み込んだ serial_to_label マップを初期設定する。
func (p *Patroller) LoadSerialLabelMap(m map[string]string) {
	if len(m) == 0 {
		return
	}
	p.serialLabelMu.Lock()
	defer p.serialLabelMu.Unlock()
	if p.serialToLabel == nil {
		p.serialToLabel = make(map[string]string)
	}
	if p.labelToSerial == nil {
		p.labelToSerial = make(map[string]string)
	}
	for serial, label := range m {
		p.serialToLabel[serial] = label
		if label != "" {
			p.labelToSerial[label] = serial
		}
	}
	log.Printf("[Patroller] serial_to_label: %d 件ロード済み", len(m))
}

// GetSerialMaps は serial→uid と serial→label の両マップのコピーを返す。
func (p *Patroller) GetSerialMaps() (uidMap map[string]uint64, labelMap map[string]string) {
	p.serialUIDMu.RLock()
	uidMap = make(map[string]uint64, len(p.serialToUID))
	for k, v := range p.serialToUID {
		uidMap[k] = v
	}
	p.serialUIDMu.RUnlock()

	p.serialLabelMu.RLock()
	labelMap = make(map[string]string, len(p.serialToLabel))
	for k, v := range p.serialToLabel {
		labelMap[k] = v
	}
	p.serialLabelMu.RUnlock()
	return
}

// SetSaveSerialLabelFn は serial→label の自動確立時に config.json へ書き戻す関数を設定する。
func (p *Patroller) SetSaveSerialLabelFn(fn func(map[string]string)) {
	p.serialLabelMu.Lock()
	p.saveSerialLabelFn = fn
	p.serialLabelMu.Unlock()
}

// RecordPatrolMove は巡回ループが ADB 切替コマンドを完了した直後に呼ばれる。
// Probe-and-Match の突合に備えて serial の切替予約を記録する。
func (p *Patroller) RecordPatrolMove(serial string, targetCh uint32, t time.Time) {
	p.pendingProbesMu.Lock()
	if p.pendingProbes == nil {
		p.pendingProbes = make(map[string]chPendingProbe)
	}
	p.pendingProbes[serial] = chPendingProbe{serial: serial, targetCh: targetCh, sentAt: t}
	p.pendingProbesMu.Unlock()

	p.deviceStatusesMu.Lock()
	if p.deviceStatuses != nil {
		if ds, ok := p.deviceStatuses[serial]; ok {
			ds.ExpectedCh = targetCh
			ds.Mismatch = false
		}
	}
	p.deviceStatusesMu.Unlock()
}

// MatchLineChange は ncap が 0x15/0x16 で (uid, lineID) を観測したときに呼ばれる。
// changedAt はセッション内で lineID が変化した時刻（probe 時刻との比較に使用）。
// 未バインドの uid は pendingProbes と突合し、候補が 1 件なら serial↔UID バインドを確立する。
// バインド済みの uid は ActualCh を更新しミスマッチを検出する。
func (p *Patroller) MatchLineChange(uid uint64, lineID uint32, changedAt time.Time) {
	if uid == 0 || lineID == 0 {
		return
	}

	// 除外 UID チェック（誤バインド防止）
	p.excludeUIDsMu.RLock()
	excluded := p.excludeUIDs[uid]
	p.excludeUIDsMu.RUnlock()
	if excluded {
		debuglog.VlogfDedup("Probe", fmt.Sprintf("exclude:%d", uid), 30*time.Second, "uid=%d は除外リストのためバインドスキップ", uid)
		return
	}

	t := changedAt

	// 既バインド: ActualCh 更新
	p.serialUIDMu.RLock()
	serial := p.uidToSerial[uid]
	p.serialUIDMu.RUnlock()
	if serial != "" {
		debuglog.VlogfDedup("0x2E", "bind:"+serial, 5*time.Second, "既バインド: serial=%s lineID=%d", serial, lineID)
		p.updateActualCh(serial, lineID, t)
		// 完了シグナルは 0x2E UUID 受信経路（NotifyPostLoadReady）で発火する
		return
	}

	// 未バインド: pendingProbes から候補を探す
	const probeWindow = 60 * time.Second
	// changedAt が probe.sentAt より 2 秒超前（ADB コマンド発行前に変化済み）なら弾く
	const probeEpsilon = 2 * time.Second
	p.pendingProbesMu.Lock()
	var matched []chPendingProbe
	for _, probe := range p.pendingProbes {
		if probe.targetCh == lineID && t.Sub(probe.sentAt) <= probeWindow &&
			!changedAt.Before(probe.sentAt.Add(-probeEpsilon)) {
			p.serialUIDMu.RLock()
			alreadyBound := p.serialToUID[probe.serial] != 0
			p.serialUIDMu.RUnlock()
			if !alreadyBound {
				matched = append(matched, probe)
			}
		}
	}
	p.pendingProbesMu.Unlock()

	debuglog.VlogfDedup("Probe", fmt.Sprintf("probe:%d", uid), 5*time.Second, "uid=%d lineID=%d: matched %d pending probes", uid, lineID, len(matched))
	if len(matched) == 1 {
		bindSerial := matched[0].serial
		p.serialUIDMu.Lock()
		if p.serialToUID == nil {
			p.serialToUID = make(map[string]uint64)
		}
		if p.uidToSerial == nil {
			p.uidToSerial = make(map[uint64]string)
		}
		if p.serialToUID[bindSerial] != 0 {
			// 並走ゴルーチンが先にバインド確立済み
			p.serialUIDMu.Unlock()
			return
		}
		p.serialToUID[bindSerial] = uid
		p.uidToSerial[uid] = bindSerial
		saveFn := p.saveSerialUIDFn
		snapshot := make(map[string]uint64, len(p.serialToUID))
		for k, v := range p.serialToUID {
			snapshot[k] = v
		}
		p.serialUIDMu.Unlock()
		log.Printf("[Patroller][Bind] serial=%s ↔ uid=%d (targetCh=%d, lineID=%d)",
			bindSerial, uid, matched[0].targetCh, lineID)
		p.updateActualCh(bindSerial, lineID, t)
		// 完了シグナルは 0x2E UUID 受信経路（NotifyPostLoadReady）で発火する
		if saveFn != nil {
			go saveFn(snapshot)
		}
	}
}

// NotifyLineIDChange は巡回状態に関係なく uid→lineID 観測を常時処理する。
func (p *Patroller) NotifyLineIDChange(uid uint64, lineID uint32, changedAt time.Time) {
	p.MatchLineChange(uid, lineID, changedAt)
}

// updateActualCh はバインド済み serial の ActualCh を更新しミスマッチを検出する。
func (p *Patroller) updateActualCh(serial string, lineID uint32, t time.Time) {
	p.deviceStatusesMu.Lock()
	defer p.deviceStatusesMu.Unlock()
	if p.deviceStatuses == nil {
		p.deviceStatuses = make(map[string]*DeviceStatus)
	}
	ds, ok := p.deviceStatuses[serial]
	if !ok {
		ds = &DeviceStatus{Serial: serial}
		p.deviceStatuses[serial] = ds
	}
	ds.ActualCh = lineID
	ds.LastObservedAt = t.UnixMilli()
	ds.CurrentCh = lineID
	if ds.ExpectedCh != 0 && ds.ExpectedCh != lineID {
		if !ds.Mismatch {
			log.Printf("[Patroller][Mismatch] serial=%s expected=%d actual=%d",
				serial, ds.ExpectedCh, lineID)
		}
		ds.Mismatch = true
	} else {
		ds.Mismatch = false
	}
}

// runStaggerProbe は未バインド serial が複数ある場合に識別フェーズを実行する。
// 各未バインド serial に異なる CH を割り当て、Probe-and-Match で全台のバインドを確立する。
// 識別後は全台が probeCh にロード完了するまで待機し、adaptiveMaxLoad の初期値を設定する。
func (p *Patroller) runStaggerProbe(ctx context.Context, serials []string, channels []uint32, cfg Config) {
	p.serialUIDMu.RLock()
	var unbound []string
	for _, ser := range serials {
		if p.serialToUID[ser] == 0 {
			unbound = append(unbound, ser)
		}
	}
	p.serialUIDMu.RUnlock()
	if len(unbound) < 2 {
		return
	}
	n := len(unbound)
	log.Printf("[Patroller][Probe] %d台が未バインド - Stagger Probe 開始", n)

	// probeMode を ON にする（CapDevice が maybeSubmitPortVote で lineID を直接確定できるようにする）。
	// defer で Identify 終了時（正常・panic 両方）に OFF へ戻す。
	p.mu.RLock()
	pm := p.onProbeMode
	p.mu.RUnlock()
	if pm != nil {
		pm(true)
		defer pm(false)
	}

	// 各未バインド serial に異なる CH を割り当てる（CH プールを均等分割）
	step := len(channels) / n
	if step < 1 {
		step = 1
	}
	serialProbeCh := make(map[string]uint32, n)
	var switchMu sync.Mutex
	switchRes := make(map[string]error)
	for i, ser := range unbound {
		probeCh := channels[(i*step)%len(channels)]
		serialProbeCh[ser] = probeCh
		// ADB switch_channel 完了（~8s）より先にサーバーが新 TCP セッションを開いて
		// 0x15 (uid+lineID) を送信してくる。SwitchGroup 後に RecordPatrolMove を呼ぶと
		// pendingProbes が空のまま observation が来てバインド失敗するため、先に登録する（§4-2.1）。
		p.RecordPatrolMove(ser, probeCh, time.Now())
		log.Printf("[Patroller][Probe] serial=%s → Ch%d", ser, probeCh)
		// currentChannel=probeCh を CapDevice に通知する（No.74 と同原則: SwitchGroup より前）。
		// probeMode=ON 中は maybeSubmitPortVote がこの currentChannel を ground-truth として
		// 再接続セッションの sess.lineID に直接確定する。
		p.mu.RLock()
		cs := p.onChannelSwitch
		p.mu.RUnlock()
		if cs != nil {
			cs(probeCh)
		}
		SwitchGroup(ctx, []string{ser}, 0, 1, probeCh, cfg, switchRes, &switchMu)
	}

	// 全台が probeCh にロード完了するまで待つ。
	// ロード完了 = 0x2E 受信 = updateActualCh 経由で ds.CurrentCh が probeCh に更新済み。
	// タイムアウトは MoveTimeout（最低 15s）を使用する。
	probeTimeout := cfg.MoveTimeout
	if probeTimeout < 15*time.Second {
		probeTimeout = 15 * time.Second
	}
	loadStart := time.Now()
	deadline := time.Now().Add(probeTimeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		statuses := p.GetDeviceStatuses()
		statusMap := make(map[string]uint32, len(statuses))
		for _, ds := range statuses {
			statusMap[ds.Serial] = ds.CurrentCh
		}
		loaded := 0
		for _, ser := range unbound {
			if statusMap[ser] == serialProbeCh[ser] {
				loaded++
			}
		}
		debuglog.Vlogf("Probe", "ロード完了待ち: %d/%d台 (%.1fs経過)", loaded, n, time.Since(loadStart).Seconds())
		if loaded >= n {
			break
		}
	}

	elapsed := time.Since(loadStart).Seconds()

	// バインド数を最終確認してログ出力
	p.serialUIDMu.RLock()
	bound := 0
	for _, ser := range unbound {
		if p.serialToUID[ser] != 0 {
			bound++
		}
	}
	p.serialUIDMu.RUnlock()
	log.Printf("[Patroller][Probe] 識別フェーズ完了: %d/%d 台バインド成立 (%.1fs)", bound, n, elapsed)

	// 初回ロード時間を adaptiveMaxLoad 用履歴に反映する
	if elapsed > 0 {
		window := cfg.AdaptiveTimeoutWindow
		if window <= 0 {
			window = 10
		}
		p.recordLoadTime(elapsed, window)
	}
}

func (p *Patroller) resolveLabel(ctx context.Context, serial string, cfg Config) string {
	// 1) 自動確立済みマップ（永続保存済み）
	p.serialLabelMu.RLock()
	if p.serialToLabel != nil {
		if label, ok := p.serialToLabel[serial]; ok && label != "" {
			p.serialLabelMu.RUnlock()
			return label
		}
	}
	saveFn := p.saveSerialLabelFn
	p.serialLabelMu.RUnlock()

	// 2) Config の手動設定
	if cfg.SerialToLabel != nil {
		if label, ok := cfg.SerialToLabel[serial]; ok && label != "" {
			return label
		}
	}

	// 3) 動的解決（IP経由）
	if cfg.GetLabelByIP == nil {
		return ""
	}
	ip := p.getCachedIP(ctx, serial, cfg)
	if ip == "" {
		return ""
	}
	label := cfg.GetLabelByIP(ip)
	if label == "" {
		return ""
	}

	// 新規確立 → 確定マップに追加して永続保存
	p.serialLabelMu.Lock()
	if p.serialToLabel == nil {
		p.serialToLabel = make(map[string]string)
	}
	if p.labelToSerial == nil {
		p.labelToSerial = make(map[string]string)
	}
	if p.serialToLabel[serial] == "" {
		p.serialToLabel[serial] = label
		p.labelToSerial[label] = serial
		snapshot := make(map[string]string, len(p.serialToLabel))
		for k, v := range p.serialToLabel {
			snapshot[k] = v
		}
		p.serialLabelMu.Unlock()
		log.Printf("[Patroller][Label] serial=%s → %s (自動確立)", serial, label)
		if saveFn != nil {
			go saveFn(snapshot)
		}
	} else {
		p.serialLabelMu.Unlock()
	}
	return label
}

// getCachedIP はADBシリアルのIPをキャッシュ付きで取得する（TTL: 5分）。
// ADB呼び出しはmutex外で実行し、ctx cancellationが効くようにする。
func (p *Patroller) getCachedIP(ctx context.Context, serial string, cfg Config) string {
	const ttl = 5 * time.Minute
	p.serialIPCacheMu.Lock()
	if t, ok := p.serialIPCacheAt[serial]; ok && time.Since(t) < ttl {
		ip := p.serialIPCache[serial]
		p.serialIPCacheMu.Unlock()
		return ip
	}
	p.serialIPCacheMu.Unlock()

	ipCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ip, err := GetDeviceIP(ipCtx, serial, cfg)
	if err != nil || ip == "" {
		return ip
	}

	p.serialIPCacheMu.Lock()
	if p.serialIPCache == nil {
		p.serialIPCache = make(map[string]string)
		p.serialIPCacheAt = make(map[string]time.Time)
	}
	p.serialIPCache[serial] = ip
	p.serialIPCacheAt[serial] = time.Now()
	p.serialIPCacheMu.Unlock()
	return ip
}

// ResolveAllIPs は serials の各シリアルに対して GetDeviceIP を並列実行し、
// serial → IP のマップを返す。取得できなかったシリアルはマップに含まれない。
func ResolveAllIPs(ctx context.Context, serials []string, cfg Config) map[string]string {
	result := make(map[string]string, len(serials))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, serial := range serials {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			ipCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ip, err := GetDeviceIP(ipCtx, s, cfg)
			if err == nil && ip != "" {
				mu.Lock()
				result[s] = ip
				mu.Unlock()
			}
		}(serial)
	}
	wg.Wait()
	return result
}

// updateDeviceStatus はシリアル単位のデバイス状態を更新する。
func (p *Patroller) updateDeviceStatus(serial, label string, ch uint32, responded bool, loadSecs float64, adbFailed bool) {
	p.deviceStatusesMu.Lock()
	defer p.deviceStatusesMu.Unlock()
	if p.deviceStatuses == nil {
		p.deviceStatuses = make(map[string]*DeviceStatus)
	}
	ds, ok := p.deviceStatuses[serial]
	if !ok {
		ds = &DeviceStatus{Serial: serial}
		p.deviceStatuses[serial] = ds
	}
	if label != "" {
		ds.Label = label
	}
	ds.ADBFailed = adbFailed
	ds.TimedOutLast = !responded && !adbFailed
	if responded {
		ds.CurrentCh = ch
		ds.LastLoadSecs = loadSecs
		ds.ConsecutiveTimeout = 0
		ds.GameCrashed = false
	} else if !adbFailed {
		ds.ConsecutiveTimeout++
	}
}

// checkAndRecover はゲームプロセスの生死確認と必要に応じた自動復帰を行う。
// タイムアウト発生時に goroutine で呼び出す。
func (p *Patroller) checkAndRecover(ctx context.Context, serial string, cfg Config) {
	if cfg.GamePackageName == "" {
		return
	}
	pidCtx, pidCancel := context.WithTimeout(ctx, 8*time.Second)
	defer pidCancel()
	out, err := adb(pidCtx, serial, cfg, "shell", "pidof", cfg.GamePackageName)
	if err != nil || strings.TrimSpace(out) != "" {
		// エラー or プロセス生存 → 何もしない
		return
	}
	// プロセス死亡 → クラッシュ確定
	p.deviceStatusesMu.Lock()
	if p.deviceStatuses == nil {
		p.deviceStatuses = make(map[string]*DeviceStatus)
	}
	ds, ok := p.deviceStatuses[serial]
	if !ok {
		ds = &DeviceStatus{Serial: serial}
		p.deviceStatuses[serial] = ds
	}
	ds.GameCrashed = true
	if !cfg.CrashRecoveryEnabled || ds.Recovering {
		p.deviceStatusesMu.Unlock()
		log.Printf("[MuMu] %s: ゲームプロセス死亡確認 (pidof %s = 空) [復帰%s]",
			serial, cfg.GamePackageName, map[bool]string{true: "無効", false: "既に実行中"}[!cfg.CrashRecoveryEnabled])
		return
	}
	ds.Recovering = true
	p.deviceStatusesMu.Unlock()

	log.Printf("[MuMu] %s: ゲームクラッシュ検知 → 復帰処理開始 (force-stop + 再起動)", serial)
	defer func() {
		p.deviceStatusesMu.Lock()
		if ds, ok := p.deviceStatuses[serial]; ok {
			ds.Recovering = false
			ds.LastRecoveredAtMs = time.Now().UnixMilli()
		}
		p.deviceStatusesMu.Unlock()
	}()

	// 1. force-stop
	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	adb(stopCtx, serial, cfg, "shell", "am", "force-stop", cfg.GamePackageName) //nolint
	stopCancel()

	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	// 2. ゲーム起動
	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	if cfg.GameLaunchActivity != "" {
		adb(startCtx, serial, cfg, "shell", "am", "start", "-n", cfg.GameLaunchActivity) //nolint
	} else {
		adb(startCtx, serial, cfg, "shell", "monkey", "-p", cfg.GamePackageName, "-c", "android.intent.category.LAUNCHER", "1") //nolint
	}
	startCancel()

	// 3. 起動待機
	delay := time.Duration(cfg.CrashRecoveryDelaySecs * float64(time.Second))
	if delay < 5*time.Second {
		delay = 30 * time.Second
	}
	log.Printf("[MuMu] %s: ゲーム起動待機 %.0fs", serial, delay.Seconds())
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	p.deviceStatusesMu.Lock()
	if ds, ok := p.deviceStatuses[serial]; ok {
		ds.GameCrashed = false
		ds.ConsecutiveTimeout = 0
	}
	p.deviceStatusesMu.Unlock()
	log.Printf("[MuMu] %s: 復帰処理完了", serial)
}

// RecoverDevice は指定シリアルのデバイスを手動で復帰させる。
// game_package_name が設定されていない場合はエラーを返す。
func (p *Patroller) RecoverDevice(ctx context.Context, serial string) error {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	if cfg.GamePackageName == "" {
		return fmt.Errorf("game_package_name が設定されていません")
	}
	go p.checkAndRecover(ctx, serial, cfg)
	return nil
}

// Stop は巡回を停止する
func (p *Patroller) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.ctx = nil
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
	// No.46: NotifyPostLoadReady で起動した待機 goroutine も全部止める
	p.postLoadCancelMu.Lock()
	for _, c := range p.postLoadCancels {
		if c != nil {
			c()
		}
	}
	p.postLoadCancels = nil
	p.postLoadCancelMu.Unlock()
	if cancel != nil {
		log.Println("[MuMu] 巡回停止")
	}
	// 巡回停止を ncap へ通知（デッドロック回避のためローカル変数経由・mu 解放後に呼ぶ）
	p.mu.RLock()
	fn := p.onPatrolActive
	p.mu.RUnlock()
	if fn != nil {
		fn(false)
	}
}

// Identify は対象 serial の既存バインドを破棄してからデバイス識別フェーズ（Stagger Probe）を実行する。
// 巡回停止中に使用することを推奨する。
// serials が空の場合は ListDevices で自動取得する。
// channels が空の場合は fmt.Errorf で即座にエラーを返す。
func (p *Patroller) Identify(ctx context.Context, serials []string, channels []uint32) error {
	cfg := p.Config()
	if len(serials) == 0 {
		lCtx, lCancel := context.WithTimeout(ctx, 10*time.Second)
		var err error
		serials, err = ListDevices(lCtx, cfg)
		lCancel()
		if err != nil {
			return fmt.Errorf("デバイス一覧取得失敗: %w", err)
		}
	}
	if len(channels) == 0 {
		return fmt.Errorf("チャンネルリストが空です")
	}

	// 対象 serial の既存バインドを破棄して強制再識別する
	p.serialUIDMu.Lock()
	cleared := 0
	for _, ser := range serials {
		if uid, ok := p.serialToUID[ser]; ok && uid != 0 {
			delete(p.serialToUID, ser)
			delete(p.uidToSerial, uid)
			cleared++
		}
	}
	saveFn := p.saveSerialUIDFn
	snapshot := make(map[string]uint64, len(p.serialToUID))
	for k, v := range p.serialToUID {
		snapshot[k] = v
	}
	p.serialUIDMu.Unlock()
	if cleared > 0 {
		log.Printf("[Patroller][Identify] 既存バインド %d件クリア", cleared)
		if saveFn != nil {
			go saveFn(snapshot)
		}
	}

	log.Printf("[Patroller][Identify] 識別開始: %d台, %d ch", len(serials), len(channels))
	p.runStaggerProbe(ctx, serials, channels, cfg)
	return nil
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
	// 前の巡回goroutineが完全に終了するまで待機（二重起動・ADB競合防止）
	p.wg.Wait()

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

	// dwell: 未設定(<=0)なら5秒、設定済みなら最低1秒（ADB連打防止の安全下限）
	dwell := cfg.DwellDuration
	if dwell <= 0 {
		dwell = 5 * time.Second
	} else if dwell < time.Second {
		dwell = time.Second
	}

	// moveSignal: [0x2E]パケット受信ごとにインスタンスラベル付きでバッファリング
	sig := make(chan moveSignalMsg, 64)

	p.mu.Lock()
	p.ctx = ctx
	p.cancel = cancel
	p.moveSignal = sig
	// インスタンス追跡を巡回再開時にリセット
	p.knownInstances = make(map[string]bool)
	p.missedCounts = make(map[string]int)
	p.crashedInstances = make(map[string]bool)
	p.switchStartTimesMu.Lock()
	p.switchStartTimes = make(map[string]time.Time)
	p.switchStartTimesMu.Unlock()
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

	// デバイス分担設定からch→許可serialセットを構築（巡回開始時に1回だけ計算）
	p.deviceAssignmentsMu.RLock()
	assignments := p.deviceAssignments
	p.deviceAssignmentsMu.RUnlock()
	var channelSerials map[uint32]map[string]bool
	if len(assignments) > 0 && len(serials) > 0 {
		channelSerials = make(map[uint32]map[string]bool)
		for _, grp := range assignments {
			var grpSerials []string
			for _, idx := range grp.DeviceIndices {
				if idx >= 0 && idx < len(serials) {
					grpSerials = append(grpSerials, serials[idx])
				}
			}
			if len(grpSerials) == 0 {
				continue
			}
			for _, ch := range grp.Channels {
				if channelSerials[ch] == nil {
					channelSerials[ch] = make(map[string]bool)
				}
				for _, s := range grpSerials {
					channelSerials[ch][s] = true
				}
			}
		}
		log.Printf("[MuMu] デバイス分担: %d グループ, 計 %d ch に分担設定を適用", len(assignments), len(channelSerials))
	}

	// channels.txt の初回モッドタイムを記録
	var lastModTime time.Time
	if channelsFile != "" {
		if fi, err := os.Stat(channelsFile); err == nil {
			lastModTime = fi.ModTime()
		}
	}

	// 巡回開始を ncap へ通知（巡回状態確定後・goroutine 起動前。
	// デッドロック回避のためローカル変数経由・mu 解放後に呼ぶ）
	p.mu.RLock()
	patrolActiveFn := p.onPatrolActive
	p.mu.RUnlock()
	if patrolActiveFn != nil {
		patrolActiveFn(true)
	}

	p.wg.Add(1)
	go func() {
		defer func() {
			p.mu.Lock()
			p.status.Running = false
			p.mu.Unlock()
			p.wg.Done()
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
			if dwell <= 0 {
				dwell = 5 * time.Second
			} else if dwell < time.Second {
				dwell = time.Second
			}
			p.mu.Unlock()

			// 毎サイクルデバイスを再取得し、接続/切断・巡回中追加に追随する
			listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
			allOnline, devErr := ListDevices(listCtx, currentCfg)
			listCancel()
			if devErr != nil {
				log.Printf("[MuMu] 巡回: デバイス取得失敗: %v", devErr)
			}
			var targets []string
			if len(serials) > 0 {
				// 初期指定シリアルとオンラインデバイスの共通部分のみ使用
				serialSet := make(map[string]bool, len(serials))
				for _, s := range serials {
					serialSet[s] = true
				}
				for _, s := range allOnline {
					if serialSet[s] {
						targets = append(targets, s)
					}
				}
				if len(targets) == 0 {
					// 全台一時オフライン: 指定リストにフォールバック（次サイクルで再確認）
					targets = serials
				}
			} else {
				targets = allOnline
			}
			if len(targets) == 0 {
				log.Println("[MuMu] 巡回: デバイスが見つかりません。巡回を停止します。手動でスキャンしてください")
				return
			}

			// デバイス分担フィルター: 当該chに割り当てられたデバイスのみ対象にする
			if channelSerials != nil {
				if allowed, ok := channelSerials[ch]; ok {
					var filtered []string
					for _, s := range targets {
						if allowed[s] {
							filtered = append(filtered, s)
						}
					}
					if len(filtered) > 0 {
						targets = filtered
					}
				}
			}

			// 既に目的 ch にいるデバイスはスイッチ不要: targets から除外
			var switchTargets []string
			p.deviceStatusesMu.RLock()
			for _, s := range targets {
				if ds, ok := p.deviceStatuses[s]; ok && ds.CurrentCh == ch {
					// already there
				} else {
					switchTargets = append(switchTargets, s)
				}
			}
			p.deviceStatusesMu.RUnlock()
			skippedCount := len(targets) - len(switchTargets)
			if skippedCount > 0 {
				log.Printf("[MuMu] 巡回: Ch%d 既到達スキップ %d台", ch, skippedCount)
			}

			// シリアル→ラベルを並列解決（IPキャッシュ付き）
			serialLabels := make(map[string]string, len(targets))
			if currentCfg.GetLabelByIP != nil || currentCfg.SerialToLabel != nil {
				var labelsMu sync.Mutex
				var labelsWg sync.WaitGroup
				for _, s := range targets {
					labelsWg.Add(1)
					go func(serial string) {
						defer labelsWg.Done()
						label := p.resolveLabel(ctx, serial, currentCfg)
						labelsMu.Lock()
						serialLabels[serial] = label
						labelsMu.Unlock()
					}(s)
				}
				labelsWg.Wait()
			}

			p.mu.Lock()
			p.status.CurrentChannel = ch
			p.status.CurrentIndex = normalIdx
			p.status.Serials = targets
			p.status.ParallelLimit = currentCfg.ParallelLimit
			p.status.ParallelGroupDelay = currentCfg.ParallelGroupDelay.Seconds()
			p.status.DwellSecs = dwell.Seconds()
			p.status.Phase = "adb_sending"
			p.status.PhaseTotalSecs = 0
			p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
			p.lastChannel = ch
			p.status.LastChannel = ch
			p.mu.Unlock()

			limit := ParallelLimit(currentCfg, len(switchTargets))
			// ParallelLimit=0（無制限）のとき グループ間ディレイは無効
			groupDelay := currentCfg.ParallelGroupDelay
			if currentCfg.ParallelLimit <= 0 {
				groupDelay = 0
			}
			log.Printf("[MuMu] 巡回: [%d/%d] Ch%d → %d台 (ParallelLimit=%d, グループ=%d台, ディレイ=%.1fs, 滞在=%.0fs)",
				normalIdx+1, n, ch, len(switchTargets), currentCfg.ParallelLimit, limit,
				groupDelay.Seconds(), dwell.Seconds())

			// 切替開始時刻を記録（この時刻以降に届いた[0x2E]のみ有効）
			switchStartAt := time.Now()
			p.mu.Lock()
			p.status.SwitchStartedAtUnixMs = switchStartAt.UnixMilli()
			p.mu.Unlock()
			// autoDelay 計算用: serial ごとに switchStartAt を記録
			p.switchStartTimesMu.Lock()
			for _, ser := range switchTargets {
				p.switchStartTimes[ser] = switchStartAt
			}
			p.switchStartTimesMu.Unlock()

			// per-device CH バインド用: 各 serial の切替予約を ADB 発行前に登録する（§4-2.1, No.35 と同根）。
			// 既到達スキップ分（switchTargets 外）も ExpectedCh 更新のため targets 全件で呼ぶ（No.29）。
			for _, ser := range targets {
				p.RecordPatrolMove(ser, ch, switchStartAt)
			}

			// 現在巡回chを CapDevice に通知する（No.74）。
			// portMap クォーラム投票は再接続セッションの lineID==0 時に currentChannel を
			// 投票chとして使うため、ADB 発行（SwitchGroup）より前＝switchStartAt 時点で
			// 新chを立てておく必要がある。SwitchGroup 完了後に通知すると、SwitchGroup 中に
			// 再接続したデバイスが旧chへ投票し portMap が 1ch ずれて汚染される（off-by-one）。
			p.mu.RLock()
			notifyFn := p.onChannelSwitch
			p.mu.RUnlock()
			if notifyFn != nil {
				go notifyFn(ch)
			}

			// デバイスをグループに分けて並列切替
			patrolResults := make(map[string]error, len(switchTargets))
			var patrolMu sync.Mutex
			for start := 0; start < len(switchTargets); start += limit {
				end := start + limit
				if end > len(switchTargets) {
					end = len(switchTargets)
				}
				if start > 0 && groupDelay > 0 {
					log.Printf("[MuMu] 巡回: グループ間ディレイ %.1fs 待機中... (グループ%d)", groupDelay.Seconds(), start/limit+1)
					select {
					case <-ctx.Done():
						return
					case <-time.After(groupDelay):
					}
				}
				SwitchGroup(ctx, switchTargets, start, end, ch, currentCfg, patrolResults, &patrolMu)
			}
			// 全台切替完了時刻を記録（この時刻以降の[0x2E]のみカウント）
			switchDoneAt := time.Now()

			// ADB 発行完了 → 0x2E UUID 受信待ちフェーズ
			p.mu.Lock()
			p.status.Phase = "wait_0x2e"
			p.status.PhaseTotalSecs = 0
			p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
			p.mu.Unlock()

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
			// ・1台目が届くまで MoveTimeout で待つ（届かなければ移動失敗と判定）
			// ・1台目受信後は MergeTimeout（0 なら MoveTimeout を流用）で残りを待つ
			// ・インスタンスごとに応答を追跡し、3回連続未応答でクラッシュ判定
			moveFailed := false
			if currentCfg.MoveTimeout > 0 {
				// No.80: screen モードは ch切替起点で各台の画面判定を起動する（0x2E 非依存）。
				// time/either は従来どおり 0x2E（NotifyPostLoadReady / NotifyChMovePacket）起点。
				var screenCancel context.CancelFunc
				if strings.ToLower(currentCfg.LoadDetectMode) == "screen" {
					screenCtx, cancel := context.WithCancel(ctx)
					screenCancel = cancel
					for _, ser := range switchTargets {
						ser := ser
						go p.runScreenStrategyAtSwitch(screenCtx, ser, ch, switchDoneAt, currentCfg)
					}
				}

				// 期待台数 = スイッチ実行台数（既到達スキップ分は除く）
				// セッションマージによって複数デバイスが同じインスタンスへ統合される可能性があるため、
				// knownInstances数ではなくADB上の実デバイス数を期待値とする。
				need := len(switchTargets)

				got := 0
				respondedSet := make(map[string]bool)

				mergeTimeout := currentCfg.MergeTimeout
				if mergeTimeout <= 0 {
					mergeTimeout = currentCfg.MoveTimeout // 未設定なら従来動作
				}
				// effectiveMove: 全台移動失敗判定タイムアウト（固定）
				// effectiveMerge: 1台目シグナル受信後に残り台数を待つタイムアウト
				effectiveMove := currentCfg.MoveTimeout
				effectiveMerge := mergeTimeout
				debuglog.Vlogf("巡回", "Ch%d: effectiveMove=%.0fs effectiveMerge=%.0fs adaptiveTimeout=%v need=%d", ch, effectiveMove.Seconds(), effectiveMerge.Seconds(), currentCfg.AdaptiveTimeout, need)
				p.mu.Lock()
				p.status.WaitingMove = true
				p.status.Phase = "wait_0x2e"
				p.status.PhaseTotalSecs = effectiveMerge.Seconds()
				p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				p.mu.Unlock()
				log.Printf("[MuMu] 巡回: Ch%d 移動完了待ち (0/%d台, 初回待ち=%.0fs, マージ待ち=%.0fs, 切替所要=%.1fs)",
					ch, need, effectiveMove.Seconds(), effectiveMerge.Seconds(),
					switchDoneAt.Sub(switchStartAt).Seconds())
				draining := true
				for draining && got < need {
					select {
					case msg := <-sig:
						if msg.t.After(switchStartAt) &&
							(msg.lineID == 0 || msg.lineID == ch) &&
							!respondedSet[msg.label] {
							got++
							respondedSet[msg.label] = true
							log.Printf("[MuMu] 巡回: Ch%d [0x2E] buffered %s (%d/%d台)", ch, msg.label, got, need)
							debuglog.Vlogf("巡回", "  → 受信遅延 %.2fs (switchDoneAt基準)", msg.t.Sub(switchDoneAt).Seconds())
							if got == 1 {
								stabilizeSecs := p.loadStabilizationDelay().Seconds()
								p.mu.Lock()
								p.status.Phase = "stabilizing"
								p.status.PhaseTotalSecs = stabilizeSecs
								p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
								p.mu.Unlock()
							}
						} else if msg.t.After(switchStartAt) && msg.lineID != 0 && msg.lineID != ch {
							debuglog.Vlogf("巡回", "  → stale lineID=%d skipped (target=%d, serial=%s)", msg.lineID, ch, msg.label)
						}
					default:
						draining = false
					}
				}

				// フェーズ1: 1台目のシグナルを MoveTimeout で待つ（バッファ消化で0台のとき）
				if got == 0 && need > 0 {
					deadline := time.NewTimer(effectiveMove)
				waitFirst:
					for got == 0 {
						select {
						case <-ctx.Done():
							deadline.Stop()
							return
						case <-deadline.C:
							moveFailed = true
							log.Printf("[MuMu] 巡回: Ch%d 移動失敗（完了シグナルなし） → スキップ", ch)
							break waitFirst
						case msg := <-sig:
							if msg.t.After(switchStartAt) &&
								(msg.lineID == 0 || msg.lineID == ch) &&
								!respondedSet[msg.label] {
								got++
								respondedSet[msg.label] = true
								log.Printf("[MuMu] 巡回: Ch%d [0x2E] %s (%d/%d台) ← マージタイマー開始 (%.0fs)",
									ch, msg.label, got, need, effectiveMerge.Seconds())
								if got == 1 {
									stabilizeSecs := p.loadStabilizationDelay().Seconds()
									p.mu.Lock()
									p.status.Phase = "stabilizing"
									p.status.PhaseTotalSecs = stabilizeSecs
									p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
									p.mu.Unlock()
								}
							} else if msg.t.After(switchStartAt) && msg.lineID != 0 && msg.lineID != ch {
								debuglog.Vlogf("巡回", "  → stale lineID=%d skipped (target=%d, serial=%s)", msg.lineID, ch, msg.label)
							}
						}
					}
					deadline.Stop()
				}

				// フェーズ2: 残りのシグナルを MergeTimeout で待つ
				if !moveFailed && got < need {
					mergeDeadline := time.NewTimer(effectiveMerge)
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
							if msg.t.After(switchStartAt) &&
								(msg.lineID == 0 || msg.lineID == ch) &&
								!respondedSet[msg.label] {
								got++
								respondedSet[msg.label] = true
								log.Printf("[MuMu] 巡回: Ch%d [0x2E] %s (%d/%d台)", ch, msg.label, got, need)
							} else if msg.t.After(switchStartAt) && msg.lineID != 0 && msg.lineID != ch {
								debuglog.Vlogf("巡回", "  → stale lineID=%d skipped (target=%d, serial=%s)", msg.lineID, ch, msg.label)
							}
						}
					}
					mergeDeadline.Stop()
				}

				// No.80: 当該 ch の画面ポーリング goroutine を停止（次 ch へ残骸を漏らさない）
				if screenCancel != nil {
					screenCancel()
				}

				// インスタンス追跡更新（moveFailed の場合はスキップ: チャンネル切替未実施）
				// respondedSet・knownInstances・missedCounts・crashedInstances はすべて serial をキーとする。
				if !moveFailed {
					p.mu.Lock()
					var newCrashed, recovered []string

					// 応答した serial を登録・ミスカウントリセット
					for serial := range respondedSet {
						p.knownInstances[serial] = true
						p.missedCounts[serial] = 0
						if p.crashedInstances[serial] {
							delete(p.crashedInstances, serial)
							recovered = append(recovered, serial)
						}
					}
					// 既知 serial のうち未応答のものはミスカウント増加
					for serial := range p.knownInstances {
						if !respondedSet[serial] {
							p.missedCounts[serial]++
							if p.missedCounts[serial] >= 3 && !p.crashedInstances[serial] {
								p.crashedInstances[serial] = true
								newCrashed = append(newCrashed, serial)
							}
						}
					}
					// PatrolStatus を更新（serial キーをそのまま格納）
					crashed := make([]string, 0, len(p.crashedInstances))
					for serial := range p.crashedInstances {
						crashed = append(crashed, serial)
					}
					sort.Strings(crashed)
					known := make([]string, 0, len(p.knownInstances))
					for serial := range p.knownInstances {
						known = append(known, serial)
					}
					sort.Strings(known)
					p.status.CrashedInstances = crashed
					p.status.KnownInstances = known
					p.mu.Unlock()

					// ログ表示はラベル解決（serialLabels はループローカル変数）
					resolveDisplay := func(serials []string) []string {
						out := make([]string, len(serials))
						for i, s := range serials {
							if l := serialLabels[s]; l != "" {
								out[i] = l
							} else {
								out[i] = s
							}
						}
						return out
					}
					sort.Strings(newCrashed)
					sort.Strings(recovered)
					if len(newCrashed) > 0 {
						log.Printf("[MuMu] 巡回: %v クラッシュ判定（3回連続未応答）→ 稼働台数から除外", resolveDisplay(newCrashed))
					}
					if len(recovered) > 0 {
						log.Printf("[MuMu] 巡回: %v 復旧確認 → 稼働台数に復帰", resolveDisplay(recovered))
					}
				}

				// 全台ロード完了時のみ実ロード時間を記録（適応型タイムアウトの学習データ）
				if !moveFailed && got == need {
					p.recordLoadTime(time.Since(switchDoneAt).Seconds(), currentCfg.AdaptiveTimeoutWindow)
				}

				// デバイス別状態を更新し、タイムアウトデバイスのクラッシュ確認を非同期実行
				loadElapsed := time.Since(switchDoneAt).Seconds()
				for _, serial := range targets {
					label := serialLabels[serial]
					// respondedSet はすべて serial キー。シグナルが届けば responded=true。
					responded := !moveFailed && respondedSet[serial]
					adbFailed := patrolResults[serial] != nil
					p.updateDeviceStatus(serial, label, ch, responded, loadElapsed, adbFailed)
					// タイムアウト（moveFailed でなく、かつ未応答）のデバイスのみ crash check
					if !moveFailed && !responded && !adbFailed {
						go p.checkAndRecover(ctx, serial, currentCfg)
					}
				}

				p.mu.Lock()
				p.status.WaitingMove = false
				if moveFailed {
					alreadyIn := false
					for _, fc := range p.status.MoveFailedChannels {
						if fc == ch {
							alreadyIn = true
							break
						}
					}
					if !alreadyIn {
						p.status.MoveFailedChannels = append(p.status.MoveFailedChannels, ch)
					}
					p.status.Phase = "adb_sending"
					p.status.PhaseTotalSecs = 0
					p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				} else {
					p.status.Phase = "dwell_wait"
					p.status.PhaseTotalSecs = dwell.Seconds()
					p.status.PhaseStartedAtUnixMs = time.Now().UnixMilli()
				}
				p.mu.Unlock()

				// 発行前再確認: Phase2 終了時点で全台ロード完了でなければ追加待ち（bounded）
				if !moveFailed && got < need {
					extraDeadline := time.NewTimer(currentCfg.MoveTimeout)
					log.Printf("[MuMu] 巡回: Ch%d move 終了時点 %d/%d台 → 発行前追加待ち最大 %.0fs",
						ch, got, need, currentCfg.MoveTimeout.Seconds())
				extraWait:
					for got < need {
						select {
						case <-ctx.Done():
							extraDeadline.Stop()
							return
						case <-extraDeadline.C:
							log.Printf("[MuMu] 巡回: Ch%d 発行前追加待ちタイムアウト (%d/%d台) → 進行", ch, got, need)
							break extraWait
						case msg := <-sig:
							if msg.t.After(switchStartAt) &&
								(msg.lineID == 0 || msg.lineID == ch) &&
								!respondedSet[msg.label] {
								got++
								respondedSet[msg.label] = true
								log.Printf("[MuMu] 巡回: Ch%d [lineID+delay] %s (%d/%d台) ← 発行前", ch, msg.label, got, need)
							} else if msg.t.After(switchStartAt) && msg.lineID != 0 && msg.lineID != ch {
								debuglog.Vlogf("巡回", "  → stale lineID=%d skipped (target=%d, serial=%s)", msg.lineID, ch, msg.label)
							}
						}
					}
					extraDeadline.Stop()
				}
			}

			// ロード猶予: postLoadLatencies P90 + 安定化遅延 + 5s を推定最大ロード時間として
			// dwell を延長し、まだロード中のデバイスへの次切替コマンドを遅延させる
			effectiveDwell := dwell
			if currentCfg.AdaptiveTimeout {
				if p90Lat := p.postLoadLatencyP90(); p90Lat > 0 {
					stab := p.loadStabilizationDelay()
					estimatedMax := time.Duration((p90Lat + stab.Seconds() + 5) * float64(time.Second))
					if elapsed := time.Since(switchDoneAt); elapsed < estimatedMax {
						if grace := estimatedMax - elapsed; grace > effectiveDwell {
							effectiveDwell = grace
							log.Printf("[MuMu] 巡回: Ch%d ロード猶予: dwell を %.1fs に延長 (推定最大=%.1fs, 経過=%.1fs)",
								ch, effectiveDwell.Seconds(), estimatedMax.Seconds(), elapsed.Seconds())
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(effectiveDwell):
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

// Tapper は指定座標への連続タップを管理する。
type Tapper struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewTapper() *Tapper { return &Tapper{} }

func (t *Tapper) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancel != nil
}

// Start は全接続デバイスに対して interval ごとに (x, y) をタップし続けるゴルーチンを起動する。
// 既に実行中の場合はエラーを返す。
func (t *Tapper) Start(ctx context.Context, cfg Config, x, y int, interval time.Duration) error {
	t.mu.Lock()
	if t.cancel != nil {
		t.mu.Unlock()
		return fmt.Errorf("既に実行中")
	}
	ctx2, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.mu.Unlock()

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			t.mu.Lock()
			t.cancel = nil
			t.mu.Unlock()
		}()

		devs, err := listDevicesOnce(ctx2, cfg)
		if err != nil || len(devs) == 0 {
			log.Println("[MuMu] 連続タップ: デバイスが見つかりません")
			return
		}
		log.Printf("[MuMu] 連続タップ開始: (%d,%d) 間隔%v デバイス:%v", x, y, interval, devs)

		xs, ys := strconv.Itoa(x), strconv.Itoa(y)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx2.Done():
				log.Println("[MuMu] 連続タップ停止")
				return
			case <-ticker.C:
				var wg sync.WaitGroup
				for _, serial := range devs {
					wg.Add(1)
					go func(s string) {
						defer wg.Done()
						if _, err := adb(ctx2, s, cfg, "shell", "input", "tap", xs, ys); err != nil {
							if ctx2.Err() == nil {
								log.Printf("[MuMu] タップ失敗 %s: %v", s, err)
							}
						}
					}(serial)
				}
				wg.Wait()
			}
		}
	}()
	return nil
}

func (t *Tapper) Stop() {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		t.wg.Wait()
	}
}
