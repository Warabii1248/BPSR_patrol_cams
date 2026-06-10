// Package main は patrol-sim.exe の実装。
// 実機なしで mumu.Patroller の巡回ロジックを検証するシミュレーターハーネス。
//
// 使い方:
//
//	patrol-sim.exe -scenario scenarios/baseline.json [-v] [-fake-adb path/to/fake-adb.exe]
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/debuglog"
	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/sim"
)

func main() {
	scenarioFlag := flag.String("scenario", "", "シナリオ JSON ファイルパス（必須）")
	verboseFlag := flag.Bool("v", false, "詳細ログ出力")
	fakeADBFlag := flag.String("fake-adb", "", "fake-adb.exe のパス（未指定時は自バイナリと同ディレクトリ）")
	flag.Parse()

	if *scenarioFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: patrol-sim -scenario <file> [-v] [-fake-adb <path>]")
		os.Exit(1)
	}

	// ログ設定
	if !*verboseFlag {
		log.SetFlags(log.Ltime | log.Lmicroseconds)
	}
	// stale lineID 等のデバッグログを常時有効にする（シナリオの require_log_patterns で参照するため）
	debuglog.Verbose = true

	// シナリオ読み込み
	scenario, err := sim.LoadScenario(*scenarioFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario load error: %v\n", err)
		os.Exit(1)
	}
	log.Printf("[patrol-sim] シナリオ: %s (%d台 × %d ch)", scenario.Name, len(scenario.Devices), len(scenario.Channels))

	// fake-adb パス解決
	fakeADBPath := sim.ResolveFakeADB(*fakeADBFlag)
	log.Printf("[patrol-sim] fake-adb: %s", fakeADBPath)

	// fake-adb が実在するか確認
	if _, err := os.Stat(fakeADBPath); err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb not found: %s\n  build with: go build -o release/fake-adb.exe ./cmd/fake-adb\n", fakeADBPath)
		os.Exit(1)
	}

	// ログキャプチャバッファ
	capture := &sim.LogCapture{}
	log.SetOutput(io.MultiWriter(os.Stderr, capture))

	// TCP シミュレーターサーバー起動
	simSrv, err := sim.NewSimServer(scenario)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sim server error: %v\n", err)
		os.Exit(1)
	}
	defer simSrv.Close()
	log.Printf("[patrol-sim] sim server listening at %s", simSrv.Addr)

	// PATROL_SIM_ADDR を設定（子プロセスに継承される）
	os.Setenv("PATROL_SIM_ADDR", simSrv.Addr)

	// fake-adb が実行可能か確認
	if !sim.IsExecutable(fakeADBPath) {
		fmt.Fprintf(os.Stderr, "fake-adb is not executable: %s\n", fakeADBPath)
		os.Exit(1)
	}

	// mumu.Config を構築
	cfg := sim.BuildMumuConfig(scenario, fakeADBPath)

	// Patroller 作成
	p := mumu.NewPatroller(cfg)

	// シリアル↔UID マップを投入
	serialUIDs := make(map[string]uint64, len(scenario.Devices))
	serialLabels := make(map[string]string, len(scenario.Devices))
	for _, dev := range scenario.Devices {
		serialUIDs[dev.Serial] = dev.UID
		serialLabels[dev.Serial] = dev.Label
	}
	p.LoadSerialUIDMap(serialUIDs)
	p.LoadSerialLabelMap(serialLabels)

	// excludeUIDs は空（シミュレーターでは除外UID なし）
	p.SetExcludeUIDs(nil)

	// GameServer（シグナル注入器）を作成
	inj := sim.SignalInjector{
		NotifyLineIDChange:  p.NotifyLineIDChange,
		NotifyPostLoadReady: p.NotifyPostLoadReady,
	}
	gs := sim.NewGameServer(scenario, simSrv, inj)

	// EventEngine（イベント注入）を作成し GameServer に接続
	eventEngine := sim.NewEventEngine(scenario, simSrv, inj)
	gs.SetEventEngine(eventEngine)

	// Phase トラッカー
	tracker := &sim.PhaseTracker{}

	// チャンネル一時ファイル（patrol-sim は本番 config/ を読まない）
	tmpChannels, err := os.CreateTemp("", "patrol-sim-channels-*.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "channels temp file: %v\n", err)
		os.Exit(1)
	}
	tmpChannels.Close()
	defer os.Remove(tmpChannels.Name())

	// serials リスト
	serials := make([]string, len(scenario.Devices))
	for i, dev := range scenario.Devices {
		serials[i] = dev.Serial
	}

	// PatrolOptions
	opts := mumu.PatrolOptions{
		LoopMode: true,
	}

	// 巡回開始（goroutine で動作）
	p.Start(serials, scenario.Channels, tmpChannels.Name(), opts)

	// 監視ループ
	log.Printf("[patrol-sim] 巡回監視開始 (max_cycles=%d)", scenario.Assert.MaxCycles)
	result := runMonitor(p, tracker, scenario, capture, eventEngine)

	// 停止
	p.Stop()
	log.Printf("[patrol-sim] 巡回停止完了")

	// アサーション評価
	logLines := capture.Lines()
	deviceSerials := serials

	getActualCh := func(serial string) (expected, actual uint32) {
		for _, ds := range p.GetDeviceStatuses() {
			if ds.Serial == serial {
				return ds.ExpectedCh, ds.ActualCh
			}
		}
		return 0, 0
	}

	assertResult := sim.Evaluate(scenario.Assert, logLines, tracker, deviceSerials, getActualCh)

	// dwell 実測値を出力
	dwells := tracker.DwellSecs()
	if len(dwells) > 0 {
		var sum float64
		for _, d := range dwells {
			sum += d
		}
		avg := sum / float64(len(dwells))
		log.Printf("[patrol-sim] dwell_wait 実測: count=%d avg=%.2fs values=%v", len(dwells), avg, formatFloats(dwells))
	}

	// 結果出力
	fmt.Println()
	if result.timedOut {
		fmt.Fprintf(os.Stderr, "[patrol-sim] TIMEOUT: 全体タイムアウト（5分）\n")
		printFailures(assertResult.Failures)
		os.Exit(1)
	}

	if assertResult.Pass {
		fmt.Printf("=== PASS: %s ===\n", scenario.Name)
		os.Exit(0)
	} else {
		fmt.Printf("=== FAIL: %s ===\n", scenario.Name)
		printFailures(assertResult.Failures)
		os.Exit(1)
	}
}

