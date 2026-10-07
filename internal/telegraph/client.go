package telegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://api.telegra.ph"
	tokenFile      = "telegraph_token.json"
	// createAccount 唯一必填项 short_name（官方限制 1-32 字符）
	shortName = "rss2telegram"
)

// Page 建页参数；硬限制预截断（title 256 / author 128 / content 64KB）
// 由调用方（rss 快照编排）完成，客户端只透传
type Page struct {
	Title      string `json:"title"`
	AuthorName string `json:"author_name,omitempty"`
	AuthorURL  string `json:"author_url,omitempty"`
	Content    []any  `json:"content"`
}

// Client Telegraph API 轻封装（无官方 SDK，社区库皆 unofficial 小众，自研决策见设计文档）
// 匿名 access_token 本质是会话凭证而非账号：懒加载 + 本地持久化，丢失自动重建；
// 旧页面仍在线，新页面归属新凭证，无碍
type Client struct {
	baseURL   string
	hc        *http.Client
	tokenPath string // 空 = 不持久化（仅内存，测试用）

	mu    sync.Mutex
	token string
}

func NewClient(dataDir, baseURL string, hc *http.Client) *Client {
	if baseURL == "" {
		// 可测试性注入点：与 telegram 包 TELEGRAM_API_URL 同款约定（本地集成测试假服务器）
		if env := os.Getenv("TELEGRAPH_API_URL"); env != "" {
			baseURL = env
		} else {
			baseURL = DefaultBaseURL
		}
	}
	if hc == nil {
		// 兜底超时；精确的 10s 子超时由调用方 ctx 控制
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	tokenPath := ""
	if dataDir != "" {
		tokenPath = filepath.Join(dataDir, tokenFile)
	}
	return &Client{baseURL: baseURL, hc: hc, tokenPath: tokenPath}
}

type apiResponse struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result"`
}

func (c *Client) call(ctx context.Context, method string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("telegraph %s: %w", method, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegraph %s: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("telegraph %s: %w", method, err)
	}
	defer resp.Body.Close()
	// 非 2xx 直接失败：避免网关/CDN 返回的 HTML 错误页走进 JSON decode 报错，
	// 语义更直白（ok:false 的 API 错误仍由下方 apiResp 处理）
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("telegraph %s: http status %d", method, resp.StatusCode)
	}
	var apiResp apiResponse
	// 响应体上限 1MB：createPage 带 return_content=false 时远小于此，防异常响应吃内存
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&apiResp); err != nil {
		return fmt.Errorf("telegraph %s: decode response: %w", method, err)
	}
	if !apiResp.OK {
		return fmt.Errorf("telegraph %s: api error: %s", method, apiResp.Error)
	}
	if out != nil {
		if err := json.Unmarshal(apiResp.Result, out); err != nil {
			return fmt.Errorf("telegraph %s: decode result: %w", method, err)
		}
	}
	return nil
}

type accountResult struct {
	AccessToken string `json:"access_token"`
}

// CreateAccount 匿名建号；成功后覆盖内存 token 并原子落盘
func (c *Client) CreateAccount(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createAccountLocked(ctx)
}

func (c *Client) createAccountLocked(ctx context.Context) (string, error) {
	var res accountResult
	if err := c.call(ctx, "createAccount", map[string]any{"short_name": shortName}, &res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("telegraph createAccount: empty access_token")
	}
	c.token = res.AccessToken
	if c.tokenPath != "" {
		if err := saveToken(c.tokenPath, res.AccessToken); err != nil {
			// 落盘失败不阻断建页：token 仍在内存，本进程可用；重启后自动重建
			log.Printf("telegraph: save token failed (will recreate on restart): %v", err)
		}
	}
	return res.AccessToken, nil
}

// accessToken 懒加载链：内存 → 持久化文件 → 匿名建号
// 持锁建号使并发首调用只发一次 createAccount（CR1-#9）
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	if c.tokenPath != "" {
		if tok, ok := loadToken(c.tokenPath); ok {
			c.token = tok
			return tok, nil
		}
	}
	return c.createAccountLocked(ctx)
}

