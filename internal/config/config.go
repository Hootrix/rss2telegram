package config

import (
	"fmt"
	"os"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Feeds    []FeedConfig   `yaml:"feeds"`
}

type TelegramConfig struct {
	BotToken      string `yaml:"bot_token"`
	CheckInterval int    `yaml:"check_interval"`
}

type FeedConfig struct {
	Name     string   `yaml:"name"`
	URL      string   `yaml:"url"`
	Channels []string `yaml:"channels"`
	Template string   `yaml:"template"`
}

// Validate 验证配置的合法性
func (c *Config) Validate() error {
	// 检查 Telegram 配置
	if c.Telegram.BotToken == "" {
		return fmt.Errorf("telegram bot token is required")
	}
	if c.Telegram.CheckInterval <= 0 {
		return fmt.Errorf("telegram check interval must be positive")
	}

	// 检查 Feeds 配置
	if len(c.Feeds) == 0 {
		return fmt.Errorf("at least one feed must be configured")
	}

	// 用于检查名称唯一性
	names := make(map[string]bool)
	// 用于检查 URL 和名称组合的唯一性
	urlNamePairs := make(map[string]bool)

	for _, feed := range c.Feeds {
		// 检查必填字段
		if feed.Name == "" {
			return fmt.Errorf("feed name is required")
		}
		if feed.URL == "" {
			return fmt.Errorf("feed URL is required")
		}
		if len(feed.Channels) == 0 {
			return fmt.Errorf("feed %s must have at least one channel", feed.Name)
		}

		// 检查名称唯一性
		if names[feed.Name] {
			return fmt.Errorf("duplicate feed name found: %s", feed.Name)
		}
		names[feed.Name] = true

		// 检查 URL 和名称组合的唯一性
		pair := feed.Name + "|" + feed.URL
		if urlNamePairs[pair] {
			return fmt.Errorf("duplicate feed name and URL combination found: %s", pair)
		}
		urlNamePairs[pair] = true

		// 检查模板
		if feed.Template == "" {
			// 设置默认模板
			feed.Template = "📰 *{title}*\n\n{description}\n\n🔗 [阅读原文]({link})"
		}
	}

	return nil
}

// LoadConfig 从文件加载配置
func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// 验证配置
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &config, nil
}
