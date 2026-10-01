package telegram

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Hootrix/rss2telegram/internal/tgmd"

	tele "gopkg.in/telebot.v3"
)

type Bot struct {
	bot *tele.Bot
}

// RateLimitError 表示 Telegram 429 限速，携带服务端指示的等待时长。
// 独立成项目内类型：handler 层用 errors.As 识别即可，无需 import telebot
type RateLimitError struct {
	RetryAfter time.Duration
	err        error // 原始 telebot 错误，Unwrap 后保持链路可追溯
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram rate limited, retry after %v: %v", e.RetryAfter, e.err)
}

func (e *RateLimitError) Unwrap() error { return e.err }

// NewRateLimitError 导出构造函数，供测试与其他调用方构造（err 字段保持未导出）
func NewRateLimitError(retryAfter time.Duration, err error) *RateLimitError {
	return &RateLimitError{RetryAfter: retryAfter, err: err}
}

// channelRecipient 直接以配置字符串充当 tele.Recipient（接口只要求返回
// 一个可作为 chat_id 的字符串），使 sendMessage 的 chat_id 原生接受
// @username 或数字 ID，省掉每条消息一次 getChat 预查询
type channelRecipient string

func (r channelRecipient) Recipient() string { return string(r) }

// newChannelRecipient 归一化频道标识：@name 原样透传；裸名补 @；
// 数字/负数 ID（-100...）原样透传
func newChannelRecipient(channel string) channelRecipient {
	if channel == "" || channel[0] == '@' || channel[0] == '-' || (channel[0] >= '0' && channel[0] <= '9') {
		return channelRecipient(channel)
	}
	return channelRecipient("@" + channel)
}

func NewBot(token string) (*Bot, error) {
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	// 可测试性注入点：设置 TELEGRAM_API_URL 时指向自定义 API 地址（如本地集成测试的假服务器）
	// 为空时保持官方默认 https://api.telegram.org，生产行为不变
	if url := os.Getenv("TELEGRAM_API_URL"); url != "" {
		pref.URL = url
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		return nil, err
	}

	return &Bot{bot: b}, nil
}

func (b *Bot) Send(channel string, message string) error {

	// [issue #6] 旧实现每条消息先调 ChatByUsername（getChat API）解析频道再发送，
	// 每条消息消耗 2 次 API 调用并增加 ~0.5s 延迟，是大批首刷时撞限速的放大器。
	// 已替换为直接以 @username 字符串作为 Recipient 发送（1 次调用）。
	// 旧逻辑保留备查：
	// chat, err := b.bot.ChatByUsername(channel)
	// if err != nil {
	// 	return err
	// }
	recipient := newChannelRecipient(channel)

	_, err := b.bot.Send(recipient, message, &tele.SendOptions{
		ParseMode: tele.ModeMarkdown,
	})
	if err != nil && isParseEntitiesError(err) {
		// issue #4 兜底：Markdown 实体解析失败属于内容层面的永久性错误，重试必然再失败，
		// 降级为纯文本（无 parse_mode）重发一次，成功后 handler 即可标记 seen 终止无限重试。
		// 不设「消息含 \」前置判断：解析失败可能来自未转义的裸字符（converter 输出的孤立 [、
		// 裸 URL 的 _），真正修复靠去掉 parse_mode，反转义只是避免纯文本下显示反斜杠
		log.Printf("markdown parse failed, falling back to plain text: %v", err)
		_, err = b.bot.Send(recipient, tgmd.Unescape(message))
	}
	if err != nil {
		// [issue #6] 服务端 429 且带 retry_after 时，telebot v3.1.3 extractOk 返回
		// FloodError 值类型（非指针、无 Unwrap），errors.As 目标必须用值类型
		// tele.FloodError；包装成项目内 RateLimitError 供上层按指示等待
		var flood tele.FloodError
		if errors.As(err, &flood) {
			return NewRateLimitError(time.Duration(flood.RetryAfter)*time.Second, err)
		}
	}
	// 注意重试放大：parse entities 失败时单次 Send 最多 2 次 API 调用，
	// 叠加 handler 外层 3 次重试最坏 6 次，属可接受代价（否则该消息永远发不出去）
	return err
}

// isParseEntitiesError 判断是否为 Markdown 实体解析 400
//
// 刻意用 err.Error() 字符串匹配而非 *tele.Error 类型断言：telebot v3.1.3 extractOk
// 先以 description 查 sentinel 表，"can't parse entities" 不在表中 → 落到
// switch e.Code 的 default 分支，返回的是 fmt.Errorf("telegram: %s (%d)") 普通错误，
// 类型断言/errors.As 永远落空（会成为死代码）。统一匹配错误文本可同时覆盖
// sentinel *Error 与 fmt.Errorf 两种形态，且免疫 telebot 版本差异
func isParseEntitiesError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't parse entities") && strings.Contains(msg, "(400)")
}
