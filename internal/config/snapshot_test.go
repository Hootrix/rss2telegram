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
