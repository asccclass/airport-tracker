// tdx/client.go 是交通部 TDX 運輸資料流通服務（Transport Data eXchange）的客戶端，
// 用來取代/替換桃園機場官方 CSV，作為航班起降 (FIDS) 資料源。
//
// 跟 CSV 那個資料源比，TDX 的優勢：
//   - 有「實際起降時間」欄位（ActualDepartureTime/ActualArrivalTime），不是只有表訂/預計時間
//   - 同時涵蓋客機與貨機（IsCargo 欄位可以分辨）
//   - 官方文件明講桃機資料涵蓋前一日、當日、次日
//
// 認證方式：OAuth2 Client Credentials Flow。用 Client Id + Client Secret 換一個
// Access Token（有效期約 1 天），之後呼叫資料 API 時帶在 Authorization header。
//
// 速率限制：使用者的方案是 5 次/分/金鑰，這是 TDX 平台端真的會擋的硬限制，不是我們自己
// 客氣禮讓而已——所以這裡實作了一個簡單的滑動窗口限制器，把「換 token」跟「拉資料」的呼叫
// 全部算在同一個配額裡，寧可讓呼叫端等待，也不要真的超過額度被 TDX 擋掉。
//
// API 規格參考官方 Swagger 文件（公共運輸-航空 API，operationId AirApi_FIDS_2015_1），
// 用的是 /v2/Air/FIDS/Airport/{IATA} 這個「機場角度」端點，一次回傳該機場的出境+入境資料，
// 比分別呼叫 Departure/Arrival 兩個端點省一半的呼叫次數，在嚴格的速率限制下特別重要。
package tdx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	defaultAuthURL = "https://tdx.transportdata.tw/auth/realms/TDXConnect/protocol/openid-connect/token"
	defaultAPIBase = "https://tdx.transportdata.tw/api/basic"
)

// FlightRecord 刻意跟 main 套件 fids.go 裡的 FlightRecord 欄位一致，
// 這樣不用改前端、也不用改 SSE 端點，兩種 FIDS 資料源可以無縫互換。
type FlightRecord struct {
	Terminal  string `json:"terminal"`
	FlightNo  string `json:"flight_no"`
	AirlineZh string `json:"airline_zh"`
	Gate      string `json:"gate"`
	Baggage   string `json:"baggage"`
	SchedTime string `json:"sched_time"`
	EstTime   string `json:"est_time"`
	PlaceZh   string `json:"place_zh"`
	StatusZh  string `json:"status_zh"`
}

type FidsSnapshot struct {
	FetchedAt         time.Time      `json:"fetched_at"`
	Departures        []FlightRecord `json:"departures"`
	Arrivals          []FlightRecord `json:"arrivals"`
	UnclassifiedCount int            `json:"unclassified_count"`
}

type Config struct {
	ClientID     string
	ClientSecret string
	AirportIATA  string // 例如 "TPE"
	Debug        bool
	AuthURL      string // 留空則用官方正式環境網址，測試時可覆寫指向本機模擬伺服器
	APIBase      string // 同上
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	limiter    *rateLimiter

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func New(cfg Config, maxPerMinute int) *Client {
	if cfg.AuthURL == "" {
		cfg.AuthURL = defaultAuthURL
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		limiter:    newRateLimiter(maxPerMinute),
	}
}

// ---------- 速率限制：滑動窗口，硬限制每分鐘最多 N 次呼叫 ----------

type rateLimiter struct {
	mu    sync.Mutex
	max   int
	calls []time.Time
}

func newRateLimiter(max int) *rateLimiter {
	return &rateLimiter{max: max}
}

// wait 會擋住呼叫端，直到「這次呼叫不會讓過去 60 秒內的呼叫次數超過上限」為止。
// 用真的等待而不是直接拒絕，因為呼叫方是背景輪詢迴圈，等一下再打沒有壞處，
// 但真的超過 TDX 的配額會被伺服器端直接拒絕，反而更麻煩。
func (r *rateLimiter) wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-time.Minute)
		kept := r.calls[:0]
		for _, t := range r.calls {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		r.calls = kept

		if len(r.calls) < r.max {
			r.calls = append(r.calls, now)
			r.mu.Unlock()
			return nil
		}

		// 算出最早那筆呼叫還要多久才會滿一分鐘，等到那個時間點再重試。
		oldest := r.calls[0]
		wait := oldest.Add(time.Minute).Sub(now) + 50*time.Millisecond
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// ---------- OAuth2 Client Credentials ----------

func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// 留 5 分鐘緩衝，避免請求送到一半 token 剛好過期。
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-5*time.Minute)) {
		return c.accessToken, nil
	}

	if err := c.limiter.wait(ctx); err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.AuthURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("換取 access token 失敗: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TDX 驗證失敗（狀態碼 %d，請確認 Client Id / Client Secret 是否正確且完整）: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("解析 token 回應失敗: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("TDX 回應沒有 access_token: %s", string(body))
	}

	c.accessToken = tokenResp.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if c.cfg.Debug {
		log.Printf("[tdx-debug] 已取得新的 access token，%d 秒後過期", tokenResp.ExpiresIn)
	}
	return c.accessToken, nil
}

