package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/ncap"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
)

// event は記録する単一イベント。時刻系フィールドは含めない（golden 決定性）。
type event struct {
	Seq        int    `json:"seq"`
	Ev         string `json:"ev"`
	UID        uint64 `json:"uid,omitempty"`
	LineID     uint32 `json:"line_id,omitempty"`
	Ch         uint32 `json:"ch,omitempty"`
	HasCh      bool   `json:"has_ch,omitempty"`
	SenderHash string `json:"sender_hash,omitempty"`
	MsgHash    string `json:"msg_hash,omitempty"`
	MonsterID  uint64 `json:"monster_id,omitempty"`
	Source     string `json:"source,omitempty"`
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

func main() {
	pcapPath := flag.String("pcap", "", "入力 .pcap ファイル（必須）")
	recordGolden := flag.String("record-golden", "", "イベント列を JSONL で書き出す")
	goldenPath := flag.String("golden", "", "JSONL と比較し差分表示")
	portmapPath := flag.String("portmap", "", "ポートマップファイル（省略時は無効）")
	verbose := flag.Bool("v", false, "全イベントを標準出力へ")
	strictDetect := flag.Bool("strict-detect", false, "golden 比較に detect イベントも含める")
	flag.Parse()

	if *pcapPath == "" {
		fmt.Fprintln(os.Stderr, "error: -pcap が必要です")
		os.Exit(1)
	}
	if *recordGolden != "" && *goldenPath != "" {
		fmt.Fprintln(os.Stderr, "error: -record-golden と -golden は同時指定できません")
		os.Exit(1)
	}

	// CapDevice 初期化（device=nil で OK）
	cd := ncap.NewCapDevice(nil, "replay")

	// portmap: 一時ディレクトリへコピーして SetPortMapFile（本番 config/ 汚染防止）
	var tmpDir string
	if *portmapPath != "" {
		var err error
		tmpDir, err = os.MkdirTemp("", "pcap-replay-portmap-*")
		if err != nil {
			log.Fatalf("一時ディレクトリ作成失敗: %v", err)
		}
		defer os.RemoveAll(tmpDir)
		dst := filepath.Join(tmpDir, filepath.Base(*portmapPath))
		if err := copyFile(*portmapPath, dst); err != nil {
			log.Fatalf("portmap コピー失敗: %v", err)
		}
		cd.SetPortMapFile(dst)
	}

	// イベント収集
	var mu sync.Mutex
	var events []event
	seq := 0

	addEvent := func(ev event) {
		mu.Lock()
		seq++
		ev.Seq = seq
		events = append(events, ev)
		mu.Unlock()
		if *verbose {
			b, _ := json.Marshal(ev)
			fmt.Println(string(b))
		}
	}

	cd.SetLineIDObserver(func(uid uint64, lineID uint32, _ time.Time) {
		addEvent(event{Ev: "lineid", UID: uid, LineID: lineID})
	})

	cd.SetPostLoadReadyCallback(func(uid uint64, lineID uint32, _ time.Time) {
		addEvent(event{Ev: "postload", UID: uid, LineID: lineID})
	})

	cd.SetChatNotifier(func(_, _ string, uid uint64, sender, message string, ch uint32, hasCh bool) {
		addEvent(event{
			Ev:         "chat",
			UID:        uid,
			Ch:         ch,
			HasCh:      hasCh,
			SenderHash: hashStr(sender),
			MsgHash:    hashStr(message),
		})
	})

	cd.SetNotifier(func(det notifier.Detection) {
		// Discord 送信は一切しない。記録のみ。
		addEvent(event{
			Ev:        "detect",
			LineID:    det.LineID,
			MonsterID: det.TemplateID,
			Source:    det.Source,
		})
	})

	// replay 実行
	packets, err := cd.ReplayFile(*pcapPath)
	if err != nil {
		log.Fatalf("ReplayFile 失敗: %v", err)
	}

	// コールバックは go ディスパッチ（cap_device.go）のため、ReplayFile の return 時点では
	// 未完了のイベントが残り得る。イベント数が 500ms 間変化しなくなるまで待つ（最大 10 秒）。
	drainEvents(&mu, &events)

	// セッション情報集計
	sessions := cd.Sessions()
	uidConfirmed := 0
	lineIDConfirmed := 0
	for _, s := range sessions {
		if s.UserUID != 0 {
			uidConfirmed++
		}
		if s.LineID != 0 {
			lineIDConfirmed++
		}
	}

	// イベント件数内訳
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Ev]++
	}

	// サマリ
	fmt.Printf("\n=== サマリ ===\n")
	fmt.Printf("  パケット数     : %d\n", packets)
	fmt.Printf("  セッション数   : %d\n", len(sessions))
	fmt.Printf("  UID 確定数     : %d\n", uidConfirmed)
	fmt.Printf("  lineID 確定数  : %d\n", lineIDConfirmed)
	fmt.Printf("  イベント内訳:\n")
	for _, k := range []string{"lineid", "postload", "chat", "detect"} {
		fmt.Printf("    %-10s : %d\n", k, counts[k])
	}

	// 警告判定
	hasWarning := false
	if counts["lineid"] == 0 {
		fmt.Fprintln(os.Stderr, "★ パース層破壊の可能性: lineid イベントが 0 件")
		hasWarning = true
	}
	if uidConfirmed == 0 {
		fmt.Fprintln(os.Stderr, "★ パース層破壊の可能性: UID 確定セッションが 0 件")
		hasWarning = true
	}

	// golden 書き出し（順序非依存の正規形で保存）
	if *recordGolden != "" {
		if err := writeGolden(*recordGolden, canonicalize(events)); err != nil {
			log.Fatalf("golden 書き出し失敗: %v", err)
		}
		fmt.Printf("golden 書き出し完了: %s\n", *recordGolden)
	}

	// golden 比較
	if *goldenPath != "" {
		ok := compareGolden(*goldenPath, events, *strictDetect)
		if !ok {
			os.Exit(1)
		}
	}

	if hasWarning {
		os.Exit(1)
	}
}

