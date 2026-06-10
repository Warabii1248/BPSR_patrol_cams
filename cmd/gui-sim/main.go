// Package main は gui-sim.exe の実装。
// 実機なしで gui.Server（本番 HTTP API）+ mumu.Patroller の
// end-to-end（ボタン押下 = HTTP API → 本番ハンドラ → Patroller → fake-adb）を検証するハーネス。
//
// 使い方:
//
//	gui-sim.exe -scenario scenarios/gui/baseline.json [-v] [-fake-adb path/to/fake-adb.exe]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/appconfig"
	"github.com/balrogsxt/StarResonanceAPI/debuglog"
	"github.com/balrogsxt/StarResonanceAPI/gui"
	"github.com/balrogsxt/StarResonanceAPI/sim"
)

func main() {
	scenarioFlag := flag.String("scenario", "", "シナリオ JSON ファイルパス（必須）")
	verboseFlag := flag.Bool("v", false, "詳細ログ出力")
	fakeADBFlag := flag.String("fake-adb", "", "fake-adb.exe のパス（未指定時は自バイナリと同ディレクトリ）")
	flag.Parse()

	if *scenarioFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: gui-sim -scenario <file> [-v] [-fake-adb <path>]")
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
	log.Printf("[gui-sim] シナリオ: %s (%d台 × %d ch)", scenario.Name, len(scenario.Devices), len(scenario.Channels))

	// fake-adb パス解決
	fakeADBPath := sim.ResolveFakeADB(*fakeADBFlag)
	log.Printf("[gui-sim] fake-adb: %s", fakeADBPath)

	// fake-adb が実在するか確認
	if _, err := os.Stat(fakeADBPath); err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb not found: %s\n  build with: go build -o release/fake-adb.exe ./cmd/fake-adb\n", fakeADBPath)
		os.Exit(1)
	}
	// 絶対パスに変換（一時 config.json の adb_path に書き込むため）
	fakeADBAbsPath, err := filepath.Abs(fakeADBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb path resolve error: %v\n", err)
		os.Exit(1)
	}

	// fake-adb が実行可能か確認
	if !sim.IsExecutable(fakeADBAbsPath) {
		fmt.Fprintf(os.Stderr, "fake-adb is not executable: %s\n", fakeADBAbsPath)
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
	log.Printf("[gui-sim] sim server listening at %s", simSrv.Addr)

	// PATROL_SIM_ADDR を設定（fake-adb 子プロセスに継承される）
	os.Setenv("PATROL_SIM_ADDR", simSrv.Addr)

	// 一時ディレクトリ（config.json / channels.txt / gold_history.json を隔離）
	tmpDir, err := os.MkdirTemp("", "gui-sim-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tmp dir create error: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	tmpConfigPath := filepath.Join(tmpDir, "config.json")
	tmpChannelsPath := filepath.Join(tmpDir, "channels.txt")
	tmpFilterPath := filepath.Join(tmpDir, "filter.json")

	// channels.txt（空ファイルでよい。Start() の channelsFile 引数用）
	if err := os.WriteFile(tmpChannelsPath, []byte{}, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "channels.txt create error: %v\n", err)
		os.Exit(1)
	}

	// serial一覧 / serial→UID / serial→label マップをシナリオから構築
	serials := make([]string, len(scenario.Devices))
	serialUIDs := make(map[string]uint64, len(scenario.Devices))
	serialLabels := make(map[string]string, len(scenario.Devices))
	for i, dev := range scenario.Devices {
		serials[i] = dev.Serial
		serialUIDs[dev.Serial] = dev.UID
		serialLabels[dev.Serial] = dev.Label
	}

	// mumu.Config を構築（Patroller の初期挙動はこちらが基準）
	mumuCfg := sim.BuildMumuConfig(scenario, fakeADBAbsPath)

	// 一時 config.json をシード（adb_path=fake-adb絶対パス）。
	// tmpConfigPath はまだ存在しないため appconfig.Load はデフォルト値（defaultConfig+applyDefaults）を返す。
	// これを base にして上書きすることで、PatrolMoveTimeoutSecs/PatrolMergeTimeoutSecs/
	// PatrolLoadStabilization* 等が 0 値のまま保存されることを防ぐ（config_identity_check による
	// GET→POST→UpdatePatrollerCfg 経由の MoveTimeout=0 化 → dwell_wait/stabilizing フェーズ消失を防止）。
	seedCfg, err := appconfig.Load(tmpConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config.json seed (defaults) load error: %v\n", err)
		os.Exit(1)
	}
	seedCfg.ADBPath = fakeADBAbsPath
	seedCfg.MumuSerials = serials
	seedCfg.GUIPort = 0
	seedCfg.FilterFile = tmpFilterPath
	seedCfg.SerialToUID = serialUIDs
	seedCfg.SerialToLabel = serialLabels
	// mumuCfg（BuildMumuConfig）と一致させ、config_identity_check の GET→POST 後も
	// Patroller の挙動（MoveTimeout/LoadStabilization/LoadDetectMode 等）が変化しないようにする。
	seedCfg.PatrolDwellSecs = mumuCfg.DwellDuration.Seconds()
	seedCfg.PatrolMoveTimeoutSecs = mumuCfg.MoveTimeout.Seconds()
	seedCfg.PatrolMergeTimeoutSecs = mumuCfg.MergeTimeout.Seconds()
	seedCfg.PatrolLoadStabilizationAuto = mumuCfg.LoadStabilizationAuto
	seedCfg.PatrolLoadStabilizationSecs = mumuCfg.LoadStabilizationDuration.Seconds()
	seedCfg.PatrolLoadDetectMode = mumuCfg.LoadDetectMode
	seedCfg.PatrolAdaptiveTimeout = mumuCfg.AdaptiveTimeout
	seedCfg.MumuClearLength = mumuCfg.ClearLength
	seedCfg.MumuTapX = mumuCfg.TapX
	seedCfg.MumuTapY = mumuCfg.TapY
	if err := appconfig.Save(tmpConfigPath, seedCfg); err != nil {
		fmt.Fprintf(os.Stderr, "config.json seed error: %v\n", err)
		os.Exit(1)
	}

	// GUI サーバー作成（port=0 → ephemeral）
	guiServer := gui.New(0, mumuCfg, scenario.Channels, tmpChannelsPath)

	// config.json の読み書きコールバック（main.go:443-477 の subset。capDevice 反映部分は省略）
	guiServer.SetConfigFns(
		func() ([]byte, error) {
			c, err := appconfig.Load(tmpConfigPath)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		},
		func(data []byte) error {
			c := &appconfig.Config{}
			if err := json.Unmarshal(data, c); err != nil {
				return err
			}
			return appconfig.Save(tmpConfigPath, c)
		},
	)

	// シリアル↔UID / シリアル↔ラベル マップを投入し、書き戻し先を一時 config に設定
	guiServer.LoadSerialUIDMap(serialUIDs)
	guiServer.SetSaveSerialUIDFn(func(m map[string]uint64) {
		c, err := appconfig.Load(tmpConfigPath)
		if err != nil {
			log.Printf("[gui-sim] serial_to_uid save: config load error: %v", err)
			return
		}
		c.SerialToUID = m
		if err := appconfig.Save(tmpConfigPath, c); err != nil {
			log.Printf("[gui-sim] serial_to_uid save error: %v", err)
		}
	})
	guiServer.LoadSerialLabelMap(serialLabels)
	guiServer.SetSaveSerialLabelFn(func(m map[string]string) {
		c, err := appconfig.Load(tmpConfigPath)
		if err != nil {
			log.Printf("[gui-sim] serial_to_label save: config load error: %v", err)
			return
		}
		c.SerialToLabel = m
		if err := appconfig.Save(tmpConfigPath, c); err != nil {
			log.Printf("[gui-sim] serial_to_label save error: %v", err)
		}
	})

	// excludeUIDs は空（シミュレーターでは除外UID なし）
	guiServer.SetExcludeUIDs(nil)
	// 起動時デバイス未検出ダイアログは sim では無意味（SSE 未配信）
	guiServer.SetShowNoDeviceDialog(false)

	// GameServer（シグナル注入器）を作成。Patroller 直接ではなく guiServer 経由で注入する。
	inj := sim.SignalInjector{
		NotifyLineIDChange:  guiServer.NotifyLineIDChange,
		NotifyPostLoadReady: guiServer.NotifyPostLoadReady,
	}
	gs := sim.NewGameServer(scenario, simSrv, inj)

	// EventEngine（イベント注入）を作成し GameServer に接続
	eventEngine := sim.NewEventEngine(scenario, simSrv, inj)
	gs.SetEventEngine(eventEngine)

	// Phase トラッカー
	tracker := &sim.PhaseTracker{}

	// HTTP サーバー起動（WebView2なし）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseURL, err := guiServer.StartServer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gui server start error: %v\n", err)
		os.Exit(1)
	}
	log.Printf("[gui-sim] GUI server listening at %s", baseURL)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// 巡回開始（= 開始ボタン押下を HTTP 経由で実行）
	startBody, _ := json.Marshal(map[string]interface{}{
		"serials":   serials,
		"channels":  scenario.Channels,
		"loop_mode": true,
	})
	startResp, err := postJSON(httpClient, baseURL+"/api/patrol/start", startBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "POST /api/patrol/start error: %v\n", err)
		os.Exit(1)
	}
	if ok, _ := startResp["ok"].(bool); !ok {
		fmt.Fprintf(os.Stderr, "POST /api/patrol/start failed: %v\n", startResp)
		os.Exit(1)
	}
	log.Printf("[gui-sim] 巡回開始 (POST /api/patrol/start ok=true)")

	// アクションドライバ初期化
	driver := newActionDriver(httpClient, baseURL, scenario, tmpConfigPath, fakeADBAbsPath)

	// 監視ループ
	log.Printf("[gui-sim] 巡回監視開始 (max_cycles=%d)", scenario.Assert.MaxCycles)
	result := runMonitor(httpClient, baseURL, tracker, scenario, eventEngine, driver)

	// 停止（= 停止ボタン押下を HTTP 経由で実行）
	stopOK := stopPatrolAndVerify(httpClient, baseURL)
	log.Printf("[gui-sim] 巡回停止完了 (running=false 確認: %v)", stopOK)

	// アサーション評価
	logLines := capture.Lines()
	deviceSerials := serials

	getActualCh := func(serial string) (expected, actual uint32) {
		statuses, err := getDeviceStatuses(httpClient, baseURL)
		if err != nil {
			return 0, 0
		}
		for _, ds := range statuses {
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
		log.Printf("[gui-sim] dwell_wait 実測: count=%d avg=%.2fs values=%v", len(dwells), avg, formatFloats(dwells))
	}

	// アクション結果の集計
	actionFailures := driver.Failures()

	// 結果出力
	fmt.Println()
	if result.timedOut {
		fmt.Fprintf(os.Stderr, "[gui-sim] TIMEOUT: 全体タイムアウト（5分）\n")
		printFailures(assertResult.Failures)
		printFailures(actionFailures)
		os.Exit(1)
	}
	if !stopOK {
		actionFailures = append(actionFailures, "patrol/stop: running=false が1秒以内に確認できなかった")
	}

	allPass := assertResult.Pass && len(actionFailures) == 0

	if allPass {
		fmt.Printf("=== PASS: %s ===\n", scenario.Name)
		os.Exit(0)
	} else {
		fmt.Printf("=== FAIL: %s ===\n", scenario.Name)
		printFailures(assertResult.Failures)
		printFailures(actionFailures)
		os.Exit(1)
	}
}

