package mumu

import (
	"reflect"
	"testing"
	"time"
)

// =====================================================================
// 純関数テスト
// =====================================================================

func TestAbsDiffUint32(t *testing.T) {
	cases := []struct {
		a, b uint32
		want uint32
	}{
		{10, 3, 7},
		{3, 10, 7},
		{0, 0, 0},
		{100, 100, 0},
		{0, 50, 50},
	}
	for _, c := range cases {
		if got := absDiffUint32(c.a, c.b); got != c.want {
			t.Errorf("absDiffUint32(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFindResumeIndex(t *testing.T) {
	channels := []uint32{1, 5, 10, 20, 50}

	cases := []struct {
		name   string
		lastCh uint32
		want   int
	}{
		{"exact_match_5", 5, 1},
		{"exact_match_50", 50, 4},
		{"nearest_to_3", 3, 0},   // 3 は 1 と 5 の中間だが 1 が先（より近い）
		{"nearest_to_8", 8, 2},   // 8 は 10 に近い
		{"nearest_to_15", 15, 2}, // 15 は 10 と 20 の中間、より近いのは 10 か 20？|15-10|=5, |15-20|=5 → 同距離なら先（10）が選ばれる
		{"nearest_to_60", 60, 4}, // 60 は 50 に最も近い
		{"zero_lastCh_returns_0", 0, 0},
		{"empty_channels", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chs := channels
			if c.name == "empty_channels" {
				chs = nil
			}
			if got := findResumeIndex(chs, c.lastCh); got != c.want {
				t.Errorf("findResumeIndex(_, %d) = %d, want %d", c.lastCh, got, c.want)
			}
		})
	}
}

func TestCanonicalADBSerial(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"emulator-5580", "127.0.0.1:5581"},
		{"emulator-5582", "127.0.0.1:5583"},
		{"emulator-5581", "emulator-5581"}, // 奇数番は変換しない
		{"127.0.0.1:5581", "127.0.0.1:5581"},
		{"  emulator-5580  ", "127.0.0.1:5581"}, // trim
		{"adb-device-001", "adb-device-001"},
		{"", ""},
	}
	for _, c := range cases {
		if got := canonicalADBSerial(c.in); got != c.want {
			t.Errorf("canonicalADBSerial(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeDeviceList_DedupAndHostPortPriority(t *testing.T) {
	// emulator-5580 と 127.0.0.1:5581 は同一実体 → host:port を優先
	in := []string{"emulator-5580", "127.0.0.1:5581", "127.0.0.1:5583", "emulator-5582"}
	got := normalizeDeviceList(in)
	want := []string{"127.0.0.1:5581", "127.0.0.1:5583"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeDeviceList = %v, want %v", got, want)
	}
}

func TestNormalizeDeviceList_TrimsAndSkipsEmpty(t *testing.T) {
	got := normalizeDeviceList([]string{"  ", "", "emulator-5581"})
	want := []string{"emulator-5581"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSelectDevicesForPatrol(t *testing.T) {
	devices := []string{"127.0.0.1:5581", "127.0.0.1:5583", "127.0.0.1:5585"}
	configured := []string{"emulator-5580", "emulator-5582"} // 5580↔5581 / 5582↔5583
	got := selectDevicesForPatrol(devices, configured)
	want := []string{"127.0.0.1:5581", "127.0.0.1:5583"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectDevicesForPatrol = %v, want %v", got, want)
	}
}

func TestSelectDevicesForPatrol_EmptyConfiguredReturnsAll(t *testing.T) {
	devices := []string{"a", "b"}
	got := selectDevicesForPatrol(devices, nil)
	if !reflect.DeepEqual(got, devices) {
		t.Errorf("empty configured: got %v, want passthrough %v", got, devices)
	}
}

func TestParallelLimit(t *testing.T) {
	cases := []struct {
		name        string
		limit, tot  int
		want        int
	}{
		{"unlimited_zero", 0, 5, 5},
		{"unlimited_negative", -1, 5, 5},
		{"over_total", 100, 3, 3},
		{"within_total", 2, 8, 2},
		{"equal", 5, 5, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{ParallelLimit: c.limit}
			if got := ParallelLimit(cfg, c.tot); got != c.want {
				t.Errorf("ParallelLimit(%d, %d) = %d, want %d", c.limit, c.tot, got, c.want)
			}
		})
	}
}

// seqCh は start..end（両端含む）の連番 ch スライスを生成するテストヘルパ。
func seqCh(start, end uint32) []uint32 {
	var chs []uint32
	for c := start; c <= end; c++ {
		chs = append(chs, c)
	}
	return chs
}

func TestComputeDeviceAssignments(t *testing.T) {
	// 1台: 全範囲
	got := ComputeDeviceAssignments(1, seqCh(1, 10))
	if len(got) != 1 || len(got[0].Channels) != 10 || got[0].Channels[9] != 10 {
		t.Errorf("1 device 10 ch: got %+v", got)
	}

	// 2台 (1ペア): 全範囲
	got = ComputeDeviceAssignments(2, seqCh(1, 100))
	if len(got) != 1 {
		t.Fatalf("2 devices: groups expected 1, got %d", len(got))
	}
	if len(got[0].Channels) != 100 || !reflect.DeepEqual(got[0].DeviceIndices, []int{0, 1}) {
		t.Errorf("2 devices: %+v", got)
	}

	// 4台 (2ペア): 50 ch ずつ
	got = ComputeDeviceAssignments(4, seqCh(1, 100))
	if len(got) != 2 {
		t.Fatalf("4 devices: groups expected 2, got %d", len(got))
	}
	if len(got[0].Channels) != 50 || len(got[1].Channels) != 50 {
		t.Errorf("4 devices: ch counts %d, %d", len(got[0].Channels), len(got[1].Channels))
	}
	if got[0].Channels[0] != 1 || got[0].Channels[49] != 50 {
		t.Errorf("group0 channels: first=%d last=%d", got[0].Channels[0], got[0].Channels[49])
	}
	if got[1].Channels[0] != 51 || got[1].Channels[49] != 100 {
		t.Errorf("group1 channels: first=%d last=%d", got[1].Channels[0], got[1].Channels[49])
	}

	// 6台 (3ペア): 33ch + 33ch + 34ch（最終グループに余り）
	got = ComputeDeviceAssignments(6, seqCh(1, 100))
	if len(got) != 3 {
		t.Fatalf("6 devices: groups expected 3, got %d", len(got))
	}
	if len(got[2].Channels) != 34 {
		t.Errorf("6 devices: last group expected 34 ch, got %d", len(got[2].Channels))
	}

	// 0台・負数: nil
	if got := ComputeDeviceAssignments(0, seqCh(1, 100)); got != nil {
		t.Errorf("0 devices: expected nil, got %+v", got)
	}
	if got := ComputeDeviceAssignments(2, nil); got != nil {
		t.Errorf("0 ch: expected nil, got %+v", got)
	}

	// 回帰: 巡回対象が 31ch 開始に偏っても全ペアが担当chを持つ（連番1..100ではなく実chを等分）
	got = ComputeDeviceAssignments(4, seqCh(31, 100)) // 70ch, 2ペア
	if len(got) != 2 {
		t.Fatalf("offset 31-100: groups expected 2, got %d", len(got))
	}
	if len(got[0].Channels) == 0 || len(got[1].Channels) == 0 {
		t.Errorf("offset 31-100: both pairs must own channels, got %d / %d", len(got[0].Channels), len(got[1].Channels))
	}
	if got[0].Channels[0] != 31 {
		t.Errorf("offset 31-100: group0 first ch expected 31, got %d", got[0].Channels[0])
	}
	if got[1].Channels[len(got[1].Channels)-1] != 100 {
		t.Errorf("offset 31-100: group1 last ch expected 100, got %d", got[1].Channels[len(got[1].Channels)-1])
	}

	// 回帰: 飛び飛びのch（GAS連携等）でも実ch順で等分される
	sparse := []uint32{31, 35, 40, 55, 70, 99}
	got = ComputeDeviceAssignments(4, sparse) // 6ch, 2ペア → 3ch ずつ
	if len(got) != 2 || len(got[0].Channels) != 3 || len(got[1].Channels) != 3 {
		t.Errorf("sparse: expected 2 groups of 3 ch, got %+v", got)
	}
	if got[0].Channels[0] != 31 || got[1].Channels[2] != 99 {
		t.Errorf("sparse: boundaries wrong: %+v", got)
	}
}

// =====================================================================
// Patroller MatchLineChange テスト
// CLAUDE.md §4-2 #3, #4, #9, §5 No.26/33 罠の防御
// =====================================================================

// newTestPatroller はテスト用の Patroller を作る。LoadStabilizationDuration を
// 極小にして遅延 goroutine が即終了するようにする（テスト中の goroutine リーク回避）。
func newTestPatroller() *Patroller {
	return NewPatroller(Config{
		LoadStabilizationDuration: time.Millisecond,
	})
}

// TestMatchLineChange_BindsOnHappyPath は probe → bind のハッピーパス。
// RecordPatrolMove → MatchLineChange の流れで serial↔UID が確立される。
func TestMatchLineChange_BindsOnHappyPath(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)

	// probe.sentAt の直後に変化シグナルが届く（実際の挙動）
	changedAt := now.Add(100 * time.Millisecond)
	p.MatchLineChange(/*uid*/ 999, /*lineID*/ 42, changedAt)

	if got := p.GetSerialUID("serial-A"); got != 999 {
		t.Errorf("after happy-path: GetSerialUID(serial-A) = %d, want 999", got)
	}
	if got := p.GetUIDSerial(999); got != "serial-A" {
		t.Errorf("reverse map: GetUIDSerial(999) = %q, want serial-A", got)
	}
	if !p.HasBinding() {
		t.Error("HasBinding expected true after bind")
	}
}

// TestMatchLineChange_RejectsExcludedUID は §4-2 #9 / No.33 核心。
// SetExcludeUIDs に登録された UID は probe マッチ候補から除外される。
func TestMatchLineChange_RejectsExcludedUID(t *testing.T) {
	p := newTestPatroller()
	excluded := uint64(12345) // 本物クライアントの UID を想定
	p.SetExcludeUIDs([]uint64{excluded})

	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)
	p.MatchLineChange(excluded, 42, now.Add(100*time.Millisecond))

	if got := p.GetSerialUID("serial-A"); got != 0 {
		t.Errorf("No.33 regression: excluded UID %d was bound to serial-A (got %d)", excluded, got)
	}
	if p.HasBinding() {
		t.Error("HasBinding expected false: excluded UID must not produce a binding")
	}
}

// TestMatchLineChange_RejectsEarlyChangedAt は §4-2 #3 / No.33 核心。
// changedAt が probe.sentAt - 2s より前なら probe マッチを拒否する。
// （ADB コマンド発行前に変化していた lineID は本物クライアントの動きと判断）
func TestMatchLineChange_RejectsEarlyChangedAt(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)

	// probe.sentAt より 3秒前（probeEpsilon=2s より大）
	earlyChangedAt := now.Add(-3 * time.Second)
	p.MatchLineChange(999, 42, earlyChangedAt)

	if got := p.GetSerialUID("serial-A"); got != 0 {
		t.Errorf("No.33 regression: changedAt before probe.sentAt-2s should not bind (got serial-A → %d)", got)
	}
}

// TestMatchLineChange_AcceptsChangedAtWithinEpsilon は §4-2 #3 の境界。
// probe.sentAt の 2s 以内であれば古い changedAt でも受け入れる。
func TestMatchLineChange_AcceptsChangedAtWithinEpsilon(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)

	// probe.sentAt より 1秒前（probeEpsilon=2s 以内）
	withinEpsilon := now.Add(-1 * time.Second)
	p.MatchLineChange(999, 42, withinEpsilon)

	if got := p.GetSerialUID("serial-A"); got != 999 {
		t.Errorf("within epsilon: expected bind, got %d", got)
	}
}

