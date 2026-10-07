package rss

// Telegraph 快照的 HTML→Node 转换与硬限制预截断
// 官方限制：title 1-256 / author_name 0-128 / author_url 0-512 / content 64KB，
// 全部在本地预截断而非发布失败（设计文档"硬限制预截断"）

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Hootrix/rss2telegram/internal/telegraph"
	"golang.org/x/net/html"
)

const maxContentBytes = 64 << 10

// Telegraph 官方支持的标签集，直接透传
var allowedTags = map[string]bool{
	"a": true, "b": true, "blockquote": true, "br": true, "code": true,
	"em": true, "figcaption": true, "figure": true, "h3": true, "h4": true,
	"hr": true, "i": true, "img": true, "li": true, "ol": true, "p": true,
	"pre": true, "s": true, "strong": true, "u": true, "ul": true,
}

// 降级映射：Telegraph 无 h1/h2/h5/h6
var degradeTags = map[string]string{
	"h1": "h3", "h2": "h4", "h5": "h4", "h6": "h4",
}

// 整体丢弃：脚本/媒体/表单等无文本价值或无法降级展示的标签（子树同丢）
var dropTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "iframe": true,
	"video": true, "audio": true, "canvas": true, "svg": true, "form": true,
	"button": true, "input": true, "select": true, "textarea": true,
	"object": true, "embed": true, "template": true, "source": true,
}

// 行内展开：子节点提升到当前层级并继承行内上下文（span → 纯文本）
var inlineUnwrapTags = map[string]bool{
	"span": true, "font": true, "small": true, "abbr": true, "time": true,
	"mark": true, "sup": true, "sub": true, "label": true,
}

// 块级展开：子节点以块级上下文提升（文档骨架标签；table 骨架逐层展开）
var blockUnwrapTags = map[string]bool{
	"section": true, "article": true, "main": true, "header": true,
	"footer": true, "nav": true, "aside": true, "center": true,
	"details": true, "summary": true, "picture": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true,
}

// 判断 div 降级方式时视为"块级"的后代标签：出现则 div 展开，否则降级为 p
var blockishTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "ul": true, "ol": true, "li": true, "blockquote": true,
	"pre": true, "figure": true, "hr": true, "table": true, "div": true,
}

// 文本型容器：子节点按行内上下文转换（保留词间空白文本）
var textContainerTags = map[string]bool{
	"p": true, "a": true, "b": true, "i": true, "em": true, "strong": true,
	"code": true, "u": true, "s": true, "h3": true, "h4": true,
	"figcaption": true, "pre": true, "li": true,
}

// htmlToNodes 将 readability 输出的正文 HTML 转为 Telegraph content 节点数组
func htmlToNodes(contentHTML string) []any {
	doc, err := html.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return nil
	}
	var out []any
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				out = append(out, convertNode(c, false)...)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// convertNode 转换单个 DOM 节点；展开类标签可能产出 0..N 个节点
// inline 表示行内上下文：行内时保留纯空白文本节点（词间空格），块级时丢弃
func convertNode(n *html.Node, inline bool) []any {
	switch n.Type {
	case html.TextNode:
		if !inline && strings.TrimSpace(n.Data) == "" {
			return nil
		}
		return []any{n.Data}
	case html.ElementNode:
	default:
		return nil // 注释/文档声明等
	}

	tag := strings.ToLower(n.Data)
	if dropTags[tag] {
		return nil
	}

	// td/th 降级：Telegraph 无表格，逐格平铺为 p，格内取纯文本（保信息舍结构）
	if tag == "td" || tag == "th" {
		text := strings.TrimSpace(innerText(n))
		if text == "" {
			return nil
		}
		return []any{telegraph.Node{Tag: "p", Children: []any{text}}}
	}

	if inlineUnwrapTags[tag] {
		return convertChildren(n, inline)
	}
	if blockUnwrapTags[tag] {
		return convertChildren(n, false)
	}

	// div：自身无块级后代 → 降级 p；否则展开（避免 p 嵌套 p）
	if tag == "div" {
		if hasBlockishDescendant(n) {
			return convertChildren(n, false)
		}
		tag = "p"
	} else if mapped, ok := degradeTags[tag]; ok {
		tag = mapped
	} else if !allowedTags[tag] {
		return convertChildren(n, false) // 未知标签展开子节点
	}

	attrs := map[string]string{}
	switch tag {
	case "a":
		// 与 img 同策略过滤 scheme：非 http(s) 地址（相对/javascript:/锚点）
		// 进 Telegraph 即成坏链——feed 模式不经 readability 绝对化，展开为文本
		// 保留信息、宁缺毋坏（issue #12）。原实现仅判空：
		// if href := attrValue(n, "href"); href != "" {
		// 	attrs["href"] = href
		// } else {
		// 	return convertChildren(n, inline)
		// }
		if href := attrValue(n, "href"); isHTTPURL(href) {
			attrs["href"] = href
		} else {
			return convertChildren(n, inline) // 无/非法 href 的锚点等价于行内文本
		}
	case "img":
		// 懒加载常见模式：src 为占位图而真实地址在 data-src；
		// data-src 非 http(s) 时回退 src，不因 data-src 坏而丢整图。
		// 原实现仅在 data-src 为空时回退：
		// src := attrValue(n, "data-src")
		// if src == "" {
		// 	src = attrValue(n, "src")
		// }
		src := attrValue(n, "data-src")
		if !isHTTPURL(src) {
			src = attrValue(n, "src")
		}
		// 非 http(s) 的图（data:/相对///cdn）原样进 Telegraph 只会成坏图，整体丢弃；
		// 不做相对 URL 解析：base 存在三义（xml:base/channel link/item link），
		// SPA hash 路由下 item link 作 base 必错，宁缺毋坏（issue #12）
		if !isHTTPURL(src) {
			return nil
		}
		attrs["src"] = src
	}
	if len(attrs) == 0 {
		attrs = nil
	}

	childInline := textContainerTags[tag]
	return []any{telegraph.Node{
		Tag:      tag,
		Attrs:    attrs,
		Children: convertChildren(n, childInline),
	}}
}

