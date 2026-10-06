package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

func TestShutdownInterruptsFeedFetch(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		if _, err := fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true}}`); err != nil {
			t.Errorf("write API: %v", err)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "rss2telegram")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./main.go")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bot: %v\n%s", err, out)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := fmt.Sprintf("telegram:\n  bot_token: '1:test'\n  check_interval: 1\nfeeds:\n  - name: f\n    url: %s/feed\n    channels: ['@ch']\n    first_push: true\n", server.URL)
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd := exec.CommandContext(ctx, binary, "-config", configPath)
	cmd.Env = append(os.Environ(), "TELEGRAM_API_URL="+server.URL)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bot: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("bot 未完成清理退出")
			}
		}
	}()
	select {
	case <-started:
	case err := <-done:
		finished = true
		t.Fatalf("bot 提前退出: %v\n%s", err, output.String())
	case <-time.After(10 * time.Second):
		t.Fatal("RSS 请求未启动")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("bot 未优雅退出: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM 未中断在途 RSS 请求")
	}
}
