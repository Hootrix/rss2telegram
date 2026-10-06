package rss

import (
	"context"
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
		result := handler.formatMessage(item, "{title}\n\n{link}", "")
		expected := `\[特惠产品]露营多挂点长方形庇护所天幕 3\*4.35米 FRESH & BLACK技术 XL ¥299.9 5折

https://example.com/p/1`
		assert.Equal(t, expected, result)
	})

	t.Run("模板手写的粗体语法保留，仅 title 值内特殊字符转义", func(t *testing.T) {
		// 模板的包裹 * 不受影响；title 内的 * 转义后恰好不与包裹符错配
		item2 := &gofeed.Item{Title: "[特惠产品]*CN Venum 拳击手套 ¥269.9"}
		result := handler.formatMessage(item2, "*{title}*", "")
		assert.Equal(t, `*\[特惠产品]\*CN Venum 拳击手套 ¥269.9*`, result)
	})

	t.Run("description 的裸特殊字符不转义（行为锁定，由降级纯文本兜底）", func(t *testing.T) {
		item3 := &gofeed.Item{
			Description: "尺寸 3*4.35米 _型号_",
		}
		result := handler.formatMessage(item3, "{description}", "")
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

// func (b *scriptBot) Send(channel, message string) error {
func (b *scriptBot) Send(ctx context.Context, channel string, message *telegram.Message) error {
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

//	func rateErr(seconds int) *telegram.RateLimitError {
//		return telegram.NewRateLimitError(
//			time.Duration(seconds)*time.Second,
//			errors.New("telegram: Too Many Requests (429)"),
//		)
//	}
func rateErr(seconds int) error {
	return telegram.NewRateLimitError(int64(seconds), errors.New("telegram: Too Many Requests (429)"))
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
	// h.sleepFn = func(d time.Duration) {
	// 	bot.recordSleep(d)
	// 	mu.Lock()
	// 	now = now.Add(d)
	// 	mu.Unlock()
	// }
	h.waitFn = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bot.recordSleep(d)
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return nil
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

	ok := h.sendWithRetry(context.Background(), "@ch", "msg", "标题")

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

	ok := h.sendWithRetry(context.Background(), "@ch", "msg", "标题")

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
//		ok := h.sendWithRetry(context.Background(), "@ch", "msg", "标题")
//
//		require.True(t, ok)
//		_, sleeps := bot.snapshot()
//		require.Len(t, sleeps, 1)
//		assert.Equal(t, 120*time.Second, sleeps[0], "300s 应被 cap 到 120s")
//	}
func TestSendWithRetryLongFloodWaitDefersChannel(t *testing.T) {
	bot := &scriptBot{script: []error{rateErr(300), nil}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry(context.Background(), "@Channel", "msg", "标题"))
	assert.False(t, h.sendWithRetry(context.Background(), "channel", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 1, calls, "冷却到期前同频道的其他消息也不得发送")
	assert.Empty(t, sleeps, "超过阻塞预算应延后处理，不应截短等待")

	assert.True(t, h.sendWithRetry(context.Background(), "@other", "msg", "其他频道"))
	calls, _ = bot.snapshot()
	assert.Equal(t, 2, calls, "其他频道不应被阻塞")

	originalNow := h.nowFn
	h.nowFn = func() time.Time { return originalNow().Add(300 * time.Second) }
	assert.True(t, h.sendWithRetry(context.Background(), "@CHANNEL", "next", "冷却后补推"))
	calls, sleeps = bot.snapshot()
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{time.Second}, sleeps, "300s 到期后仍须等完 1s 缓冲")
}

/*
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
*/

func TestSendWithRetrySharesChannelInterval(t *testing.T) {
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)
	assert.True(t, h.sendWithRetry(context.Background(), "@Channel", "msg", "标题"))
	assert.True(t, h.sendWithRetry(context.Background(), "channel", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{sendInterval}, sleeps)
}

func TestSendWithRetrySharesDigitPrefixedChannel(t *testing.T) {
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)

	assert.True(t, h.sendWithRetry(context.Background(), "123Feed", "msg", "标题"))
	assert.True(t, h.sendWithRetry(context.Background(), "@123feed", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{sendInterval}, sleeps)
}

func TestSendWithRetryKeepsExhaustedFloodCooldown(t *testing.T) {
	bot := &scriptBot{script: []error{
		rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), rateErr(1), nil,
	}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
	assert.True(t, h.sendWithRetry(context.Background(), "@ch", "next", "下一条"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 7, calls)
	require.Len(t, sleeps, 6, "放弃上一条消息后，下一条仍须遵守最后一次 429 的冷却")
	// assert.Positive(t, sleeps[5])
	assert.Equal(t, 2*time.Second, sleeps[5])
}

/*
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

			assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
			calls, sleeps := bot.snapshot()
			assert.Equal(t, 1, calls, "异常等待值应终止当前消息重试")
			assert.Empty(t, sleeps, "不得 sleep 非正数或溢出的等待值")
		})
	}
}
*/

func TestSendWithRetryInvalidSecondsUseNormalQuota(t *testing.T) {
	for _, seconds := range []int64{0, -1, 9_223_372_036, 9_223_372_037} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			bot := &scriptBot{script: []error{telegram.NewRateLimitError(seconds, errors.New("429"))}}
			h := newRetryTestHandler(bot)
			assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
			calls, sleeps := bot.snapshot()
			assert.Equal(t, 3, calls)
			assert.Len(t, sleeps, 2)
		})
	}
}

// 对照组：普通错误维持原有语义——总共 3 次尝试后放弃，429 计数不受影响
func TestSendWithRetryNormalErrorsExhaustQuota(t *testing.T) {
	generic := errors.New("telegram: Internal Server Error (500)")
	bot := &scriptBot{script: []error{generic, generic, generic}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry(context.Background(), "@ch", "msg", "标题")

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

	ok := h.sendWithRetry(context.Background(), "@ch", "msg", "标题")

	assert.False(t, ok)
	calls, _ := bot.snapshot()
	assert.Equal(t, 5, calls, "flood 2 次 + 普通 3 次，双计数独立")
}

// issue #6 修复方向 2 的落地形式：发送间隔常量 ≥3s（Telegram 单频道约 20 条/分钟）
func TestSendWithRetryCumulativeBudget(t *testing.T) {
	bot := &scriptBot{script: []error{rateErr(2)}}
	h := newRetryTestHandler(bot)
	h.messageBudget = 7 * time.Second

	assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{3 * time.Second, 3 * time.Second}, sleeps)
}

func TestSendWithRetryCancellationInterruptsBackoff(t *testing.T) {
	bot := &scriptBot{script: []error{errors.New("500")}}
	h := newRetryTestHandler(bot)
	entered := make(chan struct{})
	h.waitFn = func(ctx context.Context, _ time.Duration) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- h.sendWithRetry(ctx, "@ch", "msg", "标题") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入退避等待")
	}
	cancel()
	select {
	case ok := <-done:
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("取消未中断退避")
	}
	calls, _ := bot.snapshot()
	assert.Equal(t, 1, calls)
}

func TestSendWithRetryCancellationInterruptsChannelCooldown(t *testing.T) {
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)
	require.True(t, h.sendWithRetry(context.Background(), "@ch", "msg", "第一条"))
	entered := make(chan struct{})
	h.waitFn = func(ctx context.Context, _ time.Duration) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- h.sendWithRetry(ctx, "@ch", "next", "下一条") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入频道冷却等待")
	}
	cancel()
	select {
	case ok := <-done:
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("取消未中断频道冷却")
	}
	calls, _ := bot.snapshot()
	assert.Equal(t, 1, calls)
}

