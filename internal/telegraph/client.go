package telegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// apiError Telegraph API 业务错误（ok:false），code 为 API 返回的错误码。
// 独立类型使 isTokenInvalidError 能精确比对错误码而非在整串错误信息里找子串——
// 否则本端 "createAccount: empty access_token" 这类报错会被误判成凭证失效（外部 CR）
type apiError struct {
	method string
	code   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegraph %s: api error: %s", e.method, e.code)
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
		return &apiError{method: method, code: apiResp.Error}
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

// pageResult 为 createPage 的 result 子集；path 是官方返回的页面路径段
// （无 leading slash，如 "Test-10-01"），回填 editPage 直接使用
type pageResult struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

// isTokenInvalidError 判断 Telegraph 返回的凭证失效类错误（ACCESS_TOKEN_INVALID 等）。
// 只认 apiError 携带的错误码（含 TOKEN 即视为凭证问题），不匹配本端构造的
// "empty access_token" 等文本（外部 CR：子串匹配会误中自家报错白白重建一次）。
// 格式合法但实际失效的 token 会让每次建页降级、且只能手动删文件恢复，
// 检出后由调用方触发 refreshToken 重建重试
func isTokenInvalidError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return strings.Contains(strings.ToUpper(ae.code), "TOKEN")
}

// refreshToken 清除内存与文件中的失效 token 并匿名重建。
// stale 为本次失败时使用的凭证：比较后才换，当前 token 已被并发方重建过就直接返回——
// handler 并发处理 feed，N 个 feed 同遇失效时锁只能排队，无比较会各建一号（外部 CR）。
// 文件删除失败仅记日志：内存 token 已清，最坏重建后落盘失败也只是重启再建
func (c *Client) refreshToken(ctx context.Context, stale string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.token != stale {
		return nil // 并发方已重建，复用新 token
	}
	c.token = ""
	if c.tokenPath != "" {
		if err := os.Remove(c.tokenPath); err != nil && !os.IsNotExist(err) {
			log.Printf("telegraph: remove stale token file failed: %v", err)
		}
	}
	_, err := c.createAccountLocked(ctx)
	return err
}

// CreatePage 发布页面，返回 (url, path)。path 取 API result 自带字段，
// 比从 URL 字符串截取稳（假服务器/域名变化时截取会错，外部 CR）。
// 建页不持全局锁，可并发调用。凭证失效（ACCESS_TOKEN_INVALID 类错误）时
// 重建 token 并重试一次
func (c *Client) CreatePage(ctx context.Context, page Page) (string, string, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", "", err
	}
	url, path, err := c.createPage(ctx, tok, page)
	if err == nil || !isTokenInvalidError(err) {
		return url, path, err
	}
	if rerr := c.refreshToken(ctx, tok); rerr != nil {
		return "", "", fmt.Errorf("telegraph createPage: token refresh: %w (after %v)", rerr, err)
	}
	if tok, err = c.accessToken(ctx); err != nil {
		return "", "", err
	}
	return c.createPage(ctx, tok, page)
}

func (c *Client) createPage(ctx context.Context, tok string, page Page) (string, string, error) {
	req := struct {
		AccessToken string `json:"access_token"`
		Page
	}{AccessToken: tok, Page: page}
	var res pageResult
	if err := c.call(ctx, "createPage", req, &res); err != nil {
		return "", "", err
	}
	if res.URL == "" {
		return "", "", fmt.Errorf("telegraph createPage: empty url")
	}
	return res.URL, strings.TrimPrefix(res.Path, "/"), nil
}

// EditPage 编辑已发布页面。editPage 为全量替换语义（2026-10-07 实测真实 API）：
// title/content 必传（缺失报 TITLE_REQUIRED/CONTENT_REQUIRED），author_name/author_url
// 不传即被清空——调用方必须带上建页时的全部参数（issue #12 author_url 回填）
//
// 不做 token 失效重建重试：Telegraph 只允许建页账号编辑自己的页面，新账号的 token
// 编不了旧页面，重建重试必然再败、还白建一个账号并替换掉当前 token（外部 CR）。
// 回填失败仅让 author_url 停在频道主页兜底，可接受。
// 旧实现（同 CreatePage 的失效重建重试）注释保留：
// err := c.doEditPage(ctx, path, page)
// if err == nil || !isTokenInvalidError(err) { return err }
// if rerr := c.refreshToken(ctx, tok); rerr != nil { ... }
// return c.doEditPage(ctx, path, page)
func (c *Client) EditPage(ctx context.Context, path string, page Page) error {
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
// 格式合法但实际失效的 token 在 createPage 报 TOKEN 类错误后走 refreshToken 自愈
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
