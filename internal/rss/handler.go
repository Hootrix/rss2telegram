package rss

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strconv"
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
	parser   *gofeed.Parser
	config   *config.Config
	bot      TelegramBot
	storage  *storage.Storage
	snapshot Snapshotter
	// 图片下载器（issue #13）；nil 时 media=photo 的 feed 也只降级文本不 panic
	// （直接字面量构造的测试 handler 即此形态）
	photoFetcher PhotoFetcher
	// sleepFn func(time.Duration) // 可注入的 sleep（测试免真睡）；nil 时退回 time.Sleep
	waitFn     func(context.Context, time.Duration) error
	nowFn      func() time.Time
	sendMu     sync.Mutex
	sendStates map[string]*channelSendState
	// roundBudget   time.Duration
	// 整轮共享预算会让靠前的积压/挂起 feed 每轮耗尽 deadline，后续 feed 永远轮不到，改为每 feed 独立预算
	feedBudget    time.Duration
	messageBudget time.Duration
}

type TelegramBot interface {
	// Send(channel string, message string) error
	// [issue #12] 返回 msgID 供快照页 author_url 回填消息链接
	Send(context.Context, string, *telegram.Message) (int64, error)
}

// Snapshotter 快照编排抽象（*SnapshotService 实现）；
// nil 时配置了 snapshot 的 feed 也只跳过快照不 panic
type Snapshotter interface {
	Snapshot(ctx context.Context, feed config.FeedConfig, item *gofeed.Item) (string, error)
	// BackfillAuthor 发送成功后将快照页 author_url 更新为 msgLink（issue #12，尽力而为）
	BackfillAuthor(ctx context.Context, feed config.FeedConfig, item *gofeed.Item, msgLink string) error
}

type channelSendState struct {
	// sync.Mutex
	gate     chan struct{}
	nextSend time.Time
}

func NewRssHandler(cfg *config.Config, bot TelegramBot, store *storage.Storage, snapshot Snapshotter) *RssHandler {
	parser := gofeed.NewParser()
	// ParseURLWithContext 可取消请求；并发读取前初始化 SDK 惰性字段，避免竞争写入
	parser.Client = &http.Client{Timeout: maxBlockingBudget}
	parser.RSSTranslator = &gofeed.DefaultRSSTranslator{}
	parser.AtomTranslator = &gofeed.DefaultAtomTranslator{}
	parser.JSONTranslator = &gofeed.DefaultJSONTranslator{}
	return &RssHandler{
		// parser: gofeed.NewParser(),
		parser:       parser,
		config:       cfg,
		bot:          bot,
		storage:      store,
		snapshot:     snapshot,
		photoFetcher: newHTTPPhotoFetcher(),
		// sleepFn: time.Sleep,
		nowFn:      time.Now,
		sendStates: make(map[string]*channelSendState),
		// roundBudget:   maxBlockingBudget,
		feedBudget:    maxBlockingBudget,
		messageBudget: maxBlockingBudget,
	}
}

func (h *RssHandler) now() time.Time {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now()
}

// sleep 统一走可注入的 sleepFn，nil 安全（直接字面量构造的 handler 如模板测试）
// func (h *RssHandler) sleep(d time.Duration) {
// 	if h.sleepFn != nil {
// 		h.sleepFn(d)
// 		return
// 	}
// 	time.Sleep(d)
// }

// 累计截止时间在入队前建立，等待不可超过剩余预算，也必须响应退出取消
func (h *RssHandler) wait(ctx context.Context, d time.Duration, deadline time.Time) bool {
	if ctx.Err() != nil || !h.now().Before(deadline) {
		return false
	}
	if d <= 0 {
		return true
	}
	if d >= deadline.Sub(h.now()) {
		return false
	}
	if h.waitFn != nil {
		return h.waitFn(ctx, d) == nil && ctx.Err() == nil && h.now().Before(deadline)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil && h.now().Before(deadline)
	}
}

func blockingBudget(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return maxBlockingBudget
}

func (h *RssHandler) UpdateConfig(cfg *config.Config) {
	h.Lock()
	defer h.Unlock()
	h.config = cfg
	log.Printf("RSS处理器配置已更新")
}

/*
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
*/

