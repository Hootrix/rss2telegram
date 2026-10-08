package telegraph

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Telegraph Node 的 children 是 string 与 Node 对象的混合数组，
// 序列化必须产出 ["text", {"tag":...}] 而非统一包裹对象（官方 content 格式）
func TestNodeMarshal(t *testing.T) {
	t.Run("混合 children：字符串与嵌套 Node", func(t *testing.T) {
		n := Node{
			Tag: "p",
			Children: []any{
				"Hello ",
				Node{Tag: "b", Children: []any{"world"}},
				Node{
					Tag:      "a",
					Attrs:    map[string]string{"href": "https://example.com"},
					Children: []any{"link"},
				},
			},
		}
		b, err := json.Marshal(n)
		require.NoError(t, err)
		assert.JSONEq(t, `{"tag":"p","children":["Hello ",{"tag":"b","children":["world"]},{"tag":"a","attrs":{"href":"https://example.com"},"children":["link"]}]}`, string(b))
	})

	t.Run("空 attrs 与空 children 省略（void 标签）", func(t *testing.T) {
		n := Node{Tag: "br"}
		b, err := json.Marshal(n)
		require.NoError(t, err)
		assert.JSONEq(t, `{"tag":"br"}`, string(b))
	})

	t.Run("img 节点仅含 attrs", func(t *testing.T) {
		n := Node{Tag: "img", Attrs: map[string]string{"src": "https://example.com/a.png"}}
		b, err := json.Marshal(n)
		require.NoError(t, err)
		assert.JSONEq(t, `{"tag":"img","attrs":{"src":"https://example.com/a.png"}}`, string(b))
	})

	t.Run("反序列化往返（Node 从 JSON 恢复）", func(t *testing.T) {
		src := `{"tag":"p","children":["a",{"tag":"b","children":["c"]}]}`
		var n Node
		require.NoError(t, json.Unmarshal([]byte(src), &n))
		assert.Equal(t, "p", n.Tag)
		require.Len(t, n.Children, 2)
		assert.Equal(t, "a", n.Children[0])
		// json.Unmarshal 数字/对象统一进 any，嵌套 Node 恢复为 map[string]any
		inner, ok := n.Children[1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "b", inner["tag"])
	})
}
