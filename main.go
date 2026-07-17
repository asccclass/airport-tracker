// airport-tracker 是一個最小可行版本 (MVP)：
// 定期向 OpenSky Network 取得指定空域內的即時航班位置，
// 透過 SSE (Server-Sent Events) 推送給瀏覽器，
// 前端用 Leaflet 把飛機畫在地圖上即時移動。
//
// 只使用 Go 標準函式庫，不需要任何第三方套件，
// 編譯出來就是一個可以直接執行的單一執行檔（含前端頁面）。
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

// ---------- OpenSky 原始資料格式 ----------

// openSkyResponse 對應 https://opensky-network.org/api/states/all 的回傳格式。
// states 欄位是「陣列的陣列」，每一筆用固定順序代表不同欄位，
// 詳見 https://openskynetwork.github.io/opensky-api/rest.html
type openSkyResponse struct {
	Time   int64           `json:"time"`
	States [][]interface{} `json:"states"`
}

// Aircraft 是整理過後、要傳給前端的乾淨結構。
type Aircraft struct {
	ICAO24        string   `json:"icao24"`
	Callsign      string   `json:"callsign"`
	OriginCountry string   `json:"origin_country"`
	Longitude     *float64 `json:"longitude"`
	Latitude      *float64 `json:"latitude"`
	BaroAltitude  *float64 `json:"baro_altitude"` // 公尺
	OnGround      bool     `json:"on_ground"`
	Velocity      *float64 `json:"velocity"`   // 公尺/秒
	TrueTrack     *float64 `json:"true_track"` // 航向角度，0=北
	VerticalRate  *float64 `json:"vertical_rate"`
}

// Snapshot 是某一次輪詢後、要推送給所有前端的完整資料包。
type Snapshot struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Aircraft  []Aircraft `json:"aircraft"`
}

// ---------- 從 OpenSky 拉資料並轉換 ----------

type openSkyClient struct {
	httpClient *http.Client
	bbox       boundingBox
}

type boundingBox struct {
	LaMin, LoMin, LaMax, LoMax float64
}

func (c *openSkyClient) fetch() (*Snapshot, error) {
	url := fmt.Sprintf(
		"https://opensky-network.org/api/states/all?lamin=%f&lomin=%f&lamax=%f&lomax=%f",
		c.bbox.LaMin, c.bbox.LoMin, c.bbox.LaMax, c.bbox.LoMax,
	)

	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		return nil, fmt.Errorf("opensky 回應狀態碼 %d：%s", resp.StatusCode, string(body))
	}

	var raw openSkyResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 opensky 回應失敗: %w", err)
	}

	snapshot := &Snapshot{
		FetchedAt: time.Unix(raw.Time, 0).UTC(),
		Aircraft:  make([]Aircraft, 0, len(raw.States)),
	}

	for _, s := range raw.States {
		ac, ok := parseState(s)
		if !ok {
			continue
		}
		// 沒有座標的資料點畫不出來，直接跳過。
		if ac.Longitude == nil || ac.Latitude == nil {
			continue
		}
		snapshot.Aircraft = append(snapshot.Aircraft, ac)
	}

	return snapshot, nil
}

// parseState 把 OpenSky 那種「固定順序陣列」轉成有欄位名稱的結構。
// 順序定義：https://openskynetwork.github.io/opensky-api/rest.html#response
func parseState(s []interface{}) (Aircraft, bool) {
	if len(s) < 17 {
		return Aircraft{}, false
	}

	get := func(i int) interface{} { return s[i] }
	str := func(i int) string {
		v, _ := get(i).(string)
		return v
	}
	fptr := func(i int) *float64 {
		v, ok := get(i).(float64)
		if !ok {
			return nil
		}
		return &v
	}
	bl := func(i int) bool {
		v, _ := get(i).(bool)
		return v
	}

	return Aircraft{
		ICAO24:        str(0),
		Callsign:      trimSpace(str(1)),
		OriginCountry: str(2),
		Longitude:     fptr(5),
		Latitude:      fptr(6),
		BaroAltitude:  fptr(7),
		OnGround:      bl(8),
		Velocity:      fptr(9),
		TrueTrack:     fptr(10),
		VerticalRate:  fptr(11),
	}, true
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] == ' ' {
		start++
	}
	for end > start && s[end-1] == ' ' {
		end--
	}
	return s[start:end]
}

