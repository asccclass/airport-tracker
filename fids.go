// fids.go 負責向桃園機場官方開放資料（政府資料開放平臺）
// 抓取即時航班起降時刻表 (FIDS) 的 CSV，解析成結構化資料。
//
// 資料來源說明（data.gov.tw dataset 26194，2023081816）：
//   更新頻率：每 5 分鐘
//   編碼：UTF-8（無 BOM），CRLF 換行，每個欄位用雙引號包起來（RFC 4180 標準格式）
//   欄位（已用官方實際輸出驗證過，順序如下，但程式仍用欄位名稱動態對應以防未來調整）：
//   航廈、種類、航空公司代碼、航空公司中文、班次、機門、
//   表訂日期、表訂時間、預計日期、預計時間、
//   往來地點、往來地點英文、往來地點中文、航班狀態、機型、
//   其他航點、行李轉盤、報到櫃台、航班動態中文、航班動態英文
//
// 已用真實資料樣本驗證確認：
//   - 種類：A＝入境 (arrival)、D＝出境 (departure)
//   - 航班狀態：常見值有「已到」「出發」「取消」等
//   - 表訂時間/預計時間格式為 "HH:MM:SS"（含秒），顯示時會裁成 "HH:MM"
//   - 班次欄位常帶前導空白（例如 " 310"），會自動清除
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// FlightRecord 是整理過後、要送給前端的單筆航班資料。
type FlightRecord struct {
	Terminal   string `json:"terminal"`
	FlightNo   string `json:"flight_no"`
	AirlineZh  string `json:"airline_zh"`
	Gate       string `json:"gate"` // 出境用登機門，入境會是空的
	Baggage    string `json:"baggage"` // 入境用行李轉盤，出境會是空的
	SchedTime  string `json:"sched_time"`
	EstTime    string `json:"est_time"`
	PlaceZh    string `json:"place_zh"` // 出境=目的地，入境=起飛地
	StatusZh   string `json:"status_zh"`
}

// FidsSnapshot 是某一次抓取後、要推送給前端的完整資料包。
type FidsSnapshot struct {
	FetchedAt  time.Time      `json:"fetched_at"`
	Departures []FlightRecord `json:"departures"`
	Arrivals   []FlightRecord `json:"arrivals"`
	// Unclassified 統計有多少筆資料因為「種類」欄位辨識不出來而被跳過，
	// 用來在前端/log 提示「這裡可能需要校正」，而不是默默漏資料。
	UnclassifiedCount int `json:"unclassified_count"`
}

type fidsClient struct {
	httpClient *http.Client
	csvURL     string
	debug      bool
}

// 官方文件列出的欄位名稱 -> 我們內部想要的欄位。
// 用名稱比對而不是寫死欄位順序，這樣即使官方調整欄位順序也不會整支程式壞掉。
var fidsHeaderAliases = map[string][]string{
	"terminal":    {"航廈"},
	"direction":   {"種類"},
	"airline_zh":  {"航空公司中文"},
	"flight_no":   {"班次"},
	"gate":        {"機門"},
	"sched_date":  {"表訂日期"},
	"sched_time":  {"表訂時間"},
	"est_date":    {"預計日期"},
	"est_time":    {"預計時間"},
	"place_zh":    {"往來地點中文", "往來地點"},
	"status_zh":   {"航班狀態", "航班動態中文"},
	"baggage":     {"行李轉盤"},
}

