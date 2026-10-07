package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// 构造一段足够长(>200 rune)的中文正文，保证通过长度校验
func longText(repeat int) string {
	return strings.Repeat("成都天府市民云是一个方便市民办事的城市服务移动平台，提供社保公积金查询与生活缴费。", repeat)
}

func articleHTML(title, body string) string {
	return "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>" + title +
		"</title></head><body><article><h1>" + title + "</h1><p>" + body + "</p></article></body></html>"
}

// servePage 以指定 Content-Type 伺服 body
func servePage(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 自定义 UA 是设计要求：所有请求必须携带
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("request missing User-Agent")
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestExtractor(timeout time.Duration) *Extractor {
	e := New(nil)
	e.fetchTimeout = timeout
	return e
}

func TestFetchAndExtractOK(t *testing.T) {
	body := articleHTML("测试标题", longText(15))
	srv := servePage(t, "text/html; charset=utf-8", []byte(body))

	e := newTestExtractor(5 * time.Second)
	html, err := e.FetchAndExtract(context.Background(), srv.URL+"/a?x=1")
	require.NoError(t, err)
	// 返回的是 readability 清理后的正文 HTML，包含标题与正文段落
	assert.Contains(t, html, "测试标题")
	assert.Contains(t, html, "城市服务移动平台")
}

func TestFetchAndExtractNonHTMLContentType(t *testing.T) {
	srv := servePage(t, "application/pdf", []byte("%PDF-1.4 fake"))

	e := newTestExtractor(5 * time.Second)
	_, err := e.FetchAndExtract(context.Background(), srv.URL+"/doc.pdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content type")
}

func TestFetchAndExtractBodyTooLarge(t *testing.T) {
	// 5MB+2 字节：读到第 5MB+1 字节即必须判定失败，不依赖后续解析
	big := make([]byte, maxBodyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	srv := servePage(t, "text/html", big)

	e := newTestExtractor(10 * time.Second)
	_, err := e.FetchAndExtract(context.Background(), srv.URL+"/big")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestFetchAndExtractShortContent(t *testing.T) {
	srv := servePage(t, "text/html; charset=utf-8", []byte(articleHTML("短", "太短了")))

	e := newTestExtractor(5 * time.Second)
	_, err := e.FetchAndExtract(context.Background(), srv.URL+"/short")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestFetchAndExtractGBKDeclared(t *testing.T) {
	// 响应头声明 GBK：charset.NewReader 必须正确转码，产出可读 UTF-8
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(articleHTML("成都头条", longText(12)))
	require.NoError(t, err)
	srv := servePage(t, "text/html; charset=GBK", []byte(gbk))

	e := newTestExtractor(5 * time.Second)
	html, err := e.FetchAndExtract(context.Background(), srv.URL+"/gbk")
	require.NoError(t, err)
	assert.Contains(t, html, "成都头条")
	assert.Contains(t, html, "生活缴费")
}

func TestFetchAndExtractMojibakeUndeclared(t *testing.T) {
	// GBK 字节但无任何编码声明：解析阶段被替换为 U+FFFD（合法 UTF-8），
	// 必须靠占比阈值识别乱码并失败，而不是发布乱码快照
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(articleHTML("成都头条", longText(12)))
	require.NoError(t, err)
	srv := servePage(t, "text/html", []byte(gbk))

	e := newTestExtractor(5 * time.Second)
	_, err = e.FetchAndExtract(context.Background(), srv.URL+"/mojibake")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mojibake")
}

func TestFetchAndExtractSubTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)

	// 子超时 50ms：不等父 ctx，抓取阶段自身超时
	e := newTestExtractor(50 * time.Millisecond)
	_, err := e.FetchAndExtract(context.Background(), srv.URL+"/slow")
	require.Error(t, err)
}

func TestFetchAndExtractParentCancelled(t *testing.T) {
	srv := servePage(t, "text/html; charset=utf-8", []byte(articleHTML("t", longText(15))))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消：请求不得发出

	e := newTestExtractor(5 * time.Second)
	_, err := e.FetchAndExtract(ctx, srv.URL+"/a")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFetchAndExtractHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	e := newTestExtractor(5 * time.Second)
	_, err := e.FetchAndExtract(context.Background(), srv.URL+"/403")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

// ValidateText 与 FetchAndExtract 内部校验同一组阈值（FFFD 占比 + rune 下限），
// 抽出导出供 feed 来源路径（snapshot_source: feed）复用，两条路径失败口径一致（issue #12）
func TestValidateText(t *testing.T) {
	t.Run("正常中文正文通过", func(t *testing.T) {
		require.NoError(t, ValidateText(longText(15)))
	})

	t.Run("199/200 rune 边界", func(t *testing.T) {
		// 中文与 4 字节 emoji 混合，确保按 rune 而非字节计数
		under := strings.Repeat("正", 197) + "🙂🙂"
		require.Equal(t, 199, len([]rune(under)))
		require.Error(t, ValidateText(under))

		exact := strings.Repeat("正", 198) + "🙂🙂"
		require.Equal(t, 200, len([]rune(exact)))
		require.NoError(t, ValidateText(exact))
	})

	t.Run("空串失败", func(t *testing.T) {
		require.Error(t, ValidateText(""))
	})

	t.Run("FFFD 恰好 1% 通过，超过失败", func(t *testing.T) {
		// 构造均 ≥200 rune，确保只触发乱码规则不撞长度下限；占比 >1% 判乱码
		slightlyUnder := strings.Repeat("正", 398) + "��" // 2/400 = 0.5% < 1%
		require.NoError(t, ValidateText(slightlyUnder))

		over := strings.Repeat("正", 296) + strings.Repeat("�", 4) // 4/300 ≈ 1.33% > 1%
		require.Error(t, ValidateText(over))

		exactlyOnePct := strings.Repeat("正", 198) + "��" // 2/200 = 1%，阈值严格大于才判乱码
		require.NoError(t, ValidateText(exactlyOnePct))
	})
}
