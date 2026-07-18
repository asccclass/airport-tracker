// env.go 是一個極簡的環境變數檔案讀取器，只用標準函式庫實作。
// 行為模仿業界慣例（類似 dotenv）：
//   - 讀取執行檔同目錄（或指定路徑）下的環境變數檔案（預設檔名 envfile）
//   - KEY=VALUE 格式，支援用 # 開頭的註解與空白行
//   - VALUE 可以用雙引號包起來（會去掉引號）
//   - 只有「目前還沒設定」的環境變數才會被檔案內容蓋過去——
//     也就是說如果你在系統環境變數或啟動指令裡已經設定過，
//     檔案裡的值不會覆蓋掉它，這是 dotenv 系工具的標準行為。
//   - 找不到這個檔案不是錯誤（很多人不會用這個檔案，直接用系統環境變數或 flag）
package main

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// loadEnvFile 讀取指定路徑的環境變數檔案並設進 process 環境變數。
// 回傳實際套用了幾個變數（給啟動時的 log 用，方便確認有沒有讀到）。
func loadEnvFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("讀取 %s 時發生錯誤（略過）: %v", path, err)
		}
		return 0
	}
	defer f.Close()

	applied := 0
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			log.Printf("%s 第 %d 行格式不是 KEY=VALUE，略過: %q", path, lineNo, line)
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		if key == "" {
			continue
		}
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue // 系統環境變數優先權比檔案高，不覆蓋
		}
		if err := os.Setenv(key, value); err != nil {
			log.Printf("設定環境變數 %s 失敗: %v", key, err)
			continue
		}
		applied++
	}
	if err := scanner.Err(); err != nil {
		log.Printf("讀取 %s 時發生錯誤: %v", path, err)
	}
	return applied
}

// getenvDefault 是個小工具：環境變數存在就回傳它的值，否則回傳預設值。
// 用來當作 flag 的預設值，這樣同一個設定「可以用 envfile 設，也可以用命令列參數覆蓋」。
func getenvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
