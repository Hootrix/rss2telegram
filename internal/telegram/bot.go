package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hootrix/rss2telegram/internal/tgmd"

	tele "gopkg.in/telebot.v3"
)

// 不再持有初始化用的 tele.Bot：其 client 绑定 NewBot 内已 cancel 的 ctx，后续误用必失败
type Bot struct {
	// bot    *tele.Bot
	token  string
	url    string
	client *http.Client
}

// Message 由单个消息任务独占，重试保留纯文本降级状态，不按频道缓存解析失败
// 原文本保持不变，发送纯文本时才反转义
type Message struct {
	text  string
	plain bool
}

func NewMessage(text string) *Message { return &Message{text: text} }

// Text 只读访问原始消息文本，供跨包（rss 集成测试等）断言内容
func (m *Message) Text() string { return m.text }

// RateLimitError 表示 Telegram 429 限速，携带服务端指示的等待时长。
// 独立成项目内类型：handler 层用 errors.As 识别即可，无需 import telebot
type RateLimitError struct {
	RetryAfter time.Duration
	err        error // 原始 telebot 错误，Unwrap 后保持链路可追溯
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram rate limited, retry after %v: %v", e.RetryAfter, e.err)
}

func (e *RateLimitError) Unwrap() error { return e.err }

// NewRateLimitError 导出构造函数，供测试与其他调用方构造（err 字段保持未导出）
//
//	func NewRateLimitError(retryAfter time.Duration, err error) *RateLimitError {
//		return &RateLimitError{RetryAfter: retryAfter, err: err}
//	}
//
// 秒数在唯一构造边界验证后再换算，非法值返回普通错误，handler 不再执行不同的兜底策略
func NewRateLimitError(seconds int64, err error) error {
	const maxSeconds = int64((math.MaxInt64 - time.Second) / time.Second)
	if seconds <= 0 || seconds > maxSeconds {
		return fmt.Errorf("telegram: invalid retry_after %d: %w", seconds, err)
	}
	return &RateLimitError{RetryAfter: time.Duration(seconds) * time.Second, err: err}
}

// channelRecipient 直接以配置字符串充当 tele.Recipient（接口只要求返回
// 一个可作为 chat_id 的字符串），使 sendMessage 的 chat_id 原生接受
// @username 或数字 ID，省掉每条消息一次 getChat 预查询
type channelRecipient string

func (r channelRecipient) Recipient() string { return string(r) }

// newChannelRecipient 归一化频道标识：@name 原样透传；裸名补 @；
// 数字/负数 ID（-100...）原样透传
func newChannelRecipient(channel string) channelRecipient {
	// 首字符不足以判断整数 ID，数字开头的非整数裸名也必须补 @；旧判断保留
	// if channel == "" || channel[0] == '@' || channel[0] == '-' || (channel[0] >= '0' && channel[0] <= '9') {
	// 	return channelRecipient(channel)
	// }
	if channel == "" || channel[0] == '@' {
		return channelRecipient(channel)
	}
	id := strings.TrimPrefix(channel, "-")
	if id != "" && strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return channelRecipient(channel)
	}
	return channelRecipient("@" + channel)
}

// ChannelKey 归一化频道标识，使裸名、@ 前缀和大小写别名共享限流状态
func ChannelKey(channel string) string {
	return strings.ToLower(string(newChannelRecipient(channel)))
}

// func NewBot(token string) (*Bot, error) {
func NewBot(parent context.Context, token string) (*Bot, error) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	client := &http.Client{Timeout: time.Minute}
	// 初始化 getMe 也必须能被退出信号取消，发送任务另用独立的请求 context
	initialClient := &http.Client{Transport: contextTransport{ctx: ctx, base: http.DefaultTransport}}
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
		// Client: client,
		Client: initialClient,
	}

	// 可测试性注入点：设置 TELEGRAM_API_URL 时指向自定义 API 地址（如本地集成测试的假服务器）
	// 为空时保持官方默认 https://api.telegram.org，生产行为不变
	if url := os.Getenv("TELEGRAM_API_URL"); url != "" {
		pref.URL = url
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		// return nil, err
		return nil, fmt.Errorf("create telegram bot: %w", &maskedError{err: err, token: token})
	}

	// return &Bot{bot: b}, nil
	// return &Bot{bot: b, client: client}, nil
	return &Bot{token: b.Token, url: b.URL, client: client}, nil
}

