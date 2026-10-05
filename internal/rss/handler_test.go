package rss

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/storage"
	"github.com/Hootrix/rss2telegram/internal/telegram"
	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue #4：title 含裸 Markdown 实体字符导致 Telegram 400，
// formatMessage 需对 title 字段做 legacy Markdown 转义（数据域转义，
// 模板手写语法与操作链参数不受影响）
func TestFormatMessageTitleEscape(t *testing.T) {
	handler := &RssHandler{}

	// issue #4 线上真实失败标题
	item := &gofeed.Item{
		Title: "[特惠产品]露营多挂点长方形庇护所天幕 3*4.35米 FRESH & BLACK技术 XL ¥299.9 5折",
		Link:  "https://example.com/p/1",
	}

	t.Run("默认模板下 title 的 * 和 [ 被转义", func(t *testing.T) {
		result := handler.formatMessage(item, "{title}\n\n{link}")
		expected := `\[特惠产品]露营多挂点长方形庇护所天幕 3\*4.35米 FRESH & BLACK技术 XL ¥299.9 5折

https://example.com/p/1`
		assert.Equal(t, expected, result)
	})

	t.Run("模板手写的粗体语法保留，仅 title 值内特殊字符转义", func(t *testing.T) {
		// 模板的包裹 * 不受影响；title 内的 * 转义后恰好不与包裹符错配
		item2 := &gofeed.Item{Title: "[特惠产品]*CN Venum 拳击手套 ¥269.9"}
		result := handler.formatMessage(item2, "*{title}*")
		assert.Equal(t, `*\[特惠产品]\*CN Venum 拳击手套 ¥269.9*`, result)
	})

	t.Run("description 的裸特殊字符不转义（行为锁定，由降级纯文本兜底）", func(t *testing.T) {
		item3 := &gofeed.Item{
			Description: "尺寸 3*4.35米 _型号_",
		}
		result := handler.formatMessage(item3, "{description}")
		assert.Equal(t, "尺寸 3*4.35米 _型号_", result)
	})
}

// scriptBot 按 Send 调用次序返回预编排的错误脚本，nil 表示发送成功；
// 脚本耗尽后重复最后一个元素。记录每次调用与 handler 实际 sleep 时长，
// 供断言 issue #6 的 flood/普通双计数重试语义
type scriptBot struct {
	mu     sync.Mutex
	script []error
	calls  int
	sleeps []time.Duration
}

func (b *scriptBot) Send(channel, message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.calls
	if i >= len(b.script) {
		i = len(b.script) - 1
	}
	b.calls++
	return b.script[i]
}

func (b *scriptBot) recordSleep(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sleeps = append(b.sleeps, d)
}

func (b *scriptBot) snapshot() (int, []time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, append([]time.Duration{}, b.sleeps...)
}

func rateErr(seconds int) *telegram.RateLimitError {
	return telegram.NewRateLimitError(
		time.Duration(seconds)*time.Second,
		errors.New("telegram: Too Many Requests (429)"),
	)
}

func newRetryTestHandler(bot *scriptBot) *RssHandler {
	// h := &RssHandler{bot: bot}
	// h.sleepFn = bot.recordSleep
	// return h
	var mu sync.Mutex
	now := time.Unix(0, 0)
	h := &RssHandler{bot: bot}
	h.nowFn = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	h.sleepFn = func(d time.Duration) {
		bot.recordSleep(d)
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	return h
}

// issue #6 核心语义：连续 429 按 retry_after 等待后重试，flood 重试独立计数
// （上限 5），不消耗普通错误 3 次配额——5 次 429 后第 6 次发送应成功
func TestSendWithRetryFloodRetriesIndependentOfNormalQuota(t *testing.T) {
	bot := &scriptBot{script: []error{
		rateErr(2), rateErr(2), rateErr(2), rateErr(2), rateErr(2),
		nil, // 第 6 次：5 次 flood 重试配额内成功
	}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry("@ch", "msg", "标题")

	require.True(t, ok, "5 次 flood 后第 6 次应成功")
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 6, calls, "首次 + 5 次 flood 重试")
	require.Len(t, sleeps, 5)
	for _, d := range sleeps {
		assert.Equal(t, 3*time.Second, d, "每次 flood 等待应为 retry_after 2s + 1s 缓冲")
	}
}

// 边界：flood 重试超过 5 次上限后放弃（返回 false），且全程未消耗普通重试
func TestSendWithRetryFloodRetryExhaustion(t *testing.T) {
	// 7 连 429：若误用普通配额（3 次）会更早放弃
	bot := &scriptBot{script: []error{
		rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1),
	}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry("@ch", "msg", "标题")

	assert.False(t, ok, "超过 5 次 flood 重试应放弃")
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 6, calls, "首次 + 5 次 flood 重试后，第 6 次 429 触发放弃")
	assert.Len(t, sleeps, 5, "放弃的那次不应再 sleep")
}

