package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html/charset"
)

const (
	// 子超时：只覆盖抓取阶段，父 ctx（feed 预算）取消同样生效
	DefaultFetchTimeout = 15 * time.Second

	// 5MB+1：读到第 5MB+1 字节即明确判定失败。不依赖"解析会失败"——
	// HTML 解析器宽容，截断的半篇页面照样解析出伪快照（CR3-#2）
	maxBodyBytes = 5<<20 + 1

	// 转 UTF-8 后 U+FFFD 占比阈值：GBK 等未声明编码的字节在 HTML 解析阶段
	// 被替换为 U+FFFD（合法 UTF-8），utf8.Valid 校验永远放行，只能按占比识别
	maxReplacementRatio = 0.01

	// 正文纯文本下限：按 rune 数而非字节（中文 200 字节仅约 66 字）；
	// 纯 JS 站点（SPA 空壳）提取结果过短自然落入此路径
	minTextRunes = 200

	userAgent = "Mozilla/5.0 (compatible; rss2telegram/1.0; +https://github.com/Hootrix/rss2telegram)"
)

// Extractor 抓原文 + readability 提取正文，产出正文 HTML 供快照编排转换
type Extractor struct {
	hc *http.Client
	// 子超时，测试可缩短避免真睡；生产走 DefaultFetchTimeout
	fetchTimeout time.Duration
}

func New(hc *http.Client) *Extractor {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Extractor{hc: hc, fetchTimeout: DefaultFetchTimeout}
}

// FetchAndExtract 抓取 rawURL 并提取正文 HTML。
// 任何失败（网络/超时/Content-Type/超限/乱码/正文过短）均返回 error，由调用方降级
func (e *Extractor) FetchAndExtract(ctx context.Context, rawURL string) (string, error) {
	subCtx, cancel := context.WithTimeout(ctx, e.fetchTimeout)
	defer cancel()

	pageURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("extract %s: parse url: %w", rawURL, err)
	}

	req, err := http.NewRequestWithContext(subCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("extract %s: http status %d", rawURL, resp.StatusCode)
	}

	// 非 HTML（PDF/视频等）直接失败降级，不做提取；xhtml 属 HTML 家族一并放行
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "application/xhtml+xml") {
		return "", fmt.Errorf("extract %s: unsupported content type %q", rawURL, ct)
	}

	// 读到 5MB+1 字节即越界：防超大响应整体进内存
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("extract %s: read body: %w", rawURL, err)
	}
	if len(data) > maxBodyBytes-1 {
		return "", fmt.Errorf("extract %s: body too large (limit 5MB)", rawURL)
	}

	// 响应头 + 页面 meta 判定编码，统一转 UTF-8 后再解析（确定性，CR3-#1）
	transcoded, err := charset.NewReader(strings.NewReader(string(data)), ct)
	if err != nil {
		return "", fmt.Errorf("extract %s: determine charset: %w", rawURL, err)
	}

	article, err := readability.FromReader(transcoded, pageURL)
	if err != nil {
		return "", fmt.Errorf("extract %s: readability: %w", rawURL, err)
	}
	if article.Node == nil {
		return "", fmt.Errorf("extract %s: no article content", rawURL)
	}

	// [2026-10-08 契约变更] 正文质量校验（乱码/字数）移交 rss 层 nodes 校验：
	// 纯图帖豁免字数下限需在节点层数图，此处拿不到节点；抓取类失败语义不变。
	// 旧校验代码保留备查：
	// var text strings.Builder
	// if err := article.RenderText(&text); err != nil {
	//     return "", fmt.Errorf("extract %s: render text: %w", rawURL, err)
	// }
	// plain := text.String()
	// if err := ValidateText(plain); err != nil {
	//     return "", fmt.Errorf("extract %s: %w", rawURL, err)
	// }

	var htmlOut strings.Builder
	if err := article.RenderHTML(&htmlOut); err != nil {
		return "", fmt.Errorf("extract %s: render html: %w", rawURL, err)
	}
	return htmlOut.String(), nil
}

// ValidateMojibake 乱码校验单独导出（2026-10-08 纯图帖豁免）：
// U+FFFD 占比超 1% 判乱码，不含字数下限——rss 层 nodes 校验先拦乱码
// （含图不豁免），再按"有图豁免字数"放行，字数归 ValidateText
func ValidateMojibake(plain string) error {
	if replacementRatio(plain) > maxReplacementRatio {
		return fmt.Errorf("mojibake detected (U+FFFD ratio over 1%%)")
	}
	return nil
}

// ValidateText 对正文纯文本做质量校验：U+FFFD 占比超 1% 判乱码，纯文本不足 200 rune 判过短。
// [2026-10-08] 两条快照路径的校验已统一挪至 rss 层 validateNodes（纯图帖豁免字数下限，
// 见 docs/superpowers/specs/2026-10-08-issues12-telegraph-image-only-snapshot.md），
// 此函数保留组合语义供独立校验场景使用
func ValidateText(plain string) error {
	if err := ValidateMojibake(plain); err != nil {
		return err
	}
	if n := runeCount(plain); n < minTextRunes {
		return fmt.Errorf("text too short (%d runes)", n)
	}
	return nil
}

// replacementRatio 统计 U+FFFD 占总 rune 数比例
func replacementRatio(s string) float64 {
	runes, bad := 0, 0
	for _, r := range s {
		runes++
		if r == '�' {
			bad++
		}
	}
	if runes == 0 {
		return 0
	}
	return float64(bad) / float64(runes)
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
