package telegraph

// Telegraph 页面内容节点（https://telegra.ph/api 的 Node 结构）
// Children 为 string 与 Node 的混合数组，直接用 []any 交给 encoding/json，
// 序列化天然产出 ["text", {"tag":...}] 形态，无需自定义 MarshalJSON
type Node struct {
	Tag      string            `json:"tag"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Children []any             `json:"children,omitempty"`
}
