package config

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
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
	Name                           string   `yaml:"name"`
	URL                            string   `yaml:"url"`
	ArticleExpirationDurationHours *int     `yaml:"article_expiration_duration_hours"`
	FirstPush                      bool     `yaml:"first_push"`
	Channels                       []string `yaml:"channels"`
	Template                       string   `yaml:"template"`
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

// 配置文件自动监听Manager 配置管理器
type Manager struct {
	sync.RWMutex
	config    *Config
	filepath  string
	watcher   *fsnotify.Watcher
	callbacks []func(*Config)
}

// NewManager 创建新的配置管理器
func NewManager(filepath string) (*Manager, error) {
	m := &Manager{
		filepath:  filepath,
		callbacks: make([]func(*Config), 0),
	}

	// 初始加载配置
	if err := m.Load(); err != nil {
		return nil, err
	}

	// 初始化文件监控
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	m.watcher = watcher

	// 启动监控协程
	go m.watchConfig()

	// 添加文件监控
	if err := watcher.Add(filepath); err != nil {
		watcher.Close()
		return nil, err
	}

	return m, nil
}

// Load 加载配置文件
func (m *Manager) Load() error {
	data, err := os.ReadFile(m.filepath)
	if err != nil {
		return err
	}

	var newConfig Config
	if err := yaml.Unmarshal(data, &newConfig); err != nil {
		return err
	}

	// 验证配置
	if err := newConfig.Validate(); err != nil {
		return err
	}

	m.Lock()
	m.config = &newConfig
	callbacks := make([]func(*Config), len(m.callbacks))
	copy(callbacks, m.callbacks)
	m.Unlock()

	// 通知所有订阅者
	for _, cb := range callbacks {
		cb(&newConfig)
	}

	log.Printf("Config Reloaded: %s", m.filepath)
	return nil
}

// Get 获取当前配置
func (m *Manager) Get() *Config {
	m.RLock()
	defer m.RUnlock()
	return m.config
}

// OnConfigChange 注册配置变更回调函数
func (m *Manager) OnConfigChange(callback func(*Config)) {
	m.Lock()
	m.callbacks = append(m.callbacks, callback)
	m.Unlock()
}

// watchConfig 监控配置文件变化
func (m *Manager) watchConfig() {
	for {
		select {
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				if err := m.Load(); err != nil {
					log.Printf("Config Reload Error: %v", err)
				}
			}
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config Monitor Error: %v", err)
		}
	}
}

// Close 关闭配置管理器
func (m *Manager) Close() error {
	if m.watcher != nil {
		return m.watcher.Close()
	}
	return nil
}
