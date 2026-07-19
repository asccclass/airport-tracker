# 即時機場起降追蹤（桃園國際機場）

用 Go 標準函式庫寫的即時追蹤工具：
- 背景輪詢 OpenSky Network，取得空域內即時 ADS-B 航班位置，透過 SSE 推給前端，Leaflet 衛星底圖上顯示飛機標籤
- 背景輪詢桃園機場官方開放資料（政府資料開放平臺 data.gov.tw，dataset 26194），取得即時起降時刻表，透過另一條 SSE 推給前端，顯示在底部 DEPARTURES / ARRIVALS 表格
- 左上角、右下角各嵌入一個 YouTube 即時影像/塔台通訊小工具，可個別展開/收合、開關聲音

## 執行方式

```bash
go build -buildvcs=false -o airport-tracker.exe .
./airport-tracker.exe
```

開瀏覽器連到 http://localhost:8080。

## FIDS 資料格式（已用官方真實 CSV 樣本驗證）

- 編碼：UTF-8（無 BOM），CRLF 換行，欄位用雙引號包起來
- 官方資料集標示更新頻率為「即時」；程式預設每 1 分鐘輪詢一次（避免對官方伺服器太頻繁請求，不是資料源本身只有這個更新速度）
- 「種類」欄位：`A` = 入境、`D` = 出境
- 「航班狀態」常見值：已到、出發、取消
- 時間格式：`HH:MM:SS`（程式會自動裁成 `HH:MM` 顯示）
- 「班次」欄位常帶前導空白（程式會自動清除）

拿真實資料測試過：出境/入境分類、取消班次、時間格式都正確解析。

**顯示邏輯**：
- 離境表只顯示「還沒起飛」的班機（依表定/預計時間跟現在時間比較），依時間由近到遠排序
- 入境表只顯示「最近 1 小時內抵達」的班機，依時間由新到舊排序
- 取消的班機不受上述時間限制，一律顯示

如果之後官方調整格式，用 `-fids-debug` 可以印出診斷資訊：

```bash
./airport-tracker.exe -fids-debug
```

## 常用參數

```bash
# FIDS 相關
-fids-url string        桃園機場 CSV 資料來源網址（預設官方網址，通常不用改）
-fids-interval duration 輪詢 FIDS 的間隔（預設 1 分鐘）
-fids-debug              印出除錯資訊

# ADS-B / 地圖相關
-lamin -lomin -lamax -lomax   空域範圍
-interval                     輪詢 OpenSky 的間隔（預設 15s）
-addr                          監聽位址（預設 :8080）
```

## 已知限制

- OpenSky 匿名存取有速率限制（約每天 400 次額度）
- 目前 ADS-B 即時位置（呼號）跟 FIDS 時刻表（班次）是兩條獨立資料，沒有互相比對合併
- **FIDS 只涵蓋客運航班，不含貨機**。查證過官方網站雖然另外有獨立的「貨機起飛/貨機抵達」頁面，
  但 data.gov.tw 上沒有對應的開放資料集，我們用的這份 CSV 資料也比對不到任何已知貨運航空公司代碼
  （FedEx、UPS、Cargolux 等一筆都沒有），研判這份資料集本身就只收錄客運航班。
  目前把「只顯示客機」當作這個工具合理的功能邊界，沒有要湊貨機資料；
  如果之後真的需要，可行的路只有：(1) 去信詢問機場公司有沒有未公開的貨機資料介接方式，
  或 (2) 改用付費商用 API（FlightAware AeroAPI、Flightradar24 等，通常客貨機都涵蓋，但要付費申請）。
- ADS-B Exchange 新版 streaming API（gRPC，需付費申請 ClientId/SubscriptionId）的資料源切換功能還在開發中，
  尚未完成整合。

## FIDS 資料源：TDX（交通部運輸資料流通服務）

除了原本的桃園機場官方 CSV，現在支援切換成 TDX 平台的航班資料，優點：
- 有「實際起降時間」欄位（不是只有表訂/預計時間）
- 同時涵蓋客機與貨機（IsCargo 欄位）
- 官方文件標示桃機資料涵蓋前一日、當日、次日

### 設定方式

複製 `envfile.example` 改名成 `envfile`，填入你自己申請的憑證：

```bash
cp envfile.example envfile
```

去 https://tdx.transportdata.tw 註冊會員、於「會員中心 -> 資料服務 -> API金鑰」申請取得 Client Id / Client Secret，填進 `envfile`：

```
FIDS_SOURCE=tdx
TDX_CLIENT_ID=你的Client Id
TDX_CLIENT_SECRET=你的Client Secret
TDX_AIRPORT=TPE
```

`envfile` 只在你自己的機器上讀取，不要把它上傳到公開的地方（例如公開的 GitHub repo）。

### 速率限制

TDX 免費方案的速率限制通常是幾次/分/金鑰（實際額度依你申請的方案而定）。程式內建了一個
滑動窗口限制器，把「換 token」跟「拉資料」都算進同一個配額，用 `-tdx-rate-limit` 設定：

```bash
./airport-tracker.exe -tdx-rate-limit 5
```

即使程式邏輯出錯多打了幾次，也不會真的超過這個配額——會排隊等到下一分鐘，而不是直接超額被 TDX 擋掉。

