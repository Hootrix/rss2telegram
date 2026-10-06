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
		baseURL = DefaultBaseURL
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

// CreatePage 发布页面返回 URL；建页不持全局锁，可并发调用
func (c *Client) CreatePage(ctx context.Context, page Page) (string, error) {
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
