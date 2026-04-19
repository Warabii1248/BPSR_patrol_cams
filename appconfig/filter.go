package appconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FilterConfig はチャット候補フィルター設定（filter.json から読み書きされる）
type FilterConfig struct {
	// ChatExclude はワールドチャット検知を抑制するキーワード一覧。
	ChatExclude []string `json:"chat_exclude"`
	// ChatReportSenders は発見報告候補として扱う発言者フィルター。
	ChatReportSenders []string `json:"chat_report_senders"`
	// ChatReportExcludedSenders は候補から除外する発言者フィルター。
	ChatReportExcludedSenders []string `json:"chat_report_excluded_senders"`
	// ChatReportLocationRules は地点別名と出現候補モンスターの追加ルール。
	// 形式: "地点名|別名1,別名2,...|モンスター名1,モンスター名2,..."
	ChatReportLocationRules []string `json:"chat_report_location_rules"`
	// ChatReportMonsterAliasRules はモンスター別名の追加ルール。
	// 形式: "モンスター名|別名1,別名2,..."
	ChatReportMonsterAliasRules []string `json:"chat_report_monster_alias_rules"`
	// ChatReportMinLength は報告候補の最小文字数。0でデフォルト(4)。
	ChatReportMinLength int `json:"chat_report_min_length,omitempty"`
	// ChatReportMaxLength は報告候補の最大文字数。0でデフォルト(80)。
	ChatReportMaxLength int `json:"chat_report_max_length,omitempty"`
}

// LoadFilter は path から FilterConfig を読み込む。
// ファイルが存在しない場合はエラーなしで空の FilterConfig を返す。
func LoadFilter(path string) (*FilterConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &FilterConfig{}, nil
		}
		return nil, err
	}
	fc := &FilterConfig{}
	if err := json.Unmarshal(data, fc); err != nil {
		return nil, err
	}
	return fc, nil
}

// SaveFilter は fc をインデント付き JSON として path に書き込む。
func SaveFilter(path string, fc *FilterConfig) error {
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0644)
}
