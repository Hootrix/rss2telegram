package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

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
// [issue#3] 参数名由 filepath 改为 configPath：原参数名遮蔽了 path/filepath 包，
// 监听目录需要调用 filepath.Dir，遮蔽时无法引用包
func NewManager(configPath string) (*Manager, error) {
	m := &Manager{
		filepath:  configPath,
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

	// [issue#3] 改为监听配置文件所在目录而非文件本身
	// 原因：watcher.Add(文件) 监听的是 inode，而编辑器（vim/vscode 等）保存普遍采用
	// "写临时文件 + rename 覆盖"，rename 后 inode 替换，旧 inode 上永远收不到事件，
	// 表现为配置热更新完全失效（只能重启进程）。监听目录 + 按文件名过滤事件则不受 inode 替换影响
	// 旧逻辑（监听文件本身）注释保留：
	// if err := watcher.Add(configPath); err != nil {
	// 	watcher.Close()
	// 	return nil, err
	// }
	if err := watcher.Add(filepath.Dir(configPath)); err != nil {
		watcher.Close()
		return nil, err
	}

	// 启动监控协程
	go m.watchConfig()

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
// [issue#3] 配套 NewManager 改为监听目录后的重写版本：
// - 按文件名过滤目录事件（忽略同目录其他文件，如编辑器临时文件）
// - 事件类型扩展为 Write|Create|Rename：编辑器原子保存（tmp+rename 覆盖）在目录监听下
//   表现为 Create（新 inode 落地），原实现只认 Write 会漏掉这种保存方式
// - 增加去抖：一次保存可能连续产生多个事件（如 Create+Write），合并为一次重载
// 旧逻辑（监听文件本身、只处理 Write）注释保留：
// func (m *Manager) watchConfig() {
// 	for {
// 		select {
// 		case event, ok := <-m.watcher.Events:
// 			if !ok {
// 				return
// 			}
// 			if event.Op&fsnotify.Write == fsnotify.Write {
// 				if err := m.Load(); err != nil {
// 					log.Printf("Config Reload Error: %v", err)
// 				}
// 			}
// 		case err, ok := <-m.watcher.Errors:
// 			if !ok {
// 				return
// 			}
// 			log.Printf("Config Monitor Error: %v", err)
// 		}
// 	}
// }
func (m *Manager) watchConfig() {
	target := filepath.Base(m.filepath)
	var debounce *time.Timer

	for {
		select {
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			// 只处理目标配置文件的事件，忽略同目录其他文件
			if filepath.Base(event.Name) != target {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				// 去抖：重置定时器，静默 200ms 后才真正重载（Load 幂等，偶发重复无害）
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(200*time.Millisecond, func() {
					if err := m.Load(); err != nil {
						log.Printf("Config Reload Error: %v", err)
					}
				})
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
