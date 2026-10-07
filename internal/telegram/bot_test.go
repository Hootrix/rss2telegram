package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mu              sync.Mutex
	sends           []map[string]any // 每次 sendMessage 的 body
	failFirst       bool             // 首次 sendMessage 返回 parse entities 400
	flood429        bool             // 每次 sendMessage 返回 429 + retry_after（issue #6 线上报文形态）
	floodNo429      bool             // 每次 sendMessage 返回 429 但无 retry_after 参数
	floodRetryAfter int64
	msgID           int64 // 成功响应携带的 message_id；0 = 固定返回 1（回填测试用）
	getChatHits     int   // getChat 被调用次数（issue #6 后应为 0）
	server          *httptest.Server
}

func newFakeTG(failFirst bool) *fakeTG {
	// f := &fakeTG{failFirst: failFirst}
	f := &fakeTG{failFirst: failFirst, floodRetryAfter: 21}
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
			retryAfter := f.floodRetryAfter
			f.mu.Unlock()

			// issue #6 线上 429 报文：带 retry_after 参数，telebot extractOk
			// 对此返回 FloodError 值类型
			// if flood429 {
			if flood429 && (!f.failFirst || !first) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				// _, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 21","parameters":{"retry_after":21}}`))
				writeFakeJSON(w, map[string]any{
					"ok": false, "error_code": 429,
					"description": fmt.Sprintf("Too Many Requests: retry after %d", retryAfter),
					"parameters":  map[string]any{"retry_after": retryAfter},
				})
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
			msgID := int64(1)
			if f.msgID != 0 {
				msgID = f.msgID
			}
			writeFakeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": msgID,
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

	bot, err := NewBot(context.Background(), "1:test")
	assert.NoError(t, err)

	// 消息含 Escape 产物（title 转义后），降级纯文本时应被反转义
	_, err = bot.Send(context.Background(), "@it_test", NewMessage(`\[特惠产品]天幕 3\*4.35米`))
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

	bot, err := NewBot(context.Background(), "1:test")
	assert.NoError(t, err)

	_, err = bot.Send(context.Background(), "@it_test", NewMessage("*正常*消息"))
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

	bot, err := NewBot(context.Background(), "1:test")
	assert.NoError(t, err)

	_, err = bot.Send(context.Background(), "@it_test", NewMessage("hello"))
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

	bot, err := NewBot(context.Background(), "1:test")
	assert.NoError(t, err)

	_, err = bot.Send(context.Background(), "@it_test", NewMessage("hello"))
	require.Error(t, err)

	var rlErr *RateLimitError
	assert.False(t, errors.As(err, &rlErr), "无 retry_after 的 429 不应包装为 RateLimitError")
}

// 边界：空 channel 应提前返回错误且不产生任何 HTTP 请求
// （getChat 移除后不再被预查询拦截，空 chat_id 的 sendMessage 必 400，白调一次 API）
func TestSendEmptyChannelFailsFast(t *testing.T) {
	fake := newFakeTG(false)
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot(context.Background(), "1:test")
	assert.NoError(t, err)

	_, err = bot.Send(context.Background(), "", NewMessage("hello"))
	require.Error(t, err)

	assert.Empty(t, fake.sends_(), "空 channel 不应发起 sendMessage")
	fake.mu.Lock()
	hits := fake.getChatHits
	fake.mu.Unlock()
	assert.Zero(t, hits)
}

// channelRecipient 归一化：@name 原样透传、裸名补 @、数字/负数 ID 原样透传
func TestNewChannelRecipient(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"@channel", "@channel"},
		{"bare_name", "@bare_name"},
		{"123feed", "@123feed"},
		{"1_Channel", "@1_Channel"},
		{"-1001234567890", "-1001234567890"},
		{"123456", "123456"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, string(newChannelRecipient(c.in)), "input: %q", c.in)
	}
}

func TestSendFloodRetryAfterValidation(t *testing.T) {
	cases := []struct {
		name          string
		seconds       int64
		wantRateLimit bool
	}{
		{"long_valid_wait", 300, true},
		{"zero", 0, false},
		{"negative", -1, false},
		{"buffer_overflow", 9_223_372_036, false},
		{"duration_overflow", 9_223_372_037, false},
		{"integer_overflow", 1<<63 - 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeTG(false)
			defer fake.server.Close()
			fake.mu.Lock()
			fake.flood429 = true
			fake.floodRetryAfter = tc.seconds
			fake.mu.Unlock()
			t.Setenv("TELEGRAM_API_URL", fake.server.URL)
			bot, err := NewBot(context.Background(), "1:test")
			require.NoError(t, err)

			_, err = bot.Send(context.Background(), "@it_test", NewMessage("hello"))
			require.Error(t, err)
			var rlErr *RateLimitError
			assert.Equal(t, tc.wantRateLimit, errors.As(err, &rlErr))
			if tc.wantRateLimit {
				require.NotNil(t, rlErr)
				assert.Equal(t, time.Duration(tc.seconds)*time.Second, rlErr.RetryAfter)
			} else {
				assert.ErrorContains(t, err, "invalid retry_after")
			}
			assert.Len(t, fake.sends_(), 1)
		})
	}
}

