package main

import (
	"os"
	"testing"

	"github.com/Hootrix/rss2telegram/internal/config"

	tele "gopkg.in/telebot.v3"
	"gopkg.in/yaml.v3"
)

func TestSend(t *testing.T) {
	// 读取配置文件
	data, err := os.ReadFile("config/config.yaml")
	if err != nil {
		t.Fatalf("Error reading config file: %v", err)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Error parsing config file: %v", err)
	}

	// 创建 Telegram 机器人
	pref := tele.Settings{
		Token:   cfg.Telegram.BotToken,
		Verbose: true, // 启用详细日志
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		t.Fatalf("Error creating Telegram bot: %v", err)
	}

	// 获取机器人信息
	me := b.Me
	t.Logf("Bot Info - ID: %d, Username: @%s, First Name: %s", me.ID, me.Username, me.FirstName)

	// 尝试获取频道信息
	if len(cfg.Feeds) > 0 && len(cfg.Feeds[0].Channels) > 0 {
		channel := cfg.Feeds[0].Channels[0]
		chat, err := b.ChatByUsername(channel)
		if err != nil {
			t.Fatalf("Error getting chat info for %s: %v", channel, err)
		}
		t.Logf("Channel Info - ID: %d, Title: %s, Type: %s", chat.ID, chat.Title, chat.Type)

		// 发送测试消息
		_, err = b.Send(chat, "🤖 测试消息：检查机器人连接状态\n\n如果您看到这条消息，说明机器人已经成功连接并具有发送消息的权限。")
		if err != nil {
			t.Fatalf("Error sending message: %v", err)
		}
		t.Log("Test message sent successfully!")
	} else {
		t.Fatal("No channels configured in config.yaml")
	}
}
