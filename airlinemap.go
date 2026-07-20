// airlinemap.go 負責兩件事：
//  1. 從 OpenFlights airlines.dat 下載並解析 ICAO↔IATA 航空公司代碼對照表
//  2. 把 ADS-B 的呼號（例如 "ANA633"）比對到 FIDS 快照裡對應的班次資料，
//     抓出目的地（出境）或來源地（入境）填回 ADS-B 的資料流，讓前端標籤能顯示真實目的地
//
// 呼號格式：ICAO 航空公司代碼（3 字元）+ 班號數字，例如 ANA633
// 班次格式：IATA 航空公司代碼（2 字元）+ 班號數字，例如 NH633
// 比對邏輯：把呼號的 ICAO 代碼查對照表換成 IATA，再跟班號數字組合成 IATA 班次，
//            然後在 FIDS 清單裡找對應的班次。

package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

const airlinesDatURL = "https://raw.githubusercontent.com/jpatokal/openflights/master/data/airlines.dat"

// icaoToIATA 是 ICAO 代碼 → IATA 代碼的對照表，例如 "ANA" → "NH"。
// 用 sync.RWMutex 保護，讓背景定期更新不影響讀取。
var (
	airlineMapMu   sync.RWMutex
	icaoToIATAMap  = map[string]string{} // ICAO → IATA
	airlineMapReady bool
)

// loadAirlineMap 下載並解析 OpenFlights airlines.dat，建立 ICAO→IATA 對照表。
// 這個函式只在啟動時呼叫一次；對照表極少變動（航空公司代碼基本上不會改），
// 失敗了也不影響程式主要功能，只是呼號比對會失效。
func loadAirlineMap(httpClient *http.Client) {
	resp, err := httpClient.Get(airlinesDatURL)
	if err != nil {
		log.Printf("[airlinemap] 下載 airlines.dat 失敗（呼號比對停用）: %v", err)
		return
	}
	defer resp.Body.Close()

	// airlines.dat 是 CSV，欄位順序：
	// 0=id, 1=name, 2=alias, 3=IATA(2碼), 4=ICAO(3碼), 5=callsign, 6=country, 7=active
	r := csv.NewReader(bufio.NewReader(resp.Body))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	newMap := map[string]string{}
	count := 0
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(row) < 5 {
			continue
		}
		iata := strings.TrimSpace(row[3])
		icao := strings.TrimSpace(row[4])
		if iata == "" || iata == `\N` || icao == "" || icao == `\N` {
			continue
		}
		newMap[icao] = iata
		count++
	}

	airlineMapMu.Lock()
	icaoToIATAMap = newMap
	airlineMapReady = true
	airlineMapMu.Unlock()
	log.Printf("[airlinemap] 已載入 %d 筆航空公司代碼對照（ICAO↔IATA）", count)
}

// callsignToIATAFlight 把 ADS-B 呼號（例如 "ANA633"）轉成 IATA 班次格式（"NH633"）。
// 回傳空字串代表轉換失敗（查不到對照或呼號格式不合預期）。
func callsignToIATAFlight(callsign string) string {
	cs := strings.TrimSpace(callsign)
	if len(cs) < 4 {
		return ""
	}

	airlineMapMu.RLock()
	ready := airlineMapReady
	airlineMapMu.RUnlock()
	if !ready {
		return ""
	}

	// 分割呼號：前幾個字母是 ICAO 代碼，後面是班號（通常純數字，但也可能帶英文字尾）
	// 策略：從頭找到第一個數字為止就是 ICAO 代碼前綴
	splitAt := -1
	for i, c := range cs {
		if unicode.IsDigit(c) {
			splitAt = i
			break
		}
	}
	if splitAt <= 0 {
		return ""
	}

	icao := cs[:splitAt]
	flightNum := cs[splitAt:]

	airlineMapMu.RLock()
	iata, ok := icaoToIATAMap[icao]
	airlineMapMu.RUnlock()

	if !ok || iata == "" {
		return ""
	}
	return normalizeFlight(iata + flightNum)
}

// FlightInfo 是比對成功後、要填回 Aircraft 的資訊。
type FlightInfo struct {
	PlaceZh   string // 目的地（出境）或來源地（入境）
	StatusZh  string // 航班狀態
	AirlineZh string // 航空公司中文名（若 FIDS 有）
}

// fidsLookupTable 是從 FIDS 快照建出來的「IATA 班次 → FlightInfo」查找表，
// 每次 FIDS 快照更新就重建一次，讓 ADS-B 呼號比對永遠用最新的 FIDS 資料。
type fidsLookupTable struct {
	mu    sync.RWMutex
	table map[string]FlightInfo // key = IATA 班次（大寫，去空白）
}

var globalFIDSLookup = &fidsLookupTable{table: map[string]FlightInfo{}}

