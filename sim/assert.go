package sim

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// PhaseRecord は PatrolStatus.Phase の遷移記録。
type PhaseRecord struct {
	Phase     string
	EnteredAt time.Time
	ExitedAt  time.Time // ゼロ値 = まだ在相中
}

// PhaseTracker は Phase 遷移を時刻付きで記録する。
type PhaseTracker struct {
	mu      sync.Mutex
	records []PhaseRecord
	current string
}

// Update は新しい Phase を観測し、遷移があれば記録する。
func (t *PhaseTracker) Update(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if phase == t.current {
		return
	}
	now := time.Now()
	if len(t.records) > 0 && t.records[len(t.records)-1].ExitedAt.IsZero() {
		t.records[len(t.records)-1].ExitedAt = now
	}
	t.records = append(t.records, PhaseRecord{Phase: phase, EnteredAt: now})
	t.current = phase
}

// Records はコピーを返す。
func (t *PhaseTracker) Records() []PhaseRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PhaseRecord, len(t.records))
	copy(out, t.records)
	return out
}

// DwellSecs は "dwell_wait" フェーズの滞在秒数一覧を返す（完了済みのもののみ）。
func (t *PhaseTracker) DwellSecs() []float64 {
	records := t.Records()
	var result []float64
	for _, r := range records {
		if r.Phase == "dwell_wait" && !r.ExitedAt.IsZero() {
			result = append(result, r.ExitedAt.Sub(r.EnteredAt).Seconds())
		}
	}
	return result
}

// LogCapture は log パッケージの出力を行単位でバッファリングする。
type LogCapture struct {
	mu    sync.Mutex
	lines []string
}

// Write は io.Writer 実装。改行で分割して行バッファに追加する。
func (c *LogCapture) Write(p []byte) (n int, err error) {
	text := string(p)
	for {
		idx := strings.Index(text, "\n")
		if idx < 0 {
			if text != "" {
				c.mu.Lock()
				c.lines = append(c.lines, text)
				c.mu.Unlock()
			}
			break
		}
		line := text[:idx]
		if line != "" {
			c.mu.Lock()
			c.lines = append(c.lines, line)
			c.mu.Unlock()
		}
		text = text[idx+1:]
	}
	return len(p), nil
}

// Lines はキャプチャ済み全行のコピーを返す。
func (c *LogCapture) Lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// AssertResult はアサーション評価結果。
type AssertResult struct {
	Pass     bool
	Failures []string
}

// Evaluate はシナリオの AssertDef に従いアサーションを評価する。
func Evaluate(
	def AssertDef,
	logLines []string,
	tracker *PhaseTracker,
	deviceSerials []string,
	getActualCh func(serial string) (expected, actual uint32),
) AssertResult {
	var failures []string

	// forbid パターン
	for _, pat := range def.ForbidLogPatterns {
		for _, line := range logLines {
			if strings.Contains(line, pat) {
				failures = append(failures, fmt.Sprintf("forbid_log_pattern %q found in: %s", pat, line))
				break
			}
		}
	}

	// require パターン
	for _, pat := range def.RequireLogPatterns {
		found := false
		for _, line := range logLines {
			if strings.Contains(line, pat) {
				found = true
				break
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf("require_log_pattern %q not found in logs", pat))
		}
	}

	// dwell_phase_secs
	if def.DwellPhaseSecsMin > 0 || def.DwellPhaseSecsMax > 0 {
		dwells := tracker.DwellSecs()
		if len(dwells) == 0 {
			failures = append(failures, "dwell_phase_secs: no dwell_wait phases recorded")
		} else {
			for i, d := range dwells {
				if def.DwellPhaseSecsMin > 0 && d < def.DwellPhaseSecsMin {
					failures = append(failures, fmt.Sprintf("dwell[%d]=%.2fs < min=%.2fs", i, d, def.DwellPhaseSecsMin))
				}
				if def.DwellPhaseSecsMax > 0 && d > def.DwellPhaseSecsMax {
					failures = append(failures, fmt.Sprintf("dwell[%d]=%.2fs > max=%.2fs", i, d, def.DwellPhaseSecsMax))
				}
			}
		}
	}

	// all_devices_actual_ch_follows
	if def.AllDevicesActualChFollows && getActualCh != nil {
		for _, serial := range deviceSerials {
			expected, actual := getActualCh(serial)
			if expected != 0 && actual != expected {
				failures = append(failures, fmt.Sprintf("serial=%s expected_ch=%d actual_ch=%d (mismatch)", serial, expected, actual))
			}
		}
	}

	return AssertResult{
		Pass:     len(failures) == 0,
		Failures: failures,
	}
}
