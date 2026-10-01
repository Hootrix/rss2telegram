package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTG 模拟 Telegram Bot API：stub NewBot 所需的 getMe、ChatByUsername 所需的
// getChat、以及 sendMessage。所有 sendMessage 的原始 JSON body 被逐条记录，
// 供断言降级行为（首次带 parse_mode 400 → 二次纯文本）
type fakeTG struct {
	mu          sync.Mutex
	sends       []map[string]any // 每次 sendMessage 的 body
	failFirst   bool             // 首次 sendMessage 返回 parse entities 400
	flood429    bool             // 每次 sendMessage 返回 429 + retry_after（issue #6 线上报文形态）
	floodNo429  bool             // 每次 sendMessage 返回 429 但无 retry_after 参数
	getChatHits int              // getChat 被调用次数（issue #6 后应为 0）
	server      *httptest.Server
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
			f.mu.Lock()
			f.getChatHits++
			f.mu.Unlock()
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
			flood429 := f.flood429
			floodNo429 := f.floodNo429
			f.mu.Unlock()

			// issue #6 线上 429 报文：带 retry_after 参数，telebot extractOk
			// 对此返回 FloodError 值类型
			if flood429 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 21","parameters":{"retry_after":21}}`))
				return
			}
			if floodNo429 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 18"}`))
				return
			}

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

// issue #6：429 + retry_after 应返回携带等待时长的 *RateLimitError（errors.As 可识别），
// 且不再预先 getChat——直接以 @username 作为 chat_id 发送，每条消息 API 调用从 2 次降到 1 次
func TestSendFlood429ReturnsRateLimitErrorWithoutGetChat(t *testing.T) {
	fake := newFakeTG(false)
	fake.flood429 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot("1:test")
	assert.NoError(t, err)

	err = bot.Send("@it_test", "hello")
	require.Error(t, err)

	var rlErr *RateLimitError
	require.True(t, errors.As(err, &rlErr), "429 应被包装为 *RateLimitError")
	assert.Equal(t, 21*time.Second, rlErr.RetryAfter, "RetryAfter 应取自服务端 retry_after=21")

	// 不依赖 telegram 包错误类型也能用 Unwrap 拿到原始 telebot 错误
	assert.ErrorContains(t, rlErr.Unwrap(), "429")

	sends := fake.sends_()
	require.Len(t, sends, 1)
	assert.Equal(t, "@it_test", sends[0]["chat_id"], "chat_id 应直接使用 @username 字符串")

	fake.mu.Lock()
	hits := fake.getChatHits
	fake.mu.Unlock()
	assert.Zero(t, hits, "不应再调用 getChat 预解析频道")
}

// 边界：429 响应缺 retry_after 参数时 telebot 返回普通错误，
// 不得误判为 RateLimitError（走原有指数退避路径）
func TestSendFlood429WithoutRetryAfterStaysPlainError(t *testing.T) {
	fake := newFakeTG(false)
	fake.floodNo429 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot("1:test")
	assert.NoError(t, err)

	err = bot.Send("@it_test", "hello")
	require.Error(t, err)

	var rlErr *RateLimitError
	assert.False(t, errors.As(err, &rlErr), "无 retry_after 的 429 不应包装为 RateLimitError")
}

// channelRecipient 归一化：@name 原样透传、裸名补 @、数字/负数 ID 原样透传
func TestNewChannelRecipient(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"@channel", "@channel"},
		{"bare_name", "@bare_name"},
		{"-1001234567890", "-1001234567890"},
		{"123456", "123456"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, string(newChannelRecipient(c.in)), "input: %q", c.in)
	}
}