// monitorResult は runMonitor の結果。
type monitorResult struct {
	timedOut bool
}

// runMonitor は PatrolStatus を 100ms ポーリングし、max_cycles 巡回完了を検出する。
func runMonitor(p *mumu.Patroller, tracker *sim.PhaseTracker, scenario *sim.Scenario, capture *sim.LogCapture, eventEngine *sim.EventEngine) monitorResult {
	maxCycles := scenario.Assert.MaxCycles
	if maxCycles <= 0 {
		maxCycles = 3
	}

	channels := scenario.Channels
	nCh := len(channels)

	// チャンネル遷移カウント: maxCycles 巡回完了 = nCh * maxCycles 回の dwell_wait
	targetDwells := nCh * maxCycles

	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	prevPhase := ""
	dwellCount := 0

	for {
		select {
		case <-timeout.C:
			return monitorResult{timedOut: true}

		case <-ticker.C:
			st := p.Status()

			// Phase 遷移追跡
			tracker.Update(st.Phase)

			// イベントエンジンに Phase を通知
			if eventEngine != nil {
				eventEngine.Update(st.Phase)
			}

			if st.Phase != prevPhase {
				log.Printf("[patrol-sim] phase=%s ch=%d idx=%d", st.Phase, st.CurrentChannel, st.CurrentIndex)
				prevPhase = st.Phase
			}

			// dwell_wait フェーズに入るたびにカウント
			if st.Phase == "dwell_wait" {
				dwells := tracker.DwellSecs()
				newCount := len(dwells)
				// 進行中の dwell_wait も実測済みとして追加カウント
				newCount++ // 現在進行中のものを含む
				if newCount > dwellCount {
					dwellCount = newCount
					if dwellCount >= targetDwells {
						log.Printf("[patrol-sim] %d サイクル完了（dwell_wait %d/%d 回）", maxCycles, dwellCount, targetDwells)
						return monitorResult{}
					}
				}
			}

			// 巡回が停止していたら終了
			if !st.Running {
				log.Printf("[patrol-sim] 巡回が停止した（LoopMode=false または Stop()）")
				return monitorResult{}
			}
		}
	}
}

// printFailures はアサーション失敗一覧を出力する。
func printFailures(failures []string) {
	for _, f := range failures {
		fmt.Printf("  FAIL: %s\n", f)
	}
}

// formatFloats は float64 スライスを "%v" 形式で短く表示する。
func formatFloats(vals []float64) string {
	if len(vals) == 0 {
		return "[]"
	}
	s := "["
	for i, v := range vals {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%.2f", v)
	}
	s += "]"
	return s
}