// TestMatchLineChange_RejectsMultipleCandidates は probe 候補が複数ある場合の
// 拒否動作を確認する（CLAUDE.md §4-2 #3 補足: 1 件マッチ即確定）。
func TestMatchLineChange_RejectsMultipleCandidates(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	// 2つの serial が同じ targetCh=42 で probe 登録
	p.RecordPatrolMove("serial-A", 42, now)
	p.RecordPatrolMove("serial-B", 42, now)

	p.MatchLineChange(999, 42, now.Add(100*time.Millisecond))

	// どちらにもバインドされない
	if got := p.GetSerialUID("serial-A"); got != 0 {
		t.Errorf("multiple candidates: serial-A bound to %d (expected 0)", got)
	}
	if got := p.GetSerialUID("serial-B"); got != 0 {
		t.Errorf("multiple candidates: serial-B bound to %d (expected 0)", got)
	}
}

// TestMatchLineChange_IgnoresZeroUidOrLineID はガード節の挙動を確認。
func TestMatchLineChange_IgnoresZeroUidOrLineID(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)
	p.MatchLineChange(0, 42, now)
	p.MatchLineChange(999, 0, now)

	if p.HasBinding() {
		t.Error("zero uid or lineID must not produce binding")
	}
}

// TestMatchLineChange_NoMatchOnDifferentCh は targetCh と異なる lineID は
// マッチしないことを確認する（probe フィルタの基本動作）。
func TestMatchLineChange_NoMatchOnDifferentCh(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)

	// 別の lineID (50) が来てもマッチしない
	p.MatchLineChange(999, 50, now.Add(100*time.Millisecond))

	if got := p.GetSerialUID("serial-A"); got != 0 {
		t.Errorf("different ch: serial-A should not bind, got %d", got)
	}
}

