package rss

import (
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
)

func TestTemplateProcessor_ProcessField(t *testing.T) {
	processor := NewTemplateProcessor()

	tests := []struct {
		name     string
		field    string
		content  string
		expected string
	}{
		{
			name:     "Simple extract",
			field:    "description|extract:(\\d+)号",
			content:  "这里是123号楼",
			expected: "123",
		},
		{
			name:     "Extract with escaped pipe",
			field:    "description|extract:(\\d+号楼\\|\\d+单元)",
			content:  "这里是123号楼|2单元",
			expected: "123号楼|2单元",
		},
		{
			name:     "Extract with escaped pipe",
			field:    `description|extract:(\d+号楼\|\d+单元)`,
			content:  "这里是123号楼|2单元",
			expected: "123号楼|2单元",
		},
		{
			name:     "Simple replace",
			field:    "description|replace:楼:Building",
			content:  "123号楼",
			expected: "123号Building",
		},
		{
			name:     "Replace with regex groups",
			field:    "description|replace:(\\d{4})-(\\d{2})-(\\d{2}):${1}年${2}月${3}日",
			content:  "2024-12-10",
			expected: "2024年12月10日",
		},
		{
			name:     "Multiple operations",
			field:    "description|extract:价格：(\\d+)元|replace:\\d{4}:****",
			content:  "价格：1234元",
			expected: "****",
		},
		{
			name:     "Default value when no match",
			field:    "description|extract:价格：(\\d+)元|default:未知1",
			content:  "没有价格信息",
			expected: "未知1",
		},
		{
			name:     "Complex regex with escaped pipe",
			field:    "description|extract:(免费\\|\\d+元)",
			content:  "价格：免费|500元",
			expected: "免费|500元",
		},
		{
			name:     "Unescape params",
			field:    "description|extract:(\\d+号楼\\|\\d+单元)",
			content:  `这里是123号楼|2单元`,
			expected: "123号楼|2单元",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := processor.ProcessField(tt.field, tt.content)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatMessage(t *testing.T) {
	handler := &RssHandler{}
	now := time.Now()

	item := &gofeed.Item{
		Title:           "测试标题",
		Description:     `位于123号楼|2单元，价格：1234元，发布于2024-12-10`,
		Link:            "https://example.com",
		PublishedParsed: &now,
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			name: "Complex template with multiple operations",
			template: `📰 *{ title }*
		位置：{description|extract:(\d+号楼\|\d+单元)}
		价格：{ description|extract:价格：(\d+)元|replace:\d{4}:**** }
		时间：{ description|extract:发布于(\d{4}-\d{2}-\d{2})|replace:(\d{4})-(\d{2})-(\d{2}):${1}年${2}月${3}日 }
		🔗 [阅读原文]({ link })`,
			expected: `📰 *测试标题*
		位置：123号楼|2单元
		价格：****
		时间：2024年12月10日
		🔗 [阅读原文](https://example.com)`,
		},
		{
			name: "Template with default values",
			template: `📰 *{title}*
		类型：{description|extract:类型：(.*?)，|default:未知}
		状态：{description|extract:状态：(.*?)$|default:待定}
		🔗 [阅读原文]({link})`,
			expected: `📰 *测试标题*
		类型：未知
		状态：待定
		🔗 [阅读原文](https://example.com)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := handler.formatMessage(item, tt.template, "")
			assert.Equal(t, tt.expected, result)
		})
	}
}
