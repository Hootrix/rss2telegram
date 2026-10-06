package rss

// Telegraph 快照编排：组合 extractor 抓取提取 + Node 转换 + telegraph 发布 + 结果缓存
// 失败语义（两类，严格区分，CR2-#4）：
//   - 父 ctx 已取消（进程退出/整轮取消/feed 预算耗尽）→ 返回错误供上层延后，
//     不降级不发送（此时发送也必失败）；判定用 parent.Err() 而非错误类型，
//     因为子 ctx 继承父截止时间，父预算耗尽时子 ctx 返回的同样是 DeadlineExceeded
//   - 快照自身失败（子超时/HTTP/超限/Content-Type/乱码/正文过短/发布失败）
//     → 降级返回原文链接（item.Link），消息照发，日志记录

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/telegraph"
	"github.com/mmcdole/gofeed"
)

// createPage 独立子超时（发布阶段），与抓取 15s 分开计
const defaultPublishTimeout = 10 * time.Second

// title 占位：Telegraph title 必填 1-256，RSS item 可能无标题
const snapshotUntitled = "无标题"

// pageFetcher / pagePublisher 抽象 extractor 与 telegraph client，测试可替换
type pageFetcher interface {
	FetchAndExtract(ctx context.Context, rawURL string) (string, error)
}

type pagePublisher interface {
	CreatePage(ctx context.Context, page telegraph.Page) (string, error)
}

type SnapshotService struct {
	fetcher        pageFetcher
	publisher      pagePublisher
	cache          *SnapshotCache
	publishTimeout time.Duration
}

func NewSnapshotService(fetcher pageFetcher, publisher pagePublisher, cache *SnapshotCache) *SnapshotService {
	if cache == nil {
		cache = NewSnapshotCache(0, 0)
	}
	return &SnapshotService{
		fetcher:        fetcher,
		publisher:      publisher,
		cache:          cache,
		publishTimeout: defaultPublishTimeout,
	}
}

// Snapshot 返回快照页 URL。
// item 无 link 时跳过快照返回空串（{telegraph} 渲染空串，与 {link} 行为一致）；
// 快照自身失败时降级为原文链接且 err 为 nil；父 ctx 取消时返回 ctx 错误（上层延后）
func (s *SnapshotService) Snapshot(ctx context.Context, feed config.FeedConfig, item *gofeed.Item) (string, error) {
	if item.Link == "" {
		return "", nil
	}

	// 缓存 key 带 feed 维度：GUID 仅 feed 内唯一，跨 feed 必撞（CR2-#2）
	key := feed.Name + "|" + generateItemID(item)
	if url, ok := s.cache.get(key); ok {
		return url, nil
	}

	html, err := s.fetcher.FetchAndExtract(ctx, item.Link)
	if err != nil {
		return s.settle(ctx, item, fmt.Errorf("fetch %s: %w", item.Link, err))
	}

	content := truncateContent(htmlToNodes(html), item.Link, maxContentBytes)

	title := truncateByRunes(item.Title, 256)
	if title == "" {
		title = snapshotUntitled
	}

	pubCtx, cancel := context.WithTimeout(ctx, s.publishTimeout)
	defer cancel()
	url, err := s.publisher.CreatePage(pubCtx, telegraph.Page{
		Title:      title,
		AuthorName: truncateByRunes(feed.Name, 128),
		AuthorURL:  channelURL(feed.Channels[0]),
		Content:    content,
	})
	if err != nil {
		return s.settle(ctx, item, fmt.Errorf("createPage %s: %w", item.Link, err))
	}

	// 只缓存成功：降级值不入缓存，避免一次临时失败被永久固化
	s.cache.set(key, url)
	return url, nil
}

// settle 统一失败结算：父取消 → 延后（错误）；否则降级照发（原文链接 + 日志）
func (s *SnapshotService) settle(ctx context.Context, item *gofeed.Item, cause error) (string, error) {
	if ctx.Err() != nil {
		return "", fmt.Errorf("snapshot %s: %w (parent cancelled)", item.Link, ctx.Err())
	}
	log.Printf("snapshot degraded to original link, item %q: %v", item.Title, cause)
	return item.Link, nil
}

// channelURL 构造频道主页链接作为 Telegraph author_url：
// @name 去掉 @ 拼接；裸名直接拼接；纯数字 ID（-100xxx）无公开用户名不构造
// 私有频道主页不外可访问，已确认接受（设计边界表）
func channelURL(channel string) string {
	name := strings.TrimPrefix(channel, "@")
	if name == "" {
		return ""
	}
	id := strings.TrimPrefix(name, "-")
	if id != "" && strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return ""
	}
	return "https://t.me/" + name
}
