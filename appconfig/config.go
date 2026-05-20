package appconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WindowState はGUIウィンドウの位置・サイズを表す。
type WindowState struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// EnemyNotifyConfig は通知対象エネミーの名前と有効/無効フラグを保持する。
type EnemyNotifyConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Config はアプリ全体の設定（config.json から読み書きされる）
type Config struct {
	// NotifyEnemies は通知対象のエネミーリスト（名前とenable/disableフラグ）。
	NotifyEnemies []EnemyNotifyConfig `json:"notify_enemies"`
	// --- キャプチャ設定 ---

	// Network はキャプチャするNICの説明文。"auto" で最アクティブなNICを自動選択。
	Network string `json:"network"`
	// AutoCheck は Network=="auto" のときサンプリングする秒数。デフォルト: 3
	AutoCheck int `json:"auto_check"`
	// Locations は locations.json のパス。デフォルト: "data/locations.json"
	Locations string `json:"locations"`

	// --- 通知設定 ---

	// DiscordWebhook は Discord の Webhook URL。空で無効。
	DiscordWebhook string `json:"discord_webhook"`
	// DiscordChatReportWebhook は報告候補チャット通知専用の Discord Webhook URL。空で無効。
	DiscordChatReportWebhook string `json:"discord_chat_report_webhook"`
	// DebounceSeconds は同Ch+場所の重複通知を抑制する秒数。デフォルト: 30
	DebounceSeconds int `json:"debounce_seconds"`

	// FilterFile はチャットフィルター設定ファイルのパス。デフォルト: "filter.json"
	FilterFile string `json:"filter_file"`

	// --- フィルター設定（実行時は FilterFile から読み込まれる）---

	// ChatExclude はワールドチャット検知を抑制するキーワード一覧。
	ChatExclude []string `json:"chat_exclude,omitempty"`
	// ChatReportSenders は発見報告候補として扱う発言者フィルター。
	ChatReportSenders []string `json:"chat_report_senders,omitempty"`
	// ChatReportExcludedSenders は候補から除外する発言者フィルター。
	ChatReportExcludedSenders []string `json:"chat_report_excluded_senders,omitempty"`
	// ChatReportLocationRules は地点別名と出現候補モンスターの追加ルール。
	ChatReportLocationRules []string `json:"chat_report_location_rules,omitempty"`
	// ChatReportMonsterAliasRules はモンスター別名の追加ルール。
	ChatReportMonsterAliasRules []string `json:"chat_report_monster_alias_rules,omitempty"`
	// ChatReportMinLength は報告候補の最小文字数。0でデフォルト(4)。
	ChatReportMinLength int `json:"chat_report_min_length,omitempty"`
	// ChatReportMaxLength は報告候補の最大文字数。0でデフォルト(80)。
	ChatReportMaxLength int `json:"chat_report_max_length,omitempty"`

	// --- GUI / ADB 設定 ---

	// GUIPort はWebGUIのポート番号。0でGUI無効。デフォルト: 8080
	GUIPort int `json:"gui_port"`
	// ADBPath はadb.exeのパス。デフォルト: "adb"
	ADBPath string `json:"adb_path"`
	// MumuSerials はADBシリアル一覧。空の場合は自動検出。
	MumuSerials []string `json:"mumu_serials"`
	// MumuTapX, MumuTapY はチャンネル入力欄のタップ座標
	MumuTapX int `json:"mumu_tap_x"`
	MumuTapY int `json:"mumu_tap_y"`
	// MumuClearLength は入力前にDELを送る回数
	MumuClearLength int `json:"mumu_clear_length"`
	// MumuPreKeycode はタップ前に送るキーコード
	MumuPreKeycode string `json:"mumu_pre_keycode"`
	// MumuDelayMs は各ADBコマンド間のウェイト(ms)。デフォルト: 1200
	MumuDelayMs int `json:"mumu_delay_ms"`
	// WindowState はGUIウィンドウの位置・サイズ。
	WindowState *WindowState `json:"window_state,omitempty"`

	// --- チャンネル巡回設定 ---

	// PatrolChannelsFile はチャンネルリストファイルのパス。デフォルト: "channels.txt"
	PatrolChannelsFile string `json:"patrol_channels_file"`

	// PortMapFile はサーバーポート→ch番号の対応マップファイルのパス。デフォルト: "port_ch_map.json"
	// 巡回モードで訪れたchを自動記録し、ポート変更時は自動更新する。
	PortMapFile string `json:"port_map_file"`

	// PatrolDwellSecs はch移動完了後〜次ch移動開始までの待機秒数。デフォルト: 10
	PatrolDwellSecs float64 `json:"patrol_dwell_secs"`

	// PatrolMoveTimeoutSecs は1台目の[0x2E]パケットを待つ最大秒数。
	// 時間内に1台も来なければ満員と判定してスキップ。0=無効。デフォルト: 30
	PatrolMoveTimeoutSecs float64 `json:"patrol_move_timeout_secs"`

	// PatrolMergeTimeoutSecs は1台目受信後、残り台数を待つ最大秒数。
	// 0=PatrolMoveTimeoutSecsと同じ従来動作。デフォルト: 15
	PatrolMergeTimeoutSecs float64 `json:"patrol_merge_timeout_secs"`

	// ParallelLimit は同時切替の最大台数。0=無制限（グループディレイも無効）。
	ParallelLimit int `json:"parallel_limit"`

	// ParallelGroupDelaySecs はグループ間の待機秒数。ParallelLimit>0のとき有効。
	ParallelGroupDelaySecs float64 `json:"parallel_group_delay_secs"`

	// PatrolSerials は巡回に使うADBシリアル一覧。空の場合は全デバイスを使用。
	PatrolSerials []string `json:"patrol_serials"`

	// ActiveDeviceCount は稼働台数。0=自動検出。固定値で判定したい場合に設定。
	ActiveDeviceCount int `json:"active_device_count"`

	// FullThreshold は満員判定閾値。稼働台数の何割が移動完了シグナルを送ったら満員ではないと判断するか。0.0-1.0。0=従来通り全台。
	FullThreshold float64 `json:"full_threshold"`

	// ConsecutiveFullThreshold は連続満員スキップの閾値。クラッシュ検知用。0=無効。
	ConsecutiveFullThreshold int `json:"consecutive_full_threshold"`

	// PatrolAdaptiveTimeout は実ロード時間を学習して MoveTimeout/MergeTimeout を自動調整する（デフォルト: true）
	PatrolAdaptiveTimeout bool `json:"patrol_adaptive_timeout"`
	// PatrolAdaptiveTimeoutWindow は学習に使うサンプル数（デフォルト: 10）
	PatrolAdaptiveTimeoutWindow int `json:"patrol_adaptive_timeout_window"`

	// SerialToLabel はADBシリアル→インスタンスラベルの手動マッピング。
	// 例: {"emulator-5556": "Instance-1", "emulator-5558": "Instance-2"}
	// 空の場合は GetDeviceIP 経由で自動解決を試みる。
	SerialToLabel map[string]string `json:"serial_to_label"`

	// SerialToUID は Probe-and-Match で自動確立した ADB シリアル→userUID のバインドマップ。
	// 起動時に読み込まれ、バインド成立のたびに書き戻される。手動編集も可。
	SerialToUID map[string]uint64 `json:"serial_to_uid,omitempty"`

	// GamePackageName はゲームのAndroidパッケージ名（空=クラッシュ検知・復帰無効）。
	// adb shell pidof <package> でプロセス生死を確認する。
	GamePackageName string `json:"game_package_name"`
	// GameLaunchActivity はゲーム起動Activity（省略時は monkey -p を使用）。
	// 例: "com.example.game/.MainActivity"
	GameLaunchActivity string `json:"game_launch_activity"`
	// CrashRecoveryEnabled はタイムアウト時のゲームプロセス確認と自動復帰を有効にする。
	CrashRecoveryEnabled bool `json:"crash_recovery_enabled"`
	// CrashRecoveryDelaySecs は復帰コマンド後のゲーム起動待機秒数（デフォルト: 30）。
	CrashRecoveryDelaySecs float64 `json:"crash_recovery_delay_secs"`

	// SceneMapIds はシーン名（中国語）→ mapID のマッピング。
	// locations.json の mapId と対応させる。空の場合はデフォルト値を使用。
	SceneMapIds map[string]uint32 `json:"scene_map_ids"`

	// --- GAS 自動チャンネル更新（Chrome拡張連携） ---

	// GASTargetEnemy はChrome拡張から受信するチャンネルのフィルタ対象エネミー名。
	// 部分一致で判定。例: "金ウリボ" or "金ナッポ"。デフォルト: "金ウリボ"
	GASTargetEnemy string `json:"gas_target_enemy"`

	// ShowNoDeviceDialog は起動時にADBデバイスが見つからない場合にダイアログを表示するかどうか。
	// デフォルト: true
	ShowNoDeviceDialog bool `json:"show_no_device_dialog"`

	// DebugVerbose は詳細デバッグログを有効にする（デフォルト: false）。
	// true にすると [DBG][...] プレフィックスのログが出力される。
	DebugVerbose bool `json:"debug_verbose"`
}

