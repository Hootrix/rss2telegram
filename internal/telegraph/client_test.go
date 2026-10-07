package telegraph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTelegraph 假 Telegraph API：记录 createAccount 次数（每次发出递增 token），
// 记录最近一次 createPage/editPage 请求体；可切换为返回 API 错误
type fakeTelegraph struct {
	accounts    atomic.Int64
	pages       atomic.Int64
	edits       atomic.Int64
	apiErr      atomic.Value // string，非空时 createPage/editPage 返回该 API 错误
	mu          sync.Mutex
	lastPage    map[string]any
	lastEdit    map[string]any
	lastAccount map[string]any
}

func newFakeTelegraph(t *testing.T) (*fakeTelegraph, *httptest.Server) {
	t.Helper()
	ft := &fakeTelegraph{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /createAccount", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		ft.mu.Lock()
		ft.lastAccount = req
		ft.mu.Unlock()
		n := ft.accounts.Add(1)
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"short_name":   "rss2telegram",
				"access_token": fmt.Sprintf("TOKEN-%d", n),
			},
		})
	})

	mux.HandleFunc("POST /createPage", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		ft.mu.Lock()
		ft.lastPage = req
		ft.mu.Unlock()
		if msg, ok := ft.apiErr.Load().(string); ok && msg != "" {
			writeJSON(w, map[string]any{"ok": false, "error": msg})
			return
		}
		ft.pages.Add(1)
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"path": "/Test-10-01",
				"url":  "https://telegra.ph/Test-10-01",
			},
		})
	})

	mux.HandleFunc("POST /editPage", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		ft.mu.Lock()
		ft.lastEdit = req
		ft.mu.Unlock()
		if msg, ok := ft.apiErr.Load().(string); ok && msg != "" {
			writeJSON(w, map[string]any{"ok": false, "error": msg})
			return
		}
		ft.edits.Add(1)
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"ok": true}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ft, srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestClient(t *testing.T, dataDir, baseURL string) *Client {
	return NewClient(dataDir, baseURL, http.DefaultClient)
}

func TestCreatePageLazyAccount(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	dir := t.TempDir()
	c := newTestClient(t, dir, srv.URL)

	url, err := c.CreatePage(context.Background(), Page{
		Title:      "Test",
		AuthorName: "special-feed",
		Content:    []any{Node{Tag: "p", Children: []any{"hello"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "https://telegra.ph/Test-10-01", url)

	// 首次使用自动匿名建号一次，token 随 createPage 请求发出
	assert.Equal(t, int64(1), ft.accounts.Load())
	ft.mu.Lock()
	defer ft.mu.Unlock()
	require.NotNil(t, ft.lastPage)
	assert.Equal(t, "TOKEN-1", ft.lastPage["access_token"])
	assert.Equal(t, "Test", ft.lastPage["title"])
	assert.Equal(t, "special-feed", ft.lastPage["author_name"])
	assert.Equal(t, []any{map[string]any{"tag": "p", "children": []any{"hello"}}}, ft.lastPage["content"])

	// token 文件落盘且 0600
	info, err := os.Stat(filepath.Join(dir, tokenFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestTokenPersistenceReuse(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	dir := t.TempDir()

	c1 := newTestClient(t, dir, srv.URL)
	_, err := c1.CreatePage(context.Background(), Page{Title: "t"})
	require.NoError(t, err)

	// 新实例复用持久化 token，不重复建号
	c2 := newTestClient(t, dir, srv.URL)
	_, err = c2.CreatePage(context.Background(), Page{Title: "t2"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ft.accounts.Load())
}

func TestCorruptTokenFileRecreates(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	dir := t.TempDir()

	// 非 JSON 垃圾字节视为损坏，自动重建
	require.NoError(t, os.WriteFile(filepath.Join(dir, tokenFile), []byte("\x00garbage"), 0o600))

	c := newTestClient(t, dir, srv.URL)
	_, err := c.CreatePage(context.Background(), Page{Title: "t"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ft.accounts.Load())

	ft.mu.Lock()
	defer ft.mu.Unlock()
	assert.Equal(t, "TOKEN-1", ft.lastPage["access_token"])
}

func TestConcurrentLazyInitSingleAccount(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	dir := t.TempDir()
	c := newTestClient(t, dir, srv.URL)

	// 并发首调用只允许一次建号（mutex 懒加载，CR1-#9）
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.accessToken(context.Background())
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1), ft.accounts.Load())
}

func TestCreatePageAPIError(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	ft.apiErr.Store("AccessTokenInvalid")

	c := newTestClient(t, t.TempDir(), srv.URL)
	_, err := c.CreatePage(context.Background(), Page{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessTokenInvalid")
}

func TestCreatePageServerUnreachable(t *testing.T) {
	_, srv := newFakeTelegraph(t)
	url := srv.URL
	srv.Close() // 立即关闭，构造网络不可达

	c := newTestClient(t, t.TempDir(), url)
	_, err := c.CreatePage(context.Background(), Page{Title: "t"})
	require.Error(t, err)
}

func TestBaseURLEnvOverride(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	// 与 telegram 包 TELEGRAM_API_URL 同款约定：集成测试指向本地假服务器
	t.Setenv("TELEGRAPH_API_URL", srv.URL)
	c := NewClient(t.TempDir(), "", nil)
	_, err := c.CreatePage(context.Background(), Page{Title: "t"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ft.accounts.Load())
}

// EditPage 全量替换语义（实测 TITLE_REQUIRED/CONTENT_REQUIRED/author 不传即清空）：
// 请求体必须带全量 title/content/author_name/author_url（issue #12 回填）
func TestEditPageFullParams(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	c := newTestClient(t, t.TempDir(), srv.URL)

	content := []any{Node{Tag: "p", Children: []any{"hello"}}}
	err := c.EditPage(context.Background(), "Test-10-01", Page{
		Title:      "Test",
		AuthorName: "special-feed",
		AuthorURL:  "https://t.me/chan/42",
		Content:    content,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ft.edits.Load())

	ft.mu.Lock()
	defer ft.mu.Unlock()
	req := ft.lastEdit
	assert.Equal(t, "Test-10-01", req["path"])
	assert.Equal(t, "Test", req["title"])
	assert.Equal(t, "special-feed", req["author_name"])
	assert.Equal(t, "https://t.me/chan/42", req["author_url"])
	assert.Equal(t, []any{map[string]any{"tag": "p", "children": []any{"hello"}}}, req["content"])
	assert.NotEmpty(t, req["access_token"], "token 随请求发出（懒建号）")
}

func TestEditPageAPIError(t *testing.T) {
	ft, srv := newFakeTelegraph(t)
	ft.apiErr.Store("ACCESS_TOKEN_INVALID")
	c := newTestClient(t, t.TempDir(), srv.URL)

	err := c.EditPage(context.Background(), "Test-10-01", Page{Title: "t", Content: []any{"x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "editPage")
}