// 整轮只继承退出取消；单 feed 的阻塞由各自的 feedBudget 约束，慢 feed 只占名额不吞掉其他 feed 的机会
func (h *RssHandler) ProcessFeeds(ctx context.Context) error {
	// ctx, cancel := context.WithTimeout(parent, blockingBudget(h.roundBudget))
	// defer cancel()
	h.RLock()
	cfg := h.config
	h.RUnlock()
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 2)
	errChan := make(chan error, len(cfg.Feeds))
	for _, feed := range cfg.Feeds {
		if !acquireFeed(ctx, semaphore) {
			break
		}
		wg.Add(1)
		go func(feed config.FeedConfig) {
			defer wg.Done()
			defer func() { <-semaphore }()
			feedCtx, cancel := context.WithTimeout(ctx, blockingBudget(h.feedBudget))
			defer cancel()
			// if err := h.processFeed(ctx, feed); err != nil {
			if err := h.processFeed(feedCtx, feed); err != nil {
				errChan <- fmt.Errorf("feed %s: %w", feed.Name, err)
			}
		}(feed)
	}
	wg.Wait()
	close(errChan)
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}
	if ctx.Err() != nil {
		errs = append(errs, fmt.Errorf("processing feeds: %w", ctx.Err()))
	}
	return errors.Join(errs...)
}

// 先取得名额再创建 worker，避免无限排队 goroutine；取消同时释放排队和在途任务
func acquireFeed(ctx context.Context, semaphore chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case semaphore <- struct{}{}:
		if ctx.Err() == nil {
			return true
		}
		<-semaphore
	case <-ctx.Done():
	}
	return false
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

// func (h *RssHandler) processFeed(feedConfig config.FeedConfig) error {
func (h *RssHandler) processFeed(ctx context.Context, feedConfig config.FeedConfig) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("process feed: %w", err)
	}
	log.Printf("Processing feed: %s (%s)", feedConfig.Name, feedConfig.URL)

	// feed, err := h.parser.ParseURL(feedConfig.URL)
	feed, err := h.parser.ParseURLWithContext(feedConfig.URL, ctx)
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
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("filter feed items: %w", err)
		}
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
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("mark initial feed items: %w", err)
				}
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

	/*
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
	*/

	// 单个 feed 原本就只有一个发送名额，直接串行处理，避免每条消息排队 goroutine
	// 发送后的全 feed sleep 已在上方旧代码保留，节流仅由共享频道状态负责
	for _, item := range newItems {
		itemID := generateItemID(item)

		// [issue #12] 快照阶段：配置了 snapshot 的 feed 在渲染前建快照，
		// {telegraph} 降级值已在编排器内决定（快照失败=原文链接，无 link=空串）。
		// 失败语义与下方发送阶段一致：父预算耗尽延后下轮（不上报），退出取消上报。
		// 已知边角：快照先于 formatMessage——消息最终渲染为空被跳过时该页白建
		// （概率低：模板为空串才触发，可接受，见外部 CR）
		teleURL, err := h.snapshotForItem(ctx, feedConfig, item)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Feed %s budget exhausted during snapshot, deferring remaining items to next check", feedConfig.Name)
				return nil
			}
			return fmt.Errorf("snapshot feed items: %w", err)
		}

		message := h.formatMessage(item, feedConfig.Template, teleURL)
		if message == "" {
			continue // 既有行为；放在下载之前，空消息不白下载（issue #13）
		}
		// [issue #13] 图片推送：每 item 下载一次，频道间只读共享；
		// Message 每 (item, channel) 新建——photo 降级状态不得跨频道串味
		// photo := h.photoForItem(ctx, feedConfig, item) // 旧：返回 []byte，issue #16 起改载荷结构体
		payload := h.photoForItem(ctx, feedConfig, item)
		for _, channel := range feedConfig.Channels {
			// if err := ctx.Err(); err != nil {
			// 	return fmt.Errorf("send feed items: %w", err)
			// }
			// 发送阶段耗尽 feed 预算属于正常积压延后（剩余条目未标 seen，下轮补推），不上报为错误；
			// 退出取消仍返回错误
			if err := ctx.Err(); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.Printf("Feed %s budget exhausted, deferring remaining items to next check", feedConfig.Name)
					return nil
				}
				return fmt.Errorf("send feed items: %w", err)
			}
			if h.storage.IsItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID) {
				continue
			}
			// delivery := telegram.NewMessage(message) // 旧 sendWithRetry(ctx, channel, message, item.Title)
			// delivery := telegram.NewPhotoMessage(message, photo) // 旧：单图构造，issue #16 起按载荷分派
			delivery := newDelivery(message, payload)
			if msgID, ok := h.sendWithRetry(ctx, channel, delivery, item.Title); ok {
				if err := h.storage.MarkItemSeen(feedConfig.URL, feedConfig.Name, channel, itemID); err != nil {
					log.Printf("msg send success. MarkItemSeen ERROR!! channel %s: %v", channel, err)
				}
				// [issue #12] 快照页 author_url 回填为消息链接（公开频道，尽力而为）
				h.backfillSnapshotLink(ctx, feedConfig, item, channel, msgID)
			}
		}
	}

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
	// maxFloodWait = 2 * time.Minute
	maxFloodWait      = 10 * time.Second
	maxBlockingBudget = 2 * time.Minute
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

