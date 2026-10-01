package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTG 模拟 Telegram Bot API：stub NewBot 所需的 getMe、ChatByUsername 所需的
// getChat、以及 sendMessage。所有 sendMessage 的原始 JSON body 被逐条记录，
// 供断言降级行为（首次带 parse_mode 400 → 二次纯文本）
type fakeTG struct {
	mu        sync.Mutex
	sends     []map[string]any // 每次 sendMessage 的 body
	failFirst bool             // 首次 sendMessage 返回 parse entities 400
	server    *httptest.Server
}

func newFakeTG(failFirst bool) *fakeTG {
	f := &fakeTG{failFirst: failFirst}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch method {
		case "getMe":
			writeFakeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": 1, "is_bot": true, "first_name": "fake", "username": "fake_bot",
				},
			})
		case "getChat":
			writeFakeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": -1009999, "type": "channel", "title": "it", "username": "it_test",
				},
			})
		case "sendMessage":
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)

			f.mu.Lock()
			first := len(f.sends) == 0
			f.sends = append(f.sends, m)
			f.mu.Unlock()

			if first && f.failFirst {
				// 真实线上报文原文（issue #4）。刻意用原始 JSON 而非 tele.NewError 构造：
				// telebot v3.1.3 extractOk 对此错误返回 fmt.Errorf 而非 *tele.Error，
				// 若实现误用类型断言检测，此测试会因降级未触发而失败
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Can't find end of the entity starting at byte offset 150"}`))
				return
			}
			writeFakeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 1,
					"chat":       map[string]any{"id": -1009999, "type": "channel"},
					"date":       1,
					"text":       m["text"],
				},
			})
		default:
			// telebot 偶发的其他调用统一放行
			writeFakeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
		}
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeTG) sends_() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.sends...)
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// issue #4 兜底：markdown 解析 400 后应降级纯文本重发（去掉 parse_mode + 反转义），
// 成功后返回 nil，使 handler 能标记 seen、终止无限重试
func TestSendFallsBackToPlainTextOnParseEntitiesError(t *testing.T) {
	fake := newFakeTG(true)
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot("1:test")
	assert.NoError(t, err)

	// 消息含 Escape 产物（title 转义后），降级纯文本时应被反转义
	err = bot.Send("@it_test", `\[特惠产品]天幕 3\*4.35米`)
	assert.NoError(t, err)

	sends := fake.sends_()
	require.Len(t, sends, 2, "应恰好两次 sendMessage：markdown 一次 + 纯文本降级一次")

	assert.Equal(t, "Markdown", sends[0]["parse_mode"], "首次请求应带 Markdown parse_mode")

	_, hasParseMode := sends[1]["parse_mode"]
	assert.False(t, hasParseMode, "降级请求不应携带 parse_mode 键（纯文本）")

	assert.Equal(t, "[特惠产品]天幕 3*4.35米", sends[1]["text"], "降级文本应去除转义反斜杠")
}

// 对照守护：markdown 正常成功时不得触发降级（仅一次请求）
func TestSendMarkdownSuccessSingleRequest(t *testing.T) {
	fake := newFakeTG(false)
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot("1:test")
	assert.NoError(t, err)

	err = bot.Send("@it_test", "*正常*消息")
	assert.NoError(t, err)

	sends := fake.sends_()
	require.Len(t, sends, 1, "成功路径应仅一次 sendMessage")
	assert.Equal(t, "Markdown", sends[0]["parse_mode"])
}
