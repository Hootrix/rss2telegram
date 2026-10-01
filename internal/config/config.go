package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	// [CR#2] 去抖定时器从 watchConfig 局部变量提升为字段：Close 需要Stop掉
	// pending 中的定时器，否则 Close 之后仍可能触发一次 m.Load() 并回调订阅者
	// 仅在持 m.Lock 时读写
	debounce *time.Timer
	// Close 已调用；用于封死"Close 与 watchConfig 排定新定时器"的竞态窗口
	closed bool
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
	//
	// [CR#3] 已知限制：若 configPath 本身是符号链接，透写 target 文件产生的事件落在
	// target 所在目录（未被监听），且按文件名过滤也匹配不上 → 热更新静默失效
	// （旧实现 inotify 会 follow symlink 监听 target inode，反而能工作）
	// 边缘场景（嵌套 symlink、k8s ConfigMap 的 ..data 变体），如需支持可在启动时
	// filepath.EvalSymlinks 解析真实路径，但 ConfigMap 每次更新切换 ..data 指向仍会丢事件，
	// 届时需目录级监听策略，留待单独 issue 处理
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

	// 挂载方式体检：单文件 bind mount 会同时废掉"内容同步"与"热更新"，提前告警避免用户排错
	warnIfSingleFileMount(configPath)

	return m, nil
}

// isSingleFileMount 判断 absPath 是否作为挂载点出现在 mountinfo 中（即被单独 bind mount 的文件）
// 拆成纯函数便于测试：直接喂 /proc/self/mountinfo 格式文本即可，无需真实挂载与 root 权限
// mountinfo 行格式: "ID parent major:minor root mount_point [可选字段...] - fstype source [超参数]"
// 其中挂载点是第 5 列（index 4）
func isSingleFileMount(mountinfo string, absPath string) bool {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue // 空行/畸形行
		}
		if unescapeMountPath(fields[4]) == absPath {
			return true
		}
	}
	return false
}

// unescapeMountPath 还原 mountinfo 中的八进制转义（空格/制表符/反斜杠在挂载点路径中的转义形式）
func unescapeMountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(s)
}

// warnIfSingleFileMount 检测配置文件是否被单文件 bind mount（典型：-v config.yaml:/app/config/config.yaml）
// 单文件挂载会把 inode 钉死在容器启动时刻：宿主机编辑器"临时文件+rename"保存后换了新 inode，
// 容器内看到的永远是旧文件——内容不同步、热更新完全失效（比目录挂载+事件缺失更彻底）
// 边界：/proc/self/mountinfo 读取失败（mac/windows 宿主机直跑、受限环境）时静默跳过，不影响启动
func warnIfSingleFileMount(configPath string) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return
	}
	if !isSingleFileMount(string(data), abs) {
		return
	}
	log.Printf("⚠️  [挂载方式告警] 配置文件 %s 是单文件挂载(bind mount)：宿主机编辑器保存后容器内内容不会更新，配置热更新完全失效", abs)
	log.Printf("⚠️  请改为挂载目录，例如: docker run -v $(pwd)/rss2telegram-config:/app/config ...")
	log.Printf("⚠️  若坚持当前方式，每次修改配置后需 docker restart 重新挂载才能生效")
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
//   - 按文件名过滤目录事件（忽略同目录其他文件，如编辑器临时文件）
//   - 事件类型扩展为 Write|Create|Rename：编辑器原子保存（tmp+rename 覆盖）在目录监听下
//     表现为 Create（新 inode 落地），原实现只认 Write 会漏掉这种保存方式
//   - 增加去抖：一次保存可能连续产生多个事件（如 Create+Write），合并为一次重载
//
// 旧逻辑（监听文件本身、只处理 Write）注释保留：
//
//	func (m *Manager) watchConfig() {
//		for {
//			select {
//			case event, ok := <-m.watcher.Events:
//				if !ok {
//					return
//				}
//				if event.Op&fsnotify.Write == fsnotify.Write {
//					if err := m.Load(); err != nil {
//						log.Printf("Config Reload Error: %v", err)
//					}
//				}
//			case err, ok := <-m.watcher.Errors:
//				if !ok {
//					return
//				}
//				log.Printf("Config Monitor Error: %v", err)
//			}
//		}
//	}
func (m *Manager) watchConfig() {
	target := filepath.Base(m.filepath)

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
				// [CR#2] 旧实现 debounce 为局部变量、Close 无法 Stop，注释保留：
				// var debounce *time.Timer
				// if debounce != nil { debounce.Stop() }
				// debounce = time.AfterFunc(200*time.Millisecond, func() { m.Load() ... })
				m.Lock()
				if m.closed {
					m.Unlock()
					continue // Close 竞态窗口：不再排定新定时器
				}
				if m.debounce != nil {
					m.debounce.Stop()
				}
				m.debounce = time.AfterFunc(200*time.Millisecond, func() {
					// Stop 与触发竞态输一步时（Stop 返回 false）的兜底：已 Close 则放弃重载
					m.RLock()
					closed := m.closed
					m.RUnlock()
					if closed {
						return
					}
					if err := m.Load(); err != nil {
						log.Printf("Config Reload Error: %v", err)
					}
				})
				m.Unlock()
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
// [CR#2] 除关闭 watcher 外，还需取消 pending 中的去抖定时器，
// 否则 Close 之后定时器到点仍会触发 m.Load() 并回调所有订阅者（库语义泄漏）
// 旧逻辑（只关 watcher）注释保留：
//
//	func (m *Manager) Close() error {
//		if m.watcher != nil {
//			return m.watcher.Close()
//		}
//		return nil
//	}
func (m *Manager) Close() error {
	m.Lock()
	m.closed = true
	if m.debounce != nil {
		m.debounce.Stop()
		m.debounce = nil
	}
	m.Unlock()

	if m.watcher != nil {
		return m.watcher.Close()
	}
	return nil
}
