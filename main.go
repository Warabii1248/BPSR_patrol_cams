package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/balrogsxt/StarResonanceAPI/appconfig"
	"github.com/balrogsxt/StarResonanceAPI/gui"
	"github.com/balrogsxt/StarResonanceAPI/location"
	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/ncap"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
	"github.com/google/gopacket/pcap"
)

var (
	configPath    = flag.String("config", "config/config.json", "path to config.json")
	networkFlag   = flag.String("network", "", "NIC description (auto = auto-detect)")
	webhookFlag   = flag.String("webhook", "", "Discord webhook URL (overrides config)")
	autoCheckTime = flag.Int("auto-check", 0, "seconds to sample interfaces when using auto")

	// チャンネルデバッグ用フラグ
	// --ch-debug : 全methodId・シーン変更パケットhexをログ出力する
	chDebugFlag = flag.Bool("ch-debug", false, "debug: log all methodIds and scene-change packet hex")
	// --ch-hunt N : 全パケットから varint N を再帰スキャンし、ヒット時に [ChHunt] ★ をログ出力する
	chHuntFlag = flag.Uint("ch-hunt", 0, "debug: hunt for this channel number in all packets (0=disabled)")
	// --ch-watch : 候補パスの値が変化したときのみ [ChWatch] ★変化★ をログ出力する
	chWatchFlag = flag.Bool("ch-watch", false, "debug: monitor candidate ch paths and log on value change")
	// --ch-map-file : ポート→ch対応を収集してJSONに保存（ターミナルでch番号を入力）
	chMapFileFlag = flag.String("ch-map-file", "", "collect server-port→ch mapping; type ch number in terminal")
)