/*
func (b *Bot) Send(channel string, message string) error {
	if channel == "" {
		// [issue #6] getChat 预查询移除后空 channel 不再被拦截，会白送一次
		// chat_id 为空的 sendMessage（必 400）；提前返回保持旧的失败即退语义
		return errors.New("telegram: empty channel")
	}

	// [issue #6] 旧实现每条消息先调 ChatByUsername（getChat API）解析频道再发送，
	// 每条消息消耗 2 次 API 调用并增加 ~0.5s 延迟，是大批首刷时撞限速的放大器。
	// 已替换为直接以 @username 字符串作为 Recipient 发送（1 次调用）。
	// 旧逻辑保留备查：
	// chat, err := b.bot.ChatByUsername(channel)
	// if err != nil {
	// 	return err
	// }
	recipient := newChannelRecipient(channel)

	_, err := b.bot.Send(recipient, message, &tele.SendOptions{
		ParseMode: tele.ModeMarkdown,
	})
	if err != nil && isParseEntitiesError(err) {
		// issue #4 兜底：Markdown 实体解析失败属于内容层面的永久性错误，重试必然再失败，
		// 降级为纯文本（无 parse_mode）重发一次，成功后 handler 即可标记 seen 终止无限重试。
		// 不设「消息含 \」前置判断：解析失败可能来自未转义的裸字符（converter 输出的孤立 [、
		// 裸 URL 的 _），真正修复靠去掉 parse_mode，反转义只是避免纯文本下显示反斜杠
		log.Printf("markdown parse failed, falling back to plain text: %v", err)
		_, err = b.bot.Send(recipient, tgmd.Unescape(message))
	}
	if err != nil {
		// [issue #6] 服务端 429 且带 retry_after 时，telebot v3.1.3 extractOk 返回
		// FloodError 值类型（非指针、无 Unwrap），errors.As 目标必须用值类型
		// tele.FloodError；包装成项目内 RateLimitError 供上层按指示等待
		var flood tele.FloodError
		if errors.As(err, &flood) {
			// 秒转 Duration 前验证符号和范围，并为上层 +1s 缓冲预留空间，防止负等待或溢出
			const maxRetryAfterSeconds = (time.Duration(1<<63-1) - time.Second) / time.Second
			if flood.RetryAfter <= 0 || int64(flood.RetryAfter) > int64(maxRetryAfterSeconds) {
				return fmt.Errorf("telegram: invalid retry_after %d: %w", flood.RetryAfter, err)
			}
			return NewRateLimitError(time.Duration(flood.RetryAfter)*time.Second, err)
		}
	}
	// 注意重试放大：parse entities 失败时单次 Send 最多 2 次 API 调用，
	// 叠加 handler 外层双配额重试（普通 3 次 + flood 5 次，每次都可能触发降级重发）
	// 单条消息最坏约 16 次调用，属可接受代价（否则该消息永远发不出去）
	return err
}
*/

// Send 发送消息并返回成功消息的 message_id（issue #12：快照页 author_url 回填
// 消息链接用）；markdown 降级重发场景取最终成功那次的 message_id
func (b *Bot) Send(ctx context.Context, channel string, message *Message) (int64, error) {
	if channel == "" {
		return 0, errors.New("telegram: empty channel")
	}
	if message == nil || message.text == "" {
		return 0, errors.New("telegram: empty message")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("telegram send: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sender, err := b.contextualBot(ctx)
	if err != nil {
		return 0, fmt.Errorf("create telegram sender: %w", err)
	}
	text, options := message.text, &tele.SendOptions{}
	if message.plain {
		text = tgmd.Unescape(text)
	} else {
		options.ParseMode = tele.ModeMarkdown
	}
	sent, err := sender.Send(newChannelRecipient(channel), text, options)
	if err != nil && !message.plain && isParseEntitiesError(err) {
		message.plain = true
		log.Printf("markdown parse failed, falling back to plain text")
		sent, err = sender.Send(newChannelRecipient(channel), tgmd.Unescape(message.text))
	}
	if err != nil {
		return 0, b.sendError(ctx, err)
	}
	return int64(sent.ID), nil
}

// Raw 内部的 Background 不接收调用方取消，独立发送实例复用连接池并注入本次 context
// Offline 避免重复 getMe；请求超时由上层 context 统一控制，不使用被替换掉的 Client 定时器
func (b *Bot) contextualBot(ctx context.Context) (*tele.Bot, error) {
	client := *b.client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Timeout = 0
	client.Transport = contextTransport{ctx: ctx, base: transport}
	return tele.NewBot(tele.Settings{
		// Token: b.bot.Token, URL: b.bot.URL, Client: &client, Offline: true, Updates: 1,
		Token: b.token, URL: b.url, Client: &client, Offline: true, Updates: 1,
	})
}

type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

func (b *Bot) sendError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("telegram send: %w", ctx.Err())
	}
	// err = &maskedError{err: err, token: b.bot.Token}
	err = &maskedError{err: err, token: b.token}
	var flood tele.FloodError
	if errors.As(err, &flood) {
		return NewRateLimitError(int64(flood.RetryAfter), err)
	}
	return err
}

