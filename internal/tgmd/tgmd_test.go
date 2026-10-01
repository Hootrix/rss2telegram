package tgmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscape(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			// issue #4 线上真实失败标题：单个 * 被 legacy Markdown 解析为斜体起始符 → 400
			name:     "issue #4 case 1: 天幕标题含 3*4.35 与裸 [",
			input:    "[特惠产品]露营多挂点长方形庇护所天幕 3*4.35米 FRESH & BLACK技术 XL ¥299.9 5折",
			expected: `\[特惠产品]露营多挂点长方形庇护所天幕 3\*4.35米 FRESH & BLACK技术 XL ¥299.9 5折`,
		},
		{
			name:     "issue #4 case 2: 标题以 * 开头",
			input:    "[特惠产品]*CN Venum Contender 1.5XT 拳击手套 ¥269.9 8折",
			expected: `\[特惠产品]\*CN Venum Contender 1.5XT 拳击手套 ¥269.9 8折`,
		},
		{
			name:     "全部四个 legacy 特殊字符",
			input:    "a_b`c*d[e",
			expected: `a\_b\` + "`c\\*d\\[e",
		},
		{
			name:     "无特殊字符原样返回",
			input:    "普通标题 2024-12-10 ¥299.9",
			expected: "普通标题 2024-12-10 ¥299.9",
		},
		{
			name:     "] 和 | 不转义（| 转义曾在 Telegram 显示为 \\| 回归）",
			input:    "a]b|c",
			expected: "a]b|c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Escape(tt.input))
		})
	}
}

func TestUnescape(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "去除四个特殊字符的转义符",
			input:    `a\_b\` + "`c\\*d\\[e",
			expected: "a_b`c*d[e",
		},
		{
			name:     "非目标转义 \\| 保留不动",
			input:    `a\|b`,
			expected: `a\|b`,
		},
		{
			name:     "单个反斜杠无配对不动",
			input:    `a\b`,
			expected: `a\b`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Unescape(tt.input))
		})
	}
}

func TestEscapeUnescapeRoundTrip(t *testing.T) {
	// Escape 后 Unescape 应还原原文（降级纯文本路径依赖此性质）
	originals := []string{
		"[特惠产品]露营多挂点长方形庇护所天幕 3*4.35米 FRESH & BLACK技术 XL ¥299.9 5折",
		"[特惠产品]*CN Venum Contender 1.5XT 拳击手套 ¥269.9 8折",
		"a_b`c*d[e",
		"plain text 123",
	}
	for _, orig := range originals {
		assert.Equal(t, orig, Unescape(Escape(orig)))
	}
}
