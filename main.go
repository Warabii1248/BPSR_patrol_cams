package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/balrogsxt/StarResonanceAPI/appconfig"
	"github.com/balrogsxt/StarResonanceAPI/gui"
	"github.com/balrogsxt/StarResonanceAPI/location"
	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/gasfetch"
	"github.com/balrogsxt/StarResonanceAPI/ncap"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
	"github.com/google/gopacket/pcap"
)

var (
	configPath    = flag.String("config", "config.json", "path to config.json")
	networkFlag   = flag.String("network", "", "NIC description (auto = auto-detect)")
	webhookFlag   = flag.String("webhook", "", "Discord webhook URL (overrides config)")
	autoCheckTime = flag.Int("auto-check", 0, "seconds to sample interfaces when using auto")
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

	// ログをコンソールと log.txt の両方に出力する（起動のたびに上書き）
	logFile, err := os.OpenFile("log.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
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

	discord := &notifier.DiscordWebhook{URL: cfg.DiscordWebhook}
	if cfg.DiscordWebhook != "" {
		log.Println("discord webhook configured")
	} else {
		log.Println("discord webhook not configured; detections will log only")
	}

	// mumu.Config を構築（並列設定を含む）
	mumuCfg := mumu.Config{
		ADBPath:            cfg.ADBPath,
		TapX:               cfg.MumuTapX,
		TapY:               cfg.MumuTapY,
		ClearLength:        cfg.MumuClearLength,
		PreKeycode:         cfg.MumuPreKeycode,
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
	if err := mumu.EnsureServer(mumuCfg); err != nil {
		log.Printf("warn: ADB サーバー起動失敗（検知に影響する可能性あり）: %v", err)
	}

	// GUI サーバーを作成
	guiServer := gui.New(cfg.GUIPort, mumuCfg, patrolChannels, cfg.PatrolChannelsFile)

	// 全ログ行をGUIのSSEにも流す
	log.SetOutput(guiServer.LogWriter(log.Writer()))

	onDetect := func(det notifier.Detection) {
		log.Println("[DETECTION]\n" + notifier.Format(det))
		if err3 := discord.Send(det); err3 != nil {
			log.Printf("discord send error: %v", err3)
		}
		guiServer.OnDetect(det)
	}


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
	if cfg.MonsterScan {
		capDevice.SetMonsterScan(true)
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

	// テスト通知ボタン用：プレイヤー位置をゴールドウリボ検知として発火
	guiServer.SetTestDetectFn(func() {
		capDevice.ForceDetect()
	})

	// モンスタースキャンモード切替
	guiServer.SetMonsterScanFn(func(enabled bool) {
		capDevice.SetMonsterScan(enabled)
	})

	// チャット受信をGUIへ転送
	capDevice.SetChatNotifier(guiServer.OnChat)

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
			return appconfig.Save(*configPath, c)
		},
	)

	go func() {
		if startErr := capDevice.Start(); startErr != nil {
			log.Println("capture stopped:", startErr)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// GAS 自動チャンネル更新ゴルーチン
	if cfg.GASURL != "" {
		interval := time.Duration(cfg.GASFetchIntervalMins * float64(time.Minute))
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		go func() {
			// 起動直後に1回実行
			runGASFetch(cfg, guiServer)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					runGASFetch(cfg, guiServer)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

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

// runGASFetch は GAS から討伐情報を取得してチャンネルリストを更新する。
func runGASFetch(cfg *appconfig.Config, srv *gui.Server) {
	entries, err := gasfetch.Fetch(cfg.GASURL)
	if err != nil {
		log.Printf("[GASFetch] 取得失敗: %v", err)
		return
	}
	chs := gasfetch.FilterChannels(entries, cfg.GASSpawnThresholdHours)
	if len(chs) == 0 {
		log.Printf("[GASFetch] 閾値%.1fh以上のchなし（全%d件）", cfg.GASSpawnThresholdHours, len(entries))
		return
	}
	log.Printf("[GASFetch] チャンネル更新: %d件 (閾値=%.1fh): %v", len(chs), cfg.GASSpawnThresholdHours, chs)
	srv.UpdateChannelsFromGAS(chs)
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
