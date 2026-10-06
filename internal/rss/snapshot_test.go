package rss

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/telegraph"
	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 测试替身 ----

type fakeFetcher struct {
	calls atomic.Int32
	html  string
	err   error
	block bool // 阻塞直到 ctx 结束（模拟慢抓取触发超时链路）
}

func (f *fakeFetcher) FetchAndExtract(ctx context.Context, rawURL string) (string, error) {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return f.html, nil
}

type fakePublisher struct {
	calls atomic.Int32
	mu    sync.Mutex
	pages []telegraph.Page
	url   string
	err   error
}

func (p *fakePublisher) CreatePage(ctx context.Context, page telegraph.Page) (string, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.pages = append(p.pages, page)
	p.mu.Unlock()
	if p.err != nil {
		return "", p.err
	}
	return p.url, nil
}

func newSnapSvc(fetcher *fakeFetcher, publisher *fakePublisher) *SnapshotService {
	return NewSnapshotService(fetcher, publisher, newSnapshotCache(0, 0))
}

func snapFeed(name string, channels ...string) config.FeedConfig {
	return config.FeedConfig{Name: name, URL: "https://e.com/rss", Channels: channels, Snapshot: config.SnapshotTelegraph}
}

func snapItem(title, link, guid string) *gofeed.Item {
	return &gofeed.Item{Title: title, Link: link, GUID: guid}
}

// ---- 编排 ----

func TestSnapshotSuccessAndCacheHit(t *testing.T) {
	fetcher := &fakeFetcher{html: `<p>` + strings.Repeat("正", 300) + `</p>`}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	url, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("标题", "https://e.com/a", "g1"))
	require.NoError(t, err)
	assert.Equal(t, "https://telegra.ph/ok-1", url)

	// 同 feed 同文章二连发（多频道场景）：缓存命中，只建一页
	url2, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan", "@chan2"), snapItem("标题", "https://e.com/a", "g1"))
	require.NoError(t, err)
	assert.Equal(t, "https://telegra.ph/ok-1", url2)
	assert.Equal(t, int32(1), pub.calls.Load(), "同 feed 同文章只允许建一页")
}

func TestSnapshotDegradeOnFetchFailure(t *testing.T) {
	fetcher := &fakeFetcher{err: errors.New("http status 403")}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	// 快照自身失败：降级为原文链接，err 必须为 nil（消息照发）
	url, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("标题", "https://e.com/a", "g1"))
	require.NoError(t, err)
	assert.Equal(t, "https://e.com/a", url)
	assert.Equal(t, int32(0), pub.calls.Load())

	// 降级结果不入缓存：下一次重试允许再建页
	url2, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("标题", "https://e.com/a", "g1"))
	require.NoError(t, err)
	assert.Equal(t, "https://e.com/a", url2)
}

func TestSnapshotDegradeOnPublishFailure(t *testing.T) {
	fetcher := &fakeFetcher{html: `<p>` + strings.Repeat("正", 300) + `</p>`}
	pub := &fakePublisher{err: errors.New("telegraph createPage: api error: FloodCap")}
	svc := newSnapSvc(fetcher, pub)

	url, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("标题", "https://e.com/a", "g1"))
	require.NoError(t, err)
	assert.Equal(t, "https://e.com/a", url, "发布失败降级为原文链接")
}

// 两类失败严格区分的核心用例（CR2-#4）：不能用错误类型区分——
// 子 ctx 继承父截止时间，父预算耗尽时子 ctx 返回的同样是 DeadlineExceeded，
// 必须以 parent.Err() 判定：父取消 → 延后（返回错误），而非降级照发
func TestSnapshotParentDeadlineDefers(t *testing.T) {
	fetcher := &fakeFetcher{block: true}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	// 父预算仅剩 50ms，抓取挂起：父截止先于子超时（15s）触发
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	url, err := svc.Snapshot(ctx, snapFeed("f", "@chan"), snapItem("标题", "https://e.com/a", "g1"))
	require.Error(t, err, "父取消必须返回错误供上层延后")
	assert.Empty(t, url, "父取消不得降级为原文链接")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "handler 依赖此判定走预算耗尽延后分支")
}

