package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 回归测试：issue#3 —— 编辑器"写临时文件 + rename 覆盖"的原子保存方式必须触发配置重载
// 背景：原实现 watcher.Add(文件本身) 监听的是 inode，rename 覆盖后 inode 替换，
// 旧 inode 上永远不会再有事件 → vim/vscode 保存后配置永不重载（只能重启进程）
func TestConfigReloadOnAtomicRenameSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := "telegram:\n  bot_token: \"token-1\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	m, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	reloaded := make(chan struct{}, 1)
	m.OnConfigChange(func(c *Config) {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// 模拟 vim 式原子保存：写临时文件后 rename 覆盖原文件（inode 被替换）
	updated := "telegram:\n  bot_token: \"token-2\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	tmpPath := cfgPath + ".tmp~"
	if err := os.WriteFile(tmpPath, []byte(updated), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		t.Fatalf("rename over config: %v", err)
	}

	select {
	case <-reloaded:
		if m.Get().Telegram.BotToken != "token-2" {
			t.Fatalf("回调已触发但配置未更新：bot_token = %s", m.Get().Telegram.BotToken)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rename 原子保存后未触发配置重载（issue#3）")
	}
}

// 单文件 bind mount 检测：纯函数测试，喂构造的 mountinfo 文本，无需真实挂载/root 权限
// 真实调用方（warnIfSingleFileMount）只会传配置文件路径，用例按此语义构造
func TestIsSingleFileMount(t *testing.T) {
	// 夹具A：目录挂载（正确用法）——目录本身是挂载点，目录内的配置文件不是
	miDirMount := `36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
38 36 98:0 /another /app/config rw,atime - ext3 /dev/root rw
`
	// 夹具B：单文件挂载（问题用法）——配置文件本身作为挂载点出现
	miFileMount := `36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
37 36 98:0 /mnt1/tmp /app/config/config.yaml rw,noatime master:1 - ext3 /dev/root rw,errors=continue
`
	cases := []struct {
		name     string
		mountinf string
		path     string
		want     bool
	}{
		{"单文件挂载命中", miFileMount, "/app/config/config.yaml", true},
		{"目录挂载不误报（正确用法不能告警）", miDirMount, "/app/config/config.yaml", false},
		{"目录挂载时目录本身是挂载点（函数如实返回，但调用方不会传目录）", miDirMount, "/app/config", true},
		{"无关路径", miFileMount, "/etc/hosts", false},
		{"同目录其他文件不是挂载点", miFileMount, "/app/config/other.yaml", false},
	}
	for _, c := range cases {
		if got := isSingleFileMount(c.mountinf, c.path); got != c.want {
			t.Errorf("%s: isSingleFileMount(path=%s) = %v, want %v", c.name, c.path, got, c.want)
		}
	}

	// 挂载点含转义空格（\040）时必须还原后再比较，否则路径带空格的挂载漏检
	miSpace := "40 36 98:0 /x /app/my\\040config/config.yaml rw - ext3 /dev/root rw\n"
	if !isSingleFileMount(miSpace, "/app/my config/config.yaml") {
		t.Error("转义空格路径的挂载点未被识别")
	}

	// 空文本/畸形行不 panic、不误报
	if isSingleFileMount("", "/app/config/config.yaml") {
		t.Error("空 mountinfo 不应命中")
	}
	if isSingleFileMount("garbage line\n\n1 2 3", "/app/config/config.yaml") {
		t.Error("畸形行不应命中")
	}
}
func TestConfigReloadOnInplaceWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := "telegram:\n  bot_token: \"token-1\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	m, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	reloaded := make(chan struct{}, 1)
	m.OnConfigChange(func(c *Config) {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// 原地覆盖写（同 inode，触发 Write 事件）
	updated := "telegram:\n  bot_token: \"token-2\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	if err := os.WriteFile(cfgPath, []byte(updated), 0644); err != nil {
		t.Fatalf("overwrite config in place: %v", err)
	}

	select {
	case <-reloaded:
		if m.Get().Telegram.BotToken != "token-2" {
			t.Fatalf("回调已触发但配置未更新：bot_token = %s", m.Get().Telegram.BotToken)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("原地写入保存后未触发配置重载（防回归）")
	}
}

// CR 意见#2：Close() 必须取消 pending 中的去抖定时器
// 修复前 debounce 是 watchConfig 局部变量，Close 只关 watcher，
// 已排定的 200ms 去抖定时器仍会在 Close 之后触发 m.Load() 并回调所有订阅者
func TestCloseCancelsPendingDebounce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := "telegram:\n  bot_token: \"token-1\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	m, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	reloaded := make(chan struct{}, 1)
	m.OnConfigChange(func(c *Config) {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// 原地写触发事件 → 100ms 后定时器已排定且未到期（去抖窗口 200ms），此刻 Close
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("touch config: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	m.Close()

	select {
	case <-reloaded:
		t.Fatal("Close 后 pending 的去抖定时器仍触发了重载（CR意见#2）")
	case <-time.After(500 * time.Millisecond):
		// 预期路径：Close 已取消定时器，静默
	}
}

// 监听目录后必须按文件名过滤：同目录其他文件（编辑器临时文件、无关文件）变化不得触发重载
func TestConfigIgnoresSiblingFileChanges(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := "telegram:\n  bot_token: \"token-1\"\n  check_interval: 60\nfeeds:\n  - name: \"f\"\n    url: \"http://example.com/rss.xml\"\n    channels: [\"@c\"]\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	m, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	reloaded := make(chan struct{}, 1)
	m.OnConfigChange(func(c *Config) {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// 同目录其他文件的写入/创建/重命名（模拟编辑器临时文件与无关文件）
	for _, sibling := range []string{"config.yaml.swp", "config.yaml.tmp~", "4913", "notes.txt"} {
		p := filepath.Join(dir, sibling)
		if err := os.WriteFile(p, []byte("noise"), 0644); err != nil {
			t.Fatalf("write sibling %s: %v", sibling, err)
		}
	}
	if err := os.Rename(filepath.Join(dir, "notes.txt"), filepath.Join(dir, "notes2.txt")); err != nil {
		t.Fatalf("rename sibling: %v", err)
	}

	// 留足去抖窗口（200ms）+ 事件传播时间，期间不应有任何重载
	select {
	case <-reloaded:
		t.Fatal("同目录其他文件变化不应触发配置重载")
	case <-time.After(1 * time.Second):
		// 预期路径：静默
	}

	// 静默后对配置文件本体的修改仍需正常生效（确认过滤器没有误伤目标文件）
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("touch config: %v", err)
	}
	select {
	case <-reloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("过滤逻辑误伤目标文件：配置本体修改未触发重载")
	}
}

// issue #13：media 枚举校验——空=不启用，photo=图片推送，其他值报错
// 注：newFeed 返回 *Config——Validate 为指针接收者，Go 不允许对不可寻址的
// 返回值直接调用指针方法（newFeed("").Validate() 编译不过），故返回指针
func TestValidateMediaEnum(t *testing.T) {
	newFeed := func(media string) *Config {
		return &Config{
			Telegram: TelegramConfig{BotToken: "t", CheckInterval: 1},
			Feeds:    []FeedConfig{{Name: "f", URL: "https://e.com/rss", Channels: []string{"@c"}, Media: media}},
		}
	}

	t.Run("空串合法（默认不启用）", func(t *testing.T) {
		assert.NoError(t, newFeed("").Validate())
	})
	t.Run("photo 合法", func(t *testing.T) {
		assert.NoError(t, newFeed("photo").Validate())
	})
	t.Run("非法值报错", func(t *testing.T) {
		err := newFeed("video").Validate()
		assert.ErrorContains(t, err, `invalid media "video"`)
		assert.ErrorContains(t, err, `"photo"`)
	})
}

// issue #16 用户反馈：photo_slices/photo_overlimit 校验——
// 仅 media=photo 的 feed 允许配置（对齐 snapshot_source 依赖 snapshot 的校验关系），
// photo_slices 合法显式值 2-10（sendMediaGroup 硬上限 10，0=默认）
func TestValidatePhotoAlbumConfig(t *testing.T) {
	newFeed := func(mutate func(*FeedConfig)) *Config {
		cfg := &Config{
			Telegram: TelegramConfig{BotToken: "t", CheckInterval: 1},
			Feeds:    []FeedConfig{{Name: "f", URL: "https://e.com/rss", Channels: []string{"@c"}, Media: MediaPhoto}},
		}
		mutate(&cfg.Feeds[0])
		return cfg
	}

	t.Run("全零值默认通过且 EffectivePhotoSlices 回退 10", func(t *testing.T) {
		// Validate 挂在 *Config 上（指针接收者），经 newFeed 构造后校验
		cfg := newFeed(func(_ *FeedConfig) {})
		assert.NoError(t, cfg.Validate())
		assert.Equal(t, 10, cfg.Feeds[0].EffectivePhotoSlices())
	})

	t.Run("photo_slices 合法边界 2 与 10 通过", func(t *testing.T) {
		assert.NoError(t, newFeed(func(f *FeedConfig) { f.PhotoSlices = 2 }).Validate())
		assert.NoError(t, newFeed(func(f *FeedConfig) { f.PhotoSlices = 10 }).Validate())
	})

	t.Run("photo_slices 越界 1 与 11 报错", func(t *testing.T) {
		for _, v := range []int{1, 11} {
			err := newFeed(func(f *FeedConfig) { f.PhotoSlices = v }).Validate()
			assert.ErrorContains(t, err, "photo_slices", "值 %d 应报错", v)
		}
	})

	t.Run("photo_slices 未启用 media 时报错", func(t *testing.T) {
		err := newFeed(func(f *FeedConfig) { f.Media = ""; f.PhotoSlices = 5 }).Validate()
		assert.ErrorContains(t, err, "photo_slices")
		assert.ErrorContains(t, err, "media")
	})

	t.Run("photo_overlimit 合法值 crop 与 document 通过", func(t *testing.T) {
		assert.NoError(t, newFeed(func(f *FeedConfig) { f.PhotoOverlimit = PhotoOverlimitCrop }).Validate())
		assert.NoError(t, newFeed(func(f *FeedConfig) { f.PhotoOverlimit = PhotoOverlimitDocument }).Validate())
	})

	t.Run("photo_overlimit 非法值报错且列出合法值", func(t *testing.T) {
		err := newFeed(func(f *FeedConfig) { f.PhotoOverlimit = "archive" }).Validate()
		assert.ErrorContains(t, err, "photo_overlimit")
		assert.ErrorContains(t, err, `"crop"`)
		assert.ErrorContains(t, err, `"document"`)
	})

	t.Run("photo_overlimit 未启用 media 时报错", func(t *testing.T) {
		err := newFeed(func(f *FeedConfig) { f.Media = ""; f.PhotoOverlimit = PhotoOverlimitCrop }).Validate()
		assert.ErrorContains(t, err, "photo_overlimit")
		assert.ErrorContains(t, err, "media")
	})
}
