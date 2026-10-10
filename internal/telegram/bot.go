package telegram

import (
	"bytes"
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
//
//	type Message struct {
//		text  string
//		plain bool
//	}
type Message struct {
	// text 恒为完整文本不截断：photo 降级后文本路径用全文
	text string
	// plain 文本路径的 markdown 降级（既有）
	plain bool
	// photo 非空 → sendPhoto；图片永久失败降级时置 nil。
	// 多频道共享同一底层数组，只读；需修改字节必须先复制（issue #13）
	photo []byte
	// photos 相册切片（issue #16）：非空 → sendMediaGroup；与 photo 互斥，
	// photo 永久失败降级同时置空两者。切片字节跨频道只读共享
	photos [][]byte
	// doc 超限长图整图文件（issue #16 CR：不压缩无尺寸限制）；与 photo/photos 互斥，
	// 永久失败降级同置空。字节跨频道只读共享
	doc []byte
	// captionPlain caption 的 markdown 降级，与 plain 分离：
	// caption 解析失败常由 1024 截断切断实体引起，是 caption 独有问题；
	// 共用 plain 会让图片失败降级后的全文无辜走 plain、丢失格式（issue #13）
	captionPlain bool
	// photoTransientFails 图片瞬态失败累计次数（跨同 Message 的多次 Send，
	// 即 sendWithRetry 的重试轮次；不跨频道不跨轮——Message 每 (item,channel)
	// 每轮新建）。达 maxPhotoTransientFails 后置空 photo 降级文本（外部 CR 保险丝）
	// album 路径共用（issue #16）
	photoTransientFails int
}

func NewMessage(text string) *Message { return &Message{text: text} }

// NewPhotoMessage photo 为空字节时等价 NewMessage（调用方免判空）
func NewPhotoMessage(text string, photo []byte) *Message {
	if len(photo) == 0 {
		return NewMessage(text)
	}
	return &Message{text: text, photo: photo}
}

// NewPhotoAlbumMessage 相册切片消息（issue #16）：photos 为 ≥2 张合规 JPEG；
// 空切片等价 NewMessage（调用方免判空）
func NewPhotoAlbumMessage(text string, photos [][]byte) *Message {
	if len(photos) == 0 {
		return NewMessage(text)
	}
	return &Message{text: text, photos: photos}
}

// NewDocumentMessage 文件消息（issue #16 CR）：doc 为原图字节（≤10MB 已在下载层保证）
func NewDocumentMessage(text string, doc []byte) *Message {
	if len(doc) == 0 {
		return NewMessage(text)
	}
	return &Message{text: text, doc: doc}
}

// Text 只读访问原始消息文本，供跨包（rss 集成测试等）断言内容
func (m *Message) Text() string { return m.text }

// HasPhoto 只读访问是否仍为图片消息，供跨包断言发送/降级路径（issue #13）
func (m *Message) HasPhoto() bool { return len(m.photo) > 0 }

// HasAlbum 是否仍为相册消息（issue #16，跨包断言发送/降级路径用）
func (m *Message) HasAlbum() bool { return len(m.photos) > 0 }

// HasDoc 是否仍为文件消息（issue #16 CR）
func (m *Message) HasDoc() bool { return len(m.doc) > 0 }

// AlbumCount 相册切片张数（issue #16，跨包断言用）
func (m *Message) AlbumCount() int { return len(m.photos) }

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

// [issue #13] 旧版 Send（纯文本路径）整体替换为下方带 photo 分支的新实现，
// 旧逻辑完整保留备查。其文档注释：发送消息并返回成功消息的 message_id（issue #12：
// 快照页 author_url 回填消息链接用）；markdown 降级重发场景取最终成功那次的 message_id
/*
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
*/

// Send 发送消息并返回成功消息的 message_id（issue #12：快照页 author_url 回填
// 消息链接用）；markdown 降级重发场景取最终成功那次的 message_id
// [issue #13] 新增 photo 分支：multipart 上传、caption parse 400 → captionPlain 重试、
// 400/413 → 置空 photo 降级全文文本、其余错误保留状态交外层重试
// 重试放大（photo 路径）：单次 Send 最坏 4 次 API（photo md → photo plain →
// text md → text plain，其中最多 2 次 ≤10MB 上传）；叠加外层 8 次 Send（普通 3 + flood 5）
// 最坏 32 次调用 / 16 次上传，仅瞬态错误持续叠加时出现（纯文本现状最坏 16 次，可接受）；
// album 路径同构（issue #16）；document 路径（issue #16 CR）与两者同构
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
	recipient := newChannelRecipient(channel)

	// [issue #16 CR] document 路径：超限长图整图发送（不压缩无尺寸限制），
	// 失败分类与 photo/album 同构
	if len(message.doc) > 0 {
		sent, err := sendDocumentOnce(sender, recipient, message)
		if err == nil {
			return int64(sent.ID), nil
		}
		if isParseEntitiesError(err) && !message.captionPlain {
			message.captionPlain = true
			log.Printf("document caption parse failed, retrying document with plain caption")
			sent, err = sendDocumentOnce(sender, recipient, message)
			if err == nil {
				return int64(sent.ID), nil
			}
		}
		if ferr := handlePhotoFailure(message, err, "document"); ferr != nil {
			return 0, b.sendError(ctx, ferr)
		}
	}

	// [issue #16] album 路径：与 photo 分支同构的失败分类——
	// 成功返回首条 message_id；parse 400 → captionPlain 重试一次；
	// 400/413 → 置空相册降级文本；其余交 sendWithRetry，达保险丝阈值放弃相册
	if len(message.photos) > 0 {
		sent, err := sendAlbumOnce(sender, recipient, message)
		if err == nil {
			return int64(sent.ID), nil
		}
		if isParseEntitiesError(err) && !message.captionPlain {
			message.captionPlain = true
			log.Printf("album caption parse failed, retrying album with plain caption")
			sent, err = sendAlbumOnce(sender, recipient, message)
			if err == nil {
				return int64(sent.ID), nil
			}
		}
		if ferr := handlePhotoFailure(message, err, "album"); ferr != nil {
			return 0, b.sendError(ctx, ferr)
		}
	}

	// [issue #13] photo 路径，失败分类按序判定：
	// 1. 成功 → 返回
	// 2. parse entities（且未 plain）→ captionPlain 重试一次 photo，结果回到 3/4 判定
	// 3. 400/413 → 图片永久失败（尺寸/格式/权限/实体过大），置空 photo 当场降级文本
	// 4. 其余（网络/5xx/429/ctx）→ 交 sendWithRetry 重试，photo 与 captionPlain 状态保留
	if len(message.photo) > 0 {
		sent, err := sendPhotoOnce(sender, recipient, message)
		if err == nil {
			return int64(sent.ID), nil
		}
		if isParseEntitiesError(err) && !message.captionPlain {
			message.captionPlain = true
			log.Printf("photo caption parse failed, retrying photo with plain caption")
			sent, err = sendPhotoOnce(sender, recipient, message)
			if err == nil {
				return int64(sent.ID), nil
			}
		}
		// [issue #16] 永久/瞬态/保险丝分类提取至 handlePhotoFailure（album/photo 共用）。
		// 旧内联实现保留备查：
		// if isPhotoPermanentError(err) {
		// 	log.Printf("photo send failed permanently, falling back to text message: %v", err)
		// 	message.photo = nil // 存活于 Message：外层文本重试不再撞图片 400
		// } else if message.photoTransientFails >= maxPhotoTransientFails-1 {
		// 	// 保险丝（外部 CR）：图片持续瞬态失败（大图+慢上行最常见）会耗尽
		// 	// sendWithRetry 的 2min 预算后 defer 不标 seen，该文章按从旧到新
		// 	// 每轮重占预算 → 该 feed 其余文章永久饿死。达阈值后放弃图片降级
		// 	// 全文，文本成功即标 seen 解卡——对 spec §5 顺序 4「保留重试」的
		// 	// 假设边界（目标 feed 图 ≤2MB）补强，语义与 captionPlain 有限降级同构
		// 	log.Printf("photo transiently failed %d times, giving up photo and falling back to text: %v", message.photoTransientFails+1, err)
		// 	message.photo = nil
		// } else {
		// 	message.photoTransientFails++
		// 	return 0, b.sendError(ctx, err)
		// }
		if ferr := handlePhotoFailure(message, err, "photo"); ferr != nil {
			return 0, b.sendError(ctx, ferr)
		}
	}

	text, options := message.text, &tele.SendOptions{}
	if message.plain {
		text = tgmd.Unescape(text)
	} else {
		options.ParseMode = tele.ModeMarkdown
	}
	sent, err := sender.Send(recipient, text, options)
	if err != nil && !message.plain && isParseEntitiesError(err) {
		message.plain = true
		log.Printf("markdown parse failed, falling back to plain text")
		sent, err = sender.Send(recipient, tgmd.Unescape(message.text))
	}
	if err != nil {
		return 0, b.sendError(ctx, err)
	}
	return int64(sent.ID), nil
}

