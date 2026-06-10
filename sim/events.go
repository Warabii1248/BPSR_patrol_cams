package sim

import (
	"log"
	"sync"
	"time"
)

// EventEngine はシナリオ events 配列を処理する。
// PatrolStatus.Phase を 100ms ポーリングし、at_phase 一致時に 1 回だけイベントを発火する。
// at_cycle が指定されている場合は N 巡目のその phase で発火する（省略時は 1 巡目）。
type EventEngine struct {
	scenario  *Scenario
	simServer *SimServer
	injector  SignalInjector

	mu      sync.Mutex
	// インデックス → 発火済みサイクル番号（-1=未発火、0以上=発火済みサイクル）
	fired   map[int]int
	// 各 at_phase の通算出現回数（phase 名 → カウント）
	phaseCounts map[string]int
	prevPhase   string

	// 一時 post_load_delay オーバーライド（serial → delay）
	postLoadDelayOverrides   map[string]time.Duration
	postLoadDelayOverrideMus sync.Mutex
	// silent サイクル残数（serial → 残サイクル数）
	silentCycles   map[string]int
	silentCyclesMu sync.Mutex
}

// NewEventEngine は EventEngine を作成する。
func NewEventEngine(scenario *Scenario, simSrv *SimServer, inj SignalInjector) *EventEngine {
	e := &EventEngine{
		scenario:               scenario,
		simServer:              simSrv,
		injector:               inj,
		fired:                  make(map[int]int),
		phaseCounts:            make(map[string]int),
		postLoadDelayOverrides: make(map[string]time.Duration),
		silentCycles:           make(map[string]int),
	}
	return e
}

// Update は現在の Phase を渡して phase 遷移を追跡し、必要なイベントを発火する。
// patrol-sim の 100ms ポーリングループから呼ぶ。
func (e *EventEngine) Update(phase string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if phase == e.prevPhase {
		return
	}
	// phase が変化した
	e.phaseCounts[phase]++
	e.prevPhase = phase

	// 各イベントをチェック
	for i, ev := range e.scenario.Events {
		if ev.AtPhase != phase {
			continue
		}
		targetCycle := ev.Cycles
		if targetCycle <= 0 {
			targetCycle = 1
		}
		// この phase に入った回数
		count := e.phaseCounts[phase]
		if count != targetCycle {
			continue
		}
		// 既に発火済みか確認
		if fired, ok := e.fired[i]; ok && fired == targetCycle {
			continue
		}
		e.fired[i] = targetCycle
		// goroutine で発火（ロックを保持しない）
		evCopy := ev
		go e.fire(evCopy)
	}
}

// fire は 1 つのイベントを発火する（goroutine で呼ばれる）。
func (e *EventEngine) fire(ev EventDef) {
	log.Printf("[EventEngine] fire type=%s at_phase=%s uid=%d serial=%s to_ch=%d",
		ev.Type, ev.AtPhase, ev.UID, ev.Serial, ev.ToCh)

	switch ev.Type {
	case "native_move":
		e.fireNativeMove(ev)
	case "silent_device":
		e.fireSilentDevice(ev)
	case "burst_0x2e":
		e.fireBurst0x2e(ev)
	case "delayed_signal":
		e.fireDelayedSignal(ev)
	default:
		log.Printf("[EventEngine] unknown event type: %s", ev.Type)
	}
}

// fireNativeMove は serialToUID に存在しない UID（ネイティブクライアント）が ch 移動するシナリオ。
// NotifyLineIDChange → 300〜800ms 後 NotifyPostLoadReady を注入する。
func (e *EventEngine) fireNativeMove(ev EventDef) {
	uid := ev.UID
	ch := ev.ToCh
	if uid == 0 || ch == 0 {
		log.Printf("[EventEngine] native_move: uid/ch missing")
		return
	}
	now := time.Now()
	log.Printf("[EventEngine] native_move: NotifyLineIDChange uid=%d ch=%d", uid, ch)
	e.injector.NotifyLineIDChange(uid, ch, now)

	// 300〜800ms 後に PostLoadReady
	e.simServer.mu.Lock()
	delayMs := 300 + e.simServer.rng.Intn(501) // 300〜800
	e.simServer.mu.Unlock()
	delay := time.Duration(delayMs) * time.Millisecond

	go func() {
		time.Sleep(delay)
		now2 := time.Now()
		log.Printf("[EventEngine] native_move: NotifyPostLoadReady uid=%d ch=%d (after %.1fs)", uid, ch, delay.Seconds())
		e.injector.NotifyPostLoadReady(uid, ch, now2)
	}()
}

