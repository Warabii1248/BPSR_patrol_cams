// Package sim はパトロールシミュレーターのコアパッケージ。
// シナリオ JSON の読み込み・ゲームサーバーシミュレータ・アサーションエンジンを提供する。
package sim

import (
	"encoding/json"
	"fmt"
	"os"
)

// DelayRange は min_ms〜max_ms の一様乱数遅延仕様。
type DelayRange struct {
	MinMs int `json:"min_ms"`
	MaxMs int `json:"max_ms"`
}

// DeviceDef はシナリオ内のデバイス定義。
type DeviceDef struct {
	Serial    string `json:"serial"`
	UID       uint64 `json:"uid"`
	Label     string `json:"label"`
	InitialCh uint32 `json:"initial_ch"`
}

// PatrolDef はシナリオ内の巡回パラメーター。
type PatrolDef struct {
	DwellSecs           float64 `json:"dwell_secs"`
	MoveTimeoutSecs     float64 `json:"move_timeout_secs"`
	LoadDetectMode      string  `json:"load_detect_mode"`
	// Screen 系フィールド（load_detect_mode="screen"/"either" 用）
	ScreenPollIntervalMs    int     `json:"screen_poll_interval_ms"`
	ScreenDetectTimeoutSecs float64 `json:"screen_detect_timeout_secs"`
	ScreenRegionX           int     `json:"screen_region_x"`
	ScreenRegionY           int     `json:"screen_region_y"`
	ScreenRegionW           int     `json:"screen_region_w"`
	ScreenRegionH           int     `json:"screen_region_h"`
	ScreenBlackLuma         uint8   `json:"screen_black_luma"`
	ScreenBlackPixelRatio   float64 `json:"screen_black_pixel_ratio"`
}

// ServerDef はゲームサーバーシミュレーターのパラメーター。
type ServerDef struct {
	LineIDChangeDelay DelayRange `json:"line_id_change_delay"`
	PostLoadDelay     DelayRange `json:"post_load_delay"`
	AdbCmdDelayMs     int        `json:"adb_cmd_delay_ms"`
}

// EventDef はシナリオ内の外乱イベント定義。
type EventDef struct {
	AtPhase         string `json:"at_phase"`          // フェーズ名で発火
	Cycles          int    `json:"at_cycle"`           // N 巡目のその phase で発火（省略時=1）
	Type            string `json:"type"`               // "native_move" | "burst_0x2e" | "silent_device" | "delayed_signal"
	UID             uint64 `json:"uid"`                // native_move / burst_0x2e 用 UID
	LineID          uint32 `json:"line_id"`            // burst_0x2e 用 lineID
	ToCh            uint32 `json:"to_ch"`              // native_move 用 to_ch
	Serial          string `json:"serial"`             // silent_device / delayed_signal 用 serial
	Count           int    `json:"count"`              // burst_0x2e 用 連射数
	IntervalMs      int    `json:"interval_ms"`        // burst_0x2e 用 間隔ms
	PostLoadDelayMs int    `json:"post_load_delay_ms"` // delayed_signal 用 遅延ms
	SilentCycles    int    `json:"silent_cycles"`      // silent_device 用 無応答サイクル数（省略時=1）
}

// GUIActionDef はシナリオ内の GUI アクション定義（gui-sim 専用）。
// EventDef と同じ意味論で at_phase/at_cycle 発火判定を行う。
type GUIActionDef struct {
	AtPhase   string  `json:"at_phase"`   // フェーズ名で発火（EventDef と同じ意味論）
	AtCycle   int     `json:"at_cycle"`   // N 巡目（省略時=1）
	AfterSecs float64 `json:"after_secs"` // at_phase 省略時: 開始からの経過秒で発火

	Type   string `json:"type"`   // "api" | "sweep_readonly" | "config_patch" | "config_identity_check"
	Method string `json:"method"` // api 用: "GET"|"POST"
	Path   string `json:"path"`   // api 用: "/api/..."

	Body  json.RawMessage `json:"body"`  // api 用 POST ボディ
	Patch json.RawMessage `json:"patch"` // config_patch 用: マージするキー群

	ExpectStatus       int      `json:"expect_status"`        // 省略時=200
	ExpectBodyContains []string `json:"expect_body_contains"` // レスポンスボディ部分一致（任意）
}