func (c *fidsClient) fetch() (*FidsSnapshot, error) {
	req, err := http.NewRequest(http.MethodGet, c.csvURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "airport-tracker-mvp/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fids 回應狀態碼 %d", resp.StatusCode)
	}

	// 去掉 UTF-8 BOM（政府資料常見於 Excel 匯出的 CSV 開頭）
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	if c.debug {
		preview := body
		if len(preview) > 300 {
			preview = preview[:300]
		}
		log.Printf("[fids-debug] 原始資料前 300 bytes: %q", preview)
	}

	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1 // 對欄位數不一致的資料列保持寬容，不要整批失敗
	reader.LazyQuotes = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析 CSV 失敗: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("CSV 是空的")
	}

	colIdx, missing := buildColumnIndex(records[0])
	if c.debug {
		log.Printf("[fids-debug] 偵測到的表頭: %v", records[0])
		log.Printf("[fids-debug] 欄位對應結果: %v", colIdx)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("CSV 表頭缺少預期欄位 %v，實際表頭是 %v；請確認官方格式是否變動", missing, records[0])
	}

	get := func(row []string, key string) string {
		idx, ok := colIdx[key]
		if !ok || idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}

	snapshot := &FidsSnapshot{FetchedAt: time.Now().UTC()}

	for _, row := range records[1:] {
		if len(row) == 0 {
			continue
		}
		rec := FlightRecord{
			Terminal:  get(row, "terminal"),
			FlightNo:  get(row, "flight_no"),
			AirlineZh: get(row, "airline_zh"),
			Gate:      get(row, "gate"),
			Baggage:   get(row, "baggage"),
			SchedTime: trimSeconds(get(row, "sched_time")),
			EstTime:   trimSeconds(get(row, "est_time")),
			PlaceZh:   get(row, "place_zh"),
			StatusZh:  get(row, "status_zh"),
		}
		if rec.FlightNo == "" {
			continue // 沒有班次號的資料列沒有意義，跳過
		}

		switch classifyDirection(get(row, "direction")) {
		case dirDeparture:
			snapshot.Departures = append(snapshot.Departures, rec)
		case dirArrival:
			snapshot.Arrivals = append(snapshot.Arrivals, rec)
		default:
			snapshot.UnclassifiedCount++
		}
	}

	if snapshot.UnclassifiedCount > 0 {
		log.Printf("警告：%d 筆 FIDS 資料的「種類」欄位無法辨識為出境/入境，已略過。可加上 -fids-debug 查看原始表頭與內容以校正。", snapshot.UnclassifiedCount)
	}

	return snapshot, nil
}

// buildColumnIndex 用官方文件列出的別名去比對實際表頭，回傳「內部欄位名 -> 欄位索引」，
// 以及找不到對應的內部欄位名清單。
func buildColumnIndex(header []string) (map[string]int, []string) {
	idx := make(map[string]int)
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}

	result := make(map[string]int)
	var missing []string
	for key, aliases := range fidsHeaderAliases {
		found := false
		for _, alias := range aliases {
			if i, ok := idx[alias]; ok {
				result[key] = i
				found = true
				break
			}
		}
		// gate/baggage/est_date 之類的欄位允許缺席（例如入境沒有機門），
		// 只有核心欄位缺席才視為格式跑掉。
		if !found && isCoreField(key) {
			missing = append(missing, key)
		}
	}
	return result, missing
}

func isCoreField(key string) bool {
	switch key {
	case "flight_no", "direction", "place_zh":
		return true
	default:
		return false
	}
}

// trimSeconds 把官方資料常見的 "HH:MM:SS" 格式裁成 "HH:MM"，
// 如果不是這個格式（例如已經是 HH:MM，或空字串）就原樣回傳，不強行處理。
func trimSeconds(s string) string {
	if len(s) == 8 && s[2] == ':' && s[5] == ':' {
		return s[:5]
	}
	return s
}

type direction int

const (
	dirUnknown direction = iota
	dirDeparture
	dirArrival
)

// classifyDirection 把「種類」欄位判斷成出境/入境。
// 已用官方真實資料驗證：種類欄位就是單一字母 "A"（入境 arrival）或 "D"（出境 departure），
// 大小寫、全形/半形都做了容錯；其餘中文語彙規則保留當作備援，以防未來格式調整。
func classifyDirection(raw string) direction {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return dirUnknown
	case strings.EqualFold(s, "D"):
		return dirDeparture
	case strings.EqualFold(s, "A"):
		return dirArrival
	case strings.ContainsAny(s, "出離"), strings.Contains(strings.ToUpper(s), "DEPARTURE"):
		return dirDeparture
	case strings.ContainsAny(s, "入到"), strings.Contains(strings.ToUpper(s), "ARRIVAL"):
		return dirArrival
	default:
		return dirUnknown
	}
}

func (s *FidsSnapshot) toJSON() ([]byte, error) {
	return json.Marshal(s)
}
