// faketelegraph 本地集成冒烟用假 Telegraph API + 假 Telegram Bot API + 静态伺服
// 自包含设计：不依赖 scripts/faketelegram（那是更完整的 Telegram 假服务器），
// 本工具只为 Telegraph 快照链路冒烟实现最小集合。
//
// 注入点约定（与主程序一致）：
//
//	TELEGRAPH_API_URL=http://127.0.0.1:18931 ./rss2telegram -config ...
//	TELEGRAM_API_URL=http://127.0.0.1:18931 ./rss2telegram -config ...
//
// 路由：
//   - POST /createAccount, /createPage   假 Telegraph API（createPage 逐条记录到 -pages）
//   - /bot<token>/<method>               假 Telegram API（getMe/sendMessage，消息记录到 -sent）
//   - /s/<file>                          静态伺服 -root 目录（RSS 源与原文 HTML 都放这里）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18931", "监听地址")
	root := flag.String("root", "", "以 /s/ 前缀伺服的静态目录（rss.xml 与原文 html）")
	pagesPath := flag.String("pages", "pages.jsonl", "createPage 请求记录(jsonl)")
	sentPath := flag.String("sent", "sent.jsonl", "sendMessage 记录(jsonl)")
	flag.Parse()

	pages, err := os.OpenFile(*pagesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("open pages log: %v", err)
	}
	sent, err := os.OpenFile(*sentPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("open sent log: %v", err)
	}

	var mu sync.Mutex // 两个 jsonl 文件写入互斥
	var accounts atomic.Int64
	var pagesCount atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/createAccount" && r.Method == http.MethodPost:
			accounts.Add(1)
			writeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"short_name":   "rss2telegram",
					"access_token": fmt.Sprintf("SMOKE-TOKEN-%d", accounts.Load()),
				},
			})

		case r.URL.Path == "/createPage" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			n := pagesCount.Add(1)
			record := map[string]any{"n": n, "request": json.RawMessage(body)}
			line, _ := json.Marshal(record)
			mu.Lock()
			_, _ = pages.Write(append(line, '\n'))
			mu.Unlock()
			writeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"path": fmt.Sprintf("/smoke-%d", n),
					"url":  fmt.Sprintf("https://telegra.ph/smoke-%d", n),
				},
			})

		case strings.HasPrefix(r.URL.Path, "/bot"):
			handleBotAPI(w, r, sent, &mu)

		case strings.HasPrefix(r.URL.Path, "/s/"):
			// filepath.Clean 归一化防目录穿越
			name := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/s/"))
			http.ServeFile(w, r, filepath.Join(*root, name))

		default:
			http.NotFound(w, r)
		}
	})

	log.Printf("faketelegraph listening on %s (root=%s pages=%s sent=%s)", *addr, *root, *pagesPath, *sentPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// handleBotAPI 最小假 Telegram Bot API：getMe 供启动握手，sendMessage 记录推送内容
func handleBotAPI(w http.ResponseWriter, r *http.Request, sent io.Writer, mu *sync.Mutex) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/bot"), "/", 2)
	if len(parts) != 2 {
		http.Error(w, `{"ok":false,"description":"bad path"}`, 400)
		return
	}
	switch parts[1] {
	case "getMe":
		writeJSON(w, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": 1, "is_bot": true, "first_name": "smoke", "username": "smoke_bot"},
		})
	case "sendMessage":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		var req struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		record, _ := json.Marshal(map[string]any{"chat_id": req.ChatID, "text": req.Text})
		mu.Lock()
		_, _ = sent.Write(append(record, '\n'))
		mu.Unlock()
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": -1001, "title": "smoke", "type": "channel"},
				"text":       req.Text,
			},
		})
	default:
		http.Error(w, `{"ok":false,"description":"method not implemented"}`, 404)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
