package rss

import (
	"testing"

	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/assert"
)

// issue #13：候选图优先级 enclosure(image/*，非 svg/gif) > content <img> > description <img>，
// data-src 懒加载对齐、仅绝对 http(s)、按 URL 去重、上限 3 个
func TestPhotoCandidates(t *testing.T) {
	t.Run("enclosure 优先", func(t *testing.T) {
		item := &gofeed.Item{
			Enclosures:  []*gofeed.Enclosure{{URL: "https://e.com/a.jpg", Type: "image/jpeg"}},
			Content:     `<img src="https://c.com/c.png">`,
			Description: `<img src="https://d.com/d.png">`,
		}
		// 依次追加三级来源（spec §2）：enclosure 失败时正文/摘要图兜底
		assert.Equal(t, []string{"https://e.com/a.jpg", "https://c.com/c.png", "https://d.com/d.png"}, photoCandidates(item))
	})

	t.Run("enclosure 命中后继续追加正文图直到上限", func(t *testing.T) {
		item := &gofeed.Item{
			Enclosures: []*gofeed.Enclosure{{URL: "https://e.com/a.jpg", Type: "image/jpeg"}},
			Content:    `<img src="https://c.com/1.png"><img src="https://c.com/2.png"><img src="https://c.com/3.png">`,
		}
		assert.Equal(t, []string{"https://e.com/a.jpg", "https://c.com/1.png", "https://c.com/2.png"}, photoCandidates(item))
	})

	t.Run("svg 与 gif enclosure 跳过，降级到正文图", func(t *testing.T) {
		item := &gofeed.Item{
			Enclosures: []*gofeed.Enclosure{
				{URL: "https://e.com/s.svg", Type: "image/svg+xml"},
				{URL: "https://e.com/g.gif", Type: "image/gif"},
			},
			Content: `<img src="https://c.com/c.png">`,
		}
		assert.Equal(t, []string{"https://c.com/c.png"}, photoCandidates(item))
	})

	t.Run("非 http(s) enclosure URL 跳过", func(t *testing.T) {
		item := &gofeed.Item{Enclosures: []*gofeed.Enclosure{
			{URL: "ftp://e.com/a.jpg", Type: "image/jpeg"},
			{URL: "/rel/a.png", Type: "image/png"},
		}, Content: `<img src="https://c.com/c.png">`}
		assert.Equal(t, []string{"https://c.com/c.png"}, photoCandidates(item))
	})

	t.Run("Type 空按 URL 后缀判定（忽略 query）", func(t *testing.T) {
		ok := &gofeed.Item{Enclosures: []*gofeed.Enclosure{{URL: "https://e.com/a.JPG?w=100"}}}
		assert.Equal(t, []string{"https://e.com/a.JPG?w=100"}, photoCandidates(ok))

		bad := &gofeed.Item{Enclosures: []*gofeed.Enclosure{{URL: "https://e.com/a.html?w=100"}}}
		assert.Empty(t, photoCandidates(bad))
	})

	t.Run("content 先于 description", func(t *testing.T) {
		item := &gofeed.Item{
			Content:     `<img src="https://c.com/c.png">`,
			Description: `<img src="https://d.com/d.png">`,
		}
		assert.Equal(t, []string{"https://c.com/c.png", "https://d.com/d.png"}, photoCandidates(item))
	})

	t.Run("data-src 优先，坏 data-src 回退 src", func(t *testing.T) {
		lazy := &gofeed.Item{Content: `<img src="data:image/gif;base64,R0==" data-src="https://c.com/real.png">`}
		assert.Equal(t, []string{"https://c.com/real.png"}, photoCandidates(lazy))

		broken := &gofeed.Item{Content: `<img src="https://c.com/fallback.png" data-src="/relative/x.png">`}
		assert.Equal(t, []string{"https://c.com/fallback.png"}, photoCandidates(broken))
	})

	t.Run("相对 URL 跳过后取后续绝对 URL img", func(t *testing.T) {
		item := &gofeed.Item{Content: `<img src="/rel/a.png"><img src="https://c.com/abs.png">`}
		assert.Equal(t, []string{"https://c.com/abs.png"}, photoCandidates(item))
	})

	t.Run("长文结构：banner 重复出现去重", func(t *testing.T) {
		// 长文 feed 常见形态：description 转义 HTML，同一 banner 重复
		item := &gofeed.Item{Description: `<p><img src="https://img.example.com/banner.png" width="690"/></p><p>正文</p><img src="https://img.example.com/banner.png"/>`}
		assert.Equal(t, []string{"https://img.example.com/banner.png"}, photoCandidates(item))
	})

	t.Run("超过 3 个只取前 3", func(t *testing.T) {
		item := &gofeed.Item{Content: `<img src="https://c.com/1.png"><img src="https://c.com/2.png"><img src="https://c.com/3.png"><img src="https://c.com/4.png">`}
		assert.Equal(t, []string{"https://c.com/1.png", "https://c.com/2.png", "https://c.com/3.png"}, photoCandidates(item))
	})

	t.Run("全无命中返回 nil", func(t *testing.T) {
		assert.Nil(t, photoCandidates(&gofeed.Item{Title: "t"}))
		assert.Nil(t, photoCandidates(&gofeed.Item{Content: `<img src="/rel.png">`}))
	})
}