func TestSnapshotNoLink(t *testing.T) {
	fetcher := &fakeFetcher{}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	url, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("标题", "", "g1"))
	require.NoError(t, err)
	assert.Empty(t, url, "无 link 跳过快照，{telegraph} 渲染空串")
	assert.Equal(t, int32(0), fetcher.calls.Load())
	assert.Equal(t, int32(0), pub.calls.Load())
}

// 硬限制预截断在编排层应用：title 256 / author 128 / content 64KB
func TestSnapshotAppliesHardLimits(t *testing.T) {
	longTitle := strings.Repeat("题", 300)
	longAuthor := strings.Repeat("名", 200)
	hugeHTML := strings.Repeat("<p>"+strings.Repeat("文", 30000)+"</p>", 4)
	fetcher := &fakeFetcher{html: hugeHTML}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	_, err := svc.Snapshot(context.Background(), snapFeed(longAuthor, "@chan"), snapItem(longTitle, "https://e.com/a", "g1"))
	require.NoError(t, err)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.pages, 1)
	page := pub.pages[0]

	countRunes := func(s string) int { n := 0; for range s { n++ }; return n }
	assert.Equal(t, 256, countRunes(page.Title), "title 按 rune 截 256")
	assert.Equal(t, 128, countRunes(page.AuthorName), "author_name 按 rune 截 128")

	content, err := json.Marshal(page.Content)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(content), maxContentBytes, "content 序列化后 ≤ 64KB")
	assert.Contains(t, string(content), "查看原文", "截断时追加提示节点")
}

// 同 GUID 跨 feed：缓存 key 带 feed 维度，各建各页，author 归属正确（CR2-#2）
func TestSnapshotCacheKeyFeedIsolation(t *testing.T) {
	fetcher := &fakeFetcher{html: `<p>` + strings.Repeat("正", 300) + `</p>`}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	_, err := svc.Snapshot(context.Background(), snapFeed("feedA", "@chanA"), snapItem("标题", "https://e.com/a", "42"))
	require.NoError(t, err)
	_, err = svc.Snapshot(context.Background(), snapFeed("feedB", "@chanB"), snapItem("标题", "https://e.com/a", "42"))
	require.NoError(t, err)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.pages, 2, "同 GUID 不同 feed 各建一页")
	assert.Equal(t, "feedA", pub.pages[0].AuthorName)
	assert.Equal(t, "feedB", pub.pages[1].AuthorName)
}

func TestSnapshotEmptyTitleFallback(t *testing.T) {
	fetcher := &fakeFetcher{html: `<p>` + strings.Repeat("正", 300) + `</p>`}
	pub := &fakePublisher{url: "https://telegra.ph/ok-1"}
	svc := newSnapSvc(fetcher, pub)

	_, err := svc.Snapshot(context.Background(), snapFeed("f", "@chan"), snapItem("", "https://e.com/a", "g1"))
	require.NoError(t, err)

	// Telegraph title 必填 1-256，空 title 用占位（否则建页 API 报错）
	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.pages, 1)
	assert.NotEmpty(t, pub.pages[0].Title)
}

// ---- author_url 构造（边界表：@name 去掉 @；裸名直接拼；纯数字 ID 不构造）----

func TestChannelURL(t *testing.T) {
	assert.Equal(t, "https://t.me/my_channel", channelURL("@my_channel"))
	assert.Equal(t, "https://t.me/my_channel", channelURL("my_channel"))
	assert.Empty(t, channelURL("-1001234567890"), "纯数字 ID（含 -100xxx）无公开用户名，不构造 author_url")
	assert.Empty(t, channelURL("1234567890"))
	assert.Empty(t, channelURL("@"))
}
