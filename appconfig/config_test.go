package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// configPathInTempDir はテスト隔離用に t.TempDir/config.json のパスを返す。
// filter.json も同階層に置かれる前提（FilterFile デフォルト "config/filter.json" を
// テスト用に上書きする）。
func configPathInTempDir(t *testing.T) (cfgPath, filterPath string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "config.json"), filepath.Join(dir, "filter.json")
}

// TestDefaultConfig_DefaultTrueBoolsAreTrue は §4-3 #3 の罠を直接検査する。
// defaultConfig() が GASEnable / PatrolAdaptiveTimeout / ShowNoDeviceDialog を
// true で返さなくなったら、No.31 系のリグレッションが即発生する。
func TestDefaultConfig_DefaultTrueBoolsAreTrue(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.GASEnable {
		t.Error("GASEnable: defaultConfig() must return true (CLAUDE.md §4-3 #3)")
	}
	if !cfg.PatrolAdaptiveTimeout {
		t.Error("PatrolAdaptiveTimeout: defaultConfig() must return true (CLAUDE.md §4-3 #3)")
	}
	if !cfg.ShowNoDeviceDialog {
		t.Error("ShowNoDeviceDialog: defaultConfig() must return true (CLAUDE.md §4-3 #3)")
	}
}

// TestLoad_MissingFileReturnsDefaults はファイル未存在時にデフォルトが返ることを確認。
func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.GASEnable {
		t.Error("missing file: GASEnable expected true (default)")
	}
	if cfg.GUIPort != 8080 {
		t.Errorf("missing file: GUIPort expected 8080, got %d", cfg.GUIPort)
	}
	if cfg.PatrolDwellSecs != 10 {
		t.Errorf("missing file: PatrolDwellSecs expected 10, got %v", cfg.PatrolDwellSecs)
	}
}

// TestLoad_LegacyConfigMissingBoolKeysKeepsTrueDefaults は
// 旧 config.json（gas_enable 等のキー欠損）を読み込んでもデフォルト true が
// 維持されることを確認する（json.Unmarshal が欠損キーを上書きしない動作の検証）。
func TestLoad_LegacyConfigMissingBoolKeysKeepsTrueDefaults(t *testing.T) {
	cfgPath, filterPath := configPathInTempDir(t)
	_ = filterPath
	// 旧形式: bool キーを含まない minimal JSON
	legacy := `{"discord_webhook":"https://example.com","gui_port":9000}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GUIPort != 9000 {
		t.Errorf("explicit GUIPort lost: got %d, want 9000", cfg.GUIPort)
	}
	if !cfg.GASEnable {
		t.Error("legacy config missing gas_enable: expected true (default preserved)")
	}
	if !cfg.PatrolAdaptiveTimeout {
		t.Error("legacy config missing patrol_adaptive_timeout: expected true")
	}
	if !cfg.ShowNoDeviceDialog {
		t.Error("legacy config missing show_no_device_dialog: expected true")
	}
}

// TestLoad_ExplicitFalseBoolsArePreserved はユーザーが明示的に false に
// した bool が Load で勝手に true に戻されないことを確認する。
func TestLoad_ExplicitFalseBoolsArePreserved(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	explicit := `{"gas_enable":false,"patrol_adaptive_timeout":false,"show_no_device_dialog":false}`
	if err := os.WriteFile(cfgPath, []byte(explicit), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GASEnable {
		t.Error("explicit gas_enable=false was overwritten")
	}
	if cfg.PatrolAdaptiveTimeout {
		t.Error("explicit patrol_adaptive_timeout=false was overwritten")
	}
	if cfg.ShowNoDeviceDialog {
		t.Error("explicit show_no_device_dialog=false was overwritten")
	}
}

// TestSaveLoadRoundTrip は Save → Load の値保持を確認。
func TestSaveLoadRoundTrip(t *testing.T) {
	cfgPath, filterPath := configPathInTempDir(t)
	orig := defaultConfig()
	orig.FilterFile = filterPath
	orig.DiscordWebhook = "https://example.com/webhook"
	orig.GUIPort = 9999
	orig.PatrolDwellSecs = 7.5
	orig.SerialToUID = map[string]uint64{"emu-1": 12345}
	orig.ExcludeUIDs = []uint64{999, 1000}

	if err := Save(cfgPath, orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.DiscordWebhook != orig.DiscordWebhook {
		t.Errorf("DiscordWebhook: got %q want %q", loaded.DiscordWebhook, orig.DiscordWebhook)
	}
	if loaded.GUIPort != orig.GUIPort {
		t.Errorf("GUIPort: got %d want %d", loaded.GUIPort, orig.GUIPort)
	}
	if loaded.PatrolDwellSecs != orig.PatrolDwellSecs {
		t.Errorf("PatrolDwellSecs: got %v want %v", loaded.PatrolDwellSecs, orig.PatrolDwellSecs)
	}
	if loaded.SerialToUID["emu-1"] != 12345 {
		t.Errorf("SerialToUID: got %v", loaded.SerialToUID)
	}
	if len(loaded.ExcludeUIDs) != 2 || loaded.ExcludeUIDs[0] != 999 {
		t.Errorf("ExcludeUIDs: got %v", loaded.ExcludeUIDs)
	}
	// デフォルト true bool もラウンドトリップで維持される
	if !loaded.GASEnable {
		t.Error("GASEnable lost in round-trip")
	}
}

// TestSaveWindowState_PreservesDefaultBoolsOnLegacyConfig は §4-3 #2 の
// 核心ケース。旧 config.json (bool キー欠損) に対して SaveWindowState を
// 実行しても、デフォルト true bool が暗黙的に false に上書きされないことを
// 検証する。No.31 が再発するとここで落ちる。
func TestSaveWindowState_PreservesDefaultBoolsOnLegacyConfig(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	// 旧形式: bool キーをまったく持たない config.json
	legacy := `{"discord_webhook":"https://example.com","gui_port":9000}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// SaveWindowState を実行（ウィンドウ移動を模擬）
	ws := &WindowState{X: 100, Y: 200, Width: 800, Height: 600}
	if err := SaveWindowState(cfgPath, ws); err != nil {
		t.Fatalf("SaveWindowState: %v", err)
	}

	// 結果を生 JSON として再読込し、bool キーが「無いまま」か
	// 「true で書き込まれている」ことを確認（false で上書きされていたら NG）
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	for _, key := range []string{"gas_enable", "patrol_adaptive_timeout", "show_no_device_dialog"} {
		if v, ok := raw[key]; ok {
			// 旧 config に無かったキーが SaveWindowState で追加されたら、
			// その値は false であってはならない
			if string(v) == "false" {
				t.Errorf("No.31 regression: SaveWindowState wrote %q=false to legacy config (must stay absent or be true)", key)
			}
		}
	}

	// 既存キーが保持されているか
	if string(raw["discord_webhook"]) != `"https://example.com"` {
		t.Errorf("existing field discord_webhook lost: %s", string(raw["discord_webhook"]))
	}
	if string(raw["gui_port"]) != `9000` {
		t.Errorf("existing field gui_port lost: %s", string(raw["gui_port"]))
	}

	// 改めて Load した結果、デフォルト true bool が維持されている
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("post-save Load: %v", err)
	}
	if !cfg.GASEnable {
		t.Error("No.31 regression: GASEnable became false after SaveWindowState on legacy config")
	}
	if !cfg.PatrolAdaptiveTimeout {
		t.Error("No.31 regression: PatrolAdaptiveTimeout became false")
	}
	if !cfg.ShowNoDeviceDialog {
		t.Error("No.31 regression: ShowNoDeviceDialog became false")
	}

	// window_state は正しく書き込まれている
	if cfg.WindowState == nil {
		t.Fatal("WindowState was not saved")
	}
	if cfg.WindowState.X != 100 || cfg.WindowState.Y != 200 ||
		cfg.WindowState.Width != 800 || cfg.WindowState.Height != 600 {
		t.Errorf("WindowState mismatch: %+v", cfg.WindowState)
	}
}

