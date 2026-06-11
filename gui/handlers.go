// Package gui の handlers.go は HTTP ハンドラ群を保持する。
// gui.go から機械的に分離したもので、ロジックは変更していない。
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/balrogsxt/StarResonanceAPI/debuglog"
	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
)

// handleIndex はメインHTMLページを返す
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, s.renderIndexHTML())
}

func (s *Server) renderIndexHTML() string {
	if s.patrolEnabled {
		return processedIndexHTML
	}
	return processedIndexHTML + patrolDisabledScript
}

// handleDeviceMap はADBデバイスのエミュレータIPを取得し、
// キャプチャセッション（UID等）と紐付けた一覧をJSONで返す。
func (s *Server) handleDeviceMap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	adbDevices, err := mumu.ListDevices(ctx, s.patroller.Config())
	if err != nil {
		log.Printf("[MuMu] device-map: ListDevices error: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	uidToSess := make(map[uint64]DeviceSessionInfo)
	ipToSess := make(map[string]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.UserUID != 0 {
				uidToSess[sess.UserUID] = sess
			}
			if sess.ClientIP != "" {
				ipToSess[sess.ClientIP] = sess
			}
		}
	}

	type DeviceEntry struct {
		Serial    string `json:"serial"`
		DeviceIP  string `json:"device_ip"`
		UserUID   uint64 `json:"user_uid"`
		Label     string `json:"label"`
		MapID     uint32 `json:"map_id"`
		LineID    uint32 `json:"line_id"`
		CurrentCh uint32 `json:"current_ch"`
		Confirmed bool   `json:"confirmed"`
	}

	cfg := s.patroller.Config()
	entries := make([]DeviceEntry, len(adbDevices))
	var wg sync.WaitGroup
	for i, serial := range adbDevices {
		entries[i] = DeviceEntry{Serial: serial}
		wg.Add(1)
		go func(idx int, ser string) {
			defer wg.Done()
			devIP := getDeviceIPCtx(ctx, ser, cfg)
			entries[idx].DeviceIP = devIP
			// u512Au5148u9806u4F8D1: serialu2192UID u30D0u30A4u30F3u30C9u3067u76F4u63A5u7167u5408uFF08NAT u74B0u5883u5BFEu5FDCuFF09
			if uid := s.patroller.GetSerialUID(ser); uid != 0 {
				if sess, ok := uidToSess[uid]; ok {
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].CurrentCh = sess.CurrentCh
					entries[idx].Confirmed = sess.Confirmed
				} else {
					entries[idx].UserUID = uid
				}
			} else if devIP != "" {
				// u512Au5148u9806u4F8D2: IP u7167u5408uFF08u975E NAT u74B0u5883u30D5u30A9u30FCu30EBu30D0u30C3u30AFuFF09
				if sess, ok := ipToSess[devIP]; ok {
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].CurrentCh = sess.CurrentCh
					entries[idx].Confirmed = sess.Confirmed
				}
			}
		}(i, serial)
	}
	wg.Wait()

	// DeviceStatusuFF08ADB serial u30D9u30FCu30B9uFF09u3067 CurrentCh u3092u4E0Au66F8u304Du3059u308Bu3002
	// ActualChuFF08u30D1u30B1u30C3u30C8u89B3u6E2Cu306Eu5B9FCHuFF09u3092u6700u512Au5148u3057u3001u6B21u3044u3067 CurrentChuFF08ADBu547Du4EE4u5024uFF09u3092u4F7Fu3046u3002
	// u507Du30D5u30A9u30FCu30EBu30D0u30C3u30AFuFF08u5DE1u56DEu5168u4F53CHu3092u672Au89E3u6C7Au30C7u30D0u30A4u30B9u306Bu4E0Au66F8u304DuFF09u306Fu610Fu56F3u7684u306Bu524Au9664u3002
	if s.patroller != nil {
		statuses := s.patroller.GetDeviceStatuses()
		for i := range entries {
			for _, ds := range statuses {
				if ds.Serial != entries[i].Serial {
					continue
				}
				if ds.ActualCh > 0 {
					entries[i].CurrentCh = ds.ActualCh
				} else if ds.CurrentCh > 0 {
					entries[i].CurrentCh = ds.CurrentCh
				}
				break
			}
		}
	}

	writeJSON(w, map[string]interface{}{"devices": entries})
}

// handleDevices はADB接続デバイス一覧をJSONで返す
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	cfg := s.patroller.Config()
	devices, err := mumu.ListDevices(r.Context(), cfg)
	if err != nil {
		log.Printf("[MuMu] adb devices エラー: %v", err)
	}
	if err == nil && len(devices) == 0 {
		// 接続試行してから再確認
		mumu.ConnectConfiguredSerials(r.Context(), cfg)
		devices, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil {
			log.Printf("[MuMu] adb devices 再試行エラー: %v", err)
		}
	}
	if devices == nil {
		devices = []string{}
	}
	writeJSON(w, map[string]interface{}{"devices": devices})
}