func defaultConfig() *Config {
	return &Config{
		NotifyEnemies: []EnemyNotifyConfig{
			{Name: "ウリボ・ゴールド", Enabled: true},
			{Name: "金ナッポ", Enabled: true},
			{Name: "銀ナッポ", Enabled: true},
		},
		AutoCheck:                3,
		DebounceSeconds:          30,
		Locations:                "data/locations.json",
		GUIPort:                  8080,
		ADBPath:                  "adb",
		MumuTapX:                 975,
		MumuTapY:                 664,
		MumuClearLength:          3,
		MumuPreKeycode:           "KEYCODE_P",
		MumuDelayMs:              1200,
		ParallelLimit:            0,
		ParallelGroupDelaySecs:   0,
		PatrolChannelsFile:       "config/channels.txt",
		PortMapFile:              "config/port_ch_map.json",
		PatrolDwellSecs:          10,
		PatrolMoveTimeoutSecs:    30,
		PatrolMergeTimeoutSecs:   15,
		ActiveDeviceCount:        0,
		FullThreshold:            0.0, // 0=従来通り全台
		ConsecutiveFullThreshold:    3,    // 3連続満員でクラッシュ判定
		PatrolAdaptiveTimeout:       true,
		PatrolAdaptiveTimeoutWindow: 10,
		CrashRecoveryDelaySecs:      30,
		FilterFile:               "config/filter.json",
		SceneMapIds: map[string]uint32{
			"阿斯特里亚平原": 7, // アステリア平原
		},
		GASTargetEnemy:     "金ウリボ",
		ShowNoDeviceDialog: true,
	}
}