// rebuildFromSnapshot 把 FIDS 快照裡的出境+入境清單，
// 重建成「IATA 班次 → FlightInfo」查找表。
// 出境用「目的地」、入境用「來源地」，讓前端標籤可以顯示有意義的地點。
func (t *fidsLookupTable) rebuildFromSnapshot(deps, arrs []FlightRecord) {
	newTable := make(map[string]FlightInfo, len(deps)+len(arrs))

	for _, d := range deps {
		key := normalizeFlight(d.FlightNo)
		if key == "" {
			continue
		}
		newTable[key] = FlightInfo{
			PlaceZh:   d.PlaceZh,
			StatusZh:  d.StatusZh,
			AirlineZh: d.AirlineZh,
		}
	}
	for _, a := range arrs {
		key := normalizeFlight(a.FlightNo)
		if key == "" {
			continue
		}
		// 入境若跟出境班次衝突（同班號），入境不覆蓋出境（出境資訊通常更有用）
		if _, exists := newTable[key]; !exists {
			newTable[key] = FlightInfo{
				PlaceZh:   a.PlaceZh,
				StatusZh:  a.StatusZh,
				AirlineZh: a.AirlineZh,
			}
		}
	}

	t.mu.Lock()
	t.table = newTable
	t.mu.Unlock()
}

// lookup 把呼號（ADS-B 格式）轉成 IATA 班次後，在查找表裡找對應的 FlightInfo。
func (t *fidsLookupTable) lookup(callsign string) (FlightInfo, bool) {
	iataFlight := callsignToIATAFlight(callsign)
	if iataFlight == "" {
		return FlightInfo{}, false
	}
	t.mu.RLock()
	info, ok := t.table[iataFlight]
	t.mu.RUnlock()
	return info, ok
}

// normalizeFlight 把班次字串統一格式：去除空白、轉大寫、去除航空公司代碼後的數字前導零，
// 讓 "CX001" 跟 "CX1" 能比對到同一班機（FIDS 跟 ADS-B 呼號對前導零處理方式不一致）。
func normalizeFlight(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// 找到第一個數字的位置，把數字部分的前導零去掉
	splitAt := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if splitAt <= 0 {
		return s
	}
	prefix := s[:splitAt]
	numPart := strings.TrimLeft(s[splitAt:], "0")
	if numPart == "" {
		numPart = "0"
	}
	return prefix + numPart
}

// startAirlineMapLoader 在背景載入航空公司對照表，
// 啟動時先抓一次，之後每 24 小時更新一次（航空公司代碼幾乎不變，不用抓太頻繁）。
func startAirlineMapLoader(httpClient *http.Client) {
	go func() {
		loadAirlineMap(httpClient)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			loadAirlineMap(httpClient)
		}
	}()
}

// enrichedSnapshot 是在 Snapshot 基礎上加了 FIDS 比對資訊的版本，
// 專門用來序列化給前端，讓前端標籤可以顯示 PlaceZh。
type enrichedAircraft struct {
	ICAO24        string   `json:"icao24"`
	Callsign      string   `json:"callsign"`
	OriginCountry string   `json:"origin_country"`
	Longitude     *float64 `json:"longitude"`
	Latitude      *float64 `json:"latitude"`
	BaroAltitude  *float64 `json:"baro_altitude"`
	OnGround      bool     `json:"on_ground"`
	Velocity      *float64 `json:"velocity"`
	TrueTrack     *float64 `json:"true_track"`
	VerticalRate  *float64 `json:"vertical_rate"`
	// FIDS 比對後才有的欄位；比對失敗這兩個是空字串
	PlaceZh   string `json:"place_zh"`
	AirlineZh string `json:"airline_zh"`
}

type enrichedSnapshot struct {
	FetchedAt time.Time          `json:"fetched_at"`
	Aircraft  []enrichedAircraft `json:"aircraft"`
}

// enrichSnapshot 把原本的 Snapshot 跟目前的 FIDS 查找表合併，
// 回傳序列化好的 JSON。有比對到的填入目的地，沒比對到的照舊顯示空字串（前端會 fallback 顯示原籍國）。
func enrichSnapshot(snap *Snapshot) ([]byte, error) {
	out := enrichedSnapshot{
		FetchedAt: snap.FetchedAt,
		Aircraft:  make([]enrichedAircraft, 0, len(snap.Aircraft)),
	}
	for _, ac := range snap.Aircraft {
		ea := enrichedAircraft{
			ICAO24:        ac.ICAO24,
			Callsign:      ac.Callsign,
			OriginCountry: ac.OriginCountry,
			Longitude:     ac.Longitude,
			Latitude:      ac.Latitude,
			BaroAltitude:  ac.BaroAltitude,
			OnGround:      ac.OnGround,
			Velocity:      ac.Velocity,
			TrueTrack:     ac.TrueTrack,
			VerticalRate:  ac.VerticalRate,
		}
		if info, ok := globalFIDSLookup.lookup(ac.Callsign); ok {
			ea.PlaceZh = info.PlaceZh
			ea.AirlineZh = info.AirlineZh
		}
		out.Aircraft = append(out.Aircraft, ea)
	}
	return json.Marshal(out)
}