func TestSendWithRetryBusyChannelDefersWithoutQueueing(t *testing.T) {
	started := make(chan struct{})
	h := &RssHandler{bot: sendTestFunc(func(ctx context.Context, channel string, _ *telegram.Message) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan bool, 1)
	go func() { firstDone <- h.sendWithRetry(ctx, "@Channel", "msg", "第一条") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("发送未启动")
	}
	secondDone := make(chan bool, 1)
	go func() { secondDone <- h.sendWithRetry(context.Background(), "channel", "next", "下一条") }()
	select {
	case ok := <-secondDone:
		assert.False(t, ok, "忙频道应延后，不在频道锁后排队")
	case <-time.After(time.Second):
		t.Fatal("忙频道发生阻塞排队")
	}
	cancel()
	select {
	case ok := <-firstDone:
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("取消未中断在途发送")
	}
}

func TestProcessFeedsTwoFloodedFeedsDoNotStarveThird(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	bot := sendTestFunc(func(_ context.Context, channel string, _ *telegram.Message) error {
		mu.Lock()
		calls[channel]++
		mu.Unlock()
		if channel != "@ready" {
			return rateErr(119)
		}
		return nil
	})
	h := newProcessTestHandler(t, []string{"@flood_a", "@flood_b", "@ready"}, bot)
	require.NoError(t, h.ProcessFeeds(context.Background()))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int{"@flood_a": 1, "@flood_b": 1, "@ready": 1}, calls)
}