// sendPhotoOnce 单次 photo 发送尝试。每次新建 reader 与 Photo 实例：
// reader 读过即耗尽；telebot Photo.Send 会回写 receiver（*p = *msg.Photo），
// 复用实例会污染重试。caption 构造：先按 captionPlain 反转义再截断
// （反转义会改变长度，先截后转义可能超限）
func sendPhotoOnce(sender *tele.Bot, recipient tele.Recipient, message *Message) (*tele.Message, error) {
	caption := message.text
	if message.captionPlain {
		caption = tgmd.Unescape(caption)
	}
	caption = truncateCaption(caption)
	options := &tele.SendOptions{}
	if !message.captionPlain {
		options.ParseMode = tele.ModeMarkdown
	}
	photo := &tele.Photo{File: tele.FromReader(bytes.NewReader(message.photo)), Caption: caption}
	return sender.Send(recipient, photo, options)
}

// handlePhotoFailure [issue #16] photo/album/document 共用的失败分类：
// 429 豁免计数原样返回（外部 CR）；永久失败（400/413）置空图片就地降级文本返回 nil；
// 瞬态失败达保险丝阈值同样置空降级；否则计数并返回 err 交外层重试。
// kind 为日志来源（"photo"/"album"/"document"）：共用后日志仍可辨图片形态，排障不混
func handlePhotoFailure(message *Message, err error, kind string) error {
	// [CR 修复] 429 不计入保险丝：服务端已指示冷却时长且外层有独立 flood 配额
	// （maxFloodRetries=5）与冷却等待，计数会把首刷撞限的相册/图片系统性降级文本；
	// 置空后外层重试永远失去图片。原样返回交 sendError → RateLimitError → 外层冷却
	var flood tele.FloodError
	if errors.As(err, &flood) {
		return err
	}
	if isPhotoPermanentError(err) {
		log.Printf("%s send failed permanently, falling back to text message: %v", kind, err)
		message.photo, message.photos, message.doc = nil, nil, nil // doc 同置空（issue #16 CR，三者互斥）
		return nil
	}
	if message.photoTransientFails >= maxPhotoTransientFails-1 {
		// 保险丝（外部 CR，#13 引入）：持续瞬态失败会耗尽 sendWithRetry 预算
		// 饿死整个 feed，达阈值放弃图片降级全文，文本成功即标 seen 解卡
		log.Printf("%s transiently failed %d times, giving up and falling back to text: %v", kind, message.photoTransientFails+1, err)
		message.photo, message.photos, message.doc = nil, nil, nil // doc 同置空（issue #16 CR，三者互斥）
		return nil
	}
	message.photoTransientFails++
	return err
}

