package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