/*
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
*/

// sendWithRetry 带重试发送单条消息，返回成功消息的 message_id（失败为 0）与是否成功。
// msgID 供快照页 author_url 回填消息链接（issue #12），非快照路径可忽略
// func (h *RssHandler) sendWithRetry(parent context.Context, channel, message, itemTitle string) (int64, bool) {
// [issue #13] message string → delivery *telegram.Message：photo 模式需在
// processFeed 层构造（含图片字节），photo 降级状态随 Message 跨重试保留
func (h *RssHandler) sendWithRetry(parent context.Context, channel string, delivery *telegram.Message, itemTitle string) (int64, bool) {
	budget := blockingBudget(h.messageBudget)
	deadline := h.now().Add(budget)
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	state := h.channelState(channel)
	if !claimChannel(ctx, state) {
		log.Printf("Deferring item '%s' to channel %s: channel busy or cancelled", itemTitle, channel)
		return 0, false
	}
	defer func() { <-state.gate }()
	// delivery := telegram.NewMessage(message) // 旧：string 参数在此构造；已移至 processFeed（issue #13）
	for attempt, floodCount := 0, 0; ; {
		if !h.waitForSend(ctx, state, deadline, channel, itemTitle) {
			return 0, false
		}
		msgID, err := h.bot.Send(ctx, channel, delivery)
		if err == nil {
			state.nextSend = h.now().Add(sendInterval)
			log.Printf("Successfully sent message to channel %s: %s", channel, itemTitle)
			return msgID, true
		}
		if ctx.Err() != nil {
			// 之前静默返回，线上无法区分退出/超时与真实失败
			log.Printf("Deferring item '%s' to channel %s: %v", itemTitle, channel, ctx.Err())
			return 0, false
		}
		var rate *telegram.RateLimitError
		if errors.As(err, &rate) && rate != nil {
			h.setFloodCooldown(state, rate, channel, itemTitle)
			floodCount++
			if floodCount > maxFloodRetries {
				log.Printf("Failed to send item '%s' to channel %s after %d flood retries: %v", itemTitle, channel, maxFloodRetries, err)
				return 0, false
			}
			continue
		}
		attempt++
		if attempt >= maxSendRetries {
			log.Printf("Failed to send item '%s' to channel %s after %d retries: %v", itemTitle, channel, maxSendRetries, err)
			return 0, false
		}
		log.Printf("Error sending item '%s' to channel %s (retry %d/%d): %v", itemTitle, channel, attempt, maxSendRetries, err)
		if !h.wait(ctx, backoffWithJitter(attempt-1), deadline) {
			log.Printf("Deferring item '%s' to channel %s: backoff exceeds remaining budget or cancelled", itemTitle, channel)
			return 0, false
		}
	}
}

// backfillSnapshotLink 发送成功后将快照页 author_url 回填为消息链接（issue #12）。
// 尽力而为：仅公开频道（channelURL 判定）回填；私有频道（纯数字 ID）跳过；
// 未启用快照/编排器缺位/回填失败均不影响已推送消息，失败仅记日志。
// 多频道场景由编排层缓存 Backfilled 位去重：首个成功回填锁定（外部 CR，
// 原"最后覆盖"语义下前 N-1 次 editPage 白做且放大 Telegraph 调用量）
func (h *RssHandler) backfillSnapshotLink(ctx context.Context, feed config.FeedConfig, item *gofeed.Item, channel string, msgID int64) {
	if h.snapshot == nil || feed.Snapshot == "" || msgID <= 0 {
		return
	}
	if channelURL(channel) == "" {
		return // 私有频道无公开 t.me 消息链接
	}
	msgLink := "https://t.me/" + strings.TrimPrefix(channel, "@") + "/" + strconv.FormatInt(msgID, 10)
	if err := h.snapshot.BackfillAuthor(ctx, feed, item, msgLink); err != nil {
		log.Printf("snapshot backfill author_url failed, feed %s item %q: %v", feed.Name, item.Title, err)
	}
}