// ---------- SSE 廣播器：把最新資料推給所有連線中的瀏覽器 ----------

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	latest  []byte // 快取最新一次的資料，讓新連上的用戶端可以馬上拿到
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan []byte]struct{})}
}

func (b *broadcaster) subscribe() chan []byte {
	ch := make(chan []byte, 1)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	latest := b.latest
	b.mu.Unlock()

	if latest != nil {
		ch <- latest
	}
	return ch
}

func (b *broadcaster) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *broadcaster) publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.latest = data
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
			// 這個用戶端還沒消化完上一筆，就跳過這次，避免阻塞整個廣播。
		}
	}
}

// ---------- HTTP handlers ----------

func sseHandler(b *broadcaster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		// 定期送 heartbeat 避免某些反向代理把閒置連線斷掉。
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case data, open := <-ch:
				if !open {
					return
				}
				fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
				flusher.Flush()
			case <-heartbeat.C:
				fmt.Fprint(w, ": heartbeat\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

// ---------- 主程式 ----------

func main() {
	var (
		addr         = flag.String("addr", ":8080", "監聽位址")
		interval     = flag.Duration("interval", 15*time.Second, "輪詢 OpenSky 的間隔（匿名帳號有速率限制，不建議低於 10 秒）")
		laMin        = flag.Float64("lamin", 24.90, "空域範圍：最小緯度（預設涵蓋桃園機場）")
		loMin        = flag.Float64("lomin", 121.05, "空域範圍：最小經度")
		laMax        = flag.Float64("lamax", 25.20, "空域範圍：最大緯度")
		loMax        = flag.Float64("lomax", 121.40, "空域範圍：最大經度")
		fidsURL      = flag.String("fids-url", "https://odp.taoyuan-airport.com/dataset/2023081816?format=csv", "桃園機場即時航班 CSV 資料來源網址")
		fidsInterval = flag.Duration("fids-interval", 5*time.Minute, "輪詢 FIDS 起降時刻表的間隔（官方資料本身每 5 分鐘更新一次，抓太頻繁沒有意義）")
		fidsDebug    = flag.Bool("fids-debug", false, "印出 FIDS CSV 原始表頭與欄位對應結果，第一次串接時用來校正格式")
	)
	flag.Parse()

	client := &openSkyClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		bbox:       boundingBox{LaMin: *laMin, LoMin: *loMin, LaMax: *laMax, LoMax: *loMax},
	}
	bc := newBroadcaster()

	fc := &fidsClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		csvURL:     *fidsURL,
		debug:      *fidsDebug,
	}
	fidsBc := newBroadcaster()

	// 背景輪詢迴圈
	go func() {
		poll := func() {
			snapshot, err := client.fetch()
			if err != nil {
				log.Printf("拉取 OpenSky 資料失敗: %v", err)
				return
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				log.Printf("序列化資料失敗: %v", err)
				return
			}
			bc.publish(data)
			log.Printf("已更新 %d 架航班", len(snapshot.Aircraft))
		}

		poll() // 啟動時先抓一次，不用等第一個 interval
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for range ticker.C {
			poll()
		}
	}()

	// 背景輪詢迴圈：FIDS 起降時刻表
	go func() {
		poll := func() {
			snapshot, err := fc.fetch()
			if err != nil {
				log.Printf("拉取 FIDS 資料失敗: %v", err)
				return
			}
			data, err := snapshot.toJSON()
			if err != nil {
				log.Printf("序列化 FIDS 資料失敗: %v", err)
				return
			}
			fidsBc.publish(data)
			log.Printf("已更新 FIDS：出境 %d 筆、入境 %d 筆（%d 筆無法分類）",
				len(snapshot.Departures), len(snapshot.Arrivals), snapshot.UnclassifiedCount)
		}

		poll()
		ticker := time.NewTicker(*fidsInterval)
		defer ticker.Stop()
		for range ticker.C {
			poll()
		}
	}()

	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", sseHandler(bc))
	mux.HandleFunc("/api/fids-stream", sseHandler(fidsBc))
	mux.Handle("/", http.FileServer(http.FS(staticContent)))

	log.Printf("伺服器啟動於 http://localhost%s （每 %s 更新一次，範圍 lat[%.2f,%.2f] lon[%.2f,%.2f]）",
		*addr, *interval, *laMin, *laMax, *loMin, *loMax)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