// handleSwitch はチャンネル切替リクエストを処理する
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial  string   `json:"serial"`
		Serials []string `json:"serials"`
		Channel uint32   `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	type result struct {
		Serial string `json:"serial"`
		Error  string `json:"error,omitempty"`
		OK     bool   `json:"ok"`
	}
	var results []result

	cfg := s.patroller.Config()

	// serials が指定されていれば複数切替、なければ単体切替
	serials := req.Serials
	if len(serials) == 0 && req.Serial == "" {
		// 何も指定なし → 全台を ListDevices で取得
		var err error
		serials, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	if len(serials) > 0 {
		switchResults := make(map[string]error, len(serials))
		var switchMu sync.Mutex
		limit := mumu.ParallelLimit(cfg, len(serials))
		for start := 0; start < len(serials); start += limit {
			end := start + limit
			if end > len(serials) {
				end = len(serials)
			}
			if start > 0 && cfg.ParallelGroupDelay > 0 {
				time.Sleep(cfg.ParallelGroupDelay)
			}
			mumu.SwitchGroup(r.Context(), serials, start, end, req.Channel, cfg, switchResults, &switchMu)
		}
		for serial, err := range switchResults {
			res := result{Serial: serial, OK: err == nil}
			if err != nil {
				res.Error = err.Error()
			}
			results = append(results, res)
		}
	} else {
		// 単体切替
		err := mumu.SwitchChannel(r.Context(), req.Serial, req.Channel, cfg)
		res := result{Serial: req.Serial, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		results = append(results, res)
	}

	// 成功した切替を DeviceStatus に即時反映し、次回 device-map 取得時に最新 Ch が返るようにする
	if s.patroller != nil {
		for _, res := range results {
			if res.OK {
				s.patroller.SetDeviceCh(res.Serial, req.Channel)
			}
		}
	}

	writeJSON(w, map[string]interface{}{"results": results})
}

// handleLogs は既存ログ一覧をJSONで返す
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{"logs": lines})
}

// handlePatrolChannels はconfig読み込み済みのチャンネルリストを返す（GET）または保存する（POST）
func (s *Server) handlePatrolChannels(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]interface{}{"channels": []uint32{}})
			return
		}
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Channels []uint32 `json:"channels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		filtered := s.filterCooldown(req.Channels)
		s.patrolChannels = filtered
		s.mu.Unlock()
		if s.saveChannelsFn != nil {
			if err := s.saveChannelsFn(filtered); err != nil {
				log.Printf("[GUI] channels保存失敗: %v", err)
				http.Error(w, "save failed: "+err.Error(), 500)
				return
			}
			excluded := len(req.Channels) - len(filtered)
			if excluded > 0 {
				log.Printf("[GUI] channels.txt に %d件保存しました (%d件クールダウン除外)", len(filtered), excluded)
			} else {
				log.Printf("[GUI] channels.txt に %d件保存しました", len(filtered))
			}
		}
		writeOK(w)
		return
	}
	s.mu.RLock()
	chs := make([]uint32, len(s.patrolChannels))
	copy(chs, s.patrolChannels)
	s.mu.RUnlock()
	writeJSON(w, map[string]interface{}{"channels": chs})
}