// 忙频道快速延后，不能持有 feed 名额在另一个消息的锁后无限排队
func claimChannel(ctx context.Context, state *channelSendState) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case state.gate <- struct{}{}:
		return true
	default:
		return false
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
		// state = &channelSendState{}
		state = &channelSendState{gate: make(chan struct{}, 1)}
		h.sendStates[key] = state
	}
	return state
}

/*
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
*/

func (h *RssHandler) waitForSend(ctx context.Context, state *channelSendState, deadline time.Time, channel, itemTitle string) bool {
	wait := state.nextSend.Sub(h.now())
	if wait > maxFloodWait {
		log.Printf("Deferring item '%s' to channel %s, cooldown remaining %v", itemTitle, channel, wait)
		return false
	}
	// return h.wait(ctx, wait, deadline)
	if !h.wait(ctx, wait, deadline) {
		log.Printf("Deferring item '%s' to channel %s: cooldown %v exceeds remaining budget or cancelled", itemTitle, channel, wait)
		return false
	}
	return true
}

// 先保存完整冷却再检查重试上限，避免放弃上一条消息后立即发送下一条
/*
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
*/

func (h *RssHandler) setFloodCooldown(state *channelSendState, err *telegram.RateLimitError, channel, itemTitle string) {
	wait := err.RetryAfter + time.Second
	state.nextSend = h.now().Add(wait)
	log.Printf("Rate limited sending item '%s' to channel %s, cooldown %v: %v", itemTitle, channel, wait, err)
}

// 指数退避+随机抖动
/*
func (h *RssHandler) ExponentialBackoffWithJitter(attempt int) {
	base := time.Second
	maxJitter := 500 * time.Millisecond                    // 最大抖动 500毫秒
	delay := base * time.Duration(1<<attempt)              // 指数退避。1<<attempt表示attemp的2次幂
	jitter := time.Duration(rand.Int63n(int64(maxJitter))) // 随机抖动
	h.sleep(delay + jitter)                                // 走可注入 sleep，测试免真睡
}
*/

func backoffWithJitter(attempt int) time.Duration {
	return time.Second*time.Duration(1<<attempt) + time.Duration(rand.Int63n(int64(500*time.Millisecond)))
}

// snapshotForItem 对配置了 snapshot 的 feed 建快照；未启用或编排器缺位返回空串
func (h *RssHandler) snapshotForItem(ctx context.Context, feedConfig config.FeedConfig, item *gofeed.Item) (string, error) {
	// Validate() 已保证非空 snapshot 只能是 telegraph，无需再比对枚举值
	if feedConfig.Snapshot == "" || h.snapshot == nil {
		return "", nil
	}
	return h.snapshot.Snapshot(ctx, feedConfig, item)
}

// photoPayload [issue #16] item 级图片载荷：单图/相册/文件互斥，零值走纯文本。
// CR 修订（2026-10-10）：原「>10 片截尾+附注」路径曾废弃——sendPhoto 服务端压缩至
// 长边 ~1280px，超长切片不可读；改整图 sendDocument（doc），零信息损失。
// [issue #16 用户反馈] truncated 字段恢复（两轮历史：issue #16 初版有→CR 废弃→
// 用户反馈恢复）：photo_overlimit=crop 时超限长图改发「顶部切片预览相册」，
// caption 需注明截断，truncated 标记该附注（普通完整相册恒 false）
type photoPayload struct {
	photo     []byte   // 限内单图（或切片单片，数学上不可达，防御保留）
	album     [][]byte // 可读切片 ≤10 张（每片长边 ≤1280）
	doc       []byte   // 超限整图文件（不可读切片 / w 过宽 / 像素超限）
	truncated bool     // crop 预览相册截断标记（issue #16 用户反馈恢复，见上）
}

