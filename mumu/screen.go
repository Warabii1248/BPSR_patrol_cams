package mumu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/debuglog"
)

// runAdbBinary は adb のバイナリ stdout を取得する。
// runAdb と違い CombinedOutput を使わない（stderr が PNG に混入すると壊れるため）。
// GlobalDelay も適用しない（ポーリング用途で短周期に呼ばれるため）。
func runAdbBinary(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	cmd := newCmd(ctx, cfg.ADBPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("adb %v: %w", args, err)
	}
	return out, nil
}

// CaptureScreenshotPNG は serial デバイスのスクリーンショットを PNG バイト列として返す。
// GUI の座標選択モーダル等から呼ばれる外部公開ラッパー。
func CaptureScreenshotPNG(ctx context.Context, serial string, cfg Config) ([]byte, error) {
	return runAdbBinary(ctx, cfg, "-s", serial, "exec-out", "screencap", "-p")
}

// captureScreenPNG は adb -s <serial> exec-out screencap -p で PNG を取得しデコードする。
// shell screencap は CRLF 変換で破損するため exec-out 必須。
func captureScreenPNG(ctx context.Context, serial string, cfg Config) (image.Image, error) {
	raw, err := runAdbBinary(ctx, cfg, "-s", serial, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(raw))
}

// isRegionBlack は指定矩形領域の暗ピクセル割合が pixelRatio 以上かどうかを返す。
// (rx, ry) は領域の左上 px、rw / rh は幅・高さ。画像範囲外は自動クリップ。
// パフォーマンス最適化のため *image.NRGBA / *image.RGBA を型アサーションして直接 Pix にアクセスする。
// 機種差で他の型が返った場合は image.At フォールバックを使う。
// 全画素ではなく 4 画素おきにサブサンプリングして判定時間を 1/16 にする。
func isRegionBlack(img image.Image, rx, ry, rw, rh int, luma uint8, pixelRatio float64) bool {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || rw <= 0 || rh <= 0 {
		return false
	}
	// 画像範囲（b.Min を基点とする座標系）にクリップ
	x0 := b.Min.X + rx
	y0 := b.Min.Y + ry
	x1 := x0 + rw
	y1 := y0 + rh
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if x1 > b.Max.X {
		x1 = b.Max.X
	}
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}
	if x1-x0 < 2 || y1-y0 < 2 {
		return false
	}

	const step = 4
	var total, blackCnt int
	lumaThresh := uint32(luma)

	switch m := img.(type) {
	case *image.NRGBA:
		stride := m.Stride
		pix := m.Pix
		for y := y0; y < y1; y += step {
			base := (y-b.Min.Y)*stride + (x0-b.Min.X)*4
			for x := x0; x < x1; x += step {
				i := base + (x-x0)*4
				if i+2 >= len(pix) {
					break
				}
				r := uint32(pix[i])
				g := uint32(pix[i+1])
				bl := uint32(pix[i+2])
				lumaVal := (r*77 + g*150 + bl*29) >> 8
				if lumaVal <= lumaThresh {
					blackCnt++
				}
				total++
			}
		}
	case *image.RGBA:
		stride := m.Stride
		pix := m.Pix
		for y := y0; y < y1; y += step {
			base := (y-b.Min.Y)*stride + (x0-b.Min.X)*4
			for x := x0; x < x1; x += step {
				i := base + (x-x0)*4
				if i+2 >= len(pix) {
					break
				}
				r := uint32(pix[i])
				g := uint32(pix[i+1])
				bl := uint32(pix[i+2])
				lumaVal := (r*77 + g*150 + bl*29) >> 8
				if lumaVal <= lumaThresh {
					blackCnt++
				}
				total++
			}
		}
	default:
		for y := y0; y < y1; y += step {
			for x := x0; x < x1; x += step {
				r, g, bl, _ := img.At(x, y).RGBA()
				// RGBA() は 16bit 値（0-65535）。8bit に圧縮して比較。
				r8 := r >> 8
				g8 := g >> 8
				b8 := bl >> 8
				lumaVal := (r8*77 + g8*150 + b8*29) >> 8
				if lumaVal <= lumaThresh {
					blackCnt++
				}
				total++
			}
		}
	}
	if total == 0 {
		return false
	}
	return float64(blackCnt)/float64(total) >= pixelRatio
}