// fireSilentDevice は指定 serial の ENTER 後シグナル注入を N サイクル分抑止する。
// 無応答サイクル数は SilentCycles（at_cycle=発火タイミングとは独立）。
func (e *EventEngine) fireSilentDevice(ev EventDef) {
	cycles := ev.SilentCycles
	if cycles <= 0 {
		cycles = 1
	}
	e.silentCyclesMu.Lock()
	e.silentCycles[ev.Serial] = cycles
	e.silentCyclesMu.Unlock()
	e.simServer.SetSilent(ev.Serial, true)
	log.Printf("[EventEngine] silent_device: serial=%s cycles=%d", ev.Serial, cycles)
}

// ConsumeSilentCycle は silent サイクルを 1 消費する。
// 呼び出し元（GameServer.onEnter 相当）が ENTER 処理完了後に呼ぶ。
// 戻り値: true = サイレントモードを解除した（ログ記録のみ）
func (e *EventEngine) ConsumeSilentCycle(serial string) {
	e.silentCyclesMu.Lock()
	defer e.silentCyclesMu.Unlock()
	n := e.silentCycles[serial]
	if n <= 0 {
		return
	}
	n--
	e.silentCycles[serial] = n
	if n <= 0 {
		delete(e.silentCycles, serial)
		e.simServer.SetSilent(serial, false)
		log.Printf("[EventEngine] silent_device: serial=%s サイレント解除", serial)
	}
}

// fireBurst0x2e は同一 uid・同一 lineID で NotifyPostLoadReady を N 連射する。
func (e *EventEngine) fireBurst0x2e(ev EventDef) {
	uid := ev.UID
	lineID := ev.LineID
	count := ev.Count
	intervalMs := ev.IntervalMs
	if count <= 0 {
		count = 10
	}
	if intervalMs <= 0 {
		intervalMs = 50
	}
	if uid == 0 || lineID == 0 {
		log.Printf("[EventEngine] burst_0x2e: uid/line_id missing")
		return
	}
	log.Printf("[EventEngine] burst_0x2e: uid=%d lineID=%d count=%d interval=%dms", uid, lineID, count, intervalMs)
	go func() {
		for i := 0; i < count; i++ {
			e.injector.NotifyPostLoadReady(uid, lineID, time.Now())
			if i < count-1 {
				time.Sleep(time.Duration(intervalMs) * time.Millisecond)
			}
		}
	}()
}

// fireDelayedSignal は指定 serial の post_load_delay を一時的に上書きする。
func (e *EventEngine) fireDelayedSignal(ev EventDef) {
	if ev.Serial == "" || ev.PostLoadDelayMs <= 0 {
		log.Printf("[EventEngine] delayed_signal: serial/post_load_delay_ms missing")
		return
	}
	d := time.Duration(ev.PostLoadDelayMs) * time.Millisecond
	e.postLoadDelayOverrideMus.Lock()
	e.postLoadDelayOverrides[ev.Serial] = d
	e.postLoadDelayOverrideMus.Unlock()
	log.Printf("[EventEngine] delayed_signal: serial=%s post_load_delay=%v", ev.Serial, d)
}

// GetPostLoadDelayOverride は serial の post_load_delay オーバーライドを返す。
// オーバーライドが設定されていれば (delay, true)、なければ (0, false)。
// 呼び出し後にオーバーライドは 1 回限りでクリアされる（Cycles=1 相当）。
func (e *EventEngine) GetPostLoadDelayOverride(serial string) (time.Duration, bool) {
	e.postLoadDelayOverrideMus.Lock()
	defer e.postLoadDelayOverrideMus.Unlock()
	d, ok := e.postLoadDelayOverrides[serial]
	if ok {
		delete(e.postLoadDelayOverrides, serial)
	}
	return d, ok
}