// photoForItem [issue #13/16] 仅 media=photo 时按候选顺序下载图片：
// Fetch 只负责下载（issue #16 起校验移出），validatePhoto 在此分类——
// 尺寸类失败按 feed 级 photo_slices/photo_overlimit 三分派（issue #16 用户反馈）：
// 完整相册（needed ≤ slices）/ crop 顶部切片预览+截断附注 / doc 整图文件；
// 不可切图（过宽/超像素）恒 doc 与 photoSlices 无关。切片解码失败/其他校验失败
// 回退下一候选；全部失败返回零值（文本推送）。图片字节每 item 只下载一次，频道间只读共享
func (h *RssHandler) photoForItem(ctx context.Context, feedConfig config.FeedConfig, item *gofeed.Item) photoPayload {
	if feedConfig.Media != config.MediaPhoto || h.photoFetcher == nil {
		return photoPayload{}
	}
	// 候选为空时显式记日志：运维才能区分「无候选图」与「配置遗漏」（静默穿过则两条都查不到）
	candidates := photoCandidates(item)
	if len(candidates) == 0 {
		log.Printf("photo mode: no image candidates, feed %s item %q", feedConfig.Name, item.Title)
		return photoPayload{}
	}
	for _, u := range candidates {
		if ctx.Err() != nil {
			return photoPayload{} // 预算耗尽/退出：交由频道循环既有的 ctx 检查处理（首次进入循环的防线）
		}
		data, err := h.photoFetcher.Fetch(ctx, u)
		if err != nil {
			log.Printf("photo fetch failed, feed %s item %q url %s: %v", feedConfig.Name, item.Title, u, err)
			// 取消/预算耗尽：立即返回不再试下一候选，避免"看起来还要重试"的误导日志
			if ctx.Err() != nil {
				return photoPayload{}
			}
			continue
		}
		if err := validatePhoto(data); err != nil {
			// [issue #16 CR] 尺寸类失败：预检可读切片 → 相册；不可读 → 整图文件（胜出，
			// 不回退后续候选——文件已完整承载内容）；切片解码失败 → 回退下一候选。
			// [issue #16 用户反馈] 分派按 feed 级 photo_slices/photo_overlimit 重排：
			// needed ≤ photoSlices → 完整相册；needed > photoSlices 时 crop=顶部切片
			// 预览+截断附注、document（默认）=整图文件。
			// 旧实现（截尾相册，issue #16 初版）保留备查：
			// chunks, truncated, serr := slicePhoto(data)
			// if serr != nil { ... continue }
			// if truncated { log "photo sliced with tail dropped..." }
			// if len(chunks) == 1 { return photoPayload{photo: chunks[0]} }
			// if len(chunks) >= 2 { return photoPayload{album: chunks, truncated: truncated} }
			if isDimensionError(err) {
				// errors.As 风格取回 w/h：未来有人包装 photoDimensionError 时裸断言会 panic，
				// As 零成本防御；isDimensionError 已保证可达，此兜底防御性保留
				var dimErr *photoDimensionError
				if !errors.As(err, &dimErr) { // isDimensionError 已保证可达，防御性兜底
					continue
				}
				slices := feedConfig.EffectivePhotoSlices() // 未配置回退默认 10
				if !fitsReadableAlbum(dimErr.w, dimErr.h, slices) {
					// 切不满 photoSlices 张可读片。crop 只对「可切片」的图生效：
					// 过宽（chunkBounds 区间空）或像素超 40MP 的图物理切不出合规片，
					// 与 photoSlices 无关，维持整图文件（现状）。document（默认）= 整图文件
					// [issue #16 审查] 可切性判据收敛到 slicableBounds（与 fitsReadableAlbum 同源）
					if feedConfig.PhotoOverlimit == config.PhotoOverlimitCrop && slicableBounds(dimErr.w, dimErr.h) {
						chunks, truncated, serr := slicePhoto(data, slices)
						if serr != nil {
							// 均分不可分区（如 9500×600 连完整相册都切不出，截尾也救不了
							// 非截尾路径的均分检查）/数据损坏 → 回退下一候选
							log.Printf("photo slice failed, feed %s item %q url %s: %v", feedConfig.Name, item.Title, u, serr)
							continue
						}
						log.Printf("photo over-limit cropped preview, feed %s item %q url %s: %d/%d chunks", feedConfig.Name, item.Title, u, len(chunks), slices)
						return photoPayload{album: chunks, truncated: truncated}
					}
					log.Printf("photo over readable album limit, sending as document, feed %s item %q url %s (%dx%d)", feedConfig.Name, item.Title, u, dimErr.w, dimErr.h)
					return photoPayload{doc: data}
				}
				chunks, truncated, serr := slicePhoto(data, slices)
				if serr != nil {
					// 预检后仍失败（图片声明合规但数据损坏）→ 回退下一候选
					log.Printf("photo slice failed, feed %s item %q url %s: %v", feedConfig.Name, item.Title, u, serr)
					continue
				}
				if truncated { // 防御：预检已保证 ≤slices 片，不变量破坏时宁发文件不截尾
					log.Printf("photo slice unexpectedly truncated, sending as document, feed %s item %q url %s", feedConfig.Name, item.Title, u)
					return photoPayload{doc: data}
				}
				return photoPayload{album: chunks}
			}
			log.Printf("photo validate failed, feed %s item %q url %s: %v", feedConfig.Name, item.Title, u, err)
			continue
		}
		return photoPayload{photo: data}
	}
	return photoPayload{}
}

