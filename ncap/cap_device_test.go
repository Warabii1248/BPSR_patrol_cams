package ncap

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/pb"
	"google.golang.org/protobuf/proto"
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

// ──────────────────────────────────────────────────────────────────
// No.72 Phase 1 テスト群
// ──────────────────────────────────────────────────────────────────

// newCapDeviceForVoteTest は portMap 付きの CapDevice をテスト用に生成する。
// コールバックスパイ付き。一時ファイルに空の portMap を作成する。
func newCapDeviceForVoteTest(t *testing.T) (*CapDevice, *callbackSpy) {
	t.Helper()
	cd := newCapDeviceForTest()

	// 空の portMap ファイルを作成
	tmpDir := t.TempDir()
	pmPath := filepath.Join(tmpDir, "port_ch_map.json")
	os.WriteFile(pmPath, []byte("{}"), 0644)
	cd.portMap = LoadPortMap(pmPath)

	spy := &callbackSpy{}
	cd.onLineIDObserved = spy.onLineIDObserved
	cd.onPostLoadReady = spy.onPostLoadReady
	return cd, spy
}

// callbackSpy は onLineIDObserved / onPostLoadReady の呼出を記録するスパイ。
type callbackSpy struct {
	mu              sync.Mutex
	lineIDCalls     []lineIDCall
	postLoadCalls   []postLoadCall
}

type lineIDCall struct {
	uid    uint64
	lineID uint32
	t      time.Time
}

type postLoadCall struct {
	uid    uint64
	lineID uint32
	t      time.Time
}

func (s *callbackSpy) onLineIDObserved(uid uint64, lineID uint32, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lineIDCalls = append(s.lineIDCalls, lineIDCall{uid, lineID, t})
}

func (s *callbackSpy) onPostLoadReady(uid uint64, lineID uint32, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postLoadCalls = append(s.postLoadCalls, postLoadCall{uid, lineID, t})
}

func (s *callbackSpy) lineIDCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lineIDCalls)
}

func (s *callbackSpy) postLoadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.postLoadCalls)
}

// marshalSyncContainerData は SyncContainerData ペイロードを構築する。
// charId > 0 で charId を設定。sd != nil で SceneData を設定。
func marshalSyncContainerData(t *testing.T, charId int64, sd *pb.SceneData) []byte {
	t.Helper()
	cs := &pb.CharSerialize{}
	if charId > 0 {
		cs.CharId = charId
	}
	if sd != nil {
		cs.SceneData = sd
	}
	msg := &pb.SyncContainerData{VData: cs}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("proto.Marshal(SyncContainerData) failed: %v", err)
	}
	return data
}

// marshalSyncToMeDeltaInfo は SyncToMeDeltaInfo ペイロードを構築する。
// uuid > 0 で UUID を設定。
func marshalSyncToMeDeltaInfo(t *testing.T, uuid int64) []byte {
	t.Helper()
	delta := &pb.AoiSyncToMeDelta{}
	if uuid > 0 {
		delta.Uuid = &uuid
	}
	msg := &pb.SyncToMeDeltaInfo{
		DeltaInfo: delta,
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("proto.Marshal(SyncToMeDeltaInfo) failed: %v", err)
	}
	return data
}

// ──── T-vote: maybeSubmitPortVote + portMap quorum ────