// monitorResult は runMonitor の結果。
type monitorResult struct {
	timedOut bool
}

// patrolStatusJSON は GET /api/patrol/status の JSON デコード用。
type patrolStatusJSON struct {
	Phase          string `json:"phase"`
	CurrentChannel uint32 `json:"current_channel"`
	CurrentIndex   int    `json:"current_index"`
	Running        bool   `json:"running"`
}

// deviceStatusJSON は GET /api/patrol/device-statuses の1要素デコード用。
type deviceStatusJSON struct {
	Serial     string `json:"serial"`
	ExpectedCh uint32 `json:"expected_ch"`
	ActualCh   uint32 `json:"actual_ch"`
}

// runMonitor は GET /api/patrol/status を 100ms ポーリングし、max_cycles 巡回完了を検出する。
// patrol-sim の runMonitor と同一の終了条件（targetDwells / running=false / 5分タイムアウト）。
func runMonitor(client *http.Client, baseURL string, tracker *sim.PhaseTracker, scenario *sim.Scenario, eventEngine *sim.EventEngine, driver *actionDriver) monitorResult {
	maxCycles := scenario.Assert.MaxCycles
	if maxCycles <= 0 {
		maxCycles = 3
	}

	channels := scenario.Channels
	nCh := len(channels)

	// チャンネル遷移カウント: maxCycles 巡回完了 = nCh * maxCycles 回の dwell_wait
	targetDwells := nCh * maxCycles

	startedAt := time.Now()

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
			st, err := getPatrolStatus(client, baseURL)
			if err != nil {
				log.Printf("[gui-sim] GET /api/patrol/status error: %v", err)
				continue
			}

			// Phase 遷移追跡
			tracker.Update(st.Phase)

			// イベントエンジンに Phase を通知
			if eventEngine != nil {
				eventEngine.Update(st.Phase)
			}

			// アクションドライバに Phase / 経過時間を通知して発火判定
			driver.Update(st.Phase, time.Since(startedAt))

			if st.Phase != prevPhase {
				log.Printf("[gui-sim] phase=%s ch=%d idx=%d", st.Phase, st.CurrentChannel, st.CurrentIndex)
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
						log.Printf("[gui-sim] %d サイクル完了（dwell_wait %d/%d 回）", maxCycles, dwellCount, targetDwells)
						return monitorResult{}
					}
				}
			}

			// 巡回が停止していたら終了
			if !st.Running {
				log.Printf("[gui-sim] 巡回が停止した（LoopMode=false または Stop()）")
				return monitorResult{}
			}
		}
	}
}

