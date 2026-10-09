package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
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
	mu                  sync.Mutex
	sends               []map[string]any // 每次 sendMessage 的 body
	failFirst           bool             // 首次 sendMessage 返回 parse entities 400
	flood429            bool             // 每次 sendMessage 返回 429 + retry_after（issue #6 线上报文形态）
	floodNo429          bool             // 每次 sendMessage 返回 429 但无 retry_after 参数
	floodRetryAfter     int64
	msgID               int64       // 成功响应携带的 message_id；0 = 固定返回 1（回填测试用）
	getChatHits         int         // getChat 被调用次数（issue #6 后应为 0）
	photos              []photoSend // 每次 sendPhoto 的字段与图片字节
	photoFail400        bool        // sendPhoto 恒返回 400（图片永久失败）
	photoFail413        bool        // sendPhoto 恒返回 413（实体过大）
	photoFail500        bool        // sendPhoto 恒返回 500（瞬态失败）
	photoFailCaption400 bool        // sendPhoto 首次返回 parse entities 400
	server              *httptest.Server
}

// photoSend 记录一次 sendPhoto 请求的关键字段，供断言 multipart 内容与降级行为
type photoSend struct {
	caption   string
	parseMode string // "" = 请求未带 parse_mode
	photo     []byte
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
		case "sendPhoto":
			// telebot FromReader 走 multipart：字段 + 名为 photo 的文件部分。
			// 注意 v3.1.3 FromReader 不设置 fileName → CreateFormFile 产出 filename=""，
			// Go 服务端 ReadForm 将无 filename 的部件归为表单值而非文件，
			// 故优先取文件部件、取不到再回退 FormValue("photo")。两服务端判定规则不同：
			// 真实 Telegram（tdlib HttpReader）以 multipart 里 filename 键是否存在区分文件
			// 与普通参数——telebot FromReader 写出 filename=""（键存在、值为空），真实 TG
			// 按文件处理；Go 服务端要求 filename 非空才归文件部件，落为表单值 → 需回退
			ps := photoSend{}
			if err := r.ParseMultipartForm(32 << 20); err == nil && r.MultipartForm != nil {
				ps.caption = r.FormValue("caption")
				ps.parseMode = r.FormValue("parse_mode")
				if files := r.MultipartForm.File["photo"]; len(files) > 0 {
					fh, _ := files[0].Open()
					b, _ := io.ReadAll(fh)
					_ = fh.Close()
					ps.photo = b
				} else if v := r.FormValue("photo"); v != "" {
					ps.photo = []byte(v)
				}
			}
			f.mu.Lock()
			f.photos = append(f.photos, ps)
			photoFail400 := f.photoFail400
			photoFail413 := f.photoFail413
			photoFail500 := f.photoFail500
			failCaption := f.photoFailCaption400 && len(f.photos) == 1
			f.mu.Unlock()

			if photoFail500 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"Internal Server Error"}`))
				return
			}
			if failCaption {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Can't find end of the entity starting at byte offset 150"}`))
				return
			}
			if photoFail413 {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":413,"description":"Request Entity Too Large"}`))
				return
			}
			if photoFail400 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: PHOTO_INVALID_DIMENSIONS"}`))
				return
			}
			// 实测 sendPhoto 响应形态：result.photo 为尺寸数组，telebot 取最高清档
			writeFakeJSON(w, map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 1,
					"chat":       map[string]any{"id": -1009999, "type": "channel"},
					"date":       1,
					"photo":      []map[string]any{{"file_id": "ph1", "file_unique_id": "u1", "width": 100, "height": 100}},
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

func (f *fakeTG) photos_() []photoSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]photoSend{}, f.photos...)
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