// handlePatrolChannelsGAS は Chrome拡張からエネミー名付きチャンネルを受信し、
// GASTargetEnemy 設定でフィルタして巡回リストを更新する。
func (s *Server) handlePatrolChannelsGAS(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	gasEnabled := s.gasEnable
	s.mu.RUnlock()
	if !gasEnabled {
		http.Error(w, "GAS sync disabled", http.StatusForbidden)
		return
	}
	if !s.patrolEnabled {
		writeJSON(w, map[string]interface{}{"ok": true, "ignored": true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Entries []struct {
			Channel uint32 `json:"channel"`
			Enemy   string `json:"enemy"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	s.mu.RLock()
	target := s.gasTargetEnemy
	s.mu.RUnlock()

	var chs []uint32
	for _, e := range req.Entries {
		if target == "" || strings.Contains(e.Enemy, target) {
			chs = append(chs, e.Channel)
		}
	}
	log.Printf("[GASSync] 受信: 全%d件 → 対象(%s): %d件", len(req.Entries), target, len(chs))
	s.UpdateChannelsFromGAS(chs)
	writeOK(w)
}

// handleGoldHistory は金ウリボ検知履歴を返す
func (s *Server) handleGoldHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := make([]GoldBoarEvent, len(s.goldBoarHistory))
	copy(h, s.goldBoarHistory)
	s.mu.RUnlock()
	// 新しい順に返す
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	writeJSON(w, h)
}

// handleDeleteGoldHistory は検知履歴の1件を削除する
func (s *Server) handleDeleteGoldHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", 405)
		return
	}
	var req struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.Timestamp == 0 {
		http.Error(w, "timestamp is required", 400)
		return
	}

	s.mu.Lock()
	idx := -1
	var evCh uint32
	for i, ev := range s.goldBoarHistory {
		if ev.Timestamp == req.Timestamp {
			idx = i
			evCh = ev.Channel
			break
		}
	}
	var saveChannelsFn func([]uint32) error
	var restoredChs []uint32
	if idx >= 0 {
		s.goldBoarHistory = append(s.goldBoarHistory[:idx], s.goldBoarHistory[idx+1:]...)
		if err := s.persistGoldHistoryLocked(); err != nil {
			s.mu.Unlock()
			log.Printf("[GUI] gold history save failed: %v", err)
			http.Error(w, "save failed", 500)
			return
		}
		// クールダウン解除（GAS経由の再追加を許可）
		delete(s.cooldownChs, evCh)
		// 巡回リストに復元（未登録の場合のみ追加）
		alreadyIn := false
		for _, pc := range s.patrolChannels {
			if pc == evCh {
				alreadyIn = true
				break
			}
		}
		if !alreadyIn && evCh > 0 {
			s.patrolChannels = append(s.patrolChannels, evCh)
			restoredChs = make([]uint32, len(s.patrolChannels))
			copy(restoredChs, s.patrolChannels)
		}
		saveChannelsFn = s.saveChannelsFn
	}
	s.mu.Unlock()

	if idx < 0 {
		http.Error(w, "not found", 404)
		return
	}
	if restoredChs != nil && saveChannelsFn != nil {
		if err := saveChannelsFn(restoredChs); err != nil {
			log.Printf("[GUI] channels.txt 保存失敗 (誤報取消): %v", err)
		} else {
			log.Printf("[GUI] 誤報取消: Ch%d を巡回リストに復元 (計%d ch)", evCh, len(restoredChs))
		}
	}
	writeOK(w)
}

// handleClearGoldHistory は検知履歴を全件削除する
func (s *Server) handleClearGoldHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", 405)
		return
	}
	s.mu.Lock()
	s.goldBoarHistory = nil
	if err := s.persistGoldHistoryLocked(); err != nil {
		s.mu.Unlock()
		log.Printf("[GUI] gold history save failed: %v", err)
		http.Error(w, "save failed", 500)
		return
	}
	s.mu.Unlock()
	writeOK(w)
}

// handleConfig は config.json の読み込み（GET）または保存（POST）を行う。
// 保存後は再起動で反映される。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.saveConfigFn == nil {
			http.Error(w, "save not configured", 503)
			return
		}
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.saveConfigFn(buf); err != nil {
			log.Printf("[GUI] config保存失敗: %v", err)
			http.Error(w, "save failed: "+err.Error(), 500)
			return
		}
		log.Printf("[GUI] config.json を保存しました")

		// 保存した config を読み直して Patroller に即時反映
		if s.getConfigFn != nil {
			if savedData, rerr := s.getConfigFn(); rerr == nil {
				var appCfg struct {
					ADBPath                string   `json:"adb_path"`
					MumuSerials            []string `json:"mumu_serials"`
					MumuTapX               int      `json:"mumu_tap_x"`
					MumuTapY               int      `json:"mumu_tap_y"`
					MumuClearLength        int      `json:"mumu_clear_length"`
					MumuPreKeycode         string   `json:"mumu_pre_keycode"`
					MumuDelayMs            int      `json:"mumu_delay_ms"`
					ParallelLimit          int      `json:"parallel_limit"`
					ParallelGroupDelaySecs float64  `json:"parallel_group_delay_secs"`
					PatrolMoveTimeoutSecs            float64 `json:"patrol_move_timeout_secs"`
					PatrolMergeTimeoutSecs           float64 `json:"patrol_merge_timeout_secs"`
					PatrolLoadStabilizationSecs      float64 `json:"patrol_load_stabilization_secs"`
					PatrolLoadStabilizationAuto      bool    `json:"patrol_load_stabilization_auto"`
					PatrolLoadDetectMode             string  `json:"patrol_load_detect_mode"`
					PatrolScreenPollMs               int     `json:"patrol_screen_poll_ms"`
					PatrolScreenRegionX              int     `json:"patrol_screen_region_x"`
					PatrolScreenRegionY              int     `json:"patrol_screen_region_y"`
					PatrolScreenRegionW              int     `json:"patrol_screen_region_w"`
					PatrolScreenRegionH              int     `json:"patrol_screen_region_h"`
					PatrolScreenBlackLuma            int     `json:"patrol_screen_black_luma"`
					PatrolScreenBlackPixelRatio      float64 `json:"patrol_screen_black_pixel_ratio"`
					PatrolScreenTimeoutSecs          float64 `json:"patrol_screen_timeout_secs"`
					PatrolDwellSecs             float64 `json:"patrol_dwell_secs"`
					PatrolAdaptiveTimeout       bool              `json:"patrol_adaptive_timeout"`
					PatrolAdaptiveTimeoutWindow int               `json:"patrol_adaptive_timeout_window"`
					SerialToLabel               map[string]string `json:"serial_to_label"`
					GamePackageName             string            `json:"game_package_name"`
					GameLaunchActivity          string            `json:"game_launch_activity"`
					CrashRecoveryEnabled        bool              `json:"crash_recovery_enabled"`
					CrashRecoveryDelaySecs      float64           `json:"crash_recovery_delay_secs"`
					DebugVerbose                bool              `json:"debug_verbose"`
					ExcludeUIDs                 []uint64          `json:"exclude_uids"`
				}
				if json.Unmarshal(savedData, &appCfg) == nil {
					newCfg := mumu.Config{
						ADBPath:            appCfg.ADBPath,
						TapX:               appCfg.MumuTapX,
						TapY:               appCfg.MumuTapY,
						ClearLength:        appCfg.MumuClearLength,
						PreKeycode:         appCfg.MumuPreKeycode,
						ConnectSerials:     appCfg.MumuSerials,
						GlobalDelay:        time.Duration(appCfg.MumuDelayMs) * time.Millisecond,
						ParallelLimit:      appCfg.ParallelLimit,
						ParallelGroupDelay: time.Duration(appCfg.ParallelGroupDelaySecs * float64(time.Second)),
						MoveTimeout:               time.Duration(appCfg.PatrolMoveTimeoutSecs * float64(time.Second)),
						MergeTimeout:              time.Duration(appCfg.PatrolMergeTimeoutSecs * float64(time.Second)),
						LoadStabilizationDuration: time.Duration(appCfg.PatrolLoadStabilizationSecs * float64(time.Second)),
						LoadStabilizationAuto:     appCfg.PatrolLoadStabilizationAuto,
						LoadDetectMode:            appCfg.PatrolLoadDetectMode,
						ScreenPollInterval:        time.Duration(appCfg.PatrolScreenPollMs) * time.Millisecond,
						ScreenRegionX:             appCfg.PatrolScreenRegionX,
						ScreenRegionY:             appCfg.PatrolScreenRegionY,
						ScreenRegionW:             appCfg.PatrolScreenRegionW,
						ScreenRegionH:             appCfg.PatrolScreenRegionH,
						ScreenBlackLuma:           uint8(appCfg.PatrolScreenBlackLuma),
						ScreenBlackPixelRatio:     appCfg.PatrolScreenBlackPixelRatio,
						ScreenDetectTimeout:       time.Duration(appCfg.PatrolScreenTimeoutSecs * float64(time.Second)),
						DwellDuration:         time.Duration(appCfg.PatrolDwellSecs * float64(time.Second)),
						AdaptiveTimeout:        appCfg.PatrolAdaptiveTimeout,
						AdaptiveTimeoutWindow:  appCfg.PatrolAdaptiveTimeoutWindow,
						GetLabelByIP:           s.getLabelByIPFn,
						SerialToLabel:          appCfg.SerialToLabel,
						GamePackageName:        appCfg.GamePackageName,
						GameLaunchActivity:     appCfg.GameLaunchActivity,
						CrashRecoveryEnabled:   appCfg.CrashRecoveryEnabled,
						CrashRecoveryDelaySecs: appCfg.CrashRecoveryDelaySecs,
					}
					s.UpdatePatrollerCfg(newCfg)
					s.SetExcludeUIDs(appCfg.ExcludeUIDs)
					debuglog.Verbose = appCfg.DebugVerbose
					// マージ待ち 0 は「初回待ちを流用」の意味（mumu.Config.MergeTimeout 参照）。
					// 生値 0s を出すと設定が壊れたと誤認されるため実効挙動を表記する
					mergeStr := fmt.Sprintf("%.0fs", appCfg.PatrolMergeTimeoutSecs)
					if appCfg.PatrolMergeTimeoutSecs == 0 {
						mergeStr = "初回待ちと同値"
					}
					log.Printf("[GUI] 巡回設定を即時反映: 滞在=%.0fs, 初回待ち=%.0fs, マージ待ち=%s, グループ間=%.0fs, 並列=%d, デバッグ=%v",
						appCfg.PatrolDwellSecs, appCfg.PatrolMoveTimeoutSecs, mergeStr,
						appCfg.ParallelGroupDelaySecs, appCfg.ParallelLimit, appCfg.DebugVerbose)
				}
			}
		}

		writeOK(w)
		return
	}
	if s.getConfigFn == nil {
		http.Error(w, "config not available", 503)
		return
	}
	data, err := s.getConfigFn()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handlePatrolStart は巡回を開始する
func (s *Server) handlePatrolStart(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serials      []string `json:"serials"`
		Channels     []uint32 `json:"channels"`
		Reversed     bool     `json:"reversed"`
		LoopMode     bool     `json:"loop_mode"`
		StartChannel uint32   `json:"start_channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		s.mu.RLock()
		channels = make([]uint32, len(s.patrolChannels))
		copy(channels, s.patrolChannels)
		s.mu.RUnlock()
	}
	if len(channels) == 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "チャンネルリストが空です"})
		return
	}
	opts := mumu.PatrolOptions{
		Reversed:     req.Reversed,
		LoopMode:     req.LoopMode,
		StartChannel: req.StartChannel,
	}
	s.patroller.Start(req.Serials, channels, s.patrolChannelsFile, opts)
	writeOK(w)
}

