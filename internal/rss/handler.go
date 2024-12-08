package rss

import (
	"crypto/sha256"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/storage"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/mmcdole/gofeed"
)

type RssHandler struct {
	sync.RWMutex
	parser  *gofeed.Parser
	config  *config.Config
	bot     TelegramBot
	storage *storage.Storage
}

type TelegramBot interface {
	Send(channel string, message string) error
}

const (
	// 文章过期时间
	// 超过30天的旧文章不推送
	articleExpirationDuration = 30 * 24 * time.Hour // 30 天，与存储模块保持一致
)

func NewRssHandler(cfg *config.Config, bot TelegramBot, store *storage.Storage) *RssHandler {
	return &RssHandler{
		parser:  gofeed.NewParser(),
		config:  cfg,
		bot:     bot,
		storage: store,
	}
}

func (h *RssHandler) UpdateConfig(cfg *config.Config) {
	h.Lock()
	defer h.Unlock()
	h.config = cfg
	log.Printf("RSS处理器配置已更新")
}

func (h *RssHandler) ProcessFeeds() error {
	h.RLock()
	cfg := h.config
	h.RUnlock()

	var wg sync.WaitGroup
	// 使用信号量限制并发数量，避免过多的并发请求
	semaphore := make(chan struct{}, 2) // 处理feed name 最多2个并发

	// 用于收集错误的channel
	errChan := make(chan error, len(cfg.Feeds))

	for _, feed := range cfg.Feeds {
		wg.Add(1)
		go func(feed config.FeedConfig) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			//释放信号量
			defer func() { <-semaphore }()

			if err := h.processFeed(feed); err != nil {
				log.Printf("Error processing feed %s: %v", feed.Name, err)
				errChan <- fmt.Errorf("feed %s: %w", feed.Name, err)
			}
		}(feed)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(errChan)

	// 收集所有错误
	var errors []string
	for err := range errChan {
		if err != nil {
			errors = append(errors, err.Error())
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors processing feeds: %s", strings.Join(errors, "; "))
	}
	return nil
}

// 生成项目的唯一标识
func generateItemID(item *gofeed.Item) string {
	// 优先使用 GUID
	if item.GUID != "" {
		return item.GUID
	}

	// 如果没有 GUID，使用链接
	if item.Link != "" {
		return item.Link
	}

	// 如果都没有，使用标题和发布时间的组合
	if item.Title != "" && item.Published != "" {
		return item.Title + "|" + item.Published
	}

	// 最后才使用内容哈希
	return fmt.Sprintf("content:%x", sha256.Sum256([]byte(item.Content)))
}

func (h *RssHandler) processFeed(feedConfig config.FeedConfig) error {
	log.Printf("Processing feed: %s (%s)", feedConfig.Name, feedConfig.URL)

	feed, err := h.parser.ParseURL(feedConfig.URL)
	if err != nil {
		return fmt.Errorf("error parsing feed %s: %w", feedConfig.Name, err)
	}

	if len(feed.Items) == 0 {
		log.Printf("No items found in feed: %s", feedConfig.Name)
		return nil
	}

	// 处理新项目
	var newItems []*gofeed.Item
	seenInThisRun := make(map[string]bool)

	// 对所有项目进行处理，不再依赖发布时间排序
	for _, item := range feed.Items {
		if item.Title == "" && item.Link == "" {
			log.Printf("Skipping item without title and link in feed %s", feedConfig.Name)
			continue
		}

		itemID := generateItemID(item)

		// 检查是否在本次运行中已经处理过
		if seenInThisRun[itemID] {
			log.Printf("Item already seen in this run: %s", item.Title)
			continue
		}

		// 检查是否所有频道都已经处理过这个项目
		allChannelsProcessed := true
		for _, channel := range feedConfig.Channels {
			if !h.storage.IsItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID) {
				allChannelsProcessed = false
				break
			}
		}
		if allChannelsProcessed {
			log.Printf("Item already processed by all channels: %s", item.Title)
			continue
		}

		// 只有当文章有发布时间时才检查是否过期
		if item.PublishedParsed != nil {
			age := time.Since(*item.PublishedParsed)
			if age > articleExpirationDuration {
				log.Printf("Skipping old item (age: %v): %s", age, item.Title)
				continue
			}
		}

		newItems = append(newItems, item)
		seenInThisRun[itemID] = true
	}

	// 如果有发布时间的文章，按时间排序
	if len(newItems) > 0 {
		// 分离有发布时间和没有发布时间的文章
		var withTime, withoutTime []*gofeed.Item
		for _, item := range newItems {
			if item.PublishedParsed != nil {
				withTime = append(withTime, item)
			} else {
				withoutTime = append(withoutTime, item)
			}
		}

		// 对有发布时间的文章排序（从旧到新）
		if len(withTime) > 0 {
			sort.Slice(withTime, func(i, j int) bool {
				return withTime[i].PublishedParsed.Before(*withTime[j].PublishedParsed)
			})
		}

		// 重新组合：先发送有时间的旧文章，再发送无时间的文章
		newItems = append(withTime, withoutTime...)
	}

	// 处理新项目（推送文章）
	// 使用信号量控制并发数
	sem := make(chan struct{}, 2) // 单个feed下处理channel 最大并发数为2
	var wg sync.WaitGroup

	for _, item := range newItems {
		itemID := generateItemID(item)

		// 并发处理每个channel
		for _, channel := range feedConfig.Channels {
			// 检查这个 channel 是否已经处理过这个 item
			if h.storage.IsItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID) {
				log.Printf("Item %s already processed for channel %s", item.Title, channel)
				continue
			}

			// 格式化消息
			message := h.formatMessage(item, feedConfig.Template)
			if message == "" {
				log.Printf("formatMessage Empty Result, skip. RSS item title: %s", item.Title)
				continue
			}

			wg.Add(1)
			go func(channel string, item *gofeed.Item) {
				defer wg.Done()
				sem <- struct{}{}        // 获取信号量
				defer func() { <-sem }() // 释放信号量

				// 多次重试发送消息（包含第一次请求）
				maxRetries := 3
				var sendSuccess bool
				var lastError error
				for i := 0; i < maxRetries; i++ {
					if err := h.bot.Send(channel, message); err != nil {
						lastError = err
						if i == maxRetries-1 {
							log.Printf("Failed to send message to channel %s after %d retries: %v", channel, maxRetries, err)
							break
						}
						log.Printf("Error sending message to channel %s (retry %d/%d): %v", channel, i+1, maxRetries, err)
						h.ExponentialBackoffWithJitter(i)
						continue
					}
					log.Printf("Successfully sent message to channel %s: %s", channel, item.Title)
					sendSuccess = true
					break // 发送成功，退出重试循环
				}

				// 只有在发送成功后才标记为已处理
				if sendSuccess {
					if err := h.storage.MarkItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID); err != nil {
						log.Printf("msg send success. MarkItemSeen ERROR!!  channel %s: %v", channel, err)
					}
					time.Sleep(time.Second) // 发送间隔 1 秒
				} else if lastError != nil {
					// 如果发送失败且有错误，记录到日志
					log.Printf("msg send Failed. item '%s' for channel 「%s」: %v", item.Title, channel, lastError)
				}
			}(channel, item)
		}
	}

	wg.Wait() // 等待所有 goroutine 完成

	log.Printf("processFeed finish. name:%s, processed %d new items", feedConfig.Name, len(newItems))
	return nil
}

