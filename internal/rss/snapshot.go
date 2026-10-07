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
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/extractor"
	"github.com/Hootrix/rss2telegram/internal/telegraph"
	"github.com/mmcdole/gofeed"
)

// createPage 独立子超时（发布阶段），与抓取 15s 分开计
const defaultPublishTimeout = 10 * time.Second

// title 占位：Telegraph title 必填 1-256，RSS item 可能无标题
const snapshotUntitled = "无标题"

// 快照页面域（createPage 返回 URL 的固定前缀；缓存条目 Path 由其截取）
const telegraphPagePrefix = "https://telegra.ph/"

// feed 来源正文缺失（Content 与 Description 皆空）
var errEmptyFeedContent = errors.New("empty feed content")

// pageFetcher / pagePublisher 抽象 extractor 与 telegraph client，测试可替换
type pageFetcher interface {
	FetchAndExtract(ctx context.Context, rawURL string) (string, error)
}

type pagePublisher interface {
	CreatePage(ctx context.Context, page telegraph.Page) (string, error)
	// EditPage 全量替换语义：title/content 必传，author 不传即清空（author_url 回填用）
	EditPage(ctx context.Context, path string, page telegraph.Page) error
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
	if page, ok := s.cache.get(key); ok {
		return page.URL, nil
	}

	// 旧实现（snapshot_source 分派前：正文固定抓原文）注释保留：
	// html, err := s.fetcher.FetchAndExtract(ctx, item.Link)
	// if err != nil {
	//     return s.settle(ctx, item, fmt.Errorf("fetch %s: %w", item.Link, err))
	// }
	// content := truncateContent(htmlToNodes(html), item.Link, maxContentBytes)
	nodes, err := s.sourceNodes(ctx, feed, item)
	if err != nil {
		return s.settle(ctx, item, err)
	}
	content := truncateContent(nodes, item.Link, maxContentBytes)

	title := truncateByRunes(item.Title, 256)
	if title == "" {
		title = snapshotUntitled
	}

	pubCtx, cancel := context.WithTimeout(ctx, s.publishTimeout)
	defer cancel()
	authorName := truncateByRunes(feed.Name, 128)
	// feed.Channels 为空属 Validate 之外的非法输入（本服务是导出 API）：
	// 不 panic，按无公开频道处理，author_url 留空由回填兜底
	authorURL := ""
	if len(feed.Channels) > 0 {
		authorURL = channelURL(feed.Channels[0])
	}
	url, err := s.publisher.CreatePage(pubCtx, telegraph.Page{
		Title:      title,
		AuthorName: authorName,
		AuthorURL:  authorURL,
		Content:    content,
	})
	if err != nil {
		return s.settle(ctx, item, fmt.Errorf("createPage %s: %w", item.Link, err))
	}

	// 只缓存成功：降级值不入缓存，避免一次临时失败被永久固化。
	// 缓存完整建页参数（而非仅 URL）：editPage 全量替换语义下回填 author_url 需原样重提交
	s.cache.set(key, snapshotPageParams{
		URL:        url,
		Path:       strings.TrimPrefix(url, telegraphPagePrefix),
		Title:      title,
		Content:    content,
		AuthorName: authorName,
	})
	return url, nil
}

// BackfillAuthor 将快照页 author_url 回填为消息链接（t.me/<channel>/<msg_id>）。
// 尽力而为：未建页/降级（降级值不入缓存，天然 miss）与缓存缺失（进程重启前的旧条目）
// 静默跳过返回 nil；editPage 失败返回错误由调用方记日志，不影响已推送的消息。
// 多频道场景每次覆盖（最后成功者生效——单页只装得下一个消息链接）
func (s *SnapshotService) BackfillAuthor(ctx context.Context, feed config.FeedConfig, item *gofeed.Item, msgLink string) error {
	// 与 Snapshot 同一 key：降级（fetch 失败等）不入缓存，get 必 miss → 跳过
	page, ok := s.cache.get(feed.Name + "|" + generateItemID(item))
	if !ok {
		return nil
	}

	publishCtx, cancel := context.WithTimeout(ctx, s.publishTimeout)
	defer cancel()
	if err := s.publisher.EditPage(publishCtx, page.Path, telegraph.Page{
		Title:      page.Title,
		AuthorName: page.AuthorName,
		AuthorURL:  msgLink,
		Content:    page.Content,
	}); err != nil {
		return fmt.Errorf("editPage %s: %w", page.URL, err)
	}
	return nil
}

// sourceNodes 取快照正文并转为 Telegraph 节点，按 feed.SnapshotSource 分派（issue #12）：
//   - page（默认）：抓原文 + readability
//   - feed：取 RSS item 正文（Content 优先，空取 Description），零网络抓取；
//     不过 readability（RSS 正文已是正文片段，洗片段只有洗坏风险）
//
// 两路径正文质量统一由 validateNodes 校验（2026-10-08 前在 extractor 内，
// 移交 rss 层是为纯图帖豁免字数下限——节点层才能数图）。
// 失败统一由调用方 settle：feed 内容问题不回退抓原文（选 feed 的典型场景抓原文本就无效）
func (s *SnapshotService) sourceNodes(ctx context.Context, feed config.FeedConfig, item *gofeed.Item) ([]any, error) {
	if feed.SnapshotSource == config.SnapshotSourceFeed {
		nodes, err := feedContentNodes(item)
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", feed.Name, err)
		}
		return nodes, nil
	}

	html, err := s.fetcher.FetchAndExtract(ctx, item.Link)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", item.Link, err)
	}
	nodes := htmlToNodes(html)
	if err := validateNodes(nodes); err != nil {
		return nil, fmt.Errorf("validate %s: %w", item.Link, err)
	}
	return nodes, nil
}

// feedContentNodes 从 RSS item 取正文 HTML 转节点并交统一校验：
// 摘要型误配（无图短文）由 validateNodes 拦截，错误信息带 rune 数便于排查
func feedContentNodes(item *gofeed.Item) ([]any, error) {
	raw := item.Content
	if raw == "" {
		raw = item.Description
	}
	if raw == "" {
		return nil, errEmptyFeedContent
	}
	nodes := htmlToNodes(raw)
	if err := validateNodes(nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// validateNodes 正文质量统一校验（page/feed 两路径，2026-10-08 纯图帖豁免）：
//   - 乱码必拦，含图不豁免（乱码图帖页面同样不可读）
//   - 含 ≥1 张有效图豁免字数下限（纯图帖建页；有效性由转换层保证——
//     非 http(s) 图已在 htmlToNodes 丢弃，这里的 img 节点天然全部有效）
//   - 无图走 ValidateText：纯文本不足 200 rune 判过短
func validateNodes(nodes []any) error {
	plain := nodesPlainText(nodes)
	if err := extractor.ValidateMojibake(plain); err != nil {
		return err
	}
	if hasImageNode(nodes) {
		return nil
	}
	return extractor.ValidateText(plain)
}

// hasImageNode 判断根级节点数组是否含图；转换层只产出根级 img（Telegraph 平级结构）
func hasImageNode(nodes []any) bool {
	for _, n := range nodes {
		if node, ok := n.(telegraph.Node); ok && node.Tag == "img" {
			return true
		}
	}
	return false
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
	// id 为空说明 name 是 "-"（非法频道标识），同样不构造；原实现此处
	// 判 id != ""，会把 "https://t.me/-" 当合法链接返回
	if id == "" || strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return ""
	}
	return "https://t.me/" + name
}
