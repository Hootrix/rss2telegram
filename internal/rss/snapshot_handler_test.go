package rss

// handler 层快照接入：{telegraph} 模板字段渲染 + processFeed 渲染前建快照

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/storage"
	"github.com/Hootrix/rss2telegram/internal/telegram"
	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatMessageTelegraphField(t *testing.T) {
	handler := &RssHandler{}
	item := &gofeed.Item{Title: "标题", Link: "https://e.com/a"}

	t.Run("快照链接渲染进模板", func(t *testing.T) {
		result := handler.formatMessage(item, "🔗 [快照]({telegraph})", "https://telegra.ph/x")
		assert.Equal(t, "🔗 [快照](https://telegra.ph/x)", result)
	})

	t.Run("无快照渲染空串（行为锁定，README 提示坏链风险）", func(t *testing.T) {
		result := handler.formatMessage(item, "[快照]({telegraph})", "")
		assert.Equal(t, "[快照]()", result)
	})

	t.Run("支持 default 操作链自行兜底文案", func(t *testing.T) {
		result := handler.formatMessage(item, "[{telegraph|default:无快照}]({telegraph})", "")
		assert.Equal(t, "[无快照]()", result)
	})
}

// fakeSnapshotter 快照编排替身：记录调用并可编排返回
type fakeSnapshotter struct {
	mu        sync.Mutex
	calls     int
	url       string
	err       error
	backfills []backfillCall // BackfillAuthor 调用记录（issue #12 回填）
}

type backfillCall struct {
	feed    string
	msgLink string
}

func (f *fakeSnapshotter) BackfillAuthor(_ context.Context, feed config.FeedConfig, _ *gofeed.Item, msgLink string) error {
	f.mu.Lock()
	f.backfills = append(f.backfills, backfillCall{feed: feed.Name, msgLink: msgLink})
	f.mu.Unlock()
	return nil
}

func (f *fakeSnapshotter) Snapshot(_ context.Context, _ config.FeedConfig, _ *gofeed.Item) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.url, f.err
}

func (f *fakeSnapshotter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newSnapshotTestHandler 构造带 link 的单条目 RSS 源 + 记录消息内容的 bot
func newSnapshotTestHandler(t *testing.T, feedCfg config.FeedConfig, snap Snapshotter) (*RssHandler, *[]string) {
	t.Helper()
	const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><link>https://example.com/</link><description>d</description><item><title>文章标题</title><link>https://article.example/1</link><guid>item-1</guid></item></channel></rss>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, feedXML)
	}))
	t.Cleanup(server.Close)
	feedCfg.URL = server.URL
	feedCfg.FirstPush = true

	cfg := &config.Config{Feeds: []config.FeedConfig{feedCfg}}
	store, err := storage.NewStorage(t.TempDir())
	require.NoError(t, err)

	var mu sync.Mutex
	sent := &[]string{}
	bot := sendTestFunc(func(_ context.Context, _ string, m *telegram.Message) (int64, error) {
		mu.Lock()
		*sent = append(*sent, m.Text())
		mu.Unlock()
		return 777, nil // 固定 msgID 供回填断言（issue #12）
	})
	return NewRssHandler(cfg, bot, store, snap), sent
}

func TestProcessFeedSnapshotWired(t *testing.T) {
	feed := config.FeedConfig{
		Name: "special", Channels: []string{"@c"}, Snapshot: config.SnapshotTelegraph,
		Template: "📰 *{title}*\n🔗 [快照]({telegraph})",
	}
	snap := &fakeSnapshotter{url: "https://telegra.ph/p1"}
	h, sent := newSnapshotTestHandler(t, feed, snap)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]))
	require.Len(t, *sent, 1)
	assert.Contains(t, (*sent)[0], "https://telegra.ph/p1", "快照链接必须进入推送消息")
	assert.Equal(t, 1, snap.callCount())
	assert.True(t, h.storage.IsItemSeen(h.config.Feeds[0].URL, "special", "@c", "item-1"))
}

// 快照阶段父预算耗尽：延后下轮——不发送、不标 seen、不上报错误
func TestProcessFeedSnapshotBudgetExhaustedDefers(t *testing.T) {
	feed := config.FeedConfig{
		Name: "special", Channels: []string{"@c"}, Snapshot: config.SnapshotTelegraph,
		Template: "{title}",
	}
	snap := &fakeSnapshotter{err: fmt.Errorf("snapshot %s: %w (parent cancelled)", "https://article.example/1", context.DeadlineExceeded)}
	h, sent := newSnapshotTestHandler(t, feed, snap)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]), "预算耗尽延后不上报错误")
	assert.Empty(t, *sent, "延后时不得发送")
	assert.False(t, h.storage.IsItemSeen(h.config.Feeds[0].URL, "special", "@c", "item-1"), "未标 seen，下轮重试")
}

// 快照编排器缺位（nil）时 feed 配置了 snapshot 也不得 panic，按无快照发送
func TestProcessFeedSnapshotNilService(t *testing.T) {
	feed := config.FeedConfig{
		Name: "special", Channels: []string{"@c"}, Snapshot: config.SnapshotTelegraph,
		Template: "[快照]({telegraph})",
	}
	h, sent := newSnapshotTestHandler(t, feed, nil)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]))
	require.Len(t, *sent, 1)
	assert.Contains(t, (*sent)[0], "[快照]()")
}

// 未配置 snapshot 的 feed 完全不走快照路径（零影响，设计核心约束）
func TestProcessFeedNoSnapshotConfigured(t *testing.T) {
	feed := config.FeedConfig{
		Name: "normal", Channels: []string{"@c"},
		Template: strings.ToUpper("{title}"),
	}
	snap := &fakeSnapshotter{url: "https://telegra.ph/p1"}
	h, _ := newSnapshotTestHandler(t, feed, snap)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]))
	assert.Zero(t, snap.callCount(), "未启用快照的 feed 不得调用编排器")
}

// ---- author_url 消息链接回填（issue #12）----

// 发送成功后对公开频道回填消息链接：t.me/<name>/<msgID>，msgID 取自 Send 响应
func TestProcessFeedBackfillsMessageLink(t *testing.T) {
	feed := config.FeedConfig{
		Name: "special", Channels: []string{"@c"}, Snapshot: config.SnapshotTelegraph,
		Template: "{title}",
	}
	snap := &fakeSnapshotter{url: "https://telegra.ph/p1"}
	h, sent := newSnapshotTestHandler(t, feed, snap)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]))
	require.Len(t, *sent, 1, "前置：消息已发送")

	snap.mu.Lock()
	defer snap.mu.Unlock()
	require.Len(t, snap.backfills, 1, "发送成功后必须回填一次")
	assert.Equal(t, "special", snap.backfills[0].feed)
	assert.Equal(t, "https://t.me/c/777", snap.backfills[0].msgLink)
}

// 私有频道（纯数字 ID）无公开消息链接，不回填；消息照发
func TestProcessFeedNoBackfillForPrivateChannel(t *testing.T) {
	feed := config.FeedConfig{
		Name: "special", Channels: []string{"-1001234567890"}, Snapshot: config.SnapshotTelegraph,
		Template: "{title}",
	}
	snap := &fakeSnapshotter{url: "https://telegra.ph/p1"}
	h, sent := newSnapshotTestHandler(t, feed, snap)

	require.NoError(t, h.processFeed(context.Background(), h.config.Feeds[0]))
	require.Len(t, *sent, 1, "私有频道消息照发")

	snap.mu.Lock()
	defer snap.mu.Unlock()
	assert.Empty(t, snap.backfills, "私有频道不得回填")
}
