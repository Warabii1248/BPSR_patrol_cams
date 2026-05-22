// [DEBUG] 操作記録・クラッシュレポートパッケージ
//
// 削除方法:
//  1. このファイル（debuglog/debuglog.go）を削除する
//  2. 他ファイルの "// [DEBUG]" コメントが付いた行と、その直後の debuglog 呼び出し行を削除する
//  3. 各ファイルの import ブロックから "debuglog" の行を削除する
package debuglog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ringSize     = 200
	debugLogFile = "logs/debug_log"
)

var (
	mu           sync.Mutex
	ring         [ringSize]string
	ringIdx      int
	ringN        int
	file         *os.File
	crashWebhook string

	reURL = regexp.MustCompile(`https?://\S+`)
	reIP  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Init はデバッグログを初期化する（main() 起動直後に呼ぶ）。
// logs/ ディレクトリがなければ自動作成する。
func Init() {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll("logs", 0755); err != nil {
		log.Printf("[debuglog] ディレクトリ作成失敗: %v", err)
		return
	}
	f, err := os.OpenFile(debugLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[debuglog] ファイルオープン失敗: %v", err)
		return
	}
	file = f
	fmt.Fprintf(file, "\n=== セッション開始 %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
}

// SetCrashWebhook はクラッシュ時の Discord 通知先 URL を設定する。
// 空文字を渡すと Discord 通知を無効にする。
func SetCrashWebhook(url string) {
	mu.Lock()
	crashWebhook = url
	mu.Unlock()
}

// Op は操作をリングバッファとファイルに記録する。
// URL と IP アドレスは [REDACTED] に自動置換される。
// category は "GUI", "ADB", "DETECT" など。
func Op(category, format string, args ...interface{}) {
	msg := redact(fmt.Sprintf(format, args...))
	line := fmt.Sprintf("%s [%-6s] %s", time.Now().Format("2006-01-02 15:04:05"), category, msg)

	mu.Lock()
	defer mu.Unlock()
	ring[ringIdx%ringSize] = line
	ringIdx++
	if ringN < ringSize {
		ringN++
	}
	if file != nil {
		fmt.Fprintln(file, line)
	}
}

// WriteCrashReport はクラッシュ情報をローカルファイルへ書き出す。
// crashWebhook が設定済みなら Discord にも概要を通知する。
// main() の defer recover から呼ぶことを想定している。
func WriteCrashReport(errVal interface{}, stack []byte) {
	ts := time.Now()
	filename := filepath.Join("logs", "crash_"+ts.Format("20060102_150405")+".txt")

	mu.Lock()
	ops := recentOpLines()
	webhook := crashWebhook
	mu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "=== CRASH REPORT %s ===\n\n", ts.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "エラー: %v\n\n", errVal)
	if len(ops) > 0 {
		fmt.Fprintf(&sb, "--- 直近の操作記録 (%d件) ---\n", len(ops))
		for _, op := range ops {
			fmt.Fprintln(&sb, op)
		}
	}
	fmt.Fprintf(&sb, "\n--- スタックトレース ---\n%s\n", redact(string(stack)))

	content := sb.String()
	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		log.Printf("[debuglog] クラッシュレポート書き込み失敗: %v", err)
	} else {
		log.Printf("[debuglog] クラッシュレポート保存: %s", filename)
	}

	if webhook != "" {
		sendDiscordCrash(webhook, ts, fmt.Sprintf("%v", errVal), ops, filename)
	}
}

// recentOpLines は直近最大50件の操作記録を返す（mu ロック済みで呼ぶこと）。
func recentOpLines() []string {
	n := ringN
	if n > 50 {
		n = 50
	}
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	start := ringIdx - n
	for i := 0; i < n; i++ {
		out[i] = ring[(start+i)%ringSize]
	}
	return out
}

// redact は URL と IP アドレスをマスクする。
func redact(s string) string {
	s = reURL.ReplaceAllString(s, "[URL REDACTED]")
	s = reIP.ReplaceAllString(s, "[IP REDACTED]")
	return s
}

// sendDiscordCrash はクラッシュ概要を Discord Webhook に送信する。
func sendDiscordCrash(webhookURL string, ts time.Time, errMsg string, ops []string, reportFile string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**クラッシュ検知** (%s)\n", ts.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "エラー: `%s`\n", truncate(errMsg, 200))
	if len(ops) > 0 {
		fmt.Fprintf(&sb, "\n直近の操作:\n```\n")
		start := 0
		if len(ops) > 10 {
			start = len(ops) - 10
		}
		for _, op := range ops[start:] {
			fmt.Fprintln(&sb, op)
		}
		fmt.Fprintf(&sb, "```\n")
	}
	fmt.Fprintf(&sb, "詳細: `%s`", reportFile)

	payload, _ := json.Marshal(map[string]string{"content": truncate(sb.String(), 1900)})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[debuglog] Discord通知失敗: %v", err)
		return
	}
	resp.Body.Close()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Verbose が true のとき Vlogf がログを出力する。
// main() 起動時に cfg.DebugVerbose の値で設定する。
var Verbose bool

// Vlogf は Verbose が true の時のみ log.Printf でデバッグログを出力する。
// tag は "[DBG][tag]" プレフィックスとして使われる（例: "Probe", "巡回", "0x2E"）。
func Vlogf(tag, format string, args ...interface{}) {
	if !Verbose {
		return
	}
	log.Printf("[DBG]["+tag+"] "+format, args...)
}

var (
	vlogDedupMu   sync.Mutex
	vlogLastEmit  = map[string]time.Time{}
	vlogLastValue = map[string]string{}
)

// VlogfDedup は同一 key で同一メッセージの連続出力を抑制する。
// 値が変わった場合、または minInterval 経過後は出力する。
func VlogfDedup(tag, key string, minInterval time.Duration, format string, args ...interface{}) {
	if !Verbose {
		return
	}
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	vlogDedupMu.Lock()
	last := vlogLastEmit[key]
	lastVal := vlogLastValue[key]
	if lastVal == msg && now.Sub(last) < minInterval {
		vlogDedupMu.Unlock()
		return
	}
	vlogLastEmit[key] = now
	vlogLastValue[key] = msg
	vlogDedupMu.Unlock()
	log.Printf("[DBG]["+tag+"] "+format, args...)
}
