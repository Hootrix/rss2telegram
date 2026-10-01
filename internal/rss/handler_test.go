package rss

import (
	"testing"

	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
)

// issue #4：title 含裸 Markdown 实体字符导致 Telegram 400，
// formatMessage 需对 title 字段做 legacy Markdown 转义（数据域转义，
// 模板手写语法与操作链参数不受影响）
func TestFormatMessageTitleEscape(t *testing.T) {
	handler := &RssHandler{}

	// issue #4 线上真实失败标题
	item := &gofeed.Item{
		Title: "[特惠产品]露营多挂点长方形庇护所天幕 3*4.35米 FRESH & BLACK技术 XL ¥299.9 5折",
		Link:  "https://example.com/p/1",
	}

	t.Run("默认模板下 title 的 * 和 [ 被转义", func(t *testing.T) {
		result := handler.formatMessage(item, "{title}\n\n{link}")
		expected := `\[特惠产品]露营多挂点长方形庇护所天幕 3\*4.35米 FRESH & BLACK技术 XL ¥299.9 5折

https://example.com/p/1`
		assert.Equal(t, expected, result)
	})

	t.Run("模板手写的粗体语法保留，仅 title 值内特殊字符转义", func(t *testing.T) {
		// 模板的包裹 * 不受影响；title 内的 * 转义后恰好不与包裹符错配
		item2 := &gofeed.Item{Title: "[特惠产品]*CN Venum 拳击手套 ¥269.9"}
		result := handler.formatMessage(item2, "*{title}*")
		assert.Equal(t, `*\[特惠产品]\*CN Venum 拳击手套 ¥269.9*`, result)
	})

	t.Run("description 的裸特殊字符不转义（行为锁定，由降级纯文本兜底）", func(t *testing.T) {
		item3 := &gofeed.Item{
			Description: "尺寸 3*4.35米 _型号_",
		}
		result := handler.formatMessage(item3, "{description}")
		assert.Equal(t, "尺寸 3*4.35米 _型号_", result)
	})
}