// getPatrolStatus は GET /api/patrol/status を取得しデコードする。
func getPatrolStatus(client *http.Client, baseURL string) (patrolStatusJSON, error) {
	var st patrolStatusJSON
	resp, err := client.Get(baseURL + "/api/patrol/status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, err
	}
	return st, nil
}

// getDeviceStatuses は GET /api/patrol/device-statuses を取得しデコードする。
func getDeviceStatuses(client *http.Client, baseURL string) ([]deviceStatusJSON, error) {
	resp, err := client.Get(baseURL + "/api/patrol/device-statuses")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []deviceStatusJSON
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// stopPatrolAndVerify は POST /api/patrol/stop を実行し、1秒以内に running=false を確認する。
func stopPatrolAndVerify(client *http.Client, baseURL string) bool {
	if _, err := postJSON(client, baseURL+"/api/patrol/stop", nil); err != nil {
		log.Printf("[gui-sim] POST /api/patrol/stop error: %v", err)
		return false
	}
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		st, err := getPatrolStatus(client, baseURL)
		if err == nil && !st.Running {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// postJSON は POST リクエストを送信し、JSON レスポンスを map にデコードして返す。
func postJSON(client *http.Client, url string, body []byte) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		// ボディが空 / JSON でない場合もある（{}でないハンドラ）
		return map[string]interface{}{}, nil
	}
	return out, nil
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