func applyDefaults(cfg *Config) {
	if cfg.AutoCheck <= 0 {
		cfg.AutoCheck = 3
	}
	if cfg.DebounceSeconds <= 0 {
		cfg.DebounceSeconds = 30
	}
	if cfg.Locations == "" {
		cfg.Locations = "data/locations.json"
	}
	if cfg.GUIPort == 0 {
		cfg.GUIPort = 8080
	}
	if cfg.ADBPath == "" {
		cfg.ADBPath = "adb"
	}
	if cfg.MumuTapX == 0 {
		cfg.MumuTapX = 975
	}
	if cfg.MumuTapY == 0 {
		cfg.MumuTapY = 664
	}
	if cfg.MumuClearLength == 0 {
		cfg.MumuClearLength = 3
	}
	if cfg.MumuPreKeycode == "" {
		cfg.MumuPreKeycode = "KEYCODE_P"
	}
	if cfg.MumuDelayMs == 0 {
		cfg.MumuDelayMs = 1200
	}
	if cfg.FilterFile == "" {
		cfg.FilterFile = "config/filter.json"
	}
	if len(cfg.SceneMapIds) == 0 {
		cfg.SceneMapIds = map[string]uint32{
			"阿斯特里亚平原": 7,
		}
	}
	if cfg.GASTargetEnemy == "" {
		cfg.GASTargetEnemy = "金ウリボ"
	}
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

// Load reads config.json at path. A missing file yields defaults without error.
func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}
	applyDefaults(cfg)
	fc, err := LoadFilter(cfg.FilterFile)
	if err != nil {
		return nil, err
	}
	// filter.json を常に優先し、config.json 側の旧フィルター値は使わない。
	cfg.ChatExclude = fc.ChatExclude
	cfg.ChatReportSenders = fc.ChatReportSenders
	cfg.ChatReportExcludedSenders = fc.ChatReportExcludedSenders
	cfg.ChatReportLocationRules = fc.ChatReportLocationRules
	cfg.ChatReportMonsterAliasRules = fc.ChatReportMonsterAliasRules
	cfg.ChatReportMinLength = fc.ChatReportMinLength
	cfg.ChatReportMaxLength = fc.ChatReportMaxLength
	return cfg, nil
}

// Save はフィルター設定を FilterFile（デフォルト filter.json）に書き出し、
// config.json にはフィルターフィールドを含めずに保存する。
func Save(path string, cfg *Config) error {
	filterPath := cfg.FilterFile
	if filterPath == "" {
		filterPath = "config/filter.json"
	}
	// フィルター設定を filter.json に保存
	fc := &FilterConfig{
		ChatExclude:                 cfg.ChatExclude,
		ChatReportSenders:           cfg.ChatReportSenders,
		ChatReportExcludedSenders:   cfg.ChatReportExcludedSenders,
		ChatReportLocationRules:     cfg.ChatReportLocationRules,
		ChatReportMonsterAliasRules: cfg.ChatReportMonsterAliasRules,
		ChatReportMinLength:         cfg.ChatReportMinLength,
		ChatReportMaxLength:         cfg.ChatReportMaxLength,
	}
	if err := SaveFilter(filterPath, fc); err != nil {
		return err
	}
	// config.json にはフィルターフィールドを含めずに保存（omitempty で nil なら出力されない）
	cfgCopy := *cfg
	cfgCopy.ChatExclude = nil
	cfgCopy.ChatReportSenders = nil
	cfgCopy.ChatReportExcludedSenders = nil
	cfgCopy.ChatReportLocationRules = nil
	cfgCopy.ChatReportMonsterAliasRules = nil
	cfgCopy.ChatReportMinLength = 0
	cfgCopy.ChatReportMaxLength = 0
	data, err := json.MarshalIndent(cfgCopy, "", "  ")
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// LoadWindowState reads only window_state from config.json.
func LoadWindowState(path string) (*WindowState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return cfg.WindowState, nil
}

// SaveWindowState updates only window_state in config.json without touching filter.json.
func SaveWindowState(path string, ws *WindowState) error {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg = *defaultConfig()
		} else {
			return err
		}
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}
	}
	cfg.WindowState = ws
	cfg.ChatExclude = nil
	cfg.ChatReportSenders = nil
	cfg.ChatReportExcludedSenders = nil
	cfg.ChatReportLocationRules = nil
	cfg.ChatReportMonsterAliasRules = nil
	cfg.ChatReportMinLength = 0
	cfg.ChatReportMaxLength = 0
	data, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
