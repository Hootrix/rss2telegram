package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// snapshot 为字符串枚举：缺省空 = 不启用；仅允许 telegraph；
// 其他值启动报错（为将来其他快照后端如 archive.today 预留枚举空间）
func TestFeedConfigSnapshotValidate(t *testing.T) {
	newConfig := func(snapshot string) *Config {
		return &Config{
			Telegram: TelegramConfig{BotToken: "t", CheckInterval: 60},
			Feeds:    []FeedConfig{{Name: "f", URL: "https://e.com/rss", Channels: []string{"@c"}, Snapshot: snapshot}},
		}
	}

	t.Run("空串缺省合法", func(t *testing.T) {
		require.NoError(t, newConfig("").Validate())
	})

	t.Run("telegraph 合法", func(t *testing.T) {
		require.NoError(t, newConfig("telegraph").Validate())
	})

	t.Run("未知后端启动报错", func(t *testing.T) {
		err := newConfig("archive_today").Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "snapshot")
	})
}

// snapshot_source 为快照正文来源枚举：缺省/page = 抓原文（现有行为）；
// feed = 取 RSS item 正文，跳过原文抓取；其他值启动报错（issue #12）
func TestFeedConfigSnapshotSourceValidate(t *testing.T) {
	newConfig := func(snapshot, source string) *Config {
		return &Config{
			Telegram: TelegramConfig{BotToken: "t", CheckInterval: 60},
			Feeds: []FeedConfig{{
				Name: "f", URL: "https://e.com/rss", Channels: []string{"@c"},
				Snapshot: snapshot, SnapshotSource: source,
			}},
		}
	}

	t.Run("空串缺省合法", func(t *testing.T) {
		require.NoError(t, newConfig("", "").Validate())
		require.NoError(t, newConfig("telegraph", "").Validate())
	})

	t.Run("page 与 feed 合法", func(t *testing.T) {
		require.NoError(t, newConfig("telegraph", "page").Validate())
		require.NoError(t, newConfig("telegraph", "feed").Validate())
	})

	t.Run("非法值启动报错", func(t *testing.T) {
		err := newConfig("telegraph", "auto").Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "snapshot_source")
	})

	t.Run("非空但未启用快照报错", func(t *testing.T) {
		// 来源只服务快照，独立存在无意义且易被误以为生效
		err := newConfig("", "feed").Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "snapshot_source")
	})
}

func TestFeedConfigSnapshotSourceYAML(t *testing.T) {
	var c Config
	yamlSrc := `
telegram:
  bot_token: t
  check_interval: 60
feeds:
  - name: tianfu
    url: https://rss.hhtjim.com/tianfu.xml
    channels: ["@my_channel"]
    snapshot: telegraph
    snapshot_source: feed
`
	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &c))
	require.NoError(t, c.Validate())
	assert.Equal(t, "feed", c.Feeds[0].SnapshotSource)
}

func TestFeedConfigSnapshotYAML(t *testing.T) {
	var c Config
	yamlSrc := `
telegram:
  bot_token: t
  check_interval: 60
feeds:
  - name: special-feed
    url: https://example.com/rss
    channels: ["@my_channel"]
    snapshot: telegraph
`
	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &c))
	require.NoError(t, c.Validate())
	assert.Equal(t, "telegraph", c.Feeds[0].Snapshot)

	// 缺省字段反序列化为空串
	assert.Empty(t, c.Feeds[0].Template)
}