// sendAlbumOnce 单次相册发送尝试。每次新建 reader 与 Album 实例（reader 读过即耗尽）；
// caption 与 parse_mode 挂首片 InputMedia（telebot SendAlbum 逐项序列化）
func sendAlbumOnce(sender *tele.Bot, recipient tele.Recipient, message *Message) (tele.Message, error) {
	caption := message.text
	if message.captionPlain {
		caption = tgmd.Unescape(caption)
	}
	caption = truncateCaption(caption)
	options := &tele.SendOptions{}
	if !message.captionPlain {
		options.ParseMode = tele.ModeMarkdown
	}
	album := make(tele.Album, 0, len(message.photos))
	for i, p := range message.photos {
		ph := &tele.Photo{File: tele.FromReader(bytes.NewReader(p))}
		if i == 0 {
			ph.Caption = caption
		}
		album = append(album, ph)
	}
	sent, err := sender.SendAlbum(recipient, album, options)
	if err != nil {
		return tele.Message{}, err
	}
	if len(sent) == 0 {
		// telebot v3.1.3 对 ok:true 且 result 短于文件数的响应会在库内 index panic，
		// 此分支仅在库修复后生效，留作库行为变更保险
		return tele.Message{}, errors.New("telegram: empty album response")
	}
	return sent[0], nil
}

// sendDocumentOnce [issue #16 CR] 单次文件发送尝试。文件名按字节嗅探生成
// （FromReader 不带名，无名文件在客户端展示为无法预览的裸文件）；
// caption 语义与 photo 相同
func sendDocumentOnce(sender *tele.Bot, recipient tele.Recipient, message *Message) (tele.Message, error) {
	caption := message.text
	if message.captionPlain {
		caption = tgmd.Unescape(caption)
	}
	caption = truncateCaption(caption)
	options := &tele.SendOptions{}
	if !message.captionPlain {
		options.ParseMode = tele.ModeMarkdown
	}
	name := "long-image.bin"
	switch ct := http.DetectContentType(message.doc); ct {
	case "image/jpeg":
		name = "long-image.jpg"
	case "image/png":
		name = "long-image.png"
	}
	doc := &tele.Document{File: tele.FromReader(bytes.NewReader(message.doc)), FileName: name, Caption: caption}
	sent, err := sender.Send(recipient, doc, options)
	if err != nil {
		return tele.Message{}, err
	}
	return *sent, nil
}

