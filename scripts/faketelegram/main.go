// faketelegram 本地集成测试用假 Telegram Bot API + RSS 静态伺服
// 配合 rss2telegram 的 TELEGRAM_API_URL 环境变量注入点使用：
//
//	TELEGRAM_API_URL=http://127.0.0.1:18923 ./rss2telegram -config ...
//
// 所有 sendMessage 请求逐条记录到 -log 指定的 jsonl 文件，供断言推送条数/内容
//
// 用法: faketelegram -addr 127.0.0.1:18923 -rss <dir> -log <sent.jsonl>
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18923", "监听地址")
	rssDir := flag.String("rss", "", "以 /rss/ 前缀伺服的静态目录")
	logPath := flag.String("log", "sent.jsonl", "sendMessage 记录文件(jsonl)")
	flag.Parse()

	out, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("open log file: %v", err)
	}

	var mu sync.Mutex
	mux := http.NewServeMux()

	// catch-all 手工路由：/bot<token>/<method> 不符合 ServeMux 的前缀匹配规则
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rss/"):
			// filepath.Clean 归一化防目录穿越
			name := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/rss/"))
			http.ServeFile(w, r, filepath.Join(*rssDir, name))

		case strings.HasPrefix(r.URL.Path, "/bot"):
			// path 形如 /bot123:ABC/getMe
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/bot"), "/", 2)
			if len(parts) != 2 || parts[1] == "" {
				http.Error(w, `{"ok":false,"description":"bad path"}`, 400)
				return
			}
			handleBotAPI(w, r, parts[1], out, &mu)

		default:
			http.NotFound(w, r)
		}
	})

	log.Printf("faketelegram listening on %s (rss=%s log=%s)", *addr, *rssDir, *logPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// handleBotAPI 只实现 telebot 实际会调用的三个方法，其余一律返回 ok 空结果
func handleBotAPI(w http.ResponseWriter, r *http.Request, method string, out *os.File, mu *sync.Mutex) {
	switch method {
	case "getMe":
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"id": 1, "is_bot": true, "first_name": "fake", "username": "fake_bot",
			},
		})

	case "getChat":
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"id": -1009999, "type": "channel", "title": "it", "username": "it_test",
			},
		})

	case "sendMessage":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"ok":false,"description":"read body"}`, 400)
			return
		}
		var req struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)

		line, _ := json.Marshal(map[string]string{
			"method": "sendMessage", "chat_id": req.ChatID, "text": req.Text,
		})
		mu.Lock()
		_, _ = out.Write(append(line, '\n'))
		mu.Unlock()

		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": -1009999, "type": "channel"},
				"date":       1,
				"text":       req.Text,
			},
		})

	default:
		// telebot 偶发的其他调用统一放行
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