// TestVote_TwoLabelsReachQuorum は2台から同一(ch, port)に投票すると
// portMap が更新され、tryPortMapLineID で lineID が解決されることを確認する。
func TestVote_TwoLabelsReachQuorum(t *testing.T) {
	cd, _ := newCapDeviceForVoteTest(t)
	now := time.Now()

	// 2台分のセッションを用意（lineID=0、同一 serverIP）
	sess1 := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess1.serverIP = "203.0.113.1:20045"
	sess1.serverIPSetAt = now

	sess2 := newSession("10.0.0.1:50002", "10.0.0.1", "Instance-2")
	sess2.serverIP = "203.0.113.1:20045"
	sess2.serverIPSetAt = now

	cd.currentChannel = 45

	// 各台から投票（maybeSubmitPortVote は go で非同期だが、submitPortVote を直接呼んで確定的にテスト）
	cd.submitPortVote(45, "203.0.113.1:20045", "Instance-1", now)
	cd.submitPortVote(45, "203.0.113.1:20045", "Instance-2", now)

	// portMap が更新されたか確認
	ch, ok := cd.portMap.LookupByPort("203.0.113.1:20045")
	if !ok || ch != 45 {
		t.Fatalf("portMap.LookupByPort: got ch=%d ok=%v, want ch=45 ok=true", ch, ok)
	}

	// tryPortMapLineID で lineID が解決されるか確認
	cd.tryPortMapLineID(sess1)
	if sess1.lineID != 45 {
		t.Errorf("sess1.lineID after tryPortMapLineID: got %d, want 45", sess1.lineID)
	}
	cd.tryPortMapLineID(sess2)
	if sess2.lineID != 45 {
		t.Errorf("sess2.lineID after tryPortMapLineID: got %d, want 45", sess2.lineID)
	}
}

// TestVote_MaybeSubmitPortVote_LineIDZero は maybeSubmitPortVote が lineID==0 のとき
// のみ投票することを確認する。
func TestVote_MaybeSubmitPortVote_LineIDZero(t *testing.T) {
	cd, _ := newCapDeviceForVoteTest(t)
	now := time.Now()

	cd.currentChannel = 45

	// lineID == 0 → 投票される
	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	// maybeSubmitPortVote は go で submitPortVote を呼ぶ。テストでは直接呼んで確認。
	// lineID == 0 であることを確認
	if sess.lineID != 0 {
		t.Fatalf("precondition: sess.lineID = %d, want 0", sess.lineID)
	}
	cd.maybeSubmitPortVote(sess, "203.0.113.1:20045", now)
	// goroutine が投票するまで少し待つ
	time.Sleep(50 * time.Millisecond)

	// 1票だけなので quorum 未達だが投票自体は記録されている
	cd.portVotesMu.Lock()
	votes := cd.portVotes["20045"]
	cd.portVotesMu.Unlock()
	if len(votes) != 1 {
		t.Errorf("votes count: got %d, want 1", len(votes))
	}

	// lineID != 0 → 投票されない
	sess2 := newSession("10.0.0.1:50002", "10.0.0.1", "Instance-2")
	sess2.lineID = 45 // 既に lineID 確定済み
	cd.maybeSubmitPortVote(sess2, "203.0.113.1:20045", now)
	time.Sleep(50 * time.Millisecond)

	cd.portVotesMu.Lock()
	votes2 := cd.portVotes["20045"]
	cd.portVotesMu.Unlock()
	// Instance-2 は投票していないので票数は変わらない（1のまま）
	if len(votes2) != 1 {
		t.Errorf("votes count after lineID!=0: got %d, want 1 (no new vote)", len(votes2))
	}
}

// ──── T-No23: sd==nil + uidNewlySet + lineID!=0 → onLineIDObserved fires ────

// TestNo23_SceneDataNil_UIDNewlySet_LineIDKnown は SceneData なし・charId 新規確定・
// lineID が portMap で補完済みのケースで onLineIDObserved が発火することを確認する。
// §4-1.4（sd==nil パスに早期 return を追加してはいけない）の不変条件ガード。
func TestNo23_SceneDataNil_UIDNewlySet_LineIDKnown(t *testing.T) {
	cd, spy := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	sess.lineID = 45 // portMap で事前補完済み想定

	// SceneData なし、charId=12345（新規）
	payload := marshalSyncContainerData(t, 12345, nil)
	cd.processSyncContainerData(sess, payload)

	// onLineIDObserved は goroutine で呼ばれるので少し待つ
	time.Sleep(100 * time.Millisecond)

	if spy.lineIDCount() != 1 {
		t.Fatalf("onLineIDObserved calls: got %d, want 1", spy.lineIDCount())
	}
	spy.mu.Lock()
	call := spy.lineIDCalls[0]
	spy.mu.Unlock()
	if call.uid != 12345 {
		t.Errorf("onLineIDObserved uid: got %d, want 12345", call.uid)
	}
	if call.lineID != 45 {
		t.Errorf("onLineIDObserved lineID: got %d, want 45", call.lineID)
	}
}