### 沒有設定 TDX 憑證會怎樣

`FIDS_SOURCE=tdx` 但沒填 `TDX_CLIENT_ID`/`TDX_CLIENT_SECRET`，程式會印出警告、
自動退回用桃園機場官方 CSV，不會直接掛掉。想強制用 CSV 也可以直接設
`FIDS_SOURCE=csv`。

### 已知限制

- TDX 這個「機場角度」FIDS 端點沒有直接提供航空公司中文名稱，目前先顯示航空公司代碼
  （例如 CI、BR），要中文名稱需要額外呼叫 `/v2/Air/Airline` 拿對照表，之後可以加
- 這部分程式碼因為沙盒環境連不到 tdx.transportdata.tw，沒辦法對真實金鑰做端對端測試；
  已經用模擬伺服器驗證過 OAuth 換 token、資料解析、速率限制器的邏輯都正確，
  但真實金鑰的驗證結果、實際回傳格式細節，需要你在自己的機器上第一次執行時用
  `-fids-debug` 確認，如果有落差我可以照實際情況校正

## OpenSky 驗證身分存取（解決 429 Too many requests）

OpenSky 匿名存取額度很低，容易遇到 `429 Too many requests`。2026 年 3 月起 OpenSky
把舊的帳號密碼登入方式退休，改成 OAuth2 Client Credentials 流程。

### 設定方式（擇一）

**方式一：把官方憑證檔案放在程式同目錄（最簡單）**

登入 https://opensky-network.org 帳號，到 Account 頁面建立一個 API client，
下載 `credentials.json`，放到跟 `airport-tracker.exe` 同一個資料夾。程式啟動時
會自動讀取，不用額外設定。

**方式二：用 envfile 或參數指定**

```
OPENSKY_CLIENT_ID=你的clientId
OPENSKY_CLIENT_SECRET=你的clientSecret
```

或

```bash
./airport-tracker.exe -opensky-client-id 你的clientId -opensky-client-secret 你的clientSecret
```

這個方式優先權比 `credentials.json` 高。

### 沒有設定會怎樣

兩種方式都沒設定，程式會照舊用匿名存取（額度低，容易 429），並在啟動時印出提示，
不會直接掛掉。換 token 過程中如果失敗（例如網路問題、憑證錯誤），也只會記錄警告、
自動退回匿名請求那一次，不會讓整個輪詢中斷。

### 已知限制

跟 TDX 一樣，這部分程式碼因為沙盒環境連不到 `auth.opensky-network.org`，
沒辦法對你的真實憑證做端對端測試；已經用模擬伺服器驗證過 OAuth2 換 token、
Bearer token 帶入請求、token 快取的邏輯都正確，但真實憑證的實際行為（例如
token 有效期限是不是真的是 30 分鐘）需要你在自己的機器上第一次執行時確認。

## OpenSky 額度動態排程（依起降時間分配 4000 credits/天）

不再用固定頻率（例如每 15 秒）打 OpenSky，改成依照 FIDS 的起降時間分佈，
把一天的額度動態分配：起降密集的時段多打幾次、完全沒有起降的時段直接不打，
額度剛好在一天用完，不會離峰時段浪費、尖峰時段又不夠用。

### 運作方式

1. 每次向 OpenSky 查詢的花費，是依查詢範圍面積算的（官方分級：≤25 平方度 1 credit，
   25–100 平方度 2 credits，以此類推）。桃園機場預設範圍約 0.1 平方度，一次查詢是 1 credit，
   換算下來 4000 credits/天 = 最多 4000 次/天。
2. 把 FIDS 完整一天（含前一日、次日，未經「還沒起飛/最近1小時」那套顯示過濾）的
   離境+入境時間，依小時分桶算出「這個小時航班活動有多密集」。
3. 把每天的額度依比例分配到 24 小時，**完全沒有起降的小時分配 0 額度、直接跳過**；
   額度分配用最大餘數法，確保 24 小時總和剛好等於每日額度，不多不少。
4. 每天固定重算一次（跨天偵測），永遠用當時最新的 FIDS 資料算分佈。
5. 在還沒有任何 FIDS 資料（例如程式剛啟動）的空窗期，先用「24 小時平均分配」墊著，
   等 FIDS 資料到位後、隔天就會自動改用真正的起降分佈。

### 設定方式

```bash
# 額度大小依你的 OpenSky 帳號方案設定（Standard user 預設 4000）
./airport-tracker.exe -opensky-daily-credits 4000

# 關閉這個功能，回到固定頻率（用 -interval 控制）
./airport-tracker.exe -opensky-schedule=false
```

### 已知限制

- 只用「起降時間」當作活動指標，不會知道實際空中交通量（例如過境不降落的班機不會反映在這個分佈裡）
- 用 FIDS 的「客運航班表」估計活動時段，如果 FIDS 資料源本身覆蓋不到某些時段的班機
  （之前提過的貨機限制之類），那個時段的額度分配也會相應偏少
- 這部分邏輯用單元測試跟模擬時間戳驗證過（額度分配加總正確、離峰時段正確跳過、
  跨天正確重算），但完整跑滿 24 小時的真實情境沒辦法在這個環境驗證，
  建議你實際跑起來後，觀察 log 確認額度分配跟起降時段對得起來
