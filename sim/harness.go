package sim

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/mumu"
)

// BuildMumuConfig はシナリオから mumu.Config を構築する。
// patrol-sim / gui-sim 共通のハーネス組立ロジック（cmd/patrol-sim/main.go から移動）。
func BuildMumuConfig(scenario *Scenario, fakeADBPath string) mumu.Config {
	dwell := time.Duration(scenario.Patrol.DwellSecs * float64(time.Second))
	if dwell <= 0 {
		dwell = 2 * time.Second
	}

	moveTimeout := time.Duration(scenario.Patrol.MoveTimeoutSecs * float64(time.Second))
	if moveTimeout <= 0 {
		moveTimeout = 60 * time.Second
	}

	// Screen 系パラメーター（デフォルト値を設定）
	screenPollInterval := time.Duration(scenario.Patrol.ScreenPollIntervalMs) * time.Millisecond
	if screenPollInterval <= 0 {
		screenPollInterval = 500 * time.Millisecond
	}
	screenDetectTimeout := time.Duration(scenario.Patrol.ScreenDetectTimeoutSecs * float64(time.Second))
	if screenDetectTimeout <= 0 {
		screenDetectTimeout = 30 * time.Second
	}
	screenRegionW := scenario.Patrol.ScreenRegionW
	screenRegionH := scenario.Patrol.ScreenRegionH
	if screenRegionW <= 0 {
		screenRegionW = 16
	}
	if screenRegionH <= 0 {
		screenRegionH = 16
	}
	screenBlackLuma := scenario.Patrol.ScreenBlackLuma
	if screenBlackLuma == 0 {
		screenBlackLuma = 30
	}
	screenBlackPixelRatio := scenario.Patrol.ScreenBlackPixelRatio
	if screenBlackPixelRatio <= 0 {
		screenBlackPixelRatio = 0.8
	}

	return mumu.Config{
		ADBPath:       fakeADBPath,
		DwellDuration: dwell,
		MoveTimeout:   moveTimeout,
		// シミュレーター向け: グローバルディレイなし
		GlobalDelay:   0,
		ParallelLimit: 0, // 無制限（全台並列）
		// LoadStabilizationAuto=true で直近観測から自動算出
		LoadStabilizationAuto: true,
		// LoadDetectMode: シナリオから（デフォルト "packet" → "time" 扱い）
		LoadDetectMode: scenario.Patrol.LoadDetectMode,
		// AdaptiveTimeout: シミュレーターでは無効（実時間で動作させる）
		AdaptiveTimeout: false,
		// ClearLength: チャンネル番号最大2桁を想定して3文字分DEL
		ClearLength: 3,
		// TapX/TapY: 0 にしてタップをスキップ
		TapX: 0,
		TapY: 0,
		// Screen 系パラメーター
		ScreenPollInterval:    screenPollInterval,
		ScreenDetectTimeout:   screenDetectTimeout,
		ScreenRegionX:         scenario.Patrol.ScreenRegionX,
		ScreenRegionY:         scenario.Patrol.ScreenRegionY,
		ScreenRegionW:         screenRegionW,
		ScreenRegionH:         screenRegionH,
		ScreenBlackLuma:       screenBlackLuma,
		ScreenBlackPixelRatio: screenBlackPixelRatio,
	}
}

// ResolveFakeADB は fake-adb.exe のパスを解決する。
// flagValue が指定されていればそれを使用し、
// 未指定の場合は自バイナリと同ディレクトリの fake-adb.exe を使用する。
func ResolveFakeADB(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	exe, err := os.Executable()
	if err != nil {
		return "fake-adb.exe"
	}
	dir := filepath.Dir(exe)
	name := "fake-adb"
	if runtime.GOOS == "windows" {
		name = "fake-adb.exe"
	}
	return filepath.Join(dir, name)
}

// IsExecutable は指定パスのファイルが実行可能かチェックする。
func IsExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return !info.IsDir()
	}
	return info.Mode()&0111 != 0
}