// 旧测试预期保留：截短合法的服务端冷却会提前重发，长等待应延后频道处理
// 边界：retry_after 超过 120s 上限时等待被 cap，防服务端异常值拖死 goroutine
//
//	func TestSendWithRetryFloodWaitCapped(t *testing.T) {
//		bot := &scriptBot{script: []error{rateErr(300), nil}}
//		h := newRetryTestHandler(bot)
//
//		ok := h.sendWithRetry("@ch", "msg", "标题")
//
//		require.True(t, ok)
//		_, sleeps := bot.snapshot()
//		require.Len(t, sleeps, 1)
//		assert.Equal(t, 120*time.Second, sleeps[0], "300s 应被 cap 到 120s")
//	}
func TestSendWithRetryLongFloodWaitDefersChannel(t *testing.T) {
	bot := &scriptBot{script: []error{rateErr(300), nil}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry("@Channel", "msg", "标题"))
	assert.False(t, h.sendWithRetry("channel", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 1, calls, "冷却到期前同频道的其他消息也不得发送")
	assert.Empty(t, sleeps, "超过阻塞预算应延后处理，不应截短等待")

	assert.True(t, h.sendWithRetry("@other", "msg", "其他频道"))
	calls, _ = bot.snapshot()
	assert.Equal(t, 2, calls, "其他频道不应被阻塞")

	originalNow := h.nowFn
	h.nowFn = func() time.Time { return originalNow().Add(300 * time.Second) }
	assert.True(t, h.sendWithRetry("@CHANNEL", "next", "冷却后补推"))
	calls, sleeps = bot.snapshot()
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{time.Second}, sleeps, "300s 到期后仍须等完 1s 缓冲")
}

func TestSendWithRetrySharesChannelInterval(t *testing.T) {
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)
	start := make(chan struct{})
	results := make(chan bool, 2)
	for _, channel := range []string{"@Channel", "channel"} {
		go func(channel string) {
			<-start
			results <- h.sendWithRetry(channel, "msg", "标题")
		}(channel)
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case ok := <-results:
			require.True(t, ok)
		case <-time.After(5 * time.Second):
			t.Fatal("同频道发送未完成")
		}
	}

	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	require.Len(t, sleeps, 1, "并发 feed 必须共享同频道的发送间隔")
	// assert.Positive(t, sleeps[0])
	// assert.LessOrEqual(t, sleeps[0], sendInterval)
	assert.Equal(t, sendInterval, sleeps[0])
}

func TestSendWithRetrySharesDigitPrefixedChannel(t *testing.T) {
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)

	assert.True(t, h.sendWithRetry("123Feed", "msg", "标题"))
	assert.True(t, h.sendWithRetry("@123feed", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{sendInterval}, sleeps)
}

func TestSendWithRetryKeepsExhaustedFloodCooldown(t *testing.T) {
	bot := &scriptBot{script: []error{
		rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), nil,
	}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry("@ch", "msg", "标题"))
	assert.True(t, h.sendWithRetry("@ch", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 7, calls)
	require.Len(t, sleeps, 6, "放弃上一条消息后，下一条仍须遵守最后一次 429 的冷却")
	// assert.Positive(t, sleeps[5])
	assert.Equal(t, 2*time.Second, sleeps[5])
}

func TestSendWithRetryRejectsInvalidFloodWait(t *testing.T) {
	cases := []struct {
		name string
		wait time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
		{"buffer_overflow", time.Duration(1<<63-1) - time.Second + time.Nanosecond},
		{"duration_overflow", time.Duration(1<<63 - 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot := &scriptBot{script: []error{
				telegram.NewRateLimitError(tc.wait, errors.New("429")), nil,
			}}
			h := newRetryTestHandler(bot)

			assert.False(t, h.sendWithRetry("@ch", "msg", "标题"))
			calls, sleeps := bot.snapshot()
			assert.Equal(t, 1, calls, "异常等待值应终止当前消息重试")
			assert.Empty(t, sleeps, "不得 sleep 非正数或溢出的等待值")
		})
	}
}

// 对照组：普通错误维持原有语义——总共 3 次尝试后放弃，429 计数不受影响
func TestSendWithRetryNormalErrorsExhaustQuota(t *testing.T) {
	generic := errors.New("telegram: Internal Server Error (500)")
	bot := &scriptBot{script: []error{generic, generic, generic}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry("@ch", "msg", "标题")

	assert.False(t, ok)
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 3, calls, "普通错误应维持 3 次尝试上限")
	require.Len(t, sleeps, 2, "指数退避应发生在前两次失败后")
	// ExponentialBackoffWithJitter 底线：1s、2s（含随机抖动只会更长）
	assert.GreaterOrEqual(t, sleeps[0], time.Second)
	assert.GreaterOrEqual(t, sleeps[1], 2*time.Second)
}

