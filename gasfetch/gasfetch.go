package gasfetch

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"
)

// Entry は1チャンネルの討伐記録
type Entry struct {
	Channel  uint32
	Elapsed  time.Duration // 討伐からの実際の経過時間
	KilledAt time.Time     // 推定討伐時刻 (= now - Elapsed)
	Exceeded bool          // 28h超過フラグ
}

// gasAPIResponse は GAS の JSON API レスポンス
type gasAPIResponse struct {
	Channels []struct {
		Channel    uint32 `json:"channel"`
		ElapsedSec int64  `json:"elapsed_sec"`
	} `json:"channels"`
	ServerTime int64 `json:"server_time"`
}

// Fetch は GAS の JSON API から討伐情報を取得する。
// config の gas_url に ?action=api を自動付与してアクセスする。
func Fetch(url string) ([]Entry, error) {
	apiURL := url
	sep := "?"
	for _, c := range url {
		if c == '?' {
			sep = "&"
			break
		}
	}
	apiURL = url + sep + "action=api"

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var apiResp gasAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("JSON parse error: %w (preview: %s)", err, preview)
	}

	now := time.Now()
	var entries []Entry
	for _, ch := range apiResp.Channels {
		if ch.Channel == 0 {
			continue
		}
		elapsed := time.Duration(ch.ElapsedSec) * time.Second
		entries = append(entries, Entry{
			Channel:  ch.Channel,
			Elapsed:  elapsed,
			KilledAt: now.Add(-elapsed),
			Exceeded: elapsed >= 28*time.Hour,
		})
	}
	log.Printf("[GASFetch] 取得: %d件", len(entries))
	return entries, nil
}

// FilterChannels は閾値以上経過したチャンネル番号リストを返す。
func FilterChannels(entries []Entry, thresholdHours float64) []uint32 {
	threshold := time.Duration(thresholdHours * float64(time.Hour))
	var chs []uint32
	for _, e := range entries {
		if e.Elapsed >= threshold {
			chs = append(chs, e.Channel)
		}
	}
	sort.Slice(chs, func(i, j int) bool { return chs[i] < chs[j] })
	return chs
}