// 旧 photoForItem（issue #13 版，返回 []byte、不做校验分类）整段注释保留备查；
// issue #16 起被上方 photoPayload 版本替换：
// func (h *RssHandler) photoForItem(ctx context.Context, feedConfig config.FeedConfig, item *gofeed.Item) []byte {
// 	if feedConfig.Media != config.MediaPhoto || h.photoFetcher == nil {
// 		return nil
// 	}
// 	// 候选为空时显式记日志：运维才能区分「无候选图」与「配置遗漏」（静默穿过则两条都查不到）
// 	candidates := photoCandidates(item)
// 	if len(candidates) == 0 {
// 		log.Printf("photo mode: no image candidates, feed %s item %q", feedConfig.Name, item.Title)
// 		return nil
// 	}
// 	for _, u := range candidates {
// 		if ctx.Err() != nil {
// 			return nil // 预算耗尽/退出：交由频道循环既有的 ctx 检查处理（首次进入循环的防线）
// 		}
// 		data, err := h.photoFetcher.Fetch(ctx, u)
// 		if err != nil {
// 			log.Printf("photo fetch failed, feed %s item %q url %s: %v", feedConfig.Name, item.Title, u, err)
// 			// 取消/预算耗尽：立即返回不再试下一候选，避免"看起来还要重试"的误导日志
// 			if ctx.Err() != nil {
// 				return nil
// 			}
// 			continue
// 		}
// 		return data
// 	}
// 	return nil
// }

// newDelivery [issue #16] 按载荷形态构造频道消息：文件 > 相册 > 单图 > 纯文本；
// 每 (item,channel) 调用一次保证 Message 实例独立（降级状态不跨频道串味）。
// CR 修订（2026-10-10）曾废弃截断附注——截尾不再发生，超限图改发整图文件；
// [issue #16 用户反馈] 附注恢复：photo_overlimit=crop 的预览相册非完整内容，
// caption 必须注明截断（普通完整相册 truncated=false 不受影响）。
// [issue #16 审查] 附注经 AppendCaptionNote 在 1024 码元预算内拼接——旧裸拼接注释保留：
// text += "\n\n（长图过长，已截断）" 会被 telegram 层统一截断在长 caption 下吞掉附注
func newDelivery(text string, p photoPayload) *telegram.Message {
	if len(p.doc) > 0 {
		return telegram.NewDocumentMessage(text, p.doc)
	}
	if p.truncated { // crop 预览相册注明截断（doc 判定在前，截断只可能伴随相册）
		text = telegram.AppendCaptionNote(text, "（长图过长，已截断）")
	}
	if len(p.album) > 0 {
		return telegram.NewPhotoAlbumMessage(text, p.album)
	}
	return telegram.NewPhotoMessage(text, p.photo)
}

// 格式化消息
// [issue #12] telegraphURL 为快照编排结果：成功=页面 URL，降级=原文链接，无 link=空串
func (h *RssHandler) formatMessage(item *gofeed.Item, template string, telegraphURL string) string {
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
		case "telegraph":
			// URL 数据域，不做 Markdown 转义；空串时模板自行决定兜底（default 操作链）
			content = telegraphURL
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