func TestSendPlainTextFallbackPreservesRateLimitError(t *testing.T) {
	fake := newFakeTG(true)
	defer fake.server.Close()
	fake.mu.Lock()
	fake.flood429 = true
	fake.mu.Unlock()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	_, err = bot.Send(context.Background(), "@it_test", NewMessage(`\[标题]`))
	var rlErr *RateLimitError
	require.True(t, errors.As(err, &rlErr))
	assert.Equal(t, 21*time.Second, rlErr.RetryAfter)
	sends := fake.sends_()
	require.Len(t, sends, 2)
	assert.Equal(t, "Markdown", sends[0]["parse_mode"])
	assert.NotContains(t, sends[1], "parse_mode")
}

func TestSendRetryKeepsPlainTextFallback(t *testing.T) {
	fake := newFakeTG(true)
	defer fake.server.Close()
	fake.mu.Lock()
	fake.flood429 = true
	fake.mu.Unlock()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	// require.Error(t, bot.Send("@it_test", `\[标题]`))
	// require.Error(t, bot.Send("@it_test", `\[标题]`))
	message := NewMessage(`\[标题]`)
	require.Error(t, func() error { _, err := bot.Send(context.Background(), "@it_test", message); return err }())
	require.Error(t, func() error { _, err := bot.Send(context.Background(), "@it_test", message); return err }())
	sends := fake.sends_()
	require.Len(t, sends, 3)
	assert.Equal(t, "Markdown", sends[0]["parse_mode"])
	assert.NotContains(t, sends[1], "parse_mode")
	assert.NotContains(t, sends[2], "parse_mode", "同一消息已降级，重试不得再发送 Markdown")
}

func TestSendContextCancelsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			writeFakeJSON(w, map[string]any{"ok": true, "result": map[string]any{"id": 1, "is_bot": true}})
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("TELEGRAM_API_URL", server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := bot.Send(ctx, "@ch", NewMessage("hello")); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未启动")
	}
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), "1:test", "请求错误不得暴露 bot token")
	case <-time.After(5 * time.Second):
		t.Fatal("取消未中断 HTTP 请求")
	}
}

func TestSendDeadlineCancelsResponseBodyRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			writeFakeJSON(w, map[string]any{"ok": true, "result": map[string]any{"id": 1, "is_bot": true}})
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if _, err := io.WriteString(w, `{"ok":`); err != nil {
			t.Errorf("write response: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("TELEGRAM_API_URL", server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = bot.Send(ctx, "@ch", NewMessage("hello"))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "1:test")
}

func TestNewBotContextCancelsInitialization(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("TELEGRAM_API_URL", server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewBot(ctx, "1:test")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("getMe 请求未启动")
	}
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), "1:test")
	case <-time.After(5 * time.Second):
		t.Fatal("初始化请求未响应取消")
	}
}

func TestNewRateLimitErrorValidatesSeconds(t *testing.T) {
	for _, seconds := range []int64{0, -1, 9_223_372_036, 9_223_372_037} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			cause := errors.New("429")
			err := NewRateLimitError(seconds, cause)
			var rate *RateLimitError
			assert.False(t, errors.As(err, &rate))
			assert.ErrorIs(t, err, cause)
			assert.ErrorContains(t, err, "invalid retry_after")
		})
	}
}

func TestChannelKey(t *testing.T) {
	cases := []struct {
		channel, want string
	}{
		{"@Channel", "@channel"},
		{"CHANNEL", "@channel"},
		{"123Feed", "@123feed"},
		{"@123Feed", "@123feed"},
		{"@channel", "@channel"},
		{"-1001234567890", "-1001234567890"},
		{"123456", "123456"},
		{"@123456", "@123456"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ChannelKey(tc.channel), "channel: %q", tc.channel)
	}
}

// Send 返回成功消息的 message_id（issue #12：快照页 author_url 回填消息链接用）；
// markdown 降级重发场景取最终成功那次的 message_id
func TestSendReturnsMessageID(t *testing.T) {
	fake := newFakeTG(false)
	fake.msgID = 777
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	id, err := bot.Send(context.Background(), "@it_test", NewMessage("*正常*消息"))
	require.NoError(t, err)
	assert.Equal(t, int64(777), id)
}

func TestSendReturnsFinalMessageIDOnPlainFallback(t *testing.T) {
	fake := newFakeTG(true) // 首次 markdown 400 → 纯文本重发成功
	fake.msgID = 888
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)

	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	id, err := bot.Send(context.Background(), "@it_test", NewMessage(`\[特惠]3\*4`))
	require.NoError(t, err)
	require.Len(t, fake.sends_(), 2, "前置：确实走了降级重发")
	assert.Equal(t, int64(888), id, "取最终成功那次的 message_id")
}
