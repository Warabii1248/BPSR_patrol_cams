package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/sim"
)

// sweepReadonlyPaths は sweep_readonly アクションで疎通確認する GET 系エンドポイント一覧。
// SSE (/events, /api/chat-events) と /api/patrol/screenshot は対象外（仕様書 §4 sweep_readonly テーブル）。
// 期待ステータスは実装時に実応答で確定したもの（全件 200）。
var sweepReadonlyPaths = []string{
	"/",
	"/spawn-log",
	"/chat-log",
	"/api/devices",
	"/api/device-map",
	"/api/logs",
	"/api/patrol/status",
	"/api/patrol/device-statuses",
	"/api/patrol/channels",
	"/api/config",
	"/api/gold-history",
	"/api/adb/tap-loop/status",
	"/api/chat-log",
	"/api/portmap/pending",
	"/api/portmap/entries",
	"/api/devices/identified",
	"/api/devices/memory",
}

// actionDriver はシナリオの gui_actions を監視ループ内で発火・実行・記録する。
type actionDriver struct {
	client        *http.Client
	baseURL       string
	scenario      *sim.Scenario
	tmpConfigPath string
	fakeADBPath   string

	mu          sync.Mutex
	fired       map[int]int // インデックス → 発火済みサイクル番号
	phaseCounts map[string]int
	prevPhase   string
	firedAfter  map[int]bool // after_secs 用の発火済みフラグ

	failures []string
}

// newActionDriver は actionDriver を作成する。
func newActionDriver(client *http.Client, baseURL string, scenario *sim.Scenario, tmpConfigPath, fakeADBPath string) *actionDriver {
	return &actionDriver{
		client:        client,
		baseURL:       baseURL,
		scenario:      scenario,
		tmpConfigPath: tmpConfigPath,
		fakeADBPath:   fakeADBPath,
		fired:         make(map[int]int),
		phaseCounts:   make(map[string]int),
		firedAfter:    make(map[int]bool),
	}
}

// Update は現在の Phase と開始からの経過時間を渡し、発火条件を満たす gui_actions を実行する。
// EventEngine.Update と同じ意味論（at_phase: phase 遷移時にカウント、at_cycle 一致で1回だけ発火）。
// after_secs は at_phase 省略時のみ評価し、経過時間到達で1回だけ発火する。
func (d *actionDriver) Update(phase string, elapsed time.Duration) {
	d.mu.Lock()

	var toFire []sim.GUIActionDef

	if phase != d.prevPhase {
		d.phaseCounts[phase]++
		d.prevPhase = phase

		for i, act := range d.scenario.GUIActions {
			if act.AtPhase == "" || act.AtPhase != phase {
				continue
			}
			targetCycle := act.AtCycle
			if targetCycle <= 0 {
				targetCycle = 1
			}
			count := d.phaseCounts[phase]
			if count != targetCycle {
				continue
			}
			if fired, ok := d.fired[i]; ok && fired == targetCycle {
				continue
			}
			d.fired[i] = targetCycle
			toFire = append(toFire, act)
		}
	}

	// after_secs（at_phase 省略時のみ）
	for i, act := range d.scenario.GUIActions {
		if act.AtPhase != "" || act.AfterSecs <= 0 {
			continue
		}
		if d.firedAfter[i] {
			continue
		}
		if elapsed.Seconds() < act.AfterSecs {
			continue
		}
		d.firedAfter[i] = true
		toFire = append(toFire, act)
	}

	d.mu.Unlock()

	for _, act := range toFire {
		d.execute(act)
	}
}

// Failures は実行済みアクションの失敗一覧を返す。
func (d *actionDriver) Failures() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.failures))
	copy(out, d.failures)
	return out
}

func (d *actionDriver) addFailure(msg string) {
	d.mu.Lock()
	d.failures = append(d.failures, msg)
	d.mu.Unlock()
	log.Printf("[gui-sim] ACTION FAIL: %s", msg)
}

// execute は1つの gui_action を実行する。
func (d *actionDriver) execute(act sim.GUIActionDef) {
	log.Printf("[gui-sim] ACTION fire type=%s at_phase=%s at_cycle=%d path=%s", act.Type, act.AtPhase, act.AtCycle, act.Path)

	switch act.Type {
	case "api":
		d.execAPI(act)
	case "sweep_readonly":
		d.execSweepReadonly(act)
	case "config_patch":
		d.execConfigPatch(act)
	case "config_identity_check":
		d.execConfigIdentityCheck(act)
	default:
		d.addFailure(fmt.Sprintf("unknown gui_action type: %s", act.Type))
	}
}

