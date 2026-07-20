// schedule.go 實作 OpenSky 每日額度的動態排程：
// 依照 FIDS 起降資料統計「一天 24 小時、每小時的起降密度」，把每天的額度
// （預設 4000 credits）依比例分配到有航班活動的小時，完全沒有起降的小時
// 直接跳過（額度 0，那個小時不打 OpenSky）。目標是額度剛好在一天用完，
// 而不是不管有沒有班機都固定頻率打，浪費額度在離峰時段。
package main

import (
	"log"
	"sort"
	"sync"
	"time"
)

var fallbackActiveHours = []int{5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}

// hourlyBudget 是一天 24 小時、每小時分配到的請求次數。
type hourlyBudget [24]int

// openSkyCreditCost 依官方公告的分級表，算出「查詢這個範圍的 /states/all 一次要花幾個 credit」。
// 範圍越大 credit 花得越多，所以額度分配要先知道「每次請求的成本」才能換算成「總共能打幾次」。
func openSkyCreditCost(b boundingBox) int {
	area := (b.LaMax - b.LaMin) * (b.LoMax - b.LoMin)
	switch {
	case area <= 25:
		return 1
	case area <= 100:
		return 2
	case area <= 400:
		return 3
	default:
		return 4
	}
}

// hourOfRecord 從 FlightRecord 的時間欄位（優先用預計時間，沒有才用表訂時間）解析出小時（0-23）。
// 兩個欄位都解析不出來就回傳 -1，呼叫端要跳過這筆，不要誤植到某個小時。
func hourOfRecord(rec FlightRecord) int {
	for _, s := range []string{rec.EstTime, rec.SchedTime} {
		if len(s) >= 2 {
			h := 0
			ok := true
			for _, c := range s[:2] {
				if c < '0' || c > '9' {
					ok = false
					break
				}
				h = h*10 + int(c-'0')
			}
			if ok && h >= 0 && h <= 23 {
				return h
			}
		}
	}
	return -1
}

// computeHourlyActivity 把離境+入境的完整清單（未經時間過濾的那份）依小時分桶計數，
// 代表「這個小時的航班活動有多密集」——離境跟入境都算，因為不管起飛還是降落，
// 那個時段的空域都比較忙，值得多花一點額度盯著。
func computeHourlyActivity(departures, arrivals []FlightRecord) [24]int {
	var activity [24]int
	for _, r := range departures {
		if h := hourOfRecord(r); h >= 0 {
			activity[h]++
		}
	}
	for _, r := range arrivals {
		if h := hourOfRecord(r); h >= 0 {
			activity[h]++
		}
	}
	return activity
}

func distributeEvenly(hours []int, totalCalls int) hourlyBudget {
	var budget hourlyBudget
	if totalCalls <= 0 || len(hours) == 0 {
		return budget
	}

	base := totalCalls / len(hours)
	remainder := totalCalls % len(hours)
	for i, h := range hours {
		budget[h] = base
		if i < remainder {
			budget[h]++
		}
	}
	return budget
}

// computeHourlyBudget 把 totalCalls（一天總共能打幾次）依 activity 的比例分配到 24 小時，
// 完全沒有活動的小時分配 0（直接跳過，不是「至少打一次」）。
// 用最大餘數法（largest remainder method）分配，確保 24 小時總和「剛好」等於 totalCalls，
// 不會因為整數捨去而有額度沒用完，也不會超過預算。
func computeHourlyBudget(activity [24]int, totalCalls int) hourlyBudget {
	var budget hourlyBudget
	if totalCalls <= 0 {
		return budget
	}

	sumActivity := 0
	for _, a := range activity {
		sumActivity += a
	}

	// 完全沒有 FIDS 資料可用（例如程式剛啟動、FIDS 還沒抓到第一筆）——
	// 退回「24 小時平均分配」，不要讓 OpenSky 完全停擺在等資料的這段時間。
	if sumActivity == 0 {
		return distributeEvenly(fallbackActiveHours, totalCalls)
	}

	type frac struct {
		hour      int
		remainder float64
	}
	var fracs []frac

	assigned := 0
	for h := 0; h < 24; h++ {
		if activity[h] == 0 {
			continue // 使用者明確要求：完全沒有起降的時段不分配額度
		}
		exact := float64(totalCalls) * float64(activity[h]) / float64(sumActivity)
		whole := int(exact)
		budget[h] = whole
		assigned += whole
		fracs = append(fracs, frac{hour: h, remainder: exact - float64(whole)})
	}

	// 把因為無條件捨去而剩下的名額，依小數部分由大到小分給對應的小時，
	// 這樣 24 小時總和才會「剛好」等於 totalCalls，額度不多不少用完。
	remaining := totalCalls - assigned
	sort.Slice(fracs, func(i, j int) bool { return fracs[i].remainder > fracs[j].remainder })
	for i := 0; i < remaining && i < len(fracs); i++ {
		budget[fracs[i].hour]++
	}

	return budget
}

