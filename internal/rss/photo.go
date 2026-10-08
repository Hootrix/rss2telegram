package rss

// issue #13：media: photo 模式的候选图提取，纯函数、无状态

import (
	"strings"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

// maxPhotoCandidates 候选图上限：首选图下载/校验失败时的有限回退，
// 限制单 item 下载次数（3 × 15s 最坏耗时计入 feed 预算）
const maxPhotoCandidates = 3

// photoCandidates 按优先级依次追加候选图片 URL（最多 maxPhotoCandidates 个，spec §2）：
// enclosure（Type 为 image/* 且非 svg/gif；Type 空按 URL 后缀）> content <img> > description <img>。
// enclosure 命中后仍继续追加正文/摘要候选——候选图的全部意义是首选图
// 下载/校验失败时的有限回退（防盗链是 enclosure 图最常见失败），
// 无正文图兜底则多候选机制对 enclosure feed 完全失效。
// 全部无效时返回 nil，调用方走原文本路径
func photoCandidates(item *gofeed.Item) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(u string) {
		if isHTTPURL(u) && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	full := func() bool { return len(out) >= maxPhotoCandidates }

	for _, e := range item.Enclosures {
		if isImageEnclosure(e) {
			add(e.URL)
		}
		if full() {
			return out
		}
	}
	// 曾经的替代实现（裁决弃用）：有有效 enclosure 即返回、不追加正文候选，
	// 会使 enclosure 单点失败直接降级文本，与 spec §2 "依次追加" 相悖，保留备查：
	//
	//	if len(out) > 0 {
	//		return out
	//	}
	for _, u := range imgURLs(item.Content) {
		add(u)
		if full() {
			return out
		}
	}
	for _, u := range imgURLs(item.Description) {
		add(u)
		if full() {
			return out
		}
	}
	return out
}

// isImageEnclosure enclosure 是否可作为 sendPhoto 图源：
// Type 非空——image/* 且排除 svg/gif（Telegram sendPhoto 不收）；
// Type 空——按 URL 路径后缀判定（忽略 query，大小写不敏感）。
// Type 撒谎的漏网情形由发送失败降级文本兜底
func isImageEnclosure(e *gofeed.Enclosure) bool {
	// TrimSpace 对齐同包 attrValue 惯例：" image/jpeg" 带空格时 HasPrefix 会失败被误拒
	if t := strings.ToLower(strings.TrimSpace(e.Type)); t != "" {
		return strings.HasPrefix(t, "image/") && t != "image/svg+xml" && t != "image/gif"
	}
	path := strings.ToLower(e.URL)
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch {
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"),
		strings.HasSuffix(path, ".png"), strings.HasSuffix(path, ".webp"):
		return true
	}
	return false
}

// imgURLs 按文档顺序返回 HTML 中全部 <img> 的地址：
// data-src 优先（懒加载占位惯例），非 http(s) 回退 src，最终非 http(s) 跳过——
// 与 snapshot_node.go convertNode 的 img 惯例一致；item.Link 是文章页不是
// 资源基址，相对 URL 无法可靠解析，宁缺毋坏
func imgURLs(contentHTML string) []string {
	if strings.TrimSpace(contentHTML) == "" {
		return nil
	}
	root, err := html.Parse(strings.NewReader(contentHTML))
	if err != nil { // html.Parse 对任意输入宽容，实际不可达，防御性返回
		return nil
	}
	var urls []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "img") {
			src := attrValue(n, "data-src")
			if !isHTTPURL(src) {
				src = attrValue(n, "src")
			}
			if isHTTPURL(src) {
				urls = append(urls, src)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return urls
}