// 指数退避+随机抖动
func (h *RssHandler) ExponentialBackoffWithJitter(attempt int) {
	base := time.Second
	maxJitter := 500 * time.Millisecond                    // 最大抖动 500毫秒
	delay := base * time.Duration(1<<attempt)              // 指数退避。1<<attempt表示attemp的2次幂
	jitter := time.Duration(rand.Int63n(int64(maxJitter))) // 随机抖动
	time.Sleep(delay + jitter)
}

// 格式化消息
func (h *RssHandler) formatMessage(item *gofeed.Item, template string) string {
	if template == "" {
		template = "{title}\n\n{link}" // 默认模板
	}

	message := template
	converter := md.NewConverter("", true, nil)

	// 编译正则表达式，用于将图片标记转换为链接
	imgRegex := regexp.MustCompile(`!\[(.*?)\]\((.*?)\)`)

	if item.Title != "" {
		message = strings.ReplaceAll(message, "{title}", item.Title)
	}
	if item.Link != "" {
		message = strings.ReplaceAll(message, "{link}", item.Link)
	}
	if item.Description != "" {
		// 将 HTML 转换为 Markdown
		mdDescription, err := converter.ConvertString(item.Description)
		if err != nil {
			log.Printf("Error converting HTML to Markdown: %v", err)
			mdDescription = item.Description
		}
		// 将图片标记转换为链接
		mdDescription = imgRegex.ReplaceAllString(mdDescription, "[Media]($2)") // 保持 alt 文本和 URL，只去掉感叹号
		message = strings.ReplaceAll(message, "{description}", mdDescription)
	}
	if item.Content != "" {
		// 将 HTML 转换为 Markdown
		mdContent, err := converter.ConvertString(item.Content)
		if err != nil {
			log.Printf("Error converting HTML to Markdown: %v", err)
			mdContent = item.Content
		}
		// 将图片标记转换为链接
		mdContent = imgRegex.ReplaceAllString(mdContent, "[Media]($2)") // 保持 alt 文本和 URL，只去掉感叹号
		message = strings.ReplaceAll(message, "{content}", mdContent)
	}
	if item.PublishedParsed != nil {
		pubDate := item.PublishedParsed.Format("2006-01-02 15:04:05")
		message = strings.ReplaceAll(message, "{pubDate}", pubDate)
	}

	// 清理未被替换的占位符
	message = strings.ReplaceAll(message, "{title}", "")
	message = strings.ReplaceAll(message, "{link}", "")
	message = strings.ReplaceAll(message, "{description}", "")
	message = strings.ReplaceAll(message, "{content}", "")
	message = strings.ReplaceAll(message, "{pubDate}", "")

	// 清理多余的空行
	message = strings.TrimSpace(message)
	for strings.Contains(message, "\n\n\n") {
		message = strings.ReplaceAll(message, "\n\n\n", "\n\n")
	}

	return message
}