// 保持原始错误链，同时禁止网络错误中的请求 URL 把 token 写进日志
type maskedError struct {
	err   error
	token string
}

func (e *maskedError) Error() string { return strings.ReplaceAll(e.err.Error(), e.token, "[redacted]") }
func (e *maskedError) Unwrap() error { return e.err }

// isParseEntitiesError 判断是否为 Markdown 实体解析 400
//
// 刻意用 err.Error() 字符串匹配而非 *tele.Error 类型断言：telebot v3.1.3 extractOk
// 先以 description 查 sentinel 表，"can't parse entities" 不在表中 → 落到
// switch e.Code 的 default 分支，返回的是 fmt.Errorf("telegram: %s (%d)") 普通错误，
// 类型断言/errors.As 永远落空（会成为死代码）。统一匹配错误文本可同时覆盖
// sentinel *Error 与 fmt.Errorf 两种形态，且免疫 telebot 版本差异
func isParseEntitiesError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't parse entities") && strings.Contains(msg, "(400)")
}

// ---- caption 截断（issue #13）----

// maxCaptionUnits Telegram caption 上限，按 UTF-16 码元计（BMP 字符=1，emoji 等 ≥U+10000=2）
const maxCaptionUnits = 1024

// minNewlineFallbackUnits 换行回退阈值：最后一个换行前的码元数达此值才回退（取上限一半）
const minNewlineFallbackUnits = maxCaptionUnits / 2

// utf16Units 单个 rune 占用的 UTF-16 码元数
func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// truncateCaption 超限时按 rune 边界累加 UTF-16 码元截断（逐 rune 推进天然不劈开代理对），
// 并优先回退到最后一个换行（其前文码元数 ≥ minNewlineFallbackUnits 时）——legacy Markdown
// 实体基本不跨行，按行截可大幅降低"切断 [Media](url) 等实体 → captionPlain 降级"的概率。
// 截的是实体解析前原文，Telegram 校验解析后长度（语法字符被消耗只会更短），1024 保守安全
func truncateCaption(s string) string {
	total := 0
	for _, r := range s {
		total += utf16Units(r)
	}
	if total <= maxCaptionUnits {
		return s
	}

	// 修复：旧实现用 range 迭代，遇无效 UTF-8 字节产出宽度 1 的 U+FFFD，
	// 但 len(string(r)) 返回 3，码元累加与字节宽度不一致 → hardEnd 可越过 len(s)，
	// s[:hardEnd] panic（复现：1023 个 a + "\x80" + "x" → slice bounds [:1026]）
	// 旧实现保留备查：
	// units, hardEnd := 0, 0
	// for i, r := range s {
	// 	u := utf16Units(r)
	// 	if units+u > maxCaptionUnits {
	// 		break
	// 	}
	// 	units += u
	// 	hardEnd = i + len(string(r))
	// }
	// truncated := s[:hardEnd]
	// 改用 DecodeRuneInString 取真实字节宽度（无效字节 size=1），推进与截断点同源，不再越界；
	// U+FFFD 属 BMP 仍计 1 码元，两遍循环口径一致
	units, hardEnd, i := 0, 0, 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if units+utf16Units(r) > maxCaptionUnits {
			break
		}
		units += utf16Units(r)
		i += size
		hardEnd = i
	}
	truncated := s[:hardEnd]

	if idx := strings.LastIndexByte(truncated, '\n'); idx >= 0 {
		nlUnits := 0
		for _, r := range truncated[:idx] {
			nlUnits += utf16Units(r)
		}
		if nlUnits >= minNewlineFallbackUnits {
			truncated = truncated[:idx]
		}
	}
	return strings.TrimRight(truncated, " \t\n\r")
}
