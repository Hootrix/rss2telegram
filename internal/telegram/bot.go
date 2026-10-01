package telegram

import (
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

	chat, err := b.bot.ChatByUsername(channel)
	if err != nil {
		return err
	}

	_, err = b.bot.Send(chat, message, &tele.SendOptions{
		ParseMode: tele.ModeMarkdown,
	})
	if err != nil && isParseEntitiesError(err) {
		// issue #4 兜底：Markdown 实体解析失败属于内容层面的永久性错误，重试必然再失败，
		// 降级为纯文本（无 parse_mode）重发一次，成功后 handler 即可标记 seen 终止无限重试。
		// 不设「消息含 \」前置判断：解析失败可能来自未转义的裸字符（converter 输出的孤立 [、
		// 裸 URL 的 _），真正修复靠去掉 parse_mode，反转义只是避免纯文本下显示反斜杠
		log.Printf("markdown parse failed, falling back to plain text: %v", err)
		_, err = b.bot.Send(chat, tgmd.Unescape(message))
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