// AssertDef はアサーション仕様。
type AssertDef struct {
	MaxCycles                int      `json:"max_cycles"`
	ForbidLogPatterns        []string `json:"forbid_log_patterns"`
	RequireLogPatterns       []string `json:"require_log_patterns"`
	DwellPhaseSecsMin        float64  `json:"dwell_phase_secs_min"`
	DwellPhaseSecsMax        float64  `json:"dwell_phase_secs_max"`
	AllDevicesActualChFollows bool     `json:"all_devices_actual_ch_follows"`
}

// rawAssertDef は JSON パース用中間型（dwell_phase_secs が object の場合に対応）。
type rawAssertDef struct {
	MaxCycles                int      `json:"max_cycles"`
	ForbidLogPatterns        []string `json:"forbid_log_patterns"`
	RequireLogPatterns       []string `json:"require_log_patterns"`
	DwellPhaseSecs           *struct {
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	} `json:"dwell_phase_secs"`
	AllDevicesActualChFollows bool `json:"all_devices_actual_ch_follows"`
}

// Scenario はシナリオ JSON のトップレベル構造体。
type Scenario struct {
	Name       string         `json:"name"`
	Seed       int64          `json:"seed"`
	Devices    []DeviceDef    `json:"devices"`
	Channels   []uint32       `json:"channels"`
	Patrol     PatrolDef      `json:"patrol"`
	Server     ServerDef      `json:"server"`
	Events     []EventDef     `json:"events"`
	GUIActions []GUIActionDef `json:"gui_actions"` // gui-sim 専用（省略時 nil）
	Assert     AssertDef      `json:"-"`           // カスタムパース
}

// scenarioJSON は JSON パース用中間型。
type scenarioJSON struct {
	Name       string          `json:"name"`
	Seed       int64           `json:"seed"`
	Devices    []DeviceDef     `json:"devices"`
	Channels   []uint32        `json:"channels"`
	Patrol     PatrolDef       `json:"patrol"`
	Server     ServerDef       `json:"server"`
	Events     []EventDef      `json:"events"`
	GUIActions []GUIActionDef  `json:"gui_actions"`
	Assert     json.RawMessage `json:"assert"`
}

// LoadScenario はファイルからシナリオを読み込む。
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scenario load: %w", err)
	}

	var raw scenarioJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("scenario parse: %w", err)
	}

	s := &Scenario{
		Name:       raw.Name,
		Seed:       raw.Seed,
		Devices:    raw.Devices,
		Channels:   raw.Channels,
		Patrol:     raw.Patrol,
		Server:     raw.Server,
		Events:     raw.Events,
		GUIActions: raw.GUIActions,
	}

	// assert フィールドを中間型でパース
	if raw.Assert != nil {
		var ra rawAssertDef
		if err := json.Unmarshal(raw.Assert, &ra); err != nil {
			return nil, fmt.Errorf("assert parse: %w", err)
		}
		s.Assert = AssertDef{
			MaxCycles:                ra.MaxCycles,
			ForbidLogPatterns:        ra.ForbidLogPatterns,
			RequireLogPatterns:       ra.RequireLogPatterns,
			AllDevicesActualChFollows: ra.AllDevicesActualChFollows,
		}
		if ra.DwellPhaseSecs != nil {
			s.Assert.DwellPhaseSecsMin = ra.DwellPhaseSecs.Min
			s.Assert.DwellPhaseSecsMax = ra.DwellPhaseSecs.Max
		}
	}

	if len(s.Devices) == 0 {
		return nil, fmt.Errorf("scenario: devices は1台以上必要")
	}
	if len(s.Channels) == 0 {
		return nil, fmt.Errorf("scenario: channels は1ch以上必要")
	}

	return s, nil
}
