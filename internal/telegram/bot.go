package telegram

import (
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