// issue #13：caption 按 UTF-16 码元计上限 1024，rune 边界截断 + 换行回退
func TestTruncateCaption(t *testing.T) {
	t.Run("未超限原样返回", func(t *testing.T) {
		assert.Equal(t, "hello", truncateCaption("hello"))
		assert.Equal(t, strings.Repeat("新", 1024), truncateCaption(strings.Repeat("新", 1024)))
	})
	t.Run("纯 ASCII 超限硬截断", func(t *testing.T) {
		assert.Equal(t, strings.Repeat("a", 1024), truncateCaption(strings.Repeat("a", 2000)))
	})
	t.Run("汉字每字计 1 码元", func(t *testing.T) {
		assert.Equal(t, strings.Repeat("新", 1024), truncateCaption(strings.Repeat("新", 2000)))
	})
	t.Run("emoji 计 2 码元且不劈开", func(t *testing.T) {
		// 512 个 emoji = 1024 码元，恰满；再放一个 BMP 字符应被丢弃
		in := strings.Repeat("🎉", 512) + "a"
		assert.Equal(t, strings.Repeat("🎉", 512), truncateCaption(in))
		// 511 emoji + "a" = 1023 码元，放不下下一个 2 码元 emoji：保留 511+a，emoji 不被劈半
		in2 := strings.Repeat("🎉", 511) + "a" + "🎉"
		assert.Equal(t, strings.Repeat("🎉", 511)+"a", truncateCaption(in2))
	})
	t.Run("换行位于 512 码元以上回退到换行", func(t *testing.T) {
		in := strings.Repeat("a", 600) + "\n" + strings.Repeat("b", 600)
		assert.Equal(t, strings.Repeat("a", 600), truncateCaption(in))
	})
	t.Run("换行位于 512 码元以下保持硬截断", func(t *testing.T) {
		in := strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 1000)
		assert.Equal(t, strings.Repeat("a", 100)+"\n"+strings.Repeat("b", 923), truncateCaption(in))
	})
	t.Run("截断后去尾部空白", func(t *testing.T) {
		// 1023 a + 空格 = 1024 码元截住，尾部空格 TrimRight 掉
		in := strings.Repeat("a", 1023) + "  \n\n" + strings.Repeat("b", 100)
		assert.Equal(t, strings.Repeat("a", 1023), truncateCaption(in))
	})
	t.Run("换行恰在 512 码元处等值回退", func(t *testing.T) {
		// 锁定 >= 等值边界：换行前恰好 512 码元仍触发回退
		in := strings.Repeat("a", 512) + "\n" + strings.Repeat("b", 1000)
		assert.Equal(t, strings.Repeat("a", 512), truncateCaption(in))
	})
	t.Run("无效 UTF-8 不 panic 且按宽度 1 计", func(t *testing.T) {
		// 修复前：range 产出 U+FFFD（宽 1）但 len(string(r))==3，hardEnd 越界 panic [:1026]
		in := strings.Repeat("a", 1023) + "\x80" + "x"
		assert.Equal(t, strings.Repeat("a", 1023)+"\x80", truncateCaption(in))
	})
}

// issue #13：Message 携带可选图片字节；空字节等价普通文本消息
func TestNewPhotoMessage(t *testing.T) {
	t.Run("有图", func(t *testing.T) {
		m := NewPhotoMessage("caption", []byte{1, 2, 3})
		assert.True(t, m.HasPhoto())
		assert.Equal(t, "caption", m.Text())
	})
	t.Run("空字节退回文本消息", func(t *testing.T) {
		m := NewPhotoMessage("text", nil)
		assert.False(t, m.HasPhoto())
		assert.Equal(t, "text", m.Text())
	})
	t.Run("NewMessage 无图（回归）", func(t *testing.T) {
		m := NewMessage("text")
		assert.False(t, m.HasPhoto())
		assert.Equal(t, "text", m.Text())
	})
}

// testPNG 生成 n×n 灰度 PNG 字节（photo 测试用，尺寸不影响 bot 层断言）
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))))
	return buf.Bytes()
}

// issue #13：photo 成功路径——multipart 内图片字节与原图一致、caption 与 parse_mode 正确
func TestSendPhotoSuccess(t *testing.T) {
	fake := newFakeTG(false)
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	img := testPNG(t, 100, 100)
	msgID, err := bot.Send(context.Background(), "@chan", NewPhotoMessage("*标题*", img))

	require.NoError(t, err)
	assert.Equal(t, int64(1), msgID)
	photos := fake.photos_()
	require.Len(t, photos, 1)
	assert.Equal(t, "*标题*", photos[0].caption)
	assert.Equal(t, "Markdown", photos[0].parseMode)
	assert.Equal(t, img, photos[0].photo, "multipart 内图片字节应与原图一致")
}

// issue #13 顺序 2：caption parse 400 → 反转义后重试 photo（无 parse_mode），图片保留
func TestSendPhotoCaptionParseFallback(t *testing.T) {
	fake := newFakeTG(false)
	fake.photoFailCaption400 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	img := testPNG(t, 100, 100)
	m := NewPhotoMessage(`\[转义]标题`, img)
	_, err = bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err)
	photos := fake.photos_()
	require.Len(t, photos, 2)
	assert.Equal(t, "Markdown", photos[0].parseMode)
	assert.Equal(t, "", photos[1].parseMode, "plain 重试不应带 parse_mode")
	assert.Equal(t, `[转义]标题`, photos[1].caption, "plain 重试应反转义")
	assert.True(t, m.HasPhoto(), "caption 问题不丢图")
}

// issue #13 顺序 3：图片 400 → 丢弃图片降级 sendMessage，全文带 markdown（plain 与 captionPlain 分离）
func TestSendPhotoPermanent400FallsBackToText(t *testing.T) {
	fake := newFakeTG(false)
	fake.photoFail400 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	m := NewPhotoMessage("*全文标题*", testPNG(t, 100, 100))
	msgID, err := bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err)
	assert.Equal(t, int64(1), msgID)
	assert.Len(t, fake.photos_(), 1, "photo 只尝试一次")
	sends := fake.sends_()
	require.Len(t, sends, 1)
	assert.Equal(t, "*全文标题*", sends[0]["text"], "降级文本应为全文")
	assert.Equal(t, "Markdown", sends[0]["parse_mode"], "降级文本不受 captionPlain 影响")
	assert.False(t, m.HasPhoto(), "图片永久失败后 Message 不再持图")
}

