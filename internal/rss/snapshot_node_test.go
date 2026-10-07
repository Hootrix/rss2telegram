package rss

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Hootrix/rss2telegram/internal/telegraph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodesJSON 序列化结果，便于 JSONEq 断言
func nodesJSON(t *testing.T, nodes []any) string {
	t.Helper()
	b, err := json.Marshal(nodes)
	require.NoError(t, err)
	return string(b)
}

// 表驱动：各 tag 白名单与降级路径（设计文档"Node 转换"测试策略）
func TestHTMLToNodes(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"p 白名单保留", `<p>hello</p>`, `[{"tag":"p","children":["hello"]}]`},
		{"h1/h2 降级 h3/h4", `<h1>大标题</h1><h2>二标题</h2>`, `[{"tag":"h3","children":["大标题"]},{"tag":"h4","children":["二标题"]}]`},
		{"纯文本 div 降级 p", `<div>一段文字</div>`, `[{"tag":"p","children":["一段文字"]}]`},
		{"含块级子元素的 div 展开不包裹", `<div><p>a</p><p>b</p></div>`,
			`[{"tag":"p","children":["a"]},{"tag":"p","children":["b"]}]`},
		{"span 展开为纯文本", `<p>a<span>b</span>c</p>`, `[{"tag":"p","children":["a","b","c"]}]`},
		{"a 保留 href", `<p><a href="https://e.com/x">链接</a></p>`,
			`[{"tag":"p","children":[{"tag":"a","attrs":{"href":"https://e.com/x"},"children":["链接"]}]}]`},
		{"a 无 href 展开为文本", `<p><a>裸锚</a></p>`, `[{"tag":"p","children":["裸锚"]}]`},
		// 与 img 同策略：非 http(s) href（相对/javascript:/锚点）进 Telegraph 成坏链，
		// 展开为文本保留信息（issue #12 CR）
		{"a 相对/javascript href 展开为文本", `<p><a href="/rel">相对</a><a href="javascript:void(0)">脚本</a><a href="#top">锚</a></p>`,
			`[{"tag":"p","children":["相对","脚本","锚"]}]`},
		{"b/strong/i/em 保留", `<p><b>1</b><strong>2</strong><i>3</i><em>4</em></p>`,
			`[{"tag":"p","children":[{"tag":"b","children":["1"]},{"tag":"strong","children":["2"]},{"tag":"i","children":["3"]},{"tag":"em","children":["4"]}]}]`},
		{"img 懒加载取 data-src", `<p><img src="data:image/gif;base64,R0" data-src="https://e.com/real.jpg"></p>`,
			`[{"tag":"p","children":[{"tag":"img","attrs":{"src":"https://e.com/real.jpg"}}]}]`},
		{"img 无任何 src 丢弃", `<p><img alt="无图"></p>`, `[{"tag":"p"}]`},
		// data-src 非 http(s) 时回退 src（占位 data-src + 合法 src 的懒加载变体不丢图）
		{"img data-src 无效回退 src", `<p><img data-src="/lazy/rel.jpg" src="https://e.com/real.jpg"></p>`,
			`[{"tag":"p","children":[{"tag":"img","attrs":{"src":"https://e.com/real.jpg"}}]}]`},
		// 非 http(s) scheme 的图建出 Telegraph 坏图，整体丢弃；不做相对 URL 解析（base 歧义）（issue #12）
		{"img data: src 丢弃", `<p>前文<img src="data:image/png;base64,iVBOR">后文</p>`,
			`[{"tag":"p","children":["前文","后文"]}]`},
		{"img data-src 也非 http 同样丢弃", `<p><img data-src="data:image/webp;base64,AAAA"></p>`, `[{"tag":"p"}]`},
		{"img 相对地址丢弃", `<p><img src="/img/local.jpg"></p>`, `[{"tag":"p"}]`},
		{"img protocol-relative 丢弃", `<p><img src="//cdn.e.com/a.png"></p>`, `[{"tag":"p"}]`},
		{"img http/https 保留", `<figure><img src="http://e.com/a.png"><img src="https://e.com/b.png"></figure>`,
			`[{"tag":"figure","children":[{"tag":"img","attrs":{"src":"http://e.com/a.png"}},{"tag":"img","attrs":{"src":"https://e.com/b.png"}}]}]`},
		{"img scheme 大小写不敏感保留", `<p><img src="HTTPS://E.COM/UPPER.PNG"></p>`,
			`[{"tag":"p","children":[{"tag":"img","attrs":{"src":"HTTPS://E.COM/UPPER.PNG"}}]}]`},
		{"script/style 整体丢弃", `<p>正文</p><script>evil()</script><style>.x{}</style>`,
			`[{"tag":"p","children":["正文"]}]`},
		{"br/hr void 节点", `<p>a<br>b</p><hr>`,
			`[{"tag":"p","children":["a",{"tag":"br"},"b"]},{"tag":"hr"}]`},
		{"ul/li 嵌套保留", `<ul><li>一</li><li>二</li></ul>`,
			`[{"tag":"ul","children":[{"tag":"li","children":["一"]},{"tag":"li","children":["二"]}]}]`},
		{"blockquote/pre/code 保留", `<blockquote><p>引</p></blockquote><pre><code>x=1</code></pre>`,
			`[{"tag":"blockquote","children":[{"tag":"p","children":["引"]}]},{"tag":"pre","children":[{"tag":"code","children":["x=1"]}]}]`},
		{"table 逐格降级 p", `<table><tr><td>格1</td><td>格2</td></tr></table>`,
			`[{"tag":"p","children":["格1"]},{"tag":"p","children":["格2"]}]`},
		{"未知标签 section 展开", `<section><p>a</p></section>`, `[{"tag":"p","children":["a"]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := htmlToNodes(tc.html)
			assert.JSONEq(t, tc.want, nodesJSON(t, got))
		})
	}
}

// 按 rune 截断：中文与 emoji（多字节）不得截出半个字符
func TestTruncateByRunes(t *testing.T) {
	assert.Equal(t, "中文字", truncateByRunes("中文字符串", 3))
	assert.Equal(t, "ab", truncateByRunes("abcdef", 2))
	assert.Equal(t, "🎉🎊", truncateByRunes("🎉🎊🎈", 2))
	assert.Equal(t, "不截断", truncateByRunes("不截断", 10))
	assert.Equal(t, "", truncateByRunes("", 5))
}

// content 超限时按 Node 边界截断，末尾追加指向原文的截断提示节点，
// 提示节点自身计入 64KB 总量（预留空间），最终序列化不超上限
func TestTruncateContentOverLimit(t *testing.T) {
	big := strings.Repeat("字", 30000) // ~90KB UTF-8，单节点即超限
	small := strings.Repeat("段", 1000)
	nodes := []any{
		telegraph.Node{Tag: "p", Children: []any{small}},
		telegraph.Node{Tag: "p", Children: []any{big}},
		telegraph.Node{Tag: "p", Children: []any{"永远不该出现的第三段"}},
	}
	out := truncateContent(nodes, "https://e.com/orig", maxContentBytes)

	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(b), maxContentBytes, "截断后总量必须 ≤ 64KB")

	// 末尾是截断提示节点，链接指向原文
	last, ok := out[len(out)-1].(telegraph.Node)
	require.True(t, ok)
	assert.Equal(t, "p", last.Tag)
	assert.Contains(t, nodesJSON(t, out), "https://e.com/orig")
	assert.Contains(t, nodesJSON(t, out), "查看原文")
	// 第三段被丢弃
	assert.NotContains(t, string(b), "永远不该出现的第三段")
}

func TestTruncateContentWithinLimit(t *testing.T) {
	nodes := []any{telegraph.Node{Tag: "p", Children: []any{"短内容"}}}
	out := truncateContent(nodes, "https://e.com/orig", maxContentBytes)
	assert.JSONEq(t, nodesJSON(t, nodes), nodesJSON(t, out), "未超限不动刀、不加提示节点")
}

// 边界回归：used 恰好顶满 budget 时，末节点与提示节点间的逗号也必须计入，
// 最终序列化不得超过 limit（原 -2 实现会输出 limit+1 字节被 Telegraph 拒收）
func TestTruncateContentExactBudgetBoundary(t *testing.T) {
	noticeJSON, err := json.Marshal(truncationNotice("https://e.com/o"))
	require.NoError(t, err)
	budget := maxContentBytes - len(noticeJSON) - 3 // 与实现同口径
	wrap := len(`{"tag":"p","children":[""]}`)      // p 骨架自身开销

	n1 := telegraph.Node{Tag: "p", Children: []any{"首段"}}
	n1b, err := json.Marshal(n1)
	require.NoError(t, err)
	// n2 恰好把 used 顶到 budget：used = len(n1)+1逗号+len(n2)
	n2Size := budget - len(n1b) - 1
	n2 := telegraph.Node{Tag: "p", Children: []any{strings.Repeat("a", n2Size-wrap)}}
	require.Equal(t, n2Size, mustMarshalLen(t, n2))

	nodes := []any{n1, n2, telegraph.Node{Tag: "p", Children: []any{strings.Repeat("b", 70000)}}}
	out := truncateContent(nodes, "https://e.com/o", maxContentBytes)
	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(b), maxContentBytes, "恰好顶满 budget 也不得超限")
	assert.Contains(t, string(b), "查看原文")
}

func mustMarshalLen(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return len(b)
}

// cutUTF8 不得留下残缺多字节首字节（lead byte 也是 RuneStart）：
// 截在 "a中" 的中间时，半截的 中 必须整体丢弃，否则 JSON 序列化产出 U+FFFD
func TestCutUTF8(t *testing.T) {
	assert.Equal(t, "a", string(cutUTF8([]byte("a中中中"), 2)))
	assert.Equal(t, "a中", string(cutUTF8([]byte("a中中中"), 4)))
	assert.Equal(t, "", string(cutUTF8([]byte("中"), 1)), "首字节即截断时整体为空")
	assert.Equal(t, "abc", string(cutUTF8([]byte("abc"), 10)), "未超限原样返回")
}

// 极端兜底：首个节点单独就超预算时，节点内按 rune 截文本，页面保留正文开头
func TestTruncateContentSingleGiantNode(t *testing.T) {
	giant := []any{telegraph.Node{Tag: "p", Children: []any{strings.Repeat("巨", 40000)}}}
	out := truncateContent(giant, "https://e.com/orig", maxContentBytes)
	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(b), maxContentBytes)
	require.Greater(t, len(out), 1, "除提示节点外必须保留部分正文")
	first, ok := out[0].(telegraph.Node)
	require.True(t, ok)
	assert.Equal(t, "p", first.Tag)
	assert.NotEmpty(t, first.Children)
}
