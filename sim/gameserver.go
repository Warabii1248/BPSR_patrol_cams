package sim

import (
	"log"
	"time"
)

// SignalInjector はシグナル注入の関数型。
type SignalInjector struct {
	NotifyLineIDChange   func(uid uint64, lineID uint32, t time.Time)
	NotifyPostLoadReady  func(uid uint64, lineID uint32, t time.Time)
}

// GameServer はゲームサーバーシミュレーター。
// KEYCODE_ENTER を受信したら遅延後に LineIDChange → PostLoadReady を注入する。
type GameServer struct {
	scenario  *Scenario
	simServer *SimServer
	injector  SignalInjector

	// serial → UID のマッピング（シナリオから構築）
	serialToUID map[string]uint64
}

// NewGameServer は GameServer を作成し SimServer の onEnter コールバックを登録する。
func NewGameServer(scenario *Scenario, simSrv *SimServer, inj SignalInjector) *GameServer {
	gs := &GameServer{
		scenario:    scenario,
		simServer:   simSrv,
		injector:    inj,
		serialToUID: make(map[string]uint64),
	}
	for _, dev := range scenario.Devices {
		gs.serialToUID[dev.Serial] = dev.UID
	}

	simSrv.SetOnEnter(gs.onEnter)
	return gs
}

// onEnter は KEYCODE_ENTER 受信時に呼ばれる。
// シナリオ遅延後に NotifyLineIDChange → NotifyPostLoadReady を注入する。
func (gs *GameServer) onEnter(serial string, ch uint32) {
	uid, ok := gs.serialToUID[serial]
	if !ok {
		log.Printf("[GameServer] unknown serial=%s", serial)
		return
	}

	log.Printf("[GameServer] ENTER serial=%s uid=%d ch=%d", serial, uid, ch)

	// screencap を黒に切り替え（ロード開始）
	gs.simServer.SetScreenState(serial, "black")

	// line_id_change_delay 後に NotifyLineIDChange を注入
	lineIDDelay := gs.simServer.RandDelay(gs.scenario.Server.LineIDChangeDelay)
	go func() {
		time.Sleep(lineIDDelay)
		now := time.Now()
		log.Printf("[GameServer] NotifyLineIDChange uid=%d ch=%d (after %.1fs)", uid, ch, lineIDDelay.Seconds())
		gs.injector.NotifyLineIDChange(uid, ch, now)

		// post_load_delay 後に NotifyPostLoadReady を注入
		postLoadDelay := gs.simServer.RandDelay(gs.scenario.Server.PostLoadDelay)
		time.Sleep(postLoadDelay)
		now2 := time.Now()
		log.Printf("[GameServer] NotifyPostLoadReady uid=%d ch=%d (after %.1fs)", uid, ch, postLoadDelay.Seconds())
		gs.injector.NotifyPostLoadReady(uid, ch, now2)

		// screencap を非黒に（ロード完了）
		gs.simServer.SetScreenState(serial, "normal")
	}()
}
