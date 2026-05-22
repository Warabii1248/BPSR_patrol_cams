package ncap

import (
	"net"
	"testing"
	"time"
)

// TestIsPlayerUUID は UUID 判定の不変条件を担保する。
// CLAUDE.md §4-1 #2 関連: sess.userUID = rawUUID >> 16 の rawUUID 側を
// プレイヤーとして判定するため、下位 16bit が 640 のもののみ true。
func TestIsPlayerUUID(t *testing.T) {
	cases := []struct {
		name string
		uuid uint64
		want bool
	}{
		{"player_marker_640", 0x12340000 | 640, true},
		{"monster_marker_64", 0x12340000 | 64, false},
		{"zero", 0, false},
		{"large_player", uint64(1) << 40 | 640, true},
		{"large_monster", uint64(1) << 40 | 64, false},
		{"random_low16", 0xABCD1234, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPlayerUUID(c.uuid); got != c.want {
				t.Errorf("isPlayerUUID(%#x) = %v, want %v", c.uuid, got, c.want)
			}
		})
	}
}

// TestIsMonsterUUID は同じく下位 16bit が 64 の判定。
func TestIsMonsterUUID(t *testing.T) {
	cases := []struct {
		name string
		uuid uint64
		want bool
	}{
		{"monster_marker_64", 0x12340000 | 64, true},
		{"player_marker_640", 0x12340000 | 640, false},
		{"zero", 0, false},
		{"low_byte_64_high_bit", 0xFFFFFFFFFFFF0040, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMonsterUUID(c.uuid); got != c.want {
				t.Errorf("isMonsterUUID(%#x) = %v, want %v", c.uuid, got, c.want)
			}
		})
	}
}