// isPhotoPermanentError 图片层面永久失败（尺寸/格式/权限/实体过大）：
// 400 与 413 按错误码字符串匹配——与 isParseEntitiesError 同惯例，
// telebot 对非 sentinel 错误返回 "telegram: ... (400)" 形态的 fmt 错误，
// 类型断言永远落空。注意必须在 parse entities 判定之后调用。
// 补充：v3.1.3 的 sentinel ErrTooLarge = NewError(400, "Request Entity Too Large")，
// 真实 413 会被 telebot 改写成 "(400)" 文本，由 (400) 分支兜住；(413) 分支属纵深防御
func isPhotoPermanentError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "(400)") || strings.Contains(msg, "(413)")
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
// [issue #16 审查] 已随 truncateToUnits 参数化为 maxUnits/2，常量保留备查（仅作语义说明，
// 代码中不再引用——truncateCaption(s) ≡ truncateToUnits(s, maxCaptionUnits) 行为不变）
const minNewlineFallbackUnits = maxCaptionUnits / 2

// maxPhotoTransientFails 图片瞬态失败保险丝阈值：同一条消息累计达此次数后
// 放弃图片降级文本（防持续瞬态失败耗尽重试预算卡死 feed，见 Send 内注释）
const maxPhotoTransientFails = 2

// utf16Units 单个 rune 占用的 UTF-16 码元数
func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// captionUnits 统计字符串的 UTF-16 码元数（逐 rune 累加，无效字节产出宽 1 的 U+FFFD
// 仍计 1，与 truncateToUnits 的 DecodeRuneInString 口径一致）。
// [issue #16 审查] 从 truncateCaption 开头的 total 累加循环提取复用
func captionUnits(s string) int {
	total := 0
	for _, r := range s {
		total += utf16Units(r)
	}
	return total
}

// truncateCaption 超限时按 rune 边界累加 UTF-16 码元截断（逐 rune 推进天然不劈开代理对），
// 并优先回退到最后一个换行（其前文码元数 ≥ 上限一半时）——legacy Markdown
// 实体基本不跨行，按行截可大幅降低"切断 [Media](url) 等实体 → captionPlain 降级"的概率。
// 截的是实体解析前原文，Telegram 校验解析后长度（语法字符被消耗只会更短），1024 保守安全。
// [issue #16 审查] 核心逻辑下沉到参数化的 truncateToUnits，此处变薄壳（行为不变，
// TestTruncateCaption 守护）
func truncateCaption(s string) string {
	return truncateToUnits(s, maxCaptionUnits)
}

// truncateToUnits [issue #16 审查] truncateCaption 的参数化核心：按 rune 边界累加
// UTF-16 码元截到 maxUnits 内，换行回退阈值随参数取 maxUnits/2（原 minNewlineFallbackUnits
// 即 maxCaptionUnits/2，薄壳路径行为不变）。供 AppendCaptionNote 在子预算内预截正文复用
func truncateToUnits(s string, maxUnits int) string {
	if total := captionUnits(s); total <= maxUnits {
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
		if units+utf16Units(r) > maxUnits {
			break
		}
		units += utf16Units(r)
		i += size
		hardEnd = i
	}
	truncated := s[:hardEnd]

	if idx := strings.LastIndexByte(truncated, '\n'); idx >= 0 {
		// 换行前码元 ≥ maxUnits/2 才回退（原常量 minNewlineFallbackUnits 的参数化形态）
		if nlUnits := captionUnits(truncated[:idx]); nlUnits >= maxUnits/2 {
			truncated = truncated[:idx]
		}
	}
	return strings.TrimRight(truncated, " \t\n\r")
}

// AppendCaptionNote [issue #16 用户反馈] 在 caption 1024 码元预算内拼接截断附注：
// 先把正文预截到「1024 − 附注宽度 − 分隔符」内再拼，保证附注本身不被
// telegram 层统一截断吞掉（长描述 feed 的 caption 常 ≥1013 码元）。
// 边界：note 自身 ≥1024 码元时 budget 归零（正文整段丢弃，真实调用传固定短附注不可达）
func AppendCaptionNote(text, note string) string {
	const sep = "\n\n"
	budget := maxCaptionUnits - captionUnits(note) - captionUnits(sep)
	if budget < 0 {
		budget = 0
	}
	return truncateToUnits(text, budget) + sep + note
}
