// Package tgmd 提供 Telegram legacy Markdown（tele.ModeMarkdown）的实体字符转义
//
// Telegram 官方 legacy Markdown 规范要求：消息文本中的字面
// '_' '`' '*' '[' 必须以 '\' 前缀转义，否则孤立的起始符会导致
// 400 "can't parse entities"（issue #4）
//
// 转义集刻意只有这 4 个字符：
//   - ']' 单独出现无害，无需转义
//   - '|' 等字符转义（如 \|）在 legacy Markdown 中是非法转义，
//     反斜杠会被原样显示（html-to-markdown EscapeMode 当初被禁用即为此）
//
// rss（formatMessage 转义）与 telegram（降级纯文本反转义）两个包共用此处定义，
// 避免字符集两份实现漂移
package tgmd

import "strings"

// Escape 在 legacy Markdown 特殊字符前插入反斜杠
func Escape(s string) string {
	return escaper.Replace(s)
}

// Unescape 去除 Escape 添加的反斜杠；非本包转义集的反斜杠（如 \|）原样保留
func Unescape(s string) string {
	return unescaper.Replace(s)
}

var escaper = strings.NewReplacer(
	"_", `\_`,
	"`", "\\`",
	"*", `\*`,
	"[", `\[`,
)

var unescaper = strings.NewReplacer(
	`\_`, "_",
	"\\`", "`",
	`\*`, "*",
	`\[`, "[",
)
