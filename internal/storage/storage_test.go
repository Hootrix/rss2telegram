package storage

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
)

// 回归测试：issue#5 —— bloom 文件时间戳超过 30 天后，重新加载时不应清零去重记忆
// 背景：updatedAt 仅在推送新文章（MarkItemSeen）时刷新，低频 feed 长期无新文章时
// 时间戳会自然老化，若加载时因过期而清零，每次进程重启都会导致该 feed 全量重推
func TestBloomMemorySurvivesExpiredTimestamp(t *testing.T) {
	dir := t.TempDir()

	const (
		feedURL  = "https://example.com/feed.xml"
		feedName = "test-feed"
		channel  = "@test_channel"
		itemID   = "item-1"
	)

	// 第一步：正常创建状态并标记一条已推送记录
	s, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := s.MarkItemSeen(feedURL, feedName, channel, itemID); err != nil {
		t.Fatalf("MarkItemSeen: %v", err)
	}

	// 第二步：把 bloom 文件头 8 字节时间戳改写为 35 天前（模拟低频 feed 长期无新文章）
	bloomPath := s.GetBloomFilePath(feedURL, channel)
	f, err := os.OpenFile(bloomPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open bloom file: %v", err)
	}
	stale := make([]byte, 8)
	binary.LittleEndian.PutUint64(stale, uint64(time.Now().Add(-35*24*time.Hour).UnixNano()))
	if _, err := f.WriteAt(stale, 0); err != nil {
		t.Fatalf("rewrite timestamp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close bloom file: %v", err)
	}

	// 第三步：模拟重启（重新加载全部状态），去重记忆必须保留
	s2, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("reload NewStorage: %v", err)
	}
	if !s2.IsItemSeen(feedURL, feedName, channel, itemID) {
		t.Fatal("去重记忆被过期判定清零：35 天前的 bloom 加载后 item 应仍视为已见（issue#5）")
	}
}