// captureIsBlack は 1 回のスクショで isRegionBlack を呼ぶ。
// エラー時の戻り値:
//   - context.Canceled:   false (他戦略勝利による正常キャンセル、ループ継続無意味)
//   - DeadlineExceeded:   true  (fail-closed: 黒継続扱い、ScreenDetectTimeout で抜ける)
//   - その他のエラー:     true  (fail-closed: 真の障害でも誤判定を避ける)
//
// 8 台並列 ADB screencap は詰まりやすいため shotTimeout は十分長めに取る。
func (p *Patroller) captureIsBlack(ctx context.Context, serial string, cfg Config) bool {
	// スクショ単体タイムアウト: PollInterval * 15、最低 8 秒（並列 ADB 負荷対応）
	shotTimeout := cfg.ScreenPollInterval * 15
	if shotTimeout < 8*time.Second {
		shotTimeout = 8 * time.Second
	}
	shotCtx, cancel := context.WithTimeout(ctx, shotTimeout)
	defer cancel()
	img, err := captureScreenPNG(shotCtx, serial, cfg)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			// 他戦略（either の time 先勝）または Stop による正常キャンセル。
			// false を返すとループ即抜け（呼出元の ctx.Err() チェックで notifyMoveSignal 抑制される）。
			debuglog.Vlogf("0x2E", "[Screen] スクショ中断 serial=%s (他戦略勝利)", serial)
			return false
		case errors.Is(err, context.DeadlineExceeded):
			// shotTimeout 経過。fail-closed: 黒継続扱いでポーリングを続け、
			// 最終的に ScreenDetectTimeout (cfg.ScreenDetectTimeout) で runScreenStrategy が
			// フォールバック発火する。
			debuglog.Vlogf("0x2E", "[Screen] スクショタイムアウト serial=%s → 黒継続扱い", serial)
			return true
		default:
			// 真のエラー（adb 通信障害、PNG decode 失敗等）。fail-closed で誤判定を避ける。
			debuglog.Vlogf("0x2E", "[Screen] スクショ失敗 serial=%s err=%v → 黒継続扱い", serial, err)
			return true
		}
	}
	return isRegionBlack(img,
		cfg.ScreenRegionX, cfg.ScreenRegionY, cfg.ScreenRegionW, cfg.ScreenRegionH,
		cfg.ScreenBlackLuma, cfg.ScreenBlackPixelRatio)
}

// waitForBlackGone は黒画面が消えるまで（または timeout まで）ポーリングする。
// 戻り値: true=黒消失検知、false=タイムアウトまたは ctx.Done。
// 初回チェックで既に「非黒」なら即 true。
func (p *Patroller) waitForBlackGone(ctx context.Context, serial string, cfg Config) bool {
	if !p.captureIsBlack(ctx, serial, cfg) {
		debuglog.Vlogf("0x2E", "[Screen] serial=%s 初回非黒 → 即完了", serial)
		return true
	}
	deadline := time.NewTimer(cfg.ScreenDetectTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(cfg.ScreenPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
			if !p.captureIsBlack(ctx, serial, cfg) {
				return true
			}
		}
	}
}

// runTimeStrategy は従来動作（time.Sleep 後に notifyMoveSignal）。ctx.Done で即離脱。
func (p *Patroller) runTimeStrategy(ctx context.Context, serial string, lineID uint32, fireAt time.Time) {
	d := time.Until(fireAt)
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	} else {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	p.notifyMoveSignal(serial, lineID, fireAt)
}

// runScreenStrategy は画面判定のみで完了を待つ。タイムアウト時は強制発火（永久待ち防止）。
func (p *Patroller) runScreenStrategy(ctx context.Context, serial string, lineID uint32, startedAt time.Time, cfg Config) {
	ok := p.waitForBlackGone(ctx, serial, cfg)
	if ctx.Err() != nil {
		return
	}
	if !ok {
		log.Printf("[MuMu] 画面判定タイムアウト serial=%s → 強制進行", serial)
	} else {
		debuglog.Vlogf("0x2E", "[PostLoad] mode=screen serial=%s 黒消失検知 経過%.1fs",
			serial, time.Since(startedAt).Seconds())
	}
	p.notifyMoveSignal(serial, lineID, time.Now())
}

// runEitherStrategy は時間経過と画面判定を並走し、先勝ちで notifyMoveSignal を発火する。
func (p *Patroller) runEitherStrategy(ctx context.Context, serial string, lineID uint32, fireAt time.Time, cfg Config) {
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	winner := make(chan string, 2)

	go func() {
		d := time.Until(fireAt)
		if d <= 0 {
			select {
			case winner <- "time":
			default:
			}
			return
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-subCtx.Done():
			return
		case <-timer.C:
			select {
			case winner <- "time":
			default:
			}
		}
	}()

	go func() {
		ok := p.waitForBlackGone(subCtx, serial, cfg)
		if subCtx.Err() != nil {
			return
		}
		if ok {
			select {
			case winner <- "screen":
			default:
			}
		}
		// 画面判定タイムアウト時は winner に送らない。時間側が後勝ちで処理する。
	}()

	select {
	case <-ctx.Done():
		return
	case w := <-winner:
		debuglog.Vlogf("0x2E", "[PostLoad] mode=either serial=%s %s先勝", serial, w)
	}
	subCancel()
	p.notifyMoveSignal(serial, lineID, time.Now())
}