// fidsActivityHolder 是執行緒安全的「最新一次 FIDS 完整清單」存放處。
// FIDS 輪詢迴圈（不管是 TDX 還是 CSV）每次成功抓到資料就寫進來，
// OpenSky 排程器隨時可以讀出目前最新的起降活動分佈。
type fidsActivityHolder struct {
	mu    sync.RWMutex
	deps  []FlightRecord
	arrs  []FlightRecord
	ready bool // 還沒有任何一次成功的 FIDS 資料時是 false，排程器會用平均分配當保底
}

func (h *fidsActivityHolder) set(deps, arrs []FlightRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deps = deps
	h.arrs = arrs
	h.ready = true
}

func (h *fidsActivityHolder) get() (deps, arrs []FlightRecord, ready bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.deps, h.arrs, h.ready
}

// openSkyScheduler 是背景輪詢迴圈用的排程器：知道「現在這個小時還可以打幾次」，
// 並且會在每天固定重算一次每小時的預算分配（用當時最新的 FIDS 活動資料）。
type openSkyScheduler struct {
	dailyCredits int
	creditCost   int

	currentDay   int // 用 time.Now().YearDay() 判斷有沒有跨天，跨天要重算
	budget       hourlyBudget
	calledThisHr int
	currentHour  int
}

func (b hourlyBudget) total() int {
	total := 0
	for _, v := range b {
		total += v
	}
	return total
}

func (b hourlyBudget) logTable(prefix string) {
	log.Printf("%s hourly budget total=%d", prefix, b.total())
	for h := 0; h < 24; h++ {
		log.Printf("%s %02d:00-%02d:59 => %d calls", prefix, h, h, b[h])
	}
}

func newOpenSkyScheduler(dailyCredits, creditCost int) *openSkyScheduler {
	return &openSkyScheduler{dailyCredits: dailyCredits, creditCost: creditCost, currentDay: -1}
}

// refreshIfNeeded 在跨天時（或第一次呼叫時）用最新的 FIDS 活動資料重新計算一整天的預算分配。
func (s *openSkyScheduler) refreshIfNeeded(now time.Time, departures, arrivals []FlightRecord) {
	day := now.YearDay()
	if day == s.currentDay {
		return
	}
	s.currentDay = day
	totalCalls := s.dailyCredits / s.creditCost
	activity := computeHourlyActivity(departures, arrivals)
	s.budget = computeHourlyBudget(activity, totalCalls)
	s.currentHour = now.Hour()
	s.calledThisHr = 0
	log.Printf("OpenSky scheduler refreshed for %s: dailyCredits=%d creditCost=%d totalCalls=%d", now.Format("2006-01-02"), s.dailyCredits, s.creditCost, totalCalls)
	s.budget.logTable("OpenSky scheduler")
}

// nextDelay 回傳「距離下一次可以打 OpenSky 還要等多久」。
// 如果現在這小時的額度用完了，會直接跳到下一個「有分配到額度」的小時開頭，
// 而不是傻傻在沒有額度的小時裡空等到整點——這樣真正做到離峰時段完全不打。
func (s *openSkyScheduler) nextDelay(now time.Time) time.Duration {
	hour := now.Hour()
	if hour != s.currentHour {
		s.currentHour = hour
		s.calledThisHr = 0
	}

	limit := s.budget[hour]
	if s.calledThisHr < limit {
		// 這小時還有額度：把剩餘額度平均分攤在這小時剩下的時間裡，維持節奏平滑，
		// 而不是一口氣在整點打完、剩下 59 分鐘完全沒更新。
		remaining := limit - s.calledThisHr
		endOfHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 59, 59, 0, now.Location())
		timeLeft := endOfHour.Sub(now)
		if timeLeft <= 0 || remaining <= 0 {
			return 0
		}
		return timeLeft / time.Duration(remaining)
	}

	// 這小時額度用完（或本來就是 0）：找下一個有額度的小時，直接跳過去。
	for offset := 1; offset <= 24; offset++ {
		h := (hour + offset) % 24
		if s.budget[h] > 0 {
			target := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 1, 0, now.Location())
			if h <= hour { // 跨到隔天
				target = target.Add(24 * time.Hour)
			}
			return target.Sub(now)
		}
	}
	// 理論上不會發生（代表整天預算是 0），保底每小時檢查一次。
	return time.Hour
}

// recordCall 要在每次成功打完一次 OpenSky 之後呼叫，讓排程器知道這小時的額度用掉一次。
func (s *openSkyScheduler) recordCall() {
	s.calledThisHr++
}
