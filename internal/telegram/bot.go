package telegram

import (
	"os"
	"time"

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
	return err
}