// TestIsPrivateIP は private IP 帯の網羅。
func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.15.0.1", false}, // 172.16 未満
		{"172.32.0.1", false}, // 172.31 超過
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"192.169.0.1", false},
		{"8.8.8.8", false},
		{"127.0.0.1", false}, // loopback は対象外
		{"0.0.0.0", false},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("ParseIP failed: %s", c.ip)
			}
			if got := isPrivateIP(ip); got != c.want {
				t.Errorf("isPrivateIP(%s) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

// TestIsLoyalBoarletName はレアモンスター名判定の網羅。
// 対応キーワードを追加・削除した場合にここで気付ける。
func TestIsLoyalBoarletName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"金ウリボ", true},
		{"ゴールドウリボ", true},
		{"金ウリ", true},
		{"金豚", true},
		{"小猪·闪闪", true},
		{"金猪", true},
		{"金ナッポ", true},
		{"銀ナッポ", true},
		{"娜宝·闪闪", true},
		{"娜宝·银辉", true},
		{"普通のウリボ", false},
		{"イノシシ", false},
		{"", false},
		{"金 ウリボ", true}, // 部分一致 "金ウリ" は含まない（空白あり）→ "金ウリ" 含まない
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isLoyalBoarletName(c.name)
			// "金 ウリボ" は厳密にどれかのキーワードを含むかで判定
			// 「金ウリ」「金ウリボ」「金豚」「金猪」「金ナッポ」は半角空白挟むと一致しない
			if c.name == "金 ウリボ" {
				if got {
					t.Errorf("isLoyalBoarletName(%q): want false (空白挟みは部分一致しない想定)", c.name)
				}
				return
			}
			if got != c.want {
				t.Errorf("isLoyalBoarletName(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestToHalfWidth は全角数字→半角数字変換を確認。
func TestToHalfWidth(t *testing.T) {
	cases := []struct{ in, want string }{
		{"０", "0"},
		{"９", "9"},
		{"４５", "45"},
		{"休憩４５", "休憩45"},
		{"abc123", "abc123"},
		{"", ""},
		{"１２３休憩４５", "123休憩45"},
	}
	for _, c := range cases {
		if got := toHalfWidth(c.in); got != c.want {
			t.Errorf("toHalfWidth(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExtractChatChannel はチャットメッセージからのch番号抽出を確認。
// init() で構築される chatChLocationPatterns に依存。
func TestExtractChatChannel(t *testing.T) {
	cases := []struct {
		text string
		want uint32
	}{
		{"45 休憩", 45},
		{"休憩 45", 45},
		{"45休憩", 45},
		{"休憩45", 45},
		{"23ミンホル", 23},
		{"qk100", 100},
		{"単なる文字列", 0},
		{"", 0},
		{"99 偵察", 99},
		{"４５休憩", 45}, // 全角数字も半角化される
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := extractChatChannel(c.text); got != c.want {
				t.Errorf("extractChatChannel(%q) = %d, want %d", c.text, got, c.want)
			}
		})
	}
}

// newCapDeviceForTest は pcap.Handle を nil にした最小限の CapDevice を作る。
// nextInstanceNum / releaseInstanceLabel / assignLabel / SetReservedLabels を
// 単体テストするための初期化。
func newCapDeviceForTest() *CapDevice {
	return &CapDevice{
		sessions:      make(map[string]*session),
		activeConns:   make(map[string]string),
		debounceCache: make(map[string]time.Time),
		chatDedup:     make(map[string]time.Time),
	}
}

// TestAssignLabel_SequentialFromOne は初回採番が Instance-1 から始まることを確認。
func TestAssignLabel_SequentialFromOne(t *testing.T) {
	cd := newCapDeviceForTest()
	labels := []string{
		cd.assignLabel("192.168.0.10"),
		cd.assignLabel("192.168.0.11"),
		cd.assignLabel("192.168.0.12"),
	}
	want := []string{"Instance-1", "Instance-2", "Instance-3"}
	for i, l := range labels {
		if l != want[i] {
			t.Errorf("labels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

// TestReleaseInstanceLabel_ReusedByFreeList は解放したラベル番号が
// 次の採番で再利用されることを確認。
func TestReleaseInstanceLabel_ReusedByFreeList(t *testing.T) {
	cd := newCapDeviceForTest()
	cd.assignLabel("ip1") // Instance-1
	cd.assignLabel("ip2") // Instance-2
	cd.assignLabel("ip3") // Instance-3
	cd.releaseInstanceLabel("Instance-2")

	// 次の採番は解放済みの 2 を再利用するはず
	got := cd.assignLabel("ip4")
	if got != "Instance-2" {
		t.Errorf("after release: assignLabel = %q, want Instance-2 (free-list reuse)", got)
	}
}

// TestSetReservedLabels_HonorsReservation は予約 IP が指定番号で採番され、
// 未予約 IP は予約番号と衝突しないことを確認（No.21 の核心）。
func TestSetReservedLabels_HonorsReservation(t *testing.T) {
	cd := newCapDeviceForTest()
	cd.SetReservedLabels(map[string]int{
		"192.168.0.100": 5,
		"192.168.0.101": 7,
	})

	// 予約 IP は予約番号で採番
	if l := cd.assignLabel("192.168.0.100"); l != "Instance-5" {
		t.Errorf("reserved IP got %q, want Instance-5", l)
	}
	if l := cd.assignLabel("192.168.0.101"); l != "Instance-7" {
		t.Errorf("reserved IP got %q, want Instance-7", l)
	}

	// 未予約 IP は counter > 7 から（予約番号と衝突しない）
	l := cd.assignLabel("192.168.0.200")
	if l == "Instance-5" || l == "Instance-7" {
		t.Errorf("unreserved IP collided with reserved: %q", l)
	}
	if l != "Instance-8" {
		t.Errorf("unreserved IP after reserved (5,7) got %q, want Instance-8", l)
	}
}

// TestReleaseInstanceLabel_DoesNotReturnReservedToFreeList は
// 予約済み番号が free-list に戻されないことを確認（同 IP に再割当のため）。
func TestReleaseInstanceLabel_DoesNotReturnReservedToFreeList(t *testing.T) {
	cd := newCapDeviceForTest()
	cd.SetReservedLabels(map[string]int{
		"192.168.0.100": 3,
	})
	cd.assignLabel("192.168.0.100") // Instance-3
	cd.releaseInstanceLabel("Instance-3")

	// free-list には 3 が入っていないはず
	if len(cd.freeInstanceNums) > 0 {
		for _, n := range cd.freeInstanceNums {
			if n == 3 {
				t.Errorf("reserved Instance-3 was returned to free-list: %v", cd.freeInstanceNums)
			}
		}
	}
}
