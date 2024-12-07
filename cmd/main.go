package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/rss"
	"github.com/Hootrix/rss2telegram/internal/storage"
	"github.com/Hootrix/rss2telegram/internal/telegram"
)

func main() {
	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

	// 创建上下文，用于优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 处理系统信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %v", sig)
		cancel()
	}()

	// 读取配置文件
	configPath := "/Users/panc/CascadeProjects/rss-telegram-bot/config/config.yaml"
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// 初始化存储
	dataDir := filepath.Join(filepath.Dir(configPath), "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("Error creating data directory: %v", err)
	}

	store, err := storage.NewStorage(dataDir)
	if err != nil {
		log.Fatalf("Error initializing storage: %v", err)
	}

	// 创建 Telegram 机器人
	bot, err := telegram.NewBot(cfg.Telegram.BotToken)
	if err != nil {
		log.Fatalf("Error creating Telegram bot: %v", err)
	}

	// 创建 RSS 处理器
	handler := rss.NewRssHandler(cfg, bot, store)

	// 定时检查 RSS 更新
	ticker := time.NewTicker(time.Duration(cfg.Telegram.CheckInterval) * time.Second)
	defer ticker.Stop()

	log.Printf("Bot started. Checking feeds every %d seconds", cfg.Telegram.CheckInterval)

	// 记录启动时间
	startTime := time.Now()

	// 主循环
	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutting down... (uptime: %v)", time.Since(startTime))
			return
		case <-ticker.C:
			if err := handler.ProcessFeeds(); err != nil {
				log.Printf("Error processing feeds: %v", err)
				// 如果发生错误，等待一段时间再继续
				time.Sleep(time.Second * 5)
			}
		}
	}
}
