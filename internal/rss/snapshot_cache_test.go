package rss

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// newTestCache 构造小容量短 TTL 缓存并暴露 now 注入
func newTestCache(ttl time.Duration, cap int) (*SnapshotCache, *time.Time) {
	base := time.Now()
	now := &base
	c := NewSnapshotCache(ttl, cap)
	c.now = func() time.Time { return *now }
	return c, now
}

func TestSnapshotCacheSetGet(t *testing.T) {
	c, _ := newTestCache(time.Hour, 10)
	_, ok := c.get("feed|item1")
	assert.False(t, ok, "空缓存必 miss")

	c.set("feed|item1", "https://telegra.ph/a")
	url, ok := c.get("feed|item1")
	assert.True(t, ok)
	assert.Equal(t, "https://telegra.ph/a", url)
}

func TestSnapshotCacheTTLExpiry(t *testing.T) {
	c, now := newTestCache(24*time.Hour, 10)
	c.set("feed|item1", "https://telegra.ph/a")

	*now = now.Add(23 * time.Hour)
	_, ok := c.get("feed|item1")
	assert.True(t, ok, "TTL 内命中")

	*now = now.Add(2 * time.Hour) // 累计 25h，覆盖跨轮重试窗口后过期
	_, ok = c.get("feed|item1")
	assert.False(t, ok, "TTL 过期后 miss")
}

func TestSnapshotCacheLRUEviction(t *testing.T) {
	c, _ := newTestCache(time.Hour, 2)
	c.set("k1", "u1")
	c.set("k2", "u2")
	c.get("k1") // k1 变为最近使用，k2 成为最旧

	c.set("k3", "u3") // 容量 2，淘汰最旧的 k2
	_, ok := c.get("k2")
	assert.False(t, ok, "最旧条目被 LRU 淘汰")
	_, ok = c.get("k1")
	assert.True(t, ok, "刚访问过的保留")
	_, ok = c.get("k3")
	assert.True(t, ok)
}

func TestSnapshotCacheOverwriteNoDuplicate(t *testing.T) {
	c, _ := newTestCache(time.Hour, 2)
	c.set("k1", "u1")
	c.set("k1", "u2") // 同 key 覆盖，不占新名额
	assert.Equal(t, 1, c.len())
	url, _ := c.get("k1")
	assert.Equal(t, "u2", url, "覆盖后取新值")
}