// TestSetExcludeUIDs_PreservesExistingBinding は既にバインド済みの serial が
// excludeUIDs に追加された UID の影響を受けないことを確認する。
// （既バインドは別経路で ActualCh 更新するため、excludeUIDs の影響範囲は probe 探索のみ）
func TestSetExcludeUIDs_PreservesExistingBinding(t *testing.T) {
	p := newTestPatroller()
	now := time.Now()
	p.RecordPatrolMove("serial-A", 42, now)
	p.MatchLineChange(999, 42, now.Add(100*time.Millisecond))

	if got := p.GetSerialUID("serial-A"); got != 999 {
		t.Fatalf("setup bind failed: got %d", got)
	}

	// 既バインド後に exclude に追加（実運用では稀だが、削除→再追加のシナリオ）
	p.SetExcludeUIDs([]uint64{999})

	// バインド自体は GetSerialUID で取得可能なまま
	if got := p.GetSerialUID("serial-A"); got != 999 {
		t.Errorf("exclude after bind: existing binding lost (got %d)", got)
	}
}

// TestLoadSerialUIDMap_PrePopulatesBindings は永続化された serial↔UID マップを
// 起動時にロードするケース（HasBinding が true になる）。
func TestLoadSerialUIDMap_PrePopulatesBindings(t *testing.T) {
	p := newTestPatroller()
	p.LoadSerialUIDMap(map[string]uint64{
		"emu-1": 111,
		"emu-2": 222,
	})
	if !p.HasBinding() {
		t.Error("HasBinding expected true after LoadSerialUIDMap")
	}
	if got := p.GetSerialUID("emu-1"); got != 111 {
		t.Errorf("GetSerialUID(emu-1) = %d, want 111", got)
	}
	if got := p.GetUIDSerial(222); got != "emu-2" {
		t.Errorf("GetUIDSerial(222) = %q, want emu-2", got)
	}
}

// TestDeleteSerialUIDBinding は serial に紐付いた UID バインドを削除する動作。
func TestDeleteSerialUIDBinding(t *testing.T) {
	p := newTestPatroller()
	p.LoadSerialUIDMap(map[string]uint64{"emu-1": 111})
	p.DeleteSerialUIDBinding("emu-1")

	if got := p.GetSerialUID("emu-1"); got != 0 {
		t.Errorf("after delete: GetSerialUID = %d, want 0", got)
	}
	if got := p.GetUIDSerial(111); got != "" {
		t.Errorf("after delete: GetUIDSerial = %q, want empty", got)
	}
}