type pageResult struct {
	URL string `json:"url"`
}

// isTokenInvalidError 判断 Telegraph 返回的凭证失效类错误（ACCESS_TOKEN_INVALID 等）。
// 格式合法但实际失效的 token 会让每次建页降级、且只能手动删文件恢复（外部 CR），
// 检出后由调用方触发 refreshToken 重建重试
func isTokenInvalidError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "access_token")
}

// refreshToken 清除内存与文件中的失效 token 并匿名重建（持锁，并发下串行重建）。
// 文件删除失败仅记日志：内存 token 已清，最坏重建后落盘失败也只是重启再建
func (c *Client) refreshToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	if c.tokenPath != "" {
		if err := os.Remove(c.tokenPath); err != nil && !os.IsNotExist(err) {
			log.Printf("telegraph: remove stale token file failed: %v", err)
		}
	}
	_, err := c.createAccountLocked(ctx)
	return err
}

// CreatePage 发布页面返回 URL；建页不持全局锁，可并发调用。
// 凭证失效（ACCESS_TOKEN_INVALID 类错误）时重建 token 并重试一次
func (c *Client) CreatePage(ctx context.Context, page Page) (string, error) {
	url, err := c.doCreatePage(ctx, page)
	if err == nil || !isTokenInvalidError(err) {
		return url, err
	}
	if rerr := c.refreshToken(ctx); rerr != nil {
		return "", fmt.Errorf("telegraph createPage: token refresh: %w (after %v)", rerr, err)
	}
	return c.doCreatePage(ctx, page)
}

func (c *Client) doCreatePage(ctx context.Context, page Page) (string, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	req := struct {
		AccessToken string `json:"access_token"`
		Page
	}{AccessToken: tok, Page: page}
	var res pageResult
	if err := c.call(ctx, "createPage", req, &res); err != nil {
		return "", err
	}
	if res.URL == "" {
		return "", fmt.Errorf("telegraph createPage: empty url")
	}
	return res.URL, nil
}

// EditPage 编辑已发布页面。editPage 为全量替换语义（2026-10-07 实测真实 API）：
// title/content 必传（缺失报 TITLE_REQUIRED/CONTENT_REQUIRED），author_name/author_url
// 不传即被清空——调用方必须带上建页时的全部参数（issue #12 author_url 回填）
// 回填路径同样享受失效重建重试：凭证过期不应让 author_url 永久停在频道主页
func (c *Client) EditPage(ctx context.Context, path string, page Page) error {
	err := c.doEditPage(ctx, path, page)
	if err == nil || !isTokenInvalidError(err) {
		return err
	}
	if rerr := c.refreshToken(ctx); rerr != nil {
		return fmt.Errorf("telegraph editPage: token refresh: %w (after %v)", rerr, err)
	}
	return c.doEditPage(ctx, path, page)
}

func (c *Client) doEditPage(ctx context.Context, path string, page Page) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	req := struct {
		AccessToken string `json:"access_token"`
		Path        string `json:"path"`
		Page
	}{AccessToken: tok, Path: path, Page: page}
	return c.call(ctx, "editPage", req, nil)
}

// saveToken 临时文件 + rename 原子落盘（CR1-#9），0600 防同机其他用户读取
func saveToken(path, token string) error {
	data, err := json.Marshal(map[string]string{"access_token": token})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type tokenFileData struct {
	AccessToken string `json:"access_token"`
}

// loadToken 读取失败/JSON 损坏/空 token 一律视为不可用，触发重建；
// 格式合法但实际失效的 token 只会在 createPage 报 API 错误，快照降级照发，可接受
func loadToken(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var tf tokenFileData
	if err := json.Unmarshal(data, &tf); err != nil {
		return "", false
	}
	if tf.AccessToken == "" {
		return "", false
	}
	return tf.AccessToken, true
}
