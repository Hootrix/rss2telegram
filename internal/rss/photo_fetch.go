package rss

// issue #13：media: photo 模式的图片下载与上传前校验

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net/http"
	"time"

	_ "image/jpeg" // 注册 jpeg 解码器（DecodeConfig 只读文件头）
	_ "image/png"  // 注册 png 解码器

	"github.com/Hootrix/rss2telegram/internal/extractor"
)

const (
	// 子超时覆盖单次下载全流程；父 ctx（feed 预算）取消同样生效
	photoFetchTimeout = 15 * time.Second

	// Bot API 上传照片上限 10MB；+1 读到即判定超限（与 extractor.maxBodyBytes 同套路）
	maxPhotoBytes = 10 << 20

	// 尺寸预检阈值：w+h ≤ 10000 且长宽比 ≤ 20 是实测 Telegram Bot API 硬限制，
	// 本地拦截省一次必败上传；min(w,h) < 50 为启发式（追踪像素/图标过滤），
	// 非 Telegram 限制
	maxPhotoSideSum = 10000
	maxPhotoRatio   = 20
	minPhotoSide    = 50
)

// PhotoFetcher 下载并校验图片，返回可直接上传的字节；
// 任何失败返回 error，由调用方降级文本（issue #13）
type PhotoFetcher interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
}

// httpPhotoFetcher 标准实现：GET → 状态码/大小/嗅探格式/尺寸预检
type httpPhotoFetcher struct {
	hc           *http.Client
	fetchTimeout time.Duration // 测试可缩短
}

func newHTTPPhotoFetcher() *httpPhotoFetcher {
	// 超时由每次请求的 context 控制（子超时 + 父预算），不在 Client 上设
	return &httpPhotoFetcher{hc: &http.Client{}, fetchTimeout: photoFetchTimeout}
}

// 编译期断言实现满足接口：签名漂移前移暴露，不必等 T8 handler 注入
var _ PhotoFetcher = (*httpPhotoFetcher)(nil)

// validatePhoto 上传前校验（纯函数）：
// 格式白名单以字节嗅探为准（http.DetectContentType），不信任响应头 Content-Type
// ——图床常回 application/octet-stream；尺寸预检只读文件头（image.DecodeConfig）
func validatePhoto(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty photo data")
	}
	switch ct := http.DetectContentType(data); ct {
	case "image/jpeg", "image/png":
	case "image/webp":
		// 标准库无 webp 解码器，跳过尺寸检查，超限交 Telegram 400 → 文本降级兜底
		// TODO: 如需 webp 尺寸预检再引入 golang.org/x/image/webp（issue #13）
		return nil
	default:
		return fmt.Errorf("unsupported photo content type %q", ct)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode photo header: %w", err) // 解析失败=损坏
	}
	w, h := cfg.Width, cfg.Height
	if w+h > maxPhotoSideSum {
		return fmt.Errorf("photo dimensions %dx%d exceed w+h limit %d", w, h, maxPhotoSideSum)
	}
	maxSide, minSide := w, h
	if minSide > maxSide {
		maxSide, minSide = minSide, maxSide
	}
	if minSide < minPhotoSide {
		return fmt.Errorf("photo dimensions %dx%d below minimum side %d", w, h, minPhotoSide)
	}
	// 整数比较避免浮点：max > 20*min
	if maxSide > maxPhotoRatio*minSide {
		return fmt.Errorf("photo aspect ratio %d:%d exceeds %d", w, h, maxPhotoRatio)
	}
	return nil
}

// Fetch 下载 rawURL 并校验；任何失败返回 error（不重试，调用方按候选回退）。
// URL 来自 feed 内容，会请求任意 http(s) 地址（可达内网）：feed 由运维配置、
// 视为可信，不做私网拦截
// TODO: 若开放给不可信 feed 需补私网 IP 拦截（spec 边界表，issue #13）
func (f *httpPhotoFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	subCtx, cancel := context.WithTimeout(ctx, f.fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(subCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("photo fetch %s: new request: %w", rawURL, err)
	}
	// 与 extractor 同 UA；不带 Referer（实测目标图床无 Referer 可取）
	req.Header.Set("User-Agent", extractor.UserAgent)

	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("photo fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("photo fetch %s: status %d", rawURL, resp.StatusCode)
	}
	// 声明即超限：不读 body 直接失败
	if resp.ContentLength > maxPhotoBytes {
		return nil, fmt.Errorf("photo fetch %s: content length %d exceeds %d", rawURL, resp.ContentLength, maxPhotoBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("photo fetch %s: read body: %w", rawURL, err)
	}
	if len(data) > maxPhotoBytes {
		return nil, fmt.Errorf("photo fetch %s: body exceeds %d bytes", rawURL, maxPhotoBytes)
	}
	if err := validatePhoto(data); err != nil {
		return nil, fmt.Errorf("photo fetch %s: %w", rawURL, err)
	}
	return data, nil
}
