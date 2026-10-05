package rss

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"os"

	"github.com/Hootrix/rss2telegram/internal/config"
	"github.com/Hootrix/rss2telegram/internal/storage"
	"github.com/Hootrix/rss2telegram/internal/telegram"
	"github.com/Hootrix/rss2telegram/internal/tgmd"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/mmcdole/gofeed"
)

type RssHandler struct {
	sync.RWMutex
	parser     *gofeed.Parser
	config     *config.Config
	bot        TelegramBot
	storage    *storage.Storage
	sleepFn    func(time.Duration) // 可注入的 sleep（测试免真睡）；nil 时退回 time.Sleep
	nowFn      func() time.Time
	sendMu     sync.Mutex
	sendStates map[string]*channelSendState
}

type TelegramBot interface {
	Send(channel string, message string) error
}

type channelSendState struct {
	sync.Mutex
	nextSend time.Time
}

func NewRssHandler(cfg *config.Config, bot TelegramBot, store *storage.Storage) *RssHandler {
	return &RssHandler{
		parser:     gofeed.NewParser(),
		config:     cfg,
		bot:        bot,
		storage:    store,
		sleepFn:    time.Sleep,
		nowFn:      time.Now,
		sendStates: make(map[string]*channelSendState),
	}
}

func (h *RssHandler) now() time.Time {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now()
}

// sleep 统一走可注入的 sleepFn，nil 安全（直接字面量构造的 handler 如模板测试）
func (h *RssHandler) sleep(d time.Duration) {
	if h.sleepFn != nil {
		h.sleepFn(d)
		return
	}
	time.Sleep(d)
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
	skippedSeen := 0 // 已推送过被去重跳过的文章数，聚合进 finish 日志（替代原逐条打印）

	isFirstRun := true // 用于判断是否是第一次运行
	for _, channel := range feedConfig.Channels {
		// 检查 bloom 文件是否存在来判断是否是第一次运行
		bloomPath := h.storage.GetBloomFilePath(feedConfig.URL, channel)
		if _, err := os.Stat(bloomPath); err == nil {
			isFirstRun = false
			break
		}
	}

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

		// 如果是第一次运行且 first_push 为 false，则跳过所有项目
		if isFirstRun && !feedConfig.FirstPush {
			log.Printf("First run and first_push is false, skipping all items for feed: %s", feedConfig.Name)
			// 标记所有项目为已处理，这样下次运行时就不会重复处理
			for _, channel := range feedConfig.Channels {
				if err := h.storage.MarkItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID); err != nil {
					log.Printf("Error marking item as seen: %v", err)
				}
			}
			continue
		}

		for _, channel := range feedConfig.Channels {
			if !h.storage.IsItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID) {
				allChannelsProcessed = false
				break
			}
		}
		if allChannelsProcessed {
			// [日志降噪] 原逐条打印已推送文章的日志删除：稳态下每轮检查会对全部存量文章
			// 各刷一行（380 条的 feed 每轮 ~380 行），淹没真正有用的日志；
			// 改为 skippedSeen 聚合计数，在 processFeed finish 中汇总输出。原日志注释保留：
			// log.Printf("Item already processed by all channels: %s", item.Title)
			skippedSeen++
			continue
		}

		// 只有当文章有发布时间时才检查是否过期
		if item.PublishedParsed != nil {
			age := time.Since(*item.PublishedParsed)
			if feedConfig.ArticleExpirationDurationHours != nil {
				if age > time.Duration(*feedConfig.ArticleExpirationDurationHours)*time.Hour {
					log.Printf("Skipping old item (age: %v): %s", age, item.Title)
					continue
				}
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
	sem := make(chan struct{}, 1) // 单个feed下处理channel 最大并发数为1
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

				// [issue #6] 重试循环已提取为 sendWithRetry：429 按服务端 retry_after
				// 等待且独立计数，普通错误维持指数退避 3 次。
				// 旧实现保留备查（固定指数退避、flood 与普通错误共享 3 次配额、
				// 间隔 1s 超 Telegram 单频道 ~20 条/分钟限速）：
				// maxRetries := 3
				// var sendSuccess bool
				// var lastError error
				// for i := 0; i < maxRetries; i++ {
				// 	if err := h.bot.Send(channel, message); err != nil {
				// 		lastError = err
				// 		if i == maxRetries-1 {
				// 			log.Printf("Failed to send message to channel %s after %d retries: %v", channel, maxRetries, err)
				// 			break
				// 		}
				// 		log.Printf("Error sending message to channel %s (retry %d/%d): %v", channel, i+1, maxRetries, err)
				// 		h.ExponentialBackoffWithJitter(i)
				// 		continue
				// 	}
				// 	sendSuccess = true
				// 	break
				// }
				sendSuccess := h.sendWithRetry(channel, message, item.Title)

				// 只有在发送成功后才标记为已处理
				if sendSuccess {
					if err := h.storage.MarkItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID); err != nil {
						log.Printf("msg send success. MarkItemSeen ERROR!!  channel %s: %v", channel, err)
					}
					h.sleep(sendInterval) // 发送间隔（原 1s，已提至 sendInterval）
				}
				// [issue #6] 旧失败兜底日志已并入 sendWithRetry 内部失败日志（含 title），保留备查：
				// log.Printf("msg send Failed. item '%s' for channel 「%s」: %v", item.Title, channel, lastError)
			}(channel, item)
		}
	}

	wg.Wait() // 等待所有 goroutine 完成

	log.Printf("processFeed finish. name:%s, processed %d new items, skipped %d already-seen", feedConfig.Name, len(newItems), skippedSeen)
	return nil
}

