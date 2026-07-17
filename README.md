# 即時機場起降追蹤 — MVP（FIDS 已用真實資料驗證）

用 Go 標準函式庫寫的即時追蹤工具：
- 背景輪詢 OpenSky Network，取得空域內即時 ADS-B 航班位置，透過 SSE 推給前端，Leaflet 衛星底圖上顯示飛機標籤
- 背景輪詢桃園機場官方開放資料（政府資料開放平臺 data.gov.tw，dataset 26194），取得即時起降時刻表，透過另一條 SSE 推給前端，顯示在底部 DEPARTURES / ARRIVALS 表格

## 執行方式

```bash
go build -buildvcs=false -o airport-tracker.exe .
./airport-tracker.exe
```

開瀏覽器連到 http://localhost:8080。

## FIDS 資料格式（已用官方真實 CSV 樣本驗證）

- 編碼：UTF-8（無 BOM），CRLF 換行，欄位用雙引號包起來
- 更新頻率：每 5 分鐘
- 「種類」欄位：`A` = 入境、`D` = 出境
- 「航班狀態」常見值：已到、出發、取消
- 時間格式：`HH:MM:SS`（程式會自動裁成 `HH:MM` 顯示）
- 「班次」欄位常帶前導空白（程式會自動清除）

拿 999 筆真實資料測試：499 筆出境、499 筆入境、0 筆無法分類；取消班次、時間格式都正確解析。

如果之後官方調整格式，用 `-fids-debug` 可以印出診斷資訊：

```bash
./airport-tracker.exe -fids-debug
```

## 常用參數

```bash
# FIDS 相關
-fids-url string        桃園機場 CSV 資料來源網址（預設官方網址，通常不用改）
-fids-interval duration 輪詢 FIDS 的間隔（預設 5 分鐘，跟官方更新頻率一致）
-fids-debug              印出除錯資訊

# ADS-B / 地圖相關
-lamin -lomin -lamax -lomax   空域範圍
-interval                     輪詢 OpenSky 的間隔（預設 15s）
-addr                          監聽位址（預設 :8080）
```

## 已知限制

- OpenSky 匿名存取有速率限制（約每天 400 次額度）
- 目前 ADS-B 即時位置（呼號）跟 FIDS 時刻表（班次）是兩條獨立資料，沒有互相比對合併