// TestSaveWindowState_PreservesExplicitFalseBools は、ユーザーが明示的に
// false 設定した bool が SaveWindowState 経由でも保持されることを確認する。
func TestSaveWindowState_PreservesExplicitFalseBools(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	explicit := `{"gas_enable":false,"patrol_adaptive_timeout":false,"discord_webhook":"x"}`
	if err := os.WriteFile(cfgPath, []byte(explicit), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ws := &WindowState{X: 1, Y: 2, Width: 3, Height: 4}
	if err := SaveWindowState(cfgPath, ws); err != nil {
		t.Fatalf("SaveWindowState: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GASEnable {
		t.Error("explicit gas_enable=false was flipped to true by SaveWindowState")
	}
	if cfg.PatrolAdaptiveTimeout {
		t.Error("explicit patrol_adaptive_timeout=false was flipped to true")
	}
}

// TestSaveWindowState_NewFileUsesDefaults は新規ファイル時の挙動を確認。
// 既存ファイルが無い場合は defaultConfig() からフル生成される（既存ロジック）。
func TestSaveWindowState_NewFileUsesDefaults(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	// 既存ファイルなし
	ws := &WindowState{X: 10, Y: 20, Width: 800, Height: 600}
	if err := SaveWindowState(cfgPath, ws); err != nil {
		t.Fatalf("SaveWindowState: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.GASEnable {
		t.Error("new file: GASEnable expected true (default)")
	}
	if cfg.GUIPort != 8080 {
		t.Errorf("new file: GUIPort expected default 8080, got %d", cfg.GUIPort)
	}
	if cfg.WindowState == nil || cfg.WindowState.X != 10 {
		t.Errorf("new file: WindowState mismatch: %+v", cfg.WindowState)
	}
}

// TestLoadWindowState_MissingFileReturnsNil は WindowState の読み込み単独テスト。
func TestLoadWindowState_MissingFileReturnsNil(t *testing.T) {
	cfgPath, _ := configPathInTempDir(t)
	ws, err := LoadWindowState(cfgPath)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ws != nil {
		t.Errorf("missing file: WindowState expected nil, got %+v", ws)
	}
}