// 旧预期保留：整轮共享预算会让排队的第三个 feed 被取消，即靠后 feed 被饿死，已改为每 feed 独立预算
// func TestProcessFeedsDeadlineCancelsFetchAndQueuedFeed(t *testing.T) {
// 	started := make(chan struct{}, 3)
// 	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		started <- struct{}{}
// 		<-r.Context().Done()
// 	}))
// 	defer server.Close()
// 	store, err := storage.NewStorage(t.TempDir())
// 	require.NoError(t, err)
// 	cfg := &config.Config{}
// 	for i := 0; i < 3; i++ {
// 		cfg.Feeds = append(cfg.Feeds, config.FeedConfig{
// 			Name: fmt.Sprintf("f%d", i), URL: server.URL, Channels: []string{"@ch"}, FirstPush: true,
// 		})
// 	}
// 	h := NewRssHandler(cfg, &scriptBot{script: []error{nil}}, store, nil)
// 	h.roundBudget = 100 * time.Millisecond
// 	err = h.ProcessFeeds(context.Background())
// 	assert.ErrorIs(t, err, context.DeadlineExceeded)
// 	assert.LessOrEqual(t, len(started), 2, "第三个 feed 排队期间应被整轮预算取消")
// }

// 挂起的 RSS 源只耗尽自己的 feed 预算，排队的 feed 仍要被拉取；拉取超时仍上报为错误
func TestProcessFeedsHangingFetchDoesNotStarveQueuedFeed(t *testing.T) {
	started := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	store, err := storage.NewStorage(t.TempDir())
	require.NoError(t, err)
	cfg := &config.Config{}
	for i := 0; i < 3; i++ {
		cfg.Feeds = append(cfg.Feeds, config.FeedConfig{
			Name: fmt.Sprintf("f%d", i), URL: server.URL, Channels: []string{"@ch"}, FirstPush: true,
		})
	}
	h := NewRssHandler(cfg, &scriptBot{script: []error{nil}}, store, nil)
	h.feedBudget = 100 * time.Millisecond
	err = h.ProcessFeeds(context.Background())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Len(t, started, 3, "第三个 feed 不应因前两个挂起而被跳过")
}

// 发送阶段耗尽 feed 预算属于积压延后：不报错、不标 seen，且不影响排队中的其他 feed
func TestProcessFeedsBacklogFeedsDoNotStarveLaterFeed(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	bot := sendTestFunc(func(ctx context.Context, channel string, _ *telegram.Message) error {
		mu.Lock()
		calls[channel]++
		mu.Unlock()
		if channel == "@ready" {
			return nil
		}
		<-ctx.Done() // 模拟积压：发送一直占满 feed 预算
		return ctx.Err()
	})
	h := newProcessTestHandler(t, []string{"@slow_a", "@slow_b", "@ready"}, bot)
	h.feedBudget = 100 * time.Millisecond

	require.NoError(t, h.ProcessFeeds(context.Background()), "积压延后不应上报为错误")
	mu.Lock()
	assert.Equal(t, map[string]int{"@slow_a": 1, "@slow_b": 1, "@ready": 1}, calls)
	mu.Unlock()
	cfg := h.config
	assert.False(t, h.storage.IsItemSeen(cfg.Feeds[0].URL, cfg.Feeds[0].Name, "@slow_a", "item-1"))
	assert.True(t, h.storage.IsItemSeen(cfg.Feeds[2].URL, cfg.Feeds[2].Name, "@ready", "item-1"))
}

func TestProcessFeedBudgetExhaustedDefersRemainingChannels(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	bot := sendTestFunc(func(ctx context.Context, channel string, _ *telegram.Message) error {
		mu.Lock()
		calls[channel]++
		mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	})
	h := newProcessTestHandler(t, []string{"@slow"}, bot)
	feed := h.config.Feeds[0]
	feed.Channels = []string{"@slow", "@next"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	require.NoError(t, h.processFeed(ctx, feed))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int{"@slow": 1}, calls, "预算耗尽后剩余频道延后到下轮")
}