// handleTestDetect はテスト用ウリボ・ゴールド検知を発火する
func (s *Server) handleTestDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.testDetectFn == nil {
		http.Error(w, "test detect not configured", 503)
		return
	}
	monster := r.URL.Query().Get("monster")
	go s.testDetectFn(monster)
	writeJSON(w, map[string]interface{}{"ok": true, "monster": monster})
}

// handlePatrolClearMoveFailed は移動失敗リストをクリアする
func (s *Server) handlePatrolClearMoveFailed(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.ClearMoveFailedChannels()
	writeOK(w)
}

// handlePatrolStop は巡回を停止する
func (s *Server) handlePatrolStop(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.Stop()
	writeOK(w)
}

// handlePatrolIdentify はデバイス識別フェーズ（Stagger Probe）を手動実行する。
// 巡回中は 409、識別実行中は 429 を返す。
// 識別はバックグラウンドで実行され、即座に 200 を返す。
func (s *Server) handlePatrolIdentify(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.patroller.Status().Running {
		http.Error(w, "巡回中は識別できません", http.StatusConflict)
		return
	}
	s.mu.Lock()
	if s.identifyRunning {
		s.mu.Unlock()
		http.Error(w, "識別フェーズ実行中です", http.StatusTooManyRequests)
		return
	}
	s.identifyRunning = true
	s.mu.Unlock()

	var req struct {
		Serials  []string `json:"serials"`
		Channels []uint32 `json:"channels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.mu.Lock()
		s.identifyRunning = false
		s.mu.Unlock()
		http.Error(w, err.Error(), 400)
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		s.mu.RLock()
		channels = make([]uint32, len(s.patrolChannels))
		copy(channels, s.patrolChannels)
		s.mu.RUnlock()
	}
	if len(channels) == 0 {
		s.mu.Lock()
		s.identifyRunning = false
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"ok": false, "error": "チャンネルリストが空です"})
		return
	}

	go func() {
		defer func() {
			s.mu.Lock()
			s.identifyRunning = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.patroller.Identify(ctx, req.Serials, channels); err != nil {
			log.Printf("[GUI] デバイス識別失敗: %v", err)
		}
	}()

	writeOK(w)
}

// handleADBRestart は ADB サーバーの復旧を行う。
// 既存接続を維持する手順を優先し、必要時のみハード再起動へフォールバックする。
func (s *Server) handleADBRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := mumu.RecoverServer(r.Context(), s.patroller.Config()); err != nil {
		log.Printf("[MuMu] ADB復旧失敗: %v", err)
	}
	writeOK(w)
}

// handleADBKillServer は adb kill-server を実行してADBサーバーを停止する。
func (s *Server) handleADBKillServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	cfg := s.patroller.Config()
	out, err := exec.CommandContext(r.Context(), cfg.ADBPath, "kill-server").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil && r.Context().Err() == nil {
		log.Printf("[MuMu] adb kill-server 失敗: %v: %s", err, msg)
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if msg != "" {
		log.Printf("[MuMu] adb kill-server: %s", msg)
	}
	writeOK(w)
}

// handleADBConnect は adb connect <serial> で指定シリアルのデバイスを追加接続する。
func (s *Server) handleADBConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "serial required", http.StatusBadRequest)
		return
	}
	// host:port 形式のみ受け付ける（コマンドインジェクション防止）
	if strings.ContainsAny(req.Serial, " \t\r\n/\\;|&`$<>") {
		http.Error(w, "invalid serial format", http.StatusBadRequest)
		return
	}
	cfg := s.patroller.Config()
	out, err := exec.CommandContext(r.Context(), cfg.ADBPath, "connect", req.Serial).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil && r.Context().Err() == nil {
		log.Printf("[MuMu] adb connect %s 失敗: %v: %s", req.Serial, err, msg)
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("[MuMu] adb connect %s: %s", req.Serial, msg)
	writeJSON(w, map[string]interface{}{"ok": true, "message": msg})
}

// handleTapLoopStart はタップループを開始する。
func (s *Server) handleTapLoopStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		TapX       int      `json:"tap_x"`
		TapY       int      `json:"tap_y"`
		IntervalMs int      `json:"interval_ms"`
		Serials    []string `json:"serials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	cfg := s.patroller.Config()
	serials := req.Serials
	if len(serials) == 0 {
		var err error
		serials, err = mumu.ListDevices(r.Context(), cfg)
		if err != nil || len(serials) == 0 {
			writeJSON(w, map[string]any{"ok": false, "error": "デバイスが見つかりません"})
			return
		}
	}
	if req.IntervalMs <= 0 {
		req.IntervalMs = 1000
	}
	s.tapLooper.Start(cfg, serials, req.TapX, req.TapY, req.IntervalMs)
	writeOK(w)
}

// handleTapLoopStop はタップループを停止する。
func (s *Server) handleTapLoopStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.tapLooper.Stop()
	writeOK(w)
}

// handleTapLoopStatus はタップループの現在状態をJSONで返す。
func (s *Server) handleTapLoopStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.tapLooper.Status())
}

// handleWebhookTest は指定URLにDiscordテスト通知を送る
func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "URL必須"})
		return
	}
	if err := notifier.SendTest(req.URL); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeOK(w)
}

// handleSpawnLog は出現ログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleSpawnLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, spawnLogHTML)
}

// handleChatReportNotify はフロントエンドのスコアリングで候補確定したチャットをDiscordに通知する。
func (s *Server) handleChatReportNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Channel  uint32 `json:"channel"`
		Message  string `json:"message"`
		Location string `json:"location"`
		Monster  string `json:"monster"`
		Sender   string `json:"sender"`
		Score    int    `json:"score"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if s.chatReportFn == nil {
		writeOK(w)
		return
	}
	// 5分間のdedup（複数ブラウザタブや再描画での二重送信を防ぐ）
	key := fmt.Sprintf("%d|%s", req.Channel, req.Message)
	now := time.Now()
	s.chatReportDedupMu.Lock()
	if s.chatReportDedup == nil {
		s.chatReportDedup = make(map[string]time.Time)
	}
	if t, seen := s.chatReportDedup[key]; seen && now.Sub(t) < 5*time.Minute {
		s.chatReportDedupMu.Unlock()
		writeOK(w)
		return
	}
	s.chatReportDedup[key] = now
	for k, t := range s.chatReportDedup {
		if now.Sub(t) > 10*time.Minute {
			delete(s.chatReportDedup, k)
		}
	}
	s.chatReportDedupMu.Unlock()

	det := notifier.Detection{
		Source:      notifier.SourceChat,
		ChatLineID:  req.Channel,
		LineID:      req.Channel,
		Location:    req.Location,
		MonsterName: req.Monster,
		Message:     req.Message,
		PlayerName:  req.Sender,
		Score:       req.Score,
		Time:        now,
	}
	go s.chatReportFn(det)
	writeOK(w)
}