// ---------- FIDS 資料抓取 ----------

// tdxAirportFIDS 對應官方 Swagger 的 Airport_FIDS schema。
type tdxAirportFIDS struct {
	AirportID     string           `json:"AirportID"`
	FIDSDeparture []tdxDeparture   `json:"FIDSDeparture"`
	FIDSArrival   []tdxArrival     `json:"FIDSArrival"`
	UpdateTime    string           `json:"UpdateTime"`
}

type tdxDeparture struct {
	FlightNumber           string `json:"FlightNumber"`
	AirlineID              string `json:"AirlineID"`
	ArrivalAirportID       string `json:"ArrivalAirportID"`
	ScheduleDepartureTime  string `json:"ScheduleDepartureTime"`
	ActualDepartureTime    string `json:"ActualDepartureTime"`
	EstimatedDepartureTime string `json:"EstimatedDepartureTime"`
	DepartureRemark        string `json:"DepartureRemark"`
	Terminal               string `json:"Terminal"`
	Gate                   string `json:"Gate"`
	IsCargo                bool   `json:"IsCargo"`
	CheckCounter           string `json:"CheckCounter"`
}

type tdxArrival struct {
	FlightNumber         string `json:"FlightNumber"`
	AirlineID            string `json:"AirlineID"`
	DepartureAirportID   string `json:"DepartureAirportID"`
	ScheduleArrivalTime  string `json:"ScheduleArrivalTime"`
	ActualArrivalTime    string `json:"ActualArrivalTime"`
	EstimatedArrivalTime string `json:"EstimatedArrivalTime"`
	ArrivalRemark        string `json:"ArrivalRemark"`
	Terminal              string `json:"Terminal"`
	Gate                  string `json:"Gate"`
	IsCargo               bool   `json:"IsCargo"`
	BaggageClaim          string `json:"BaggageClaim"`
}

// Fetch 拉取指定機場的即時航班資料（客機+貨機），轉成我們自己的格式。
// 這個方法內部會先確保有效 token、再過速率限制器，兩者共用同一個每分鐘配額。
func (c *Client) Fetch(ctx context.Context) (*FidsSnapshot, error) {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}

	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/v2/Air/FIDS/Airport/%s?%s",
		c.cfg.APIBase, url.PathEscape(c.cfg.AirportIATA), url.Values{"$format": {"JSON"}}.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("呼叫 TDX FIDS API 失敗: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TDX FIDS API 回應狀態碼 %d: %s", resp.StatusCode, string(body))
	}

	if c.cfg.Debug {
		preview := body
		if len(preview) > 500 {
			preview = preview[:500]
		}
		log.Printf("[tdx-debug] 原始回應前 500 bytes: %s", preview)
	}

	var results []tdxAirportFIDS
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("解析 TDX 回應失敗: %w", err)
	}
	if len(results) == 0 {
		return &FidsSnapshot{FetchedAt: time.Now().UTC()}, nil
	}

	data := results[0]
	snapshot := &FidsSnapshot{FetchedAt: time.Now().UTC()}

	for _, d := range data.FIDSDeparture {
		if d.FlightNumber == "" {
			continue
		}
		snapshot.Departures = append(snapshot.Departures, FlightRecord{
			Terminal:  d.Terminal,
			FlightNo:  d.AirlineID + d.FlightNumber,
			AirlineZh: d.AirlineID, // TDX 這個端點沒有直接給航空公司中文名稱，先用代碼；
			                        // 要中文名稱需要另外呼叫 /v2/Air/Airline 拿對照表，之後可以加。
			Gate:      d.Gate,
			SchedTime: isoToClock(d.ScheduleDepartureTime),
			EstTime:   firstNonEmpty(isoToClock(d.ActualDepartureTime), isoToClock(d.EstimatedDepartureTime)),
			PlaceZh:   d.ArrivalAirportID,
			StatusZh:  d.DepartureRemark,
		})
	}

	for _, a := range data.FIDSArrival {
		if a.FlightNumber == "" {
			continue
		}
		snapshot.Arrivals = append(snapshot.Arrivals, FlightRecord{
			Terminal:  a.Terminal,
			FlightNo:  a.AirlineID + a.FlightNumber,
			AirlineZh: a.AirlineID,
			Baggage:   a.BaggageClaim,
			SchedTime: isoToClock(a.ScheduleArrivalTime),
			EstTime:   firstNonEmpty(isoToClock(a.ActualArrivalTime), isoToClock(a.EstimatedArrivalTime)),
			PlaceZh:   a.DepartureAirportID,
			StatusZh:  a.ArrivalRemark,
		})
	}

	return snapshot, nil
}

// isoToClock 把 TDX 的 ISO8601 時間字串（yyyy-MM-ddTHH:mm）裁成 "HH:MM" 給前端顯示用。
// 解析不出來就原樣回傳，避免因為格式意外而整批資料掛掉。
func isoToClock(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := time.Parse("2006-01-02T15:04:05", iso)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04", iso)
		if err != nil {
			return iso
		}
	}
	return t.Format("15:04")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
