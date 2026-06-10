// Package main は fake-adb.exe の実装。
// PATROL_SIM_ADDR 環境変数が示す TCP サーバーへ接続し、
// ADB コマンド引数を JSON で送信して応答を受け取り、
// delay_ms だけ待機してから stdout に出力して終了する。
// PATROL_SIM_ADDR 未設定時は即 exit 1（本物環境への誤混入防止）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"time"
)

// request は fake-adb がサーバーへ送信する JSON メッセージ。
type request struct {
	Args []string `json:"args"`
	Ts   int64    `json:"ts"`
}

// response はサーバーから受け取る JSON メッセージ。
type response struct {
	Exit    int    `json:"exit"`
	Stdout  string `json:"stdout"`
	DelayMs int    `json:"delay_ms"`
	Screen  string `json:"screen"` // "black" | "normal" | ""
}

func main() {
	addr := os.Getenv("PATROL_SIM_ADDR")
	if addr == "" {
		fmt.Fprintln(os.Stderr, "fake-adb: PATROL_SIM_ADDR not set — refusing to run (safety guard)")
		os.Exit(1)
	}

	args := os.Args[1:]

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb: connect %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer conn.Close()

	req := request{Args: args, Ts: time.Now().UnixMilli()}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb: send: %v\n", err)
		os.Exit(1)
	}

	var resp response
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "fake-adb: recv: %v\n", err)
		os.Exit(1)
	}

	if resp.DelayMs > 0 {
		time.Sleep(time.Duration(resp.DelayMs) * time.Millisecond)
	}

	// screencap コマンドの場合は PNG バイナリを stdout に出力する。
	if isScreencapCmd(args) {
		var img *image.NRGBA
		if resp.Screen == "normal" {
			img = makeImage(255) // 白（非黒）
		} else {
			img = makeImage(0) // 黒
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			fmt.Fprintf(os.Stderr, "fake-adb: png encode: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(buf.Bytes())
	} else {
		fmt.Print(resp.Stdout)
	}

	os.Exit(resp.Exit)
}

// isScreencapCmd は引数に "screencap" が含まれているか判定する。
func isScreencapCmd(args []string) bool {
	for _, a := range args {
		if a == "screencap" {
			return true
		}
	}
	return false
}

// makeImage は 16x16 の単色 PNG 画像を生成する。
func makeImage(luma uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	c := color.NRGBA{R: luma, G: luma, B: luma, A: 255}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}