// handlePatrolRemoveCh は検知不要プレイヤーの発言から ch を巡回リストに削除するエンドポイント。
// Discord 通知・検知履歴追加は行わず、patrolChannels 削除と 30 分クールダウンのみ実施する。
func (s *Server) handlePatrolRemoveCh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Channel uint32 `json:"channel"`
		Reason  string `json:"reason"`
		Sender  string `json:"sender"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Channel == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 5 分間の二重削除抑止
	now := time.Now()
	s.patrolRemoveDedupMu.Lock()
	if s.patrolRemoveDedup == nil {
		s.patrolRemoveDedup = make(map[uint32]time.Time)
	}
	if t, seen := s.patrolRemoveDedup[req.Channel]; seen && now.Sub(t) < 5*time.Minute {
		s.patrolRemoveDedupMu.Unlock()
		writeOK(w)
		return
	}
	s.patrolRemoveDedup[req.Channel] = now
	for ch, t := range s.patrolRemoveDedup {
		if now.Sub(t) > 10*time.Minute {
			delete(s.patrolRemoveDedup, ch)
		}
	}
	s.patrolRemoveDedupMu.Unlock()

	s.mu.Lock()
	newChs := make([]uint32, 0, len(s.patrolChannels))
	removed := false
	for _, pc := range s.patrolChannels {
		if pc == req.Channel {
			removed = true
			continue
		}
		newChs = append(newChs, pc)
	}
	if removed {
		s.patrolChannels = newChs
	}
	s.cooldownChs[req.Channel] = now.Add(30 * time.Minute)
	saveChannelsFn := s.saveChannelsFn
	s.mu.Unlock()

	if removed {
		log.Printf("[GUI] 検知不要プレイヤー発言: Ch%d を巡回リストから削除 (sender=%q msg=%q 残%d ch)",
			req.Channel, req.Sender, req.Message, len(newChs))
		if saveChannelsFn != nil {
			if err := saveChannelsFn(newChs); err != nil {
				log.Printf("[GUI] channels.txt 保存失敗: %v", err)
			}
		}
	}
	writeOK(w)
}

// handleChatLog はチャットログ履歴をJSONで返す
func (s *Server) handleChatLog(w http.ResponseWriter, r *http.Request) {
	s.chatMu.RLock()
	events := make([]ChatEvent, len(s.chatLog))
	copy(events, s.chatLog)
	s.chatMu.RUnlock()
	writeJSON(w, events)
}

// handleChatEvents はチャットをServer-Sent Eventsでリアルタイム配信する
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	s.sseStream(w, r,
		func(ch chan string) {
			s.chatMu.Lock()
			s.chatClients = append(s.chatClients, ch)
			s.chatMu.Unlock()
		},
		func(ch chan string) {
			s.chatMu.Lock()
			for i, c := range s.chatClients {
				if c == ch {
					s.chatClients = append(s.chatClients[:i], s.chatClients[i+1:]...)
					break
				}
			}
			s.chatMu.Unlock()
		},
		func(msg string) string { return msg },
	)
}

// handlePortMapPending は確認待ち portMap 変更の一覧を返す。
func (s *Server) handlePortMapPending(w http.ResponseWriter, r *http.Request) {
	s.pendingPortMapMu.Lock()
	list := make([]PendingPortMapChange, len(s.pendingPortMaps))
	copy(list, s.pendingPortMaps)
	s.pendingPortMapMu.Unlock()
	writeJSON(w, list)
}

// handlePortMapConfirm は portMap 変更の適用・却下を処理する。
// リクエスト: {"ids":["pm-1","pm-2"],"action":"apply"|"reject"}
func (s *Server) handlePortMapConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	idSet := make(map[string]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		idSet[id] = struct{}{}
	}

	s.pendingPortMapMu.Lock()
	var toApply []PendingPortMapChange
	remaining := s.pendingPortMaps[:0]
	for _, p := range s.pendingPortMaps {
		if _, matched := idSet[p.ID]; matched {
			if req.Action == "apply" {
				toApply = append(toApply, p)
			}
		} else {
			remaining = append(remaining, p)
		}
	}
	s.pendingPortMaps = remaining
	applyFn := s.portMapApplyFn
	s.pendingPortMapMu.Unlock()

	if applyFn != nil {
		for _, p := range toApply {
			applyFn(p.Ch, p.NewIP)
		}
	}
	writeOK(w)
}

// handlePortMapEntries はポートマップの全エントリを返す。
func (s *Server) handlePortMapEntries(w http.ResponseWriter, r *http.Request) {
	fn := s.getPortMapFn
	if fn == nil {
		writeJSON(w, []PortMapEntry{})
		return
	}
	writeJSON(w, fn())
}

// handlePortMapMapCh は現在セッションを指定chにマッピングする。
// リクエスト: {"ch": 5}
func (s *Server) handlePortMapMapCh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Ch uint32 `json:"ch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ch == 0 {
		http.Error(w, "ch must be a positive integer", http.StatusBadRequest)
		return
	}
	fn := s.mapChFn
	if fn != nil {
		fn(req.Ch)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "ch": req.Ch})
}