// [issue #6] 发送重试参数
const (
	// 发送间隔：Telegram 对单频道的发送限速约 20 条/分钟，3s/条（≤20 条/分钟）
	// 可稳定限内；原 1s 间隔是大批首刷持续撞 429 的直接原因
	sendInterval = 3 * time.Second

	maxSendRetries  = 3 // 普通错误的总尝试次数（含首次），维持原语义
	maxFloodRetries = 5 // 429 flood 独立重试上限，不消耗普通配额
	// 旧说明保留：该上限只控制单次阻塞，不得截短合法的服务端冷却
	// maxFloodWait = 2 * time.Minute // 单次 flood 等待上限，防服务端异常值（如数小时）拖死 goroutine
	maxFloodWait = 2 * time.Minute
)

// sendWithRetry 带重试发送单条消息，返回是否成功。
//
// [issue #6] 双计数设计：普通错误走指数退避，总共 maxSendRetries 次尝试；
// 429 限速识别 RateLimitError 后按服务端 retry_after 指示等待再重试——
// 服务端明确告知何时可重试，固定指数退避（1s/2s/4s）必然全部撞墙，因此
// flood 等待不消耗普通配额、独立计数 maxFloodRetries 次防死循环。
// 超限放弃返回 false（不标 seen，由下轮 check 补推，与原失败语义一致）
/*
func (h *RssHandler) sendWithRetry(channel, message, itemTitle string) bool {
	for attempt, floodCount := 0, 0; ; {
		err := h.bot.Send(channel, message)
		if err == nil {
			log.Printf("Successfully sent message to channel %s: %s", channel, itemTitle)
			return true
		}

		var rlErr *telegram.RateLimitError
		if errors.As(err, &rlErr) {
			floodCount++
			if floodCount > maxFloodRetries {
				log.Printf("Failed to send item '%s' to channel %s after %d flood retries: %v", itemTitle, channel, maxFloodRetries, err)
				return false
			}
			wait := rlErr.RetryAfter + time.Second // +1s 缓冲，避免卡点重试再次撞限
			if wait > maxFloodWait {
				wait = maxFloodWait
			}
			log.Printf("Rate limited sending item '%s' to channel %s (flood retry %d/%d), waiting %v: %v", itemTitle, channel, floodCount, maxFloodRetries, wait, err)
			h.sleep(wait)
			continue
		}

		attempt++
		if attempt >= maxSendRetries {
			log.Printf("Failed to send item '%s' to channel %s after %d retries: %v", itemTitle, channel, maxSendRetries, err)
			return false
		}
		log.Printf("Error sending item '%s' to channel %s (retry %d/%d): %v", itemTitle, channel, attempt, maxSendRetries, err)
		h.ExponentialBackoffWithJitter(attempt - 1)
	}
}
*/

func (h *RssHandler) sendWithRetry(channel, message, itemTitle string) bool {
	state := h.channelState(channel)
	state.Lock()
	defer state.Unlock()

	for attempt, floodCount := 0, 0; ; {
		if !h.waitForSend(state, channel, itemTitle) {
			return false
		}
		err := h.bot.Send(channel, message)
		if err == nil {
			state.nextSend = h.now().Add(sendInterval)
			log.Printf("Successfully sent message to channel %s: %s", channel, itemTitle)
			return true
		}

		var rlErr *telegram.RateLimitError
		if errors.As(err, &rlErr) {
			floodCount++
			if !h.setFloodCooldown(state, rlErr, floodCount, channel, itemTitle) {
				return false
			}
			continue
		}

		attempt++
		if attempt >= maxSendRetries {
			log.Printf("Failed to send item '%s' to channel %s after %d retries: %v", itemTitle, channel, maxSendRetries, err)
			return false
		}
		log.Printf("Error sending item '%s' to channel %s (retry %d/%d): %v", itemTitle, channel, attempt, maxSendRetries, err)
		h.ExponentialBackoffWithJitter(attempt - 1)
	}
}

// 不同 feed 和检查轮次共享同一归一化频道的状态，feed 局部信号量无法保证频道限速
func (h *RssHandler) channelState(channel string) *channelSendState {
	key := telegram.ChannelKey(channel)
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if h.sendStates == nil {
		h.sendStates = make(map[string]*channelSendState)
	}
	state := h.sendStates[key]
	if state == nil {
		state = &channelSendState{}
		h.sendStates[key] = state
	}
	return state
}

