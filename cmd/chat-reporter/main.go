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
	"github.com/balrogsxt/StarResonanceAPI/ncap"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
	"github.com/google/gopacket/pcap"
)

var (
	configPath    = flag.String("config", "config/config.json", "path to config.json")
	networkFlag   = flag.String("network", "", "NIC description (auto = auto-detect)")
	webhookFlag   = flag.String("webhook", "", "Discord webhook URL (overrides config)")
	autoCheckTime = flag.Int("auto-check", 0, "seconds to sample interfaces when using auto")

	chDebugFlag = flag.Bool("ch-debug", false, "debug: log all methodIds and scene-change packet hex")
	chHuntFlag  = flag.Uint("ch-hunt", 0, "debug: hunt for this channel number in all packets (0=disabled)")
	chWatchFlag = flag.Bool("ch-watch", false, "debug: monitor candidate ch paths and log on value change")
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Fatalf("fatal: %v\n%s", r, debug.Stack())
		}
	}()
	flag.Parse()

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
	if cfg.DiscordChatReportWebhook != "" {
		log.Println("discord chat report webhook configured")
	} else if cfg.DiscordWebhook != "" {
		log.Println("discord webhook configured")
	} else {
		log.Println("discord webhook not configured; notifications will log only")
	}

	guiServer := gui.NewWithOptions(cfg.GUIPort, mumu.Config{}, nil, "", gui.Options{PatrolEnabled: false})
	guiServer.SetGASEnable(cfg.GASEnable)
	guiServer.SetGASTargetEnemy(cfg.GASTargetEnemy)
	log.SetOutput(guiServer.LogWriter(log.Writer()))

	onDetect := func(det notifier.Detection) {
		if det.Source != notifier.SourceChat {
			return
		}
		ch := det.ChatLineID
		if ch == 0 {
			ch = det.LineID
		}
		if ch > 100 {
			log.Printf("[CHAT_REPORT] 通知対象外ch: %d (通知スキップ)", ch)
			return
		}
		log.Println("[CHAT_REPORT]\n" + notifier.Format(det))
		if err3 := discord.Send(det); err3 != nil {
			log.Printf("discord send error: %v", err3)
		}
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

	capDevice.SetChatNotifier(guiServer.OnChat)

	capDevice.SetPortMapPendingFn(func(ch uint32, newIP, oldIP string, voteCount int) {
		guiServer.AddPortMapPending(ch, newIP, oldIP, voteCount)
	})
	guiServer.SetPortMapApplyFn(func(ch uint32, serverIP string) {
		capDevice.ApplyPortMapUpdate(ch, serverIP)
	})

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
			discord.URL = c.DiscordWebhook
			discord.ChatReportURL = c.DiscordChatReportWebhook
			capDevice.SetDebounce(time.Duration(c.DebounceSeconds) * time.Second)
			capDevice.SetChatExclude(c.ChatExclude)
			if len(c.SceneMapIds) > 0 {
				capDevice.SetSceneMapIds(c.SceneMapIds)
			}
			guiServer.SetGASTargetEnemy(c.GASTargetEnemy)
			guiServer.SetGASEnable(c.GASEnable)
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
		cancel()
	} else {
		log.Println("GUI disabled (gui_port=0)")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}
	log.Println("shutting down...")
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