// execAPI は "api" アクションを実行する。Method/Path/Body をそのまま送信し、ステータスとボディを照合する。
func (d *actionDriver) execAPI(act sim.GUIActionDef) {
	method := act.Method
	if method == "" {
		method = http.MethodGet
	}
	expectStatus := act.ExpectStatus
	if expectStatus == 0 {
		expectStatus = 200
	}

	var bodyReader io.Reader
	if len(act.Body) > 0 {
		bodyReader = bytes.NewReader(act.Body)
	}

	req, err := http.NewRequest(method, d.baseURL+act.Path, bodyReader)
	if err != nil {
		d.addFailure(fmt.Sprintf("api %s %s: request build error: %v", method, act.Path, err))
		return
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.addFailure(fmt.Sprintf("api %s %s: request error: %v", method, act.Path, err))
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != expectStatus {
		d.addFailure(fmt.Sprintf("api %s %s: expected status=%d actual=%d body=%s", method, act.Path, expectStatus, resp.StatusCode, string(bodyBytes)))
		return
	}

	for _, want := range act.ExpectBodyContains {
		if !strings.Contains(string(bodyBytes), want) {
			d.addFailure(fmt.Sprintf("api %s %s: response body does not contain %q (body=%s)", method, act.Path, want, string(bodyBytes)))
		}
	}
}

// execSweepReadonly はハーネス組込テーブルの GET 系全エンドポイントを順に叩き、全件 200 を確認する。
func (d *actionDriver) execSweepReadonly(act sim.GUIActionDef) {
	for _, path := range sweepReadonlyPaths {
		resp, err := d.client.Get(d.baseURL + path)
		if err != nil {
			d.addFailure(fmt.Sprintf("sweep_readonly GET %s: request error: %v", path, err))
			continue
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			d.addFailure(fmt.Sprintf("sweep_readonly GET %s: expected status=200 actual=%d body=%s", path, resp.StatusCode, string(bodyBytes)))
		}
	}
}

// execConfigPatch は GET /api/config → 取得 JSON に Patch のキーをマージ → POST /api/config を実行する。
// GUI JS の実挙動（全量GET→編集→全量POST）を模倣する。部分 JSON を直接 POST してはいけない。
func (d *actionDriver) execConfigPatch(act sim.GUIActionDef) {
	cur, err := d.getConfigMap()
	if err != nil {
		d.addFailure(fmt.Sprintf("config_patch: GET /api/config error: %v", err))
		return
	}

	var patch map[string]interface{}
	if len(act.Patch) > 0 {
		if err := json.Unmarshal(act.Patch, &patch); err != nil {
			d.addFailure(fmt.Sprintf("config_patch: patch unmarshal error: %v", err))
			return
		}
	}
	for k, v := range patch {
		cur[k] = v
	}

	merged, err := json.Marshal(cur)
	if err != nil {
		d.addFailure(fmt.Sprintf("config_patch: marshal error: %v", err))
		return
	}

	resp, err := postJSON(d.client, d.baseURL+"/api/config", merged)
	if err != nil {
		d.addFailure(fmt.Sprintf("config_patch: POST /api/config error: %v", err))
		return
	}
	if ok, _ := resp["ok"].(bool); !ok {
		d.addFailure(fmt.Sprintf("config_patch: POST /api/config response not ok: %v", resp))
	}
}

// execConfigIdentityCheck は GET → そのまま POST → 再 GET → 正規化 JSON 比較で完全一致を確認する。
// Load/Save 非対称（No.31 型）が混入すると、ここでデフォルト値の脱落が差分として検出される。
func (d *actionDriver) execConfigIdentityCheck(act sim.GUIActionDef) {
	before, err := d.getConfigRaw()
	if err != nil {
		d.addFailure(fmt.Sprintf("config_identity_check: GET(1) /api/config error: %v", err))
		return
	}

	resp, err := postJSON(d.client, d.baseURL+"/api/config", before)
	if err != nil {
		d.addFailure(fmt.Sprintf("config_identity_check: POST /api/config error: %v", err))
		return
	}
	if ok, _ := resp["ok"].(bool); !ok {
		d.addFailure(fmt.Sprintf("config_identity_check: POST /api/config response not ok: %v", resp))
		return
	}

	after, err := d.getConfigRaw()
	if err != nil {
		d.addFailure(fmt.Sprintf("config_identity_check: GET(2) /api/config error: %v", err))
		return
	}

	beforeNorm, err := normalizeJSON(before)
	if err != nil {
		d.addFailure(fmt.Sprintf("config_identity_check: normalize(before) error: %v", err))
		return
	}
	afterNorm, err := normalizeJSON(after)
	if err != nil {
		d.addFailure(fmt.Sprintf("config_identity_check: normalize(after) error: %v", err))
		return
	}

	if !reflect.DeepEqual(beforeNorm, afterNorm) {
		d.addFailure(fmt.Sprintf("config_identity_check: GET→POST→GET で内容が変化した\n  before=%s\n  after=%s", mustMarshal(beforeNorm), mustMarshal(afterNorm)))
	}
}

// getConfigMap は GET /api/config を map[string]interface{} としてデコードする。
func (d *actionDriver) getConfigMap() (map[string]interface{}, error) {
	raw, err := d.getConfigRaw()
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// getConfigRaw は GET /api/config の生レスポンスボディを返す。
func (d *actionDriver) getConfigRaw() ([]byte, error) {
	resp, err := d.client.Get(d.baseURL + "/api/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// normalizeJSON は JSON バイト列を map[string]interface{} にデコードして返す（キー順序非依存比較用）。
func normalizeJSON(data []byte) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mustMarshal はデバッグ表示用に JSON へ変換する（map のキーは Marshal が常にソートする）。
func mustMarshal(m map[string]interface{}) string {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(b)
}