// Version はビルド時に -ldflags "-X main.Version=x.y.z" で注入される
var Version = "dev"

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Fatalf("fatal: %v\n%s", r, debug.Stack())
		}
	}()
	flag.Parse()

	// ログをコンソールと logs/log.txt の両方に出力する（起動のたびに上書き）
	if err := os.MkdirAll("logs", 0755); err != nil {
		log.Printf("warn: cannot create logs dir: %v", err)
	}
	logFile, err := os.OpenFile("logs/log.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("warn: cannot open log.txt: %v", err)
	} else {
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	}

	cfg, err := appconfig.Load(*configPath)
	if err != nil {
		log.Fatalf("config load error: %v", err)
	}
	if *networkFlag != "" {
		cfg.Network = *networkFlag
	}
	if *autoCheckTime != 0 {
		cfg.AutoCheck = *autoCheckTime
	}
	if *webhookFlag != "" {
		cfg.DiscordWebhook = *webhookFlag
	}

	devices, err := pcap.FindAllDevs()
	if err != nil {
		log.Fatalf("find interfaces: %v", err)
	}
	if len(devices) == 0 {
		log.Fatal("no pcap interfaces found (Npcap/WinPcap required)")
	}

	selectedDesc := cfg.Network
	if selectedDesc == "" || selectedDesc == "auto" {
		log.Printf("auto-selecting NIC (sampling %ds)...", cfg.AutoCheck)
		if active := ncap.GetActiveNetworkCards(devices, cfg.AutoCheck); active != nil {
			log.Printf("auto-selected: %s (pkts=%d bytes=%d)", active.Desc, active.PacketCount, active.ByteCount)
			selectedDesc = active.Desc
		} else {
			selectedDesc = ""
		}
	}

	if selectedDesc == "" {
		options := make([]string, len(devices))
		for i, d := range devices {
			name := d.Description
			if name == "" {
				name = d.Name
			}
			options[i] = name
		}
		var choice string
		prompt := &survey.Select{
			Message: "Select the network interface to capture on:",
			Options: options,
		}
		if err2 := survey.AskOne(prompt, &choice); err2 != nil {
			log.Fatalf("selection cancelled: %v", err2)
		}
		selectedDesc = choice
	}

	handle, err := openByDescription(devices, selectedDesc)
	if err != nil {
		log.Fatalf("open interface: %v", err)
	}
	defer handle.Close()
	log.Printf("capturing on: %s", selectedDesc)

	var locStore *location.Store
	if cfg.Locations != "" {
		if store, loadErr := location.Load(cfg.Locations); loadErr != nil {
			log.Printf("warn: locations load failed (%v); names unavailable", loadErr)
		} else {
			locStore = store
			log.Printf("loaded %d locations from %s", store.Count(), cfg.Locations)
		}
	}

	discord := &notifier.DiscordWebhook{URL: cfg.DiscordWebhook, ChatReportURL: cfg.DiscordChatReportWebhook}
	if cfg.DiscordWebhook != "" {
		log.Println("discord webhook configured")
	} else {
		log.Println("discord webhook not configured; detections will log only")
	}
	if cfg.DiscordChatReportWebhook != "" {
		log.Println("discord chat report webhook configured")
	}

	// mumu.Config を構築（並列設定を含む）
	mumuCfg := mumu.Config{
		ADBPath:            cfg.ADBPath,
		TapX:               cfg.MumuTapX,
		TapY:               cfg.MumuTapY,
		ClearLength:        cfg.MumuClearLength,
		PreKeycode:         cfg.MumuPreKeycode,
		ConnectSerials:     cfg.MumuSerials,
		GlobalDelay:        time.Duration(cfg.MumuDelayMs) * time.Millisecond,
		ParallelLimit:      cfg.ParallelLimit,
		ParallelGroupDelay: time.Duration(cfg.ParallelGroupDelaySecs * float64(time.Second)),
		MoveTimeout:        time.Duration(cfg.PatrolMoveTimeoutSecs * float64(time.Second)),
		MergeTimeout:       time.Duration(cfg.PatrolMergeTimeoutSecs * float64(time.Second)),
		DwellDuration:      time.Duration(cfg.PatrolDwellSecs * float64(time.Second)),
	}

	var patrolChannels []uint32
	if cfg.PatrolChannelsFile != "" {
		if chs, loadErr := mumu.LoadChannels(cfg.PatrolChannelsFile); loadErr == nil {
			patrolChannels = chs
			log.Printf("patrol channels: %d loaded", len(patrolChannels))
		} else {
			log.Printf("warn: patrol channels load failed: %v", loadErr)
		}
	}

	// 起動時に ADB サーバーを確実に起動する（未起動の場合の検知失敗を防ぐ）
	if err := mumu.EnsureServer(context.Background(), mumuCfg); err != nil {
		log.Printf("warn: ADB サーバー起動失敗（検知に影響する可能性あり）: %v", err)
	}

	// GUI サーバーを作成
	guiServer := gui.New(cfg.GUIPort, mumuCfg, patrolChannels, cfg.PatrolChannelsFile)

	// 全ログ行をGUIのSSEにも流す
	log.SetOutput(guiServer.LogWriter(log.Writer()))

	onDetect := func(det notifier.Detection) {
		// アステリア平原は1-100chのみ通知
		ch := det.LineID
		if det.ChatLineID > 0 {
			ch = det.ChatLineID
		}
		if ch > 100 {
			// ch=0 は「チャンネル不明」（テスト通知含む）のためスキップしない
			log.Printf("[DETECTION] 通知対象外ch: %d (通知スキップ)", ch)
			return
		}
		// 通知対象エネミーのみ通知（テンプレートIDまたはモンスター名で判定）
		{
			// 設定名 → テンプレートID のマッピング
			enemyIDMap := map[string]uint64{
				"ウリボ・ゴールド": ncap.LoyalBoarletTemplateID,
				"金ナッポ":     ncap.GoldNappoTemplateID,
				"銀ナッポ":     ncap.SilverNappoTemplateID,
			}
			// テンプレートID → 設定名 の逆引き
			idToName := map[uint64]string{
				ncap.LoyalBoarletTemplateID: "ウリボ・ゴールド",
				ncap.GoldNappoTemplateID:    "金ナッポ",
				ncap.SilverNappoTemplateID:  "銀ナッポ",
			}
			// 対象モンスターの設定名を特定
			var configName string
			if det.TemplateID != 0 {
				configName = idToName[det.TemplateID]
			} else if _, ok := enemyIDMap[det.MonsterName]; ok {
				// TemplateID未設定時はモンスター名でフォールバック
				configName = det.MonsterName
			}
			// 対象モンスターの場合のみフィルターを適用
			if configName != "" {
				allowed := false
				for _, e := range cfg.NotifyEnemies {
					if e.Name == configName && e.Enabled {
						allowed = true
						break
					}
				}
				if !allowed {
					log.Printf("[DETECTION] 通知対象外エネミー: %s (通知スキップ)", configName)
					return
				}
			}
		}
		log.Println("[DETECTION]\n" + notifier.Format(det))
		if err3 := discord.Send(det); err3 != nil {
			log.Printf("discord send error: %v", err3)
		}
		guiServer.OnDetect(det)
	}

	guiServer.SetChatReportFn(onDetect)

	capDevice := ncap.NewCapDevice(handle, selectedDesc)
	capDevice.SetNotifier(onDetect)
	if locStore != nil {
		capDevice.SetLocations(locStore)
	}
	if cfg.DebounceSeconds > 0 {
		capDevice.SetDebounce(time.Duration(cfg.DebounceSeconds) * time.Second)
	}
	if len(cfg.ChatExclude) > 0 {
		capDevice.SetChatExclude(cfg.ChatExclude)
		log.Printf("chat_exclude: %v", cfg.ChatExclude)
	}
	if len(cfg.SceneMapIds) > 0 {
		capDevice.SetSceneMapIds(cfg.SceneMapIds)
		log.Printf("scene_map_ids: %d件設定", len(cfg.SceneMapIds))
	}
	if cfg.PortMapFile != "" {
		capDevice.SetPortMapFile(cfg.PortMapFile)
		log.Printf("port map: %s", cfg.PortMapFile)
	}
	if *chDebugFlag {
		capDevice.SetChDebug(true)
	}
	if *chHuntFlag > 0 {
		capDevice.SetChHunt(uint32(*chHuntFlag))
	}
	if *chWatchFlag {
		capDevice.SetChWatch(true)
	}

	// Patroller がチャンネル切替時に capDevice へ現在チャンネルを通知する
	guiServer.SetChannelNotifyFn(func(ch uint32) {
		capDevice.SetCurrentChannel(ch)
	})

	// ADBデバイス ↔ キャプチャUID の対応表をGUIに提供
	guiServer.SetSessionProvider(func() []gui.DeviceSessionInfo {
		raw := capDevice.Sessions()
		out := make([]gui.DeviceSessionInfo, len(raw))
		for i, s := range raw {
			out[i] = gui.DeviceSessionInfo{
				Label:     s.Label,
				ClientIP:  s.ClientIP,
				UserUID:   s.UserUID,
				MapID:     s.MapID,
				LineID:    s.LineID,
				Confirmed: s.Confirmed,
			}
		}
		return out
	})

	// テスト通知ボタン用：プレイヤー位置を指定モンスター検知として発火
	guiServer.SetTestDetectFn(func(monster string) {
		capDevice.ForceDetect(monster)
	})

	// チャット受信をGUIへ転送
	capDevice.SetChatNotifier(guiServer.OnChat)

	// portMap 変更はGUI確認後に適用
	capDevice.SetPortMapPendingFn(func(ch uint32, newIP, oldIP string, voteCount int) {
		guiServer.AddPortMapPending(ch, newIP, oldIP, voteCount)
	})
	guiServer.SetPortMapApplyFn(func(ch uint32, serverIP string) {
		capDevice.ApplyPortMapUpdate(ch, serverIP)
	})

	// ポートマップ関連コールバック
	guiServer.SetGetPortMapFn(func() []gui.PortMapEntry {
		entries := capDevice.PortMapEntries()
		out := make([]gui.PortMapEntry, len(entries))
		for i, e := range entries {
			out[i] = gui.PortMapEntry{
				Ch:        e.Ch,
				ServerIP:  e.ServerIP,
				UpdatedAt: e.UpdatedAt,
			}
		}
		return out
	})
	guiServer.SetMapChFn(func(ch uint32) {
		sessions := capDevice.Sessions()
		for _, s := range sessions {
			if s.ServerIP != "" {
				capDevice.ApplyPortMapUpdate(ch, s.ServerIP)
			}
		}
		log.Printf("[GUI] 指定chマッピング: ch=%d, %d件のセッションを登録", ch, len(sessions))
	})
	guiServer.SetMapAllFn(func() int {
		sessions := capDevice.Sessions()
		count := 0
		for _, s := range sessions {
			if s.ServerIP != "" && s.LineID > 0 {
				capDevice.ApplyPortMapUpdate(s.LineID, s.ServerIP)
				count++
			}
		}
		log.Printf("[GUI] 全chマッピング: %d件のセッションを登録", count)
		return count
	})

	// チャンネルリストをファイルに保存するコールバック
	guiServer.SetSaveChannelsFn(func(channels []uint32) error {
		return mumu.SaveChannels(cfg.PatrolChannelsFile, channels)
	})

	// config.json の読み書きコールバック
	guiServer.SetConfigFns(
		func() ([]byte, error) {
			c, err := appconfig.Load(*configPath)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		},
		func(data []byte) error {
			c := &appconfig.Config{}
			if err := json.Unmarshal(data, c); err != nil {
				return err
			}
			if err := appconfig.Save(*configPath, c); err != nil {
				return err
			}
			// 通知エネミー
			cfg.NotifyEnemies = c.NotifyEnemies
			// Discord Webhook
			discord.URL = c.DiscordWebhook
			discord.ChatReportURL = c.DiscordChatReportWebhook
			// デバウンス
			capDevice.SetDebounce(time.Duration(c.DebounceSeconds) * time.Second)
			// チャットフィルター（filter.json から保存された値）
			capDevice.SetChatExclude(c.ChatExclude)
			// シーンマップID
			if len(c.SceneMapIds) > 0 {
				capDevice.SetSceneMapIds(c.SceneMapIds)
			}
			// GAS 対象エネミー（即時反映）
			guiServer.SetGASTargetEnemy(c.GASTargetEnemy)
			return nil
		},
	)
	guiServer.SetWindowStateFns(
		func() (*appconfig.WindowState, error) {
			return appconfig.LoadWindowState(*configPath)
		},
		func(ws *appconfig.WindowState) error {
			return appconfig.SaveWindowState(*configPath, ws)
		},
	)

	go func() {
		if startErr := capDevice.Start(); startErr != nil {
			log.Println("capture stopped:", startErr)
		}
	}()

	if *chMapFileFlag != "" {
		go runChMapCollector(*chMapFileFlag, func() []ncap.CaptureSession {
			return capDevice.Sessions()
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.GUIPort > 0 {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			cancel()
		}()
		if err := guiServer.RunWindow(ctx); err != nil {
			log.Printf("GUI error: %v", err)
		}
		// ウィンドウが閉じられたら明示的にキャンセル（goroutine終了）
		cancel()
	} else {
		log.Println("GUI disabled (gui_port=0)")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}
	log.Println("shutting down...")
}


// runChMapCollector はターミナルからch番号を受け取り、
// 現在のセッションのサーバーポートをch番号と対応付けてJSONファイルに保存する。
// 使い方: エミュレーターをch Xに移動後、ターミナルに "X" と入力してEnter。
func runChMapCollector(mapFile string, getSessions func() []ncap.CaptureSession) {
	// 既存のマッピングを読み込む
	mapping := make(map[string]int) // "ip:port" → ch
	if data, err := os.ReadFile(mapFile); err == nil {
		_ = json.Unmarshal(data, &mapping)
		log.Printf("[ChMap] 既存マッピング読み込み: %d件 (%s)", len(mapping), mapFile)
	}

	log.Printf("[ChMap] ポート→ch収集モード開始 (保存先: %s)", mapFile)
	log.Println("[ChMap] 使い方: エミュレーターをch移動後、ch番号を入力してEnter")
	log.Println("[ChMap]   例: 40 → ch40のポートを記録")
	log.Println("[ChMap]   'show' → 現在のマッピング一覧")
	log.Println("[ChMap]   'sessions' → 現在のセッション一覧")

	save := func() {
		data, _ := json.MarshalIndent(mapping, "", "  ")
		if err := os.WriteFile(mapFile, data, 0644); err != nil {
			log.Printf("[ChMap] 保存失敗: %v", err)
		} else {
			log.Printf("[ChMap] 保存完了: %d件 → %s", len(mapping), mapFile)
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "show" {
			log.Printf("[ChMap] 現在のマッピング (%d件):", len(mapping))
			for addr, ch := range mapping {
				log.Printf("[ChMap]   ch=%d ← %s", ch, addr)
			}
			continue
		}
		if line == "sessions" {
			sessions := getSessions()
			log.Printf("[ChMap] 現在のセッション (%d件):", len(sessions))
			for _, s := range sessions {
				log.Printf("[ChMap]   [%s] server=%s uid=%d", s.Label, s.ServerIP, s.UserUID)
			}
			continue
		}
		ch, err := strconv.Atoi(line)
		if err != nil || ch <= 0 {
			log.Printf("[ChMap] 無効な入力: %q (ch番号の整数、'show'、'sessions' のいずれかを入力)", line)
			continue
		}
		sessions := getSessions()
		if len(sessions) == 0 {
			log.Printf("[ChMap] ch=%d: セッションなし（まだ接続されていない可能性）", ch)
			continue
		}
		recorded := 0
		for _, s := range sessions {
			if s.ServerIP == "" {
				continue
			}
			mapping[s.ServerIP] = ch
			log.Printf("[ChMap] ch=%d ← %s [%s] を記録", ch, s.ServerIP, s.Label)
			recorded++
		}
		if recorded > 0 {
			save()
		} else {
			log.Printf("[ChMap] ch=%d: ServerIPが空のセッションのみ（接続待ち?）", ch)
		}
	}
}

func openByDescription(devices []pcap.Interface, desc string) (*pcap.Handle, error) {
	for _, d := range devices {
		name := d.Description
		if name == "" {
			name = d.Name
		}
		if name == desc {
			h, err := pcap.OpenLive(d.Name, 1024*1024*10, true, pcap.BlockForever)
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", d.Name, err)
			}
			return h, nil
		}
	}
	return nil, fmt.Errorf("interface %q not found", desc)
}
