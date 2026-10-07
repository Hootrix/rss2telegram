package rss

// 快照结果缓存：key = feed.Name + "|" + itemID
// GUID 只保证 feed 内唯一（常见自增数字，跨 feed 必撞），feed.Name 配置层
// 强制唯一 → 组合后系统内唯一；同 URL 拆多条 feed 时各建各页，归属正确（CR2-#2）
// 只缓存成功结果：降级值（原文链接/空串）绝不入缓存，避免临时失败被永久固化

import (
	"container/list"
	"sync"
	"time"
)

const (
	defaultSnapshotCacheTTL = 24 * time.Hour // 覆盖跨轮重试
	defaultSnapshotCacheCap = 1024
)

// snapshotPageParams 建页参数快照：editPage 为全量替换语义（不传即清空），
// author_url 回填需原样重提交 title/content/author_name（issue #12）
type snapshotPageParams struct {
	URL        string
	Path       string // editPage 用：URL 去掉页面域前缀
	Title      string
	Content    []any
	AuthorName string
	// Backfilled 表示 author_url 已回填过消息链接：多频道场景首个成功回填
	// 即锁定，后续频道不再重复 editPage（外部 CR：串行调用白做且放大调用量）
	Backfilled bool
}

type cacheEntry struct {
	key       string
	page      snapshotPageParams
	expiresAt time.Time
}

// SnapshotCache mutex 保护的 TTL+LRU 缓存
// 不用 singleflight：合并调用继承第一个调用者的 ctx，其取消会带崩父 ctx 正常
// 的其他调用者（CR2-#4）；重复建页概率极低，与"重启后重复建页"同一接受标准
type SnapshotCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	cap     int
	entries map[string]*list.Element
	order   *list.List // front = 最近使用
	now     func() time.Time
}

func NewSnapshotCache(ttl time.Duration, cap int) *SnapshotCache {
	if ttl <= 0 {
		ttl = defaultSnapshotCacheTTL
	}
	if cap <= 0 {
		cap = defaultSnapshotCacheCap
	}
	return &SnapshotCache{
		ttl:     ttl,
		cap:     cap,
		entries: make(map[string]*list.Element),
		order:   list.New(),
		now:     time.Now,
	}
}

// get 命中返回建页参数快照；过期条目即删（惰性淘汰）
func (c *SnapshotCache) get(key string) (snapshotPageParams, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return snapshotPageParams{}, false
	}
	entry := el.Value.(*cacheEntry)
	if !c.now().Before(entry.expiresAt) {
		c.order.Remove(el)
		delete(c.entries, key)
		return snapshotPageParams{}, false
	}
	c.order.MoveToFront(el)
	return entry.page, true
}

// set 写入并置于队首；超容量按最旧淘汰（LRU 兜底，防慢泄漏）
func (c *SnapshotCache) set(key string, page snapshotPageParams) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*cacheEntry).page = page
		el.Value.(*cacheEntry).expiresAt = c.now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{
		key:       key,
		page:      page,
		expiresAt: c.now().Add(c.ttl),
	})
	c.entries[key] = el
	for c.order.Len() > c.cap {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

// markBackfilled 置位回填完成标记（幂等）；条目已被淘汰/不存在时 no-op
func (c *SnapshotCache) markBackfilled(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*cacheEntry).page.Backfilled = true
	}
}

// len 供测试断言容量行为
func (c *SnapshotCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