func convertChildren(n *html.Node, inline bool) []any {
	var out []any
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, convertNode(c, inline)...)
	}
	return out
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

// isHTTPURL 判断地址是否为绝对 http(s) URL（scheme 大小写不敏感，空串自然为 false）
func isHTTPURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// hasBlockishDescendant 判断子树内是否含块级标签（用于 div 降级决策）
func hasBlockishDescendant(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			if blockishTags[strings.ToLower(c.Data)] || hasBlockishDescendant(c) {
				return true
			}
		}
	}
	return false
}

// innerText 收集子树全部文本（td 平铺用）
func innerText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// ---- 硬限制预截断 ----

// truncateByRunes 按 rune 截断，多字节字符（中文/emoji）不被切半
func truncateByRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// truncationNotice 截断提示节点，链接指向原文
func truncationNotice(origURL string) telegraph.Node {
	return telegraph.Node{
		Tag: "p",
		Children: []any{
			"（内容过长已截断，",
			telegraph.Node{Tag: "a", Attrs: map[string]string{"href": origURL}, Children: []any{"查看原文"}},
			"）",
		},
	}
}

// truncateContent 按 Node 边界以序列化字节数截断至 limit；
// 提示节点自身计入总量（预算中先预留），发生截断时追加到末尾。
// 极端兜底：首节点单独超预算时节点内按 rune 截文本，保证截断页仍保留正文开头
func truncateContent(nodes []any, origURL string, limit int) []any {
	if contentSize(nodes) <= limit {
		return nodes
	}
	notice := truncationNotice(origURL)
	noticeJSON, err := json.Marshal(notice)
	if err != nil {
		return []any{notice} // 输入不可序列化时整体放弃，仅保留提示（理论不可达）
	}

	// 最终数组 [n1..nk,notice] 的开销 = 1"[" + Σ节点 + k 逗号 + notice + 1"]"
	// [bugfix] 末节点与 notice 之间还有一个逗号，原 -2 少算它：used 恰好顶到
	// budget 时输出 limit+1 字节，被 Telegraph 拒收导致整页降级。原值保留备查：
	// budget := limit - len(noticeJSON) - 2
	budget := limit - len(noticeJSON) - 3

	var out []any
	used := 0
	for _, n := range nodes {
		nb, err := json.Marshal(n)
		if err != nil {
			continue
		}
		sep := 0
		if len(out) > 0 {
			sep = 1
		}
		if used+sep+len(nb) > budget {
			break
		}
		out = append(out, n)
		used += sep + len(nb)
	}

	// 单节点即超预算：取其纯文本截断保留，包 p 节点与其他段落形态一致
	if len(out) == 0 && len(nodes) > 0 {
		wrap := len(`{"tag":"p","children":[""]}`) // p 骨架自身开销
		if text := fitText(nodeText(nodes[0]), budget-1-wrap); text != "" {
			out = []any{telegraph.Node{Tag: "p", Children: []any{text}}}
		}
	}
	return append(out, notice)
}

func contentSize(nodes []any) int {
	b, err := json.Marshal(nodes)
	if err != nil {
		return 1 << 30 // 不可序列化视为超限，走截断分支统一兜底
	}
	return len(b)
}

// nodesPlainText 拼接根级节点数组的全部纯文本，节点间补空格避免相邻段落文字粘连
// （feed 来源的正文校验用；FFFD 分母与 rune 计数口径不受影响，issue #12）
func nodesPlainText(nodes []any) string {
	var b strings.Builder
	for i, n := range nodes {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(nodeText(n))
	}
	return b.String()
}

// nodeText 提取根级元素（Node 或 string）的全部纯文本
func nodeText(n any) string {
	switch v := n.(type) {
	case string:
		return v
	case telegraph.Node:
		var b strings.Builder
		for _, c := range v.Children {
			b.WriteString(nodeText(c))
		}
		return b.String()
	default:
		return ""
	}
}

// fitText 将文本裁剪到 JSON 编码后 ≤ max 字节：
// 先按 rune 边界粗裁，再处理转义膨胀（引号/控制字符会放大字节数）
func fitText(s string, max int) string {
	if max < 2 {
		return ""
	}
	b := []byte(s)
	if len(b) > max {
		b = cutUTF8(b, max)
	}
	for {
		enc, err := json.Marshal(string(b))
		if err != nil {
			return ""
		}
		if len(enc) <= max || len(b) == 0 {
			return string(b)
		}
		// 转义膨胀：砍一成再试
		b = cutUTF8(b, len(b)*9/10)
	}
}

// cutUTF8 按字节上限裁剪并回退到 rune 边界，避免切出半个字符
// [bugfix] utf8.RuneStart 对多字节首字节同样为 true，截断点落在
// lead+continuation 之间时原实现只删 continuation，留下残缺 lead byte，
// JSON 序列化产出 U+FFFD；改为删到恢复合法 UTF-8 前缀（输入本身合法，
// 最多回退一个 rune 的字节数）。原实现保留备查：
//
//	for len(out) > 0 && !utf8.RuneStart(out[len(out)-1]) {
//		out = out[:len(out)-1]
//	}
func cutUTF8(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	out := b[:max]
	for len(out) > 0 && !utf8.Valid(out) {
		out = out[:len(out)-1]
	}
	return out
}
