package rss

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
	h := &RssHandler{bot: bot}
	h.sleepFn = bot.recordSleep
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

// 边界：retry_after 超过 120s 上限时等待被 cap，防服务端异常值拖死 goroutine
func TestSendWithRetryFloodWaitCapped(t *testing.T) {
	bot := &scriptBot{script: []error{rateErr(300), nil}}
	h := newRetryTestHandler(bot)

	ok := h.sendWithRetry("@ch", "msg", "标题")

	require.True(t, ok)
	_, sleeps := bot.snapshot()
	require.Len(t, sleeps, 1)
	assert.Equal(t, 120*time.Second, sleeps[0], "300s 应被 cap 到 120s")
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