func TestProcessFeedsParentCancelReportsError(t *testing.T) {
	h := newProcessTestHandler(t, []string{"@a"}, &scriptBot{script: []error{nil}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, h.ProcessFeeds(ctx), context.Canceled)
}

func newProcessTestHandler(t *testing.T, channels []string, bot TelegramBot) *RssHandler {
	t.Helper()
	const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><link>https://example.com/</link><description>test</description><item><title>item</title><guid>item-1</guid></item></channel></rss>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, feedXML); err != nil {
			t.Errorf("write RSS: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	store, err := storage.NewStorage(t.TempDir())
	require.NoError(t, err)
	cfg := &config.Config{}
	for i, channel := range channels {
		cfg.Feeds = append(cfg.Feeds, config.FeedConfig{
			Name: fmt.Sprintf("f%d", i), URL: server.URL + fmt.Sprintf("/%d", i),
			Channels: []string{channel}, FirstPush: true, Template: "{title}",
		})
	}
	return NewRssHandler(cfg, bot, store, nil)
}

func TestSendIntervalWithinChannelLimit(t *testing.T) {
	assert.GreaterOrEqual(t, sendInterval, 3*time.Second, "发送间隔不得低于 3s")
}

func TestSendWithRetryRepeated119SecondsDefers(t *testing.T) {
	bot := &scriptBot{script: []error{rateErr(119)}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 1, calls, "119s 冷却应立即延后，不能在单条消息里等待五轮")
	assert.Empty(t, sleeps)
}

func TestSendWithRetryInvalidRetryAfterUsesNormalQuota(t *testing.T) {
	bot := &scriptBot{script: []error{
		telegram.NewRateLimitError(0, errors.New("429")),
	}}
	h := newRetryTestHandler(bot)

	assert.False(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 3, calls, "非法 retry_after 应统一走普通错误配额")
	assert.Len(t, sleeps, 2)
}

func TestProcessFeedDoesNotWaitAfterSendingDifferentChannels(t *testing.T) {
	const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><link>https://example.com/</link><description>test</description><item><title>item</title><guid>item-1</guid></item></channel></rss>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, feedXML); err != nil {
			t.Errorf("write RSS: %v", err)
		}
	}))
	defer server.Close()
	store, err := storage.NewStorage(t.TempDir())
	require.NoError(t, err)
	bot := &scriptBot{script: []error{nil}}
	h := newRetryTestHandler(bot)
	h.storage, h.parser = store, gofeed.NewParser()

	err = h.processFeed(context.Background(), config.FeedConfig{
		Name: "f", URL: server.URL, Channels: []string{"@a", "@b"}, FirstPush: true, Template: "{title}",
	})
	require.NoError(t, err)
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Empty(t, sleeps, "不同频道没有冷却，发送后不应再占用 feed 名额等待 3s")
}

/*
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

			assert.Equal(t, tc.wantSuccess, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
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
*/

func TestSendWithRetryShortWaitThreshold(t *testing.T) {
	for _, seconds := range []int64{9, 10, 119, 9_223_372_035} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			bot := &scriptBot{script: []error{telegram.NewRateLimitError(seconds, errors.New("429")), nil}}
			h := newRetryTestHandler(bot)
			assert.Equal(t, seconds == 9, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
			calls, sleeps := bot.snapshot()
			if seconds == 9 {
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

	assert.True(t, h.sendWithRetry(context.Background(), "@ch", "msg", "标题"))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{2 * time.Second}, sleeps)
}

// type sendTestFunc func(string, string) error
// func (f sendTestFunc) Send(channel, message string) error { return f(channel, message) }
type sendTestFunc func(context.Context, string, *telegram.Message) error

func (f sendTestFunc) Send(ctx context.Context, channel string, message *telegram.Message) error {
	return f(ctx, channel, message)
}

func TestSendWithRetryDifferentChannelsCanProceed(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan bool, 1)
	h := &RssHandler{bot: sendTestFunc(func(_ context.Context, channel string, _ *telegram.Message) error {
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
	go func() { firstDone <- h.sendWithRetry(context.Background(), "@blocked", "msg", "第一频道") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("第一频道未开始发送")
	}

	secondDone := make(chan bool, 1)
	go func() { secondDone <- h.sendWithRetry(context.Background(), "@other", "msg", "其他频道") }()
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

	require.NoError(t, h.ProcessFeeds(context.Background()))
	h.UpdateConfig(cfg)
	require.NoError(t, h.ProcessFeeds(context.Background()))
	calls, sleeps := bot.snapshot()
	assert.Equal(t, 1, calls, "两个 feed、配置重载及下一轮检查共享同一冷却")
	assert.Empty(t, sleeps)
	for _, feed := range cfg.Feeds {
		assert.False(t, store.IsItemSeen(feed.URL, feed.Name, feed.Channels[0], "item-1"))
	}

	originalNow := h.nowFn
	h.nowFn = func() time.Time { return originalNow().Add(301 * time.Second) }
	// 忙频道快速延后而非排队，同频道的两个 feed 最多经下一轮完成补推
	require.NoError(t, h.ProcessFeeds(context.Background()))
	require.NoError(t, h.ProcessFeeds(context.Background()))
	calls, _ = bot.snapshot()
	assert.Equal(t, 3, calls, "完整冷却到期后两个 feed 都应补推")
	for _, feed := range cfg.Feeds {
		assert.True(t, store.IsItemSeen(feed.URL, feed.Name, feed.Channels[0], "item-1"))
	}
	require.NoError(t, h.ProcessFeeds(context.Background()))
	calls, _ = bot.snapshot()
	assert.Equal(t, 3, calls, "成功标记 seen 后不应重复推送")
}