// handlePortMapMapAll は全セッションをLineIDでマッピングする。
func (s *Server) handlePortMapMapAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fn := s.mapAllFn
	count := 0
	if fn != nil {
		count = fn()
	}
	writeJSON(w, map[string]interface{}{"ok": true, "mapped": count})
}

// handlePortMapManual は ch/IP/ポートを手動でポートマップに登録する。
func (s *Server) handlePortMapManual(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Ch   uint32 `json:"ch"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Ch == 0 || req.IP == "" || req.Port <= 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "ch / ip / port が不正です"})
		return
	}
	serverIP := fmt.Sprintf("%s:%d", req.IP, req.Port)
	if fn := s.portMapApplyFn; fn != nil {
		fn(req.Ch, serverIP)
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleDevicesIdentified は全巡回対象デバイスが追跡可能状態かどうかを返す。
// 各シリアルに対応するセッションが Confirmed && LineID > 0 を満たすか確認する。
func (s *Server) handleDevicesIdentified(w http.ResponseWriter, r *http.Request) {
	serials := s.patroller.Status().Serials
	if len(serials) == 0 {
		serials = s.patroller.Config().ConnectSerials
	}
	total := len(serials)

	var sessions []DeviceSessionInfo
	if s.getSessions != nil {
		sessions = s.getSessions()
	}
	uidToSess := make(map[uint64]DeviceSessionInfo)
	for _, sess := range sessions {
		if sess.UserUID != 0 {
			uidToSess[sess.UserUID] = sess
		}
	}

	ready := 0
	var missing []string
	for _, serial := range serials {
		uid := s.patroller.GetSerialUID(serial)
		if uid == 0 {
			missing = append(missing, serial)
			continue
		}
		sess, ok := uidToSess[uid]
		if !ok || !sess.Confirmed || sess.LineID == 0 {
			missing = append(missing, serial)
			continue
		}
		ready++
	}
	writeJSON(w, map[string]interface{}{
		"identified": total > 0 && ready >= total,
		"total":      total,
		"ready":      ready,
		"missing":    missing,
	})
}

// handleDevicesMemory は Patroller が記憶している serial→uid/label マップを返す。
func (s *Server) handleDevicesMemory(w http.ResponseWriter, r *http.Request) {
	uidMap, labelMap := s.patroller.GetSerialMaps()

	allSerials := make(map[string]struct{})
	for serial := range uidMap {
		allSerials[serial] = struct{}{}
	}
	for serial := range labelMap {
		allSerials[serial] = struct{}{}
	}

	uidToSess := make(map[uint64]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.UserUID != 0 {
				uidToSess[sess.UserUID] = sess
			}
		}
	}

	type DeviceMemoryEntry struct {
		Serial    string `json:"serial"`
		UID       uint64 `json:"uid"`
		Label     string `json:"label"`
		CurrentCh uint32 `json:"current_ch"`
		Confirmed bool   `json:"confirmed"`
	}

	entries := make([]DeviceMemoryEntry, 0, len(allSerials))
	for serial := range allSerials {
		e := DeviceMemoryEntry{
			Serial: serial,
			UID:    uidMap[serial],
			Label:  labelMap[serial],
		}
		if e.UID != 0 {
			if sess, ok := uidToSess[e.UID]; ok {
				e.CurrentCh = sess.CurrentCh
				e.Confirmed = sess.Confirmed
			}
		}
		entries = append(entries, e)
	}
	writeJSON(w, entries)
}

// handleDeleteSerialUID は serial に紐付いた UID バインドを削除する。
func (s *Server) handleDeleteSerialUID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	s.patroller.DeleteSerialUIDBinding(req.Serial)
	writeOK(w)
}

// handleComputeAssignments は現在のデバイス台数からch分担を計算してファイルに保存し、Patrollerへ反映する。
func (s *Server) handleComputeAssignments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serials := s.patroller.Status().Serials
	if len(serials) == 0 {
		serials = s.patroller.Config().ConnectSerials
	}
	deviceCount := len(serials)
	if deviceCount == 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "デバイス未検出"})
		return
	}
	assignments := mumu.ComputeDeviceAssignments(deviceCount, 100)
	s.mu.RLock()
	file := s.assignmentsFile
	s.mu.RUnlock()
	if file == "" {
		file = "config/device_assignments.json"
	}
	if err := mumu.SaveDeviceAssignments(file, assignments); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	s.patroller.SetDeviceAssignments(assignments)
	log.Printf("[GUI] デバイス分担計算: %d台 → %dグループ保存 (%s)", deviceCount, len(assignments), file)
	writeJSON(w, map[string]interface{}{"ok": true, "device_count": deviceCount, "groups": len(assignments)})
}

// handleChatLogPage はチャットログ専用の分離ウィンドウ用HTMLページを返す
func (s *Server) handleChatLogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, chatLogHTML)
}

// handlePatrolScreenshot は serial デバイスのスクリーンショット PNG を返す。
// GUI の監視矩形ドラッグ選択モーダルから呼ばれる。
func (s *Server) handlePatrolScreenshot(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", 405)
		return
	}
	serial := r.URL.Query().Get("serial")
	if serial == "" {
		http.Error(w, "serial required", 400)
		return
	}
	cfg := s.patroller.Config()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	png, err := mumu.CaptureScreenshotPNG(ctx, serial, cfg)
	if err != nil {
		http.Error(w, "screenshot failed: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(png)
}

// handlePatrolStatus は現在の巡回状態を返す
func (s *Server) handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		writeJSON(w, map[string]interface{}{"running": false, "last_channel": 0})
		return
	}
	writeJSON(w, s.patroller.Status())
}

// handlePatrolDeviceStatuses はデバイスごとの巡回状態を返す
func (s *Server) handlePatrolDeviceStatuses(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, s.patroller.GetDeviceStatuses())
}

// handlePatrolRecover は指定シリアルのゲームクラッシュ復帰を手動トリガーする
func (s *Server) handlePatrolRecover(w http.ResponseWriter, r *http.Request) {
	if !s.patrolEnabled {
		http.Error(w, "patrol disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		http.Error(w, "serial required", http.StatusBadRequest)
		return
	}
	go func() {
		if err := s.patroller.RecoverDevice(r.Context(), req.Serial); err != nil {
			log.Printf("[GUI] recover device %s: %v", req.Serial, err)
		}
	}()
	writeJSON(w, map[string]interface{}{"ok": true, "serial": req.Serial})
}

// sseStream はSSEエンドポイントの共通ループ処理。
// add/remove でクライアントチャネルの登録・解除を行い、format でメッセージを整形する。
func (s *Server) sseStream(w http.ResponseWriter, r *http.Request,
	add func(chan string),
	remove func(chan string),
	format func(string) string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	add(ch)
	defer remove(ch)

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", format(msg))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// handleSSE はServer-Sent Eventsで検知ログをリアルタイム配信する
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	s.sseStream(w, r,
		func(ch chan string) {
			s.mu.Lock()
			s.clients = append(s.clients, ch)
			s.mu.Unlock()
		},
		func(ch chan string) {
			s.mu.Lock()
			for i, c := range s.clients {
				if c == ch {
					s.clients = append(s.clients[:i], s.clients[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
		},
		func(msg string) string { return strings.ReplaceAll(msg, "\n", "\\n") },
	)
}

// processedIndexHTML は indexHTML に対して起動時に一度だけ適用した加工済みHTML。
// handleIndex が呼ばれるたびに同じ置換処理を繰り返さないようにする。
var processedIndexHTML = func() string {
	html := indexHTML
	html = strings.ReplaceAll(html, "setTimeout(function(){switchView('dashboard'", "")
	html = strings.ReplaceAll(html, "setInterval(function(){switchView('dashboard'", "")
	html = strings.ReplaceAll(html, "function switchView(id,navEl){", "function switchView(id,navEl){if(window.__disableAutoSwitchView)return;")
	return html
}()

const patrolDisabledScript = `<script>
(function(){
	window.__patrolDisabled=true;
	function hide(id){var el=document.getElementById(id);if(el)el.remove();}
	hide('nav-patrol');
	hide('view-patrol');
	hide('card-dash-patrol');
	hide('nav-data-management');
	hide('view-data-management');
	if(typeof loadPatrolChannels==='function'){loadPatrolChannels=async function(){window.patrolChannels=[];};}
	if(typeof pollPatrolStatus==='function'){pollPatrolStatus=function(){};}
	if(typeof patrolStart==='function'){patrolStart=async function(){alert('このアプリでは巡回機能は無効です');};}
	if(typeof patrolStop==='function'){patrolStop=async function(){};}
	if(typeof saveChannels==='function'){saveChannels=async function(){};}
	if(typeof clearMoveFailedChannels==='function'){clearMoveFailedChannels=async function(){};}
})();
</script>`
