package rss

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Hootrix/rss2telegram/internal/extractor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeGray 生成 w×h 灰度图字节（jpeg/png），validatePhoto 只读文件头不解码像素
func encodeGray(t *testing.T, w, h int, asJPEG bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, w, h))
	if asJPEG {
		require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50}))
	} else {
		require.NoError(t, png.Encode(&buf, img))
	}
	return buf.Bytes()
}

// issue #13：validatePhoto——格式白名单（字节嗅探为准）+ 尺寸预检
// （w+h ≤ 10000、比例 ≤ 20、min ≥ 50；webp 跳过尺寸检查）
func TestValidatePhoto(t *testing.T) {
	t.Run("正常 png/jpeg 通过", func(t *testing.T) {
		assert.NoError(t, validatePhoto(encodeGray(t, 640, 480, false)))
		assert.NoError(t, validatePhoto(encodeGray(t, 690, 253, true)))
	})
	t.Run("空数据拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto(nil))
	})
	t.Run("gif 拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto([]byte("GIF89a"+string(make([]byte, 32)))))
	})
	t.Run("html 文本拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto([]byte("<html><body>x</body></html>")))
	})
	t.Run("损坏 jpeg 头拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}))
	})
	t.Run("w+h 超过 10000 拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto(encodeGray(t, 1080, 10492, true)))
	})
	t.Run("w+h 恰好 10000 通过（均衡尺寸）", func(t *testing.T) {
		assert.NoError(t, validatePhoto(encodeGray(t, 5000, 5000, true)))
	})
	t.Run("比例恰好 20 通过", func(t *testing.T) {
		assert.NoError(t, validatePhoto(encodeGray(t, 1000, 50, false)))
	})
	t.Run("比例超过 20 拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto(encodeGray(t, 60, 1260, false)))
	})
	t.Run("短边小于 50 拒绝", func(t *testing.T) {
		assert.Error(t, validatePhoto(encodeGray(t, 100, 49, false)))
	})
	t.Run("webp 魔数通过（跳过尺寸检查）", func(t *testing.T) {
		// RIFF....WEBP 头 + VP8 载荷占位，DetectContentType 判 image/webp
		webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
		assert.NoError(t, validatePhoto(webp))
	})
	// issue #16：尺寸/比例超限须返回可判别的 photoDimensionError（handler 据此触发切片），
	// 其余失败类别必须不可误判
	t.Run("w+h 超限返回可切片的 dimension 错误", func(t *testing.T) {
		err := validatePhoto(encodeGray(t, 1080, 10492, true))
		assert.True(t, isDimensionError(err), "w+h 超限应可切片: %v", err)
	})
	t.Run("比例超限返回可切片的 dimension 错误", func(t *testing.T) {
		err := validatePhoto(encodeGray(t, 60, 1260, false))
		assert.True(t, isDimensionError(err), "比例超限应可切片: %v", err)
	})
	t.Run("短边过小不可切片（切片只会更小）", func(t *testing.T) {
		assert.False(t, isDimensionError(validatePhoto(encodeGray(t, 100, 49, false))))
	})
	t.Run("非尺寸失败不可切片", func(t *testing.T) {
		assert.False(t, isDimensionError(validatePhoto([]byte("<html>x</html>"))))
	})
}

// issue #13：httpPhotoFetcher.Fetch——状态码/大小/嗅探/UA/超时
func TestPhotoFetch(t *testing.T) {
	pngBytes := encodeGray(t, 640, 480, false)

	t.Run("成功返回原字节且带 UA", func(t *testing.T) {
		// buffered channel 传出 UA：handler goroutine 与测试 goroutine 间
		// 无 happens-before（TCP 收发不构成同步），裸变量读写 -race 下有概率报
		uaCh := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uaCh <- r.UserAgent()
			_, _ = w.Write(pngBytes)
		}))
		defer srv.Close()

		f := newHTTPPhotoFetcher()
		data, err := f.Fetch(context.Background(), srv.URL)

		require.NoError(t, err)
		assert.Equal(t, pngBytes, data)
		assert.Equal(t, extractor.UserAgent, <-uaCh)
	})

	t.Run("非 200 失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}))
		defer srv.Close()

		_, err := newHTTPPhotoFetcher().Fetch(context.Background(), srv.URL)
		assert.ErrorContains(t, err, "status 404")
	})

	t.Run("Content-Length 声明超限提前失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(maxPhotoBytes+1))
		}))
		defer srv.Close()

		_, err := newHTTPPhotoFetcher().Fetch(context.Background(), srv.URL)
		assert.ErrorContains(t, err, "content length")
	})

	t.Run("无 Content-Length 流式超限失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher, ok := w.(http.Flusher)
			require.True(t, ok, "httptest 需支持 flush 强制 chunked")
			// 分块写超限字节，避免响应头带 Content-Length
			chunk := make([]byte, 1<<20)
			for i := 0; i <= maxPhotoBytes/(1<<20); i++ {
				_, _ = w.Write(chunk)
				flusher.Flush()
			}
		}))
		defer srv.Close()

		_, err := newHTTPPhotoFetcher().Fetch(context.Background(), srv.URL)
		assert.ErrorContains(t, err, "exceeds")
	})

	t.Run("响应头谎报 Content-Type 仍按字节嗅探", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pngBytes)
		}))
		defer srv.Close()

		data, err := newHTTPPhotoFetcher().Fetch(context.Background(), srv.URL)
		require.NoError(t, err)
		assert.Equal(t, pngBytes, data)
	})

	t.Run("子超时生效", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write(pngBytes)
		}))
		defer srv.Close()

		f := newHTTPPhotoFetcher()
		f.fetchTimeout = 50 * time.Millisecond
		_, err := f.Fetch(context.Background(), srv.URL)
		assert.Error(t, err)
	})
}