// issue #13 顺序 3：413 同样降级文本
func TestSendPhoto413FallsBackToText(t *testing.T) {
	fake := newFakeTG(false)
	fake.photoFail413 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	m := NewPhotoMessage("t", testPNG(t, 100, 100))
	_, err = bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err)
	assert.False(t, m.HasPhoto())
	assert.Len(t, fake.sends_(), 1)
}

// issue #13 顺序 4：瞬态错误（500）返回错误且图片保留，交外层重试
func TestSendPhotoTransientErrorKeepsPhoto(t *testing.T) {
	fake := newFakeTG(false)
	fake.photoFail500 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	m := NewPhotoMessage("t", testPNG(t, 100, 100))
	_, err = bot.Send(context.Background(), "@chan", m)

	assert.Error(t, err)
	assert.True(t, m.HasPhoto(), "瞬态错误不丢图，外层重试仍走 photo")
	assert.Empty(t, fake.sends_(), "不应降级发文本")
}

// issue #13：连续两次 Send，第二次 multipart 仍为完整字节（reader 不复用）
func TestSendPhotoFreshReaderEachAttempt(t *testing.T) {
	fake := newFakeTG(false)
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	img := testPNG(t, 100, 100)
	m := NewPhotoMessage("t", img)
	_, err1 := bot.Send(context.Background(), "@chan", m)
	// 同一 Message 实例再发一次：模拟外层 sendWithRetry 用同一条消息重试的真实场景
	_, err2 := bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err1)
	require.NoError(t, err2)
	photos := fake.photos_()
	require.Len(t, photos, 2)
	assert.Equal(t, img, photos[1].photo, "第二次上传必须是完整字节")
}

// issue #13 组合边界：photo 400 永久失败降级文本后，文本路径自身仍要能走
// markdown → parse 400 → 纯文本重发的完整降级链（photo 降级不得吞掉文本降级）
func TestSendPhotoFallbackTextPlainCombo(t *testing.T) {
	fake := newFakeTG(true) // sendMessage 首次返回 parse entities 400
	fake.photoFail400 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	m := NewPhotoMessage(`\[组合]标题 \*4.35米`, testPNG(t, 100, 100))
	msgID, err := bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err)
	assert.Equal(t, int64(1), msgID)
	assert.Len(t, fake.photos_(), 1, "图片永久失败只尝试一次 photo")
	assert.False(t, m.HasPhoto())
	sends := fake.sends_()
	require.Len(t, sends, 2, "photo 降级后文本应走 markdown → 纯文本两级")
	assert.Equal(t, "Markdown", sends[0]["parse_mode"])
	_, hasParseMode := sends[1]["parse_mode"]
	assert.False(t, hasParseMode, "文本降级重发不应带 parse_mode")
	assert.Equal(t, "[组合]标题 *4.35米", sends[1]["text"], "最终文本应为反转义全文")
}

// issue #13 组合边界：caption md parse 400 → photo plain 重试恰逢图片永久 400 →
// 降级全文文本。锁死时序：captionPlain 已置 true 但 plain 未动，
// 降级文本必须仍带 Markdown 且为反转义前的全文原文
func TestSendPhotoCaptionPlainThenPermanentFallsBackWithMarkdown(t *testing.T) {
	fake := newFakeTG(false)
	fake.photoFailCaption400 = true
	fake.photoFail400 = true
	defer fake.server.Close()
	t.Setenv("TELEGRAM_API_URL", fake.server.URL)
	bot, err := NewBot(context.Background(), "1:test")
	require.NoError(t, err)

	m := NewPhotoMessage(`\[标题]正文`, testPNG(t, 100, 100))
	msgID, err := bot.Send(context.Background(), "@chan", m)

	require.NoError(t, err)
	assert.Equal(t, int64(1), msgID)
	photos := fake.photos_()
	require.Len(t, photos, 2, "caption parse 重试一次，共两次 photo")
	assert.Equal(t, "Markdown", photos[0].parseMode)
	assert.Equal(t, "", photos[1].parseMode, "caption plain 重试不带 parse_mode")
	assert.Equal(t, "[标题]正文", photos[1].caption)
	assert.False(t, m.HasPhoto())
	sends := fake.sends_()
	require.Len(t, sends, 1, "图片永久失败后只发一次文本")
	assert.Equal(t, "Markdown", sends[0]["parse_mode"], "captionPlain 不得污染 plain，全文仍走 markdown")
	assert.Equal(t, `\[标题]正文`, sends[0]["text"], "降级文本为反转义前的全文原文")
}