func (h *RssHandler) waitForSend(state *channelSendState, channel, itemTitle string) bool {
	wait := state.nextSend.Sub(h.now())
	if wait <= 0 {
		return true
	}
	// 超过阻塞预算只延后处理，保留完整截止时间，不得在服务端冷却期内重发
	if wait > maxFloodWait {
		log.Printf("Deferring item '%s' to channel %s, cooldown remaining %v", itemTitle, channel, wait)
		return false
	}
	h.sleep(wait)
	return true
}

// 先保存完整冷却再检查重试上限，避免放弃上一条消息后立即发送下一条
func (h *RssHandler) setFloodCooldown(state *channelSendState, err *telegram.RateLimitError, floodCount int, channel, itemTitle string) bool {
	if err.RetryAfter <= 0 || err.RetryAfter > time.Duration(1<<63-1)-time.Second {
		log.Printf("Invalid retry_after for item '%s' to channel %s: %v", itemTitle, channel, err)
		return false
	}
	wait := err.RetryAfter + time.Second
	state.nextSend = h.now().Add(wait)
	if floodCount > maxFloodRetries {
		log.Printf("Failed to send item '%s' to channel %s after %d flood retries: %v", itemTitle, channel, maxFloodRetries, err)
		return false
	}
	log.Printf("Rate limited sending item '%s' to channel %s (flood retry %d/%d), cooldown %v: %v", itemTitle, channel, floodCount, maxFloodRetries, wait, err)
	return true
}

// 指数退避+随机抖动
func (h *RssHandler) ExponentialBackoffWithJitter(attempt int) {
	base := time.Second
	maxJitter := 500 * time.Millisecond                    // 最大抖动 500毫秒
	delay := base * time.Duration(1<<attempt)              // 指数退避。1<<attempt表示attemp的2次幂
	jitter := time.Duration(rand.Int63n(int64(maxJitter))) // 随机抖动
	h.sleep(delay + jitter)                                // 走可注入 sleep，测试免真睡
}

// 格式化消息
func (h *RssHandler) formatMessage(item *gofeed.Item, template string) string {
	if template == "" {
		template = "{title}\n\n{link}" // 默认模板
	}

	processor := NewTemplateProcessor()
	converter := md.NewConverter("", true, &md.Options{
		EscapeMode: "disabled", // 禁用转义  包括针对|的转义
	})

	// 编译正则表达式，用于将图片标记转换为链接
	imgRegex := regexp.MustCompile(`!\[(.*?)\]\((.*?)\)`)

	replaceOpFieldFunc := func(match, field string) string {
		// 获取基础字段内容
		var content string
		basefield := strings.SplitN(field, "|", 2)[0]
		switch basefield {
		case "title":
			// title 是纯文本数据，不经 HTML→Markdown converter，需在此转义 legacy
			// Markdown 特殊字符，否则裸 * [ 等会触发 Telegram 400 (issue #4)
			// 转义在操作链之前：用户在操作参数里手写的 Markdown 不受影响；
			// 代价是 extract/replace 的正则匹配的是转义后文本（如 3*4 → 3\*4）
			content = tgmd.Escape(item.Title)
		case "description":
			if item.Description != "" {
				// 将 HTML 转换为 Markdown
				mdContent, err := converter.ConvertString(item.Description)
				if err != nil {
					log.Printf("Error converting HTML to Markdown: %v", err)
					content = item.Description
				} else {
					// 将图片标记转换为链接
					content = imgRegex.ReplaceAllString(mdContent, "[Media]($2)")
				}
			}
		case "content":
			if item.Content != "" {
				// 将 HTML 转换为 Markdown
				mdContent, err := converter.ConvertString(item.Content)
				if err != nil {
					log.Printf("Error converting HTML to Markdown: %v", err)
					content = item.Content
				} else {
					// 将图片标记转换为链接
					content = imgRegex.ReplaceAllString(mdContent, "[Media]($2)")
				}
			}
		case "link":
			content = item.Link
		case "pubDate":
			if item.PublishedParsed != nil {
				content = item.PublishedParsed.Format("2006-01-02 15:04:05")
			}
		default:
			return match
		}

		// 处理操作链
		return processor.ProcessField(field, content)
	}

	// 使用正则表达式找出所有模板字段
	fieldRegex := regexp.MustCompile(`{ (.*?) }`) //支持正则中使用花括号
	message := fieldRegex.ReplaceAllStringFunc(template, func(match string) string {
		// 去掉花括号
		field := match[2 : len(match)-2]

		return replaceOpFieldFunc(match, field)
	})

	fieldRegex = regexp.MustCompile(`{([^}]+)}`) //正则中不使用花括号的情况
	message = fieldRegex.ReplaceAllStringFunc(message, func(match string) string {
		// 去掉花括号
		field := match[1 : len(match)-1]

		return replaceOpFieldFunc(match, field)
	})

	// 清理多余的空行
	message = strings.TrimSpace(message)
	for strings.Contains(message, "\n\n\n") {
		message = strings.ReplaceAll(message, "\n\n\n", "\n\n")
	}

	return message
}