// 混合场景：429 与普通错误交错，两种计数互不挤占
// （2 次 flood + 3 次普通 = 5 次失败后才放弃；若共享同一配额则 3 次就放弃）
func TestSendWithRetryMixedErrorsKeepSeparateQuotas(t *testing.T) {
	generic := fmt.Errorf("telegram: Bad Request (400)")
	bot := &scriptBot{script: []error{
		rateErr(1), rateErr(1), generic, generic, generic,
	}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry("@ch", "msg", "标题")

	assert.False(t, ok)
	calls, _ := bot.snapshot()
	assert.Equal(t, 5, calls, "flood 2 次 + 普通 3 次，双计数独立")
}

// issue #6 修复方向 2 的落地形式：发送间隔常量 ≥3s（Telegram 单频道约 20 条/分钟）
func TestSendIntervalWithinChannelLimit(t *testing.T) {
	assert.GreaterOrEqual(t, sendInterval, 3*time.Second, "发送间隔不得低于 3s")
}

func TestSendWithRetryFloodWaitBudgetBoundary(t *testing.T) {
	cases := []struct {
		name        string
		wait        time.Duration
		wantSuccess bool
	}{
		{"within_budget", maxFloodWait - time.Second, true},
		{"over_budget", maxFloodWait - time.Second + time.Nanosecond, false},
		{"largest_valid_duration", time.Duration(1<<63-1) - time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot := &scriptBot{script: []error{
				telegram.NewRateLimitError(tc.wait, errors.New("429")), nil,
			}}
			h := newRetryTestHandler(bot)

			assert.Equal(t, tc.wantSuccess, h.sendWithRetry("@ch", "msg", "标题"))
			calls, sleeps := bot.snapshot()
			if tc.wantSuccess {
				assert.Equal(t, 2, calls)
				assert.Equal(t, []time.Duration{maxFloodWait}, sleeps)
			} else {
				assert.Equal(t, 1, calls)
				assert.Empty(t, sleeps)
			}
		})
	}
}

func TestSendWithRetryRecognizesWrappedRateLimitError(t *testing.T) {
	bot := &scriptBot{script: []error{fmt.Errorf("send: %w", rateErr(1)), nil}}
	h := newRetryTestHandler(bot)

	assert.True(t, h.sendWithRetry("@ch", "msg", "标题"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{2 * time.Second}, sleeps)
}

type sendTestFunc func(string, string) error

func (f sendTestFunc) Send(channel, message string) error { return f(channel, message) }

func TestSendWithRetryDifferentChannelsCanProceed(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan bool, 1)
	h := &RssHandler{bot: sendTestFunc(func(channel, message string) error {
		if channel == "@blocked" {
			close(started)
			<-release
		}
		return nil
	})}
	defer func() {
		close(release)
		select {
		case ok := <-firstDone:
			assert.True(t, ok)
		case <-time.After(5 * time.Second):
			t.Error("被阻塞的发送未退出")
		}
	}()
	go func() { firstDone <- h.sendWithRetry("@blocked", "msg", "第一频道") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("第一频道未开始发送")
	}

	secondDone := make(chan bool, 1)
	go func() { secondDone <- h.sendWithRetry("@other", "msg", "其他频道") }()
	select {
	case ok := <-secondDone:
		assert.True(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("其他频道不应等待第一频道")
	}
}

func TestProcessFeedsSharedChannelCooldown(t *testing.T) {
	const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><link>https://example.com/</link><description>test</description><item><title>item</title><link>https://example.com/item</link><guid>item-1</guid></item></channel></rss>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		if _, err := fmt.Fprint(w, feedXML); err != nil {
			t.Errorf("write RSS: %v", err)
		}
	}))
	defer server.Close()
	store, err := storage.NewStorage(t.TempDir())
	require.NoError(t, err)
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "a", URL: server.URL + "/a", Channels: []string{"@Channel"}, FirstPush: true, Template: "{title}"},
		{Name: "b", URL: server.URL + "/b", Channels: []string{"channel"}, FirstPush: true, Template: "{title}"},
	}}
	bot := &scriptBot{script: []error{rateErr(300), nil}}
	h := newRetryTestHandler(bot)
	h.config, h.storage = cfg, store
	h.parser = gofeed.NewParser()
	// 预初始化 gofeed 的惰性字段，隔离既有解析器竞态，只验证并发发送调度
	h.parser.Client = server.Client()
	h.parser.RSSTranslator = &gofeed.DefaultRSSTranslator{}

	require.NoError(t, h.ProcessFeeds())
	h.UpdateConfig(cfg)
	require.NoError(t, h.ProcessFeeds())
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 1, calls, "两个 feed、配置重载及下一轮检查共享同一冷却")
	assert.Empty(t, sleeps)
	for _, feed := range cfg.Feeds {
		assert.False(t, store.IsItemSeen(feed.URL, feed.Name, feed.Channels[0], "item-1"))
	}

	originalNow := h.nowFn
	h.nowFn = func() time.Time { return originalNow().Add(301 * time.Second) }
	require.NoError(t, h.ProcessFeeds())
	calls, _ = bot.snapshot()
	assert.Equal(t, 3, calls, "完整冷却到期后两个 feed 都应补推")
	for _, feed := range cfg.Feeds {
		assert.True(t, store.IsItemSeen(feed.URL, feed.Name, feed.Channels[0], "item-1"))
	}
	require.NoError(t, h.ProcessFeeds())
	calls, _ = bot.snapshot()
	assert.Equal(t, 3, calls, "成功标记 seen 后不应重复推送")
}