// ──── T-No34: sd==nil + uidNewlySet + lineID==0 → onLineIDObserved does NOT fire ────
//              sd==nil + uidNewlySet + lineID!=0 → fires (covered by T-No23 above)

func TestNo34_SceneDataNil_UIDNewlySet_LineIDZero_NoFire(t *testing.T) {
	cd, spy := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	// lineID == 0 (portMap 未解決)

	payload := marshalSyncContainerData(t, 12345, nil)
	cd.processSyncContainerData(sess, payload)

	time.Sleep(100 * time.Millisecond)

	if spy.lineIDCount() != 0 {
		t.Errorf("onLineIDObserved calls: got %d, want 0 (lineID=0 should not fire)", spy.lineIDCount())
	}
}

// ──── T-No33: sd!=nil + oldCh==lineID + uidNewlySet=false → no fire ────

func TestNo33_SceneDataPresent_SameLineID_NoNewUID_NoFire(t *testing.T) {
	cd, spy := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	sess.lineID = 45
	sess.userUID = 12345 // 既存 UID（新規ではない）

	// SceneData あり、lineID=45（既存と同じ）、charId は既存 UID と同じ
	sd := &pb.SceneData{}
	lineID := uint32(45)
	sd.LineId = &lineID
	payload := marshalSyncContainerData(t, 12345, sd) // charId=12345 は既存と同じ

	cd.processSyncContainerData(sess, payload)

	time.Sleep(100 * time.Millisecond)

	if spy.lineIDCount() != 0 {
		t.Errorf("onLineIDObserved calls: got %d, want 0 (same lineID, no new UID)", spy.lineIDCount())
	}
}

// ──── T-No33 variant: sd!=nil + lineID changes → fires ────

func TestNo33_SceneDataPresent_LineIDChanges_Fires(t *testing.T) {
	cd, spy := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	sess.lineID = 45
	sess.userUID = 12345

	// lineID 変化: 45→50
	sd := &pb.SceneData{}
	lineID := uint32(50)
	sd.LineId = &lineID
	payload := marshalSyncContainerData(t, 12345, sd)

	cd.processSyncContainerData(sess, payload)

	time.Sleep(100 * time.Millisecond)

	if spy.lineIDCount() != 1 {
		t.Fatalf("onLineIDObserved calls: got %d, want 1 (lineID changed)", spy.lineIDCount())
	}
	spy.mu.Lock()
	call := spy.lineIDCalls[0]
	spy.mu.Unlock()
	if call.lineID != 50 {
		t.Errorf("onLineIDObserved lineID: got %d, want 50", call.lineID)
	}
}

// ──── T: PostLoadReady fires correctly via 0x2E ────

func TestPostLoadReady_FiresOncePerLineID(t *testing.T) {
	cd, spy := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	sess.lineID = 45
	sess.userUID = 12345

	// rawUUID は uid<<16 | 640（プレイヤーマーカー）。UID = rawUUID >> 16 = 12345
	rawUUID := int64(12345<<16 | 640)
	payload := marshalSyncToMeDeltaInfo(t, rawUUID)

	cd.sessionsMu.Lock()
	cd.sessions["10.0.0.1:50001"] = sess
	cd.sessionsMu.Unlock()

	// 1回目: 発火する
	cd.processSyncToMeDeltaInfo(sess, payload)
	time.Sleep(100 * time.Millisecond)
	if spy.postLoadCount() != 1 {
		t.Fatalf("1st 0x2E: postLoadReady calls: got %d, want 1", spy.postLoadCount())
	}

	// 2回目: 同 lineID なので抑制（No.44 dedup）
	cd.processSyncToMeDeltaInfo(sess, payload)
	time.Sleep(100 * time.Millisecond)
	if spy.postLoadCount() != 1 {
		t.Errorf("2nd 0x2E same lineID: postLoadReady calls: got %d, want 1 (dedup)", spy.postLoadCount())
	}

	// lineID 変化後に再発火
	sess.lineID = 50
	cd.processSyncToMeDeltaInfo(sess, payload)
	time.Sleep(100 * time.Millisecond)
	if spy.postLoadCount() != 2 {
		t.Errorf("3rd 0x2E new lineID: postLoadReady calls: got %d, want 2", spy.postLoadCount())
	}
}