// drainEvents はイベント数が 500ms 間変化しなくなるまで待つ（最大 10 秒）。
// コールバックの go ディスパッチが全て完了するのを静止判定で待機する。
func drainEvents(mu *sync.Mutex, events *[]event) {
	deadline := time.Now().Add(10 * time.Second)
	stableSince := time.Now()
	mu.Lock()
	last := len(*events)
	mu.Unlock()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		cur := len(*events)
		mu.Unlock()
		if cur != last {
			last = cur
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= 500*time.Millisecond {
			return
		}
	}
}

// canonicalize はイベント列を順序非依存の正規形に変換する。
// コールバックの発火順序は goroutine スケジューリング依存で非決定的なため、
// golden の書き出し・比較は正規ソート済みマルチセットで行う
// （重複発火・欠落は件数差として検出できる）。seq は 1..N で振り直す。
func canonicalize(events []event) []event {
	out := make([]event, len(events))
	copy(out, events)
	sort.Slice(out, func(i, j int) bool { return eventKey(out[i]) < eventKey(out[j]) })
	for i := range out {
		out[i].Seq = i + 1
	}
	return out
}

// eventKey は seq を除いた比較用キーを返す。
func eventKey(ev event) string {
	ev.Seq = 0
	b, _ := json.Marshal(ev)
	return string(b)
}

// writeGolden はイベントスライスを JSONL ファイルへ書き出す。
func writeGolden(path string, events []event) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// compareGolden は golden ファイルと events を比較し、不一致を表示する。
// strictDetect=false の場合 detect イベントを両側から除外して比較。
// 不一致があれば false を返す。
func compareGolden(path string, actual []event, strictDetect bool) bool {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "golden 読み込み失敗: %v\n", err)
		return false
	}
	defer f.Close()

	var expected []event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			fmt.Fprintf(os.Stderr, "golden パース失敗: %v\n", err)
			return false
		}
		expected = append(expected, ev)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "golden 読み込みエラー: %v\n", err)
		return false
	}

	// detect 除外フィルタ
	filter := func(evs []event) []event {
		if strictDetect {
			return evs
		}
		out := make([]event, 0, len(evs))
		for _, ev := range evs {
			if ev.Ev != "detect" {
				out = append(out, ev)
			}
		}
		return out
	}

	// 両側を正規ソート済みマルチセットに揃えて比較する
	// （発火順序は非決定的のため。旧形式＝未ソート golden もここで吸収される）
	expFiltered := canonicalize(filter(expected))
	actFiltered := canonicalize(filter(actual))
	toKey := eventKey

	maxDiff := 20
	diffCount := 0
	ok := true

	maxLen := len(expFiltered)
	if len(actFiltered) > maxLen {
		maxLen = len(actFiltered)
	}

	for i := 0; i < maxLen; i++ {
		if diffCount >= maxDiff {
			fmt.Fprintf(os.Stderr, "... （%d 件以上の差分、上位 %d 件を表示）\n", diffCount, maxDiff)
			break
		}
		var expStr, actStr string
		if i < len(expFiltered) {
			expStr = toKey(expFiltered[i])
		} else {
			expStr = "<なし>"
		}
		if i < len(actFiltered) {
			actStr = toKey(actFiltered[i])
		} else {
			actStr = "<なし>"
		}
		if expStr != actStr {
			fmt.Fprintf(os.Stderr, "seq %d: expected %s / actual %s\n", i+1, expStr, actStr)
			diffCount++
			ok = false
		}
	}
	if ok {
		fmt.Printf("golden 比較: OK（%d イベント一致）\n", len(expFiltered))
	} else {
		fmt.Fprintf(os.Stderr, "golden 比較: NG（%d 件の差分）\n", diffCount)
	}
	return ok
}

// copyFile はファイルをコピーする。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