// ──── D-log: SceneData 有無ログの分岐条件テスト ────

func TestVersionDetection_SceneDataFlag(t *testing.T) {
	cd, _ := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"

	// 初期状態: sceneDataSeen == 0（未観測）
	if sess.sceneDataSeen != 0 {
		t.Fatalf("initial sceneDataSeen: got %d, want 0", sess.sceneDataSeen)
	}

	// SceneData なしのパケットを処理
	payload := marshalSyncContainerData(t, 12345, nil)
	cd.processSyncContainerData(sess, payload)
	if sess.sceneDataSeen != -1 {
		t.Errorf("after sd==nil: sceneDataSeen = %d, want -1", sess.sceneDataSeen)
	}

	// 同じ SceneData なしを再度 → フラグは変化しない（ログスパム防止）
	oldVal := sess.sceneDataSeen
	cd.processSyncContainerData(sess, payload)
	if sess.sceneDataSeen != oldVal {
		t.Errorf("after 2nd sd==nil: sceneDataSeen changed from %d to %d", oldVal, sess.sceneDataSeen)
	}

	// SceneData ありのパケットを処理 → フラグ変化
	sd := &pb.SceneData{}
	lineID := uint32(45)
	sd.LineId = &lineID
	payloadWithSD := marshalSyncContainerData(t, 12345, sd)
	cd.processSyncContainerData(sess, payloadWithSD)
	if sess.sceneDataSeen != 1 {
		t.Errorf("after sd!=nil: sceneDataSeen = %d, want 1", sess.sceneDataSeen)
	}
}

func TestVersionDetection_DeltaUUIDFlag(t *testing.T) {
	cd, _ := newCapDeviceForVoteTest(t)

	sess := newSession("10.0.0.1:50001", "10.0.0.1", "Instance-1")
	sess.serverIP = "203.0.113.1:20045"
	sess.lineID = 45
	sess.userUID = 12345

	cd.sessionsMu.Lock()
	cd.sessions["10.0.0.1:50001"] = sess
	cd.sessionsMu.Unlock()

	// 初期状態: deltaUUIDSeen == 0（未観測）
	if sess.deltaUUIDSeen != 0 {
		t.Fatalf("initial deltaUUIDSeen: got %d, want 0", sess.deltaUUIDSeen)
	}

	// UUID ありのパケットを処理
	rawUUID := int64(12345<<16 | 640)
	payload := marshalSyncToMeDeltaInfo(t, rawUUID)
	cd.processSyncToMeDeltaInfo(sess, payload)
	if sess.deltaUUIDSeen != 1 {
		t.Errorf("after uuid!=nil: deltaUUIDSeen = %d, want 1", sess.deltaUUIDSeen)
	}

	// UUID なしのパケットを構築して処理
	// DeltaInfo は存在するが Uuid フィールドが nil
	noUUIDPayload := marshalSyncToMeDeltaInfo(t, 0) // uuid=0 → Uuid フィールドは設定されない
	cd.processSyncToMeDeltaInfo(sess, noUUIDPayload)
	if sess.deltaUUIDSeen != -1 {
		t.Errorf("after uuid==nil: deltaUUIDSeen = %d, want -1", sess.deltaUUIDSeen)
	}
}

// ──── T-No09: 同 clientIP 複数台でも投票が正しく機能する ────

func TestNo09_SameClientIP_SeparateVotes(t *testing.T) {
	cd, _ := newCapDeviceForVoteTest(t)
	now := time.Now()

	// NAT 環境: 同一 clientIP の 2 台（serial/label は別）
	cd.currentChannel = 45

	cd.submitPortVote(45, "203.0.113.1:20045", "Instance-1", now)
	cd.submitPortVote(45, "203.0.113.1:20045", "Instance-2", now)

	ch, ok := cd.portMap.LookupByPort("203.0.113.1:20045")
	if !ok || ch != 45 {
		t.Fatalf("portMap.LookupByPort: got ch=%d ok=%v, want ch=45 ok=true", ch, ok)
	}
}
