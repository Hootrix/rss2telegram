package rss

// issue #16：超限长图切片。slicePhoto = 解码（DecodeConfig 像素预检）+ sliceImage 分段 + encodeChunkJPEG
import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeGrad 生成 w×h 渐变灰度图（非纯色，避免 JPEG 编码退化到几百字节，保证编码体积断言有意义）
func encodeGrad(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x + y) % 256)})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}))
	return buf.Bytes()
}

// dims 便捷解包 DecodeConfig
func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	return cfg.Width, cfg.Height
}

func TestSlicePhoto(t *testing.T) {
	t.Run("w+h 超限长图切成多片且逐片合规", func(t *testing.T) {
		// 200×9900：w+h=10100 触发；chunkMax=min(9800, 4000)=4000；n=3；base=3300
		chunks, truncated, err := slicePhoto(encodeGrad(t, 200, 9900))
		require.NoError(t, err)
		assert.False(t, truncated)
		require.Len(t, chunks, 3)
		for i, c := range chunks {
			w, h := dims(t, c)
			assert.Equal(t, 200, w)
			assert.LessOrEqual(t, w+h, maxPhotoSideSum, "片 %d 超 w+h", i)
			assert.GreaterOrEqual(t, h, (200+maxPhotoRatio-1)/maxPhotoRatio, "片 %d 低于片高下限", i)
		}
		// 切片无损拼回原高
		total := 0
		for _, c := range chunks {
			_, h := dims(t, c)
			total += h
		}
		assert.Equal(t, 9900, total)
	})

	t.Run("比例超限窄长条切为合规片", func(t *testing.T) {
		// 100×5000：比例 50 > 20 触发；chunkMax=min(9900,2000)=2000；n=3；base=1666/1667
		chunks, _, err := slicePhoto(encodeGrad(t, 100, 5000))
		require.NoError(t, err)
		require.Len(t, chunks, 3)
	})

	t.Run("恰好 10 片不截断", func(t *testing.T) {
		// 100×20000：chunkMax=min(9900,2000)=2000 → n=⌈20000/2000⌉=10，恰等 maxAlbumPhotos：
		// truncated := n > maxAlbumPhotos 的 off-by-one 守护（误写 >= 此用例必红）
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 20000))
		require.NoError(t, err)
		assert.False(t, truncated)
		require.Len(t, chunks, maxAlbumPhotos)
		total := 0
		for i, c := range chunks {
			w, h := dims(t, c)
			assert.Equal(t, 100, w)
			assert.Equal(t, 2000, h, "片 %d 高度应为满额 chunkMax", i)
			total += h
		}
		assert.Equal(t, 20000, total, "各片拼回总高")
	})

	t.Run("超过 10 片截尾并置 truncated", func(t *testing.T) {
		// 100×25000：chunkMax=2000；n=13 > 10 → 10×2000，尾部 5000 丢弃
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 25000))
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.Len(t, chunks, maxAlbumPhotos)
		for _, c := range chunks {
			_, h := dims(t, c)
			assert.Equal(t, 2000, h)
		}
	})

	t.Run("w 过大无合法片高区间降级", func(t *testing.T) {
		// 9600×1000：chunkMax=min(400, 192000)=400 < chunkMin=480 → 区间空
		_, _, err := slicePhoto(encodeGrad(t, 9600, 1000))
		assert.ErrorContains(t, err, "chunk")
	})

	t.Run("均匀切分后片高低于下限不可分区", func(t *testing.T) {
		// 9500×600：区间 [475,500] 非空但 600 无法分为合法段（n=2 → base=300 < 475）
		_, _, err := slicePhoto(encodeGrad(t, 9500, 600))
		assert.ErrorContains(t, err, "partition")
	})

	t.Run("像素超 40MP 拒绝", func(t *testing.T) {
		// 直接测 sliceImage：无需真解码 40MP 字节（10000×10001 = 100,010,000 > 40MP）
		_, _, err := sliceImage(image.NewGray(image.Rect(0, 0, 10000, 10001)))
		assert.ErrorContains(t, err, "pixels")
	})

	t.Run("宽低于短边下限拒绝（切片只会更窄）", func(t *testing.T) {
		// 外部 CR：切片是纵向切，片宽=图宽，w < 50 的图切出来仍不可用；
		// validatePhoto 已在 fetch 路径拦截，此处兜底直接传位图的调用方
		_, _, err := sliceImage(image.NewGray(image.Rect(0, 0, 40, 200)))
		assert.ErrorContains(t, err, "below minimum side")
	})

	t.Run("损坏数据解码失败", func(t *testing.T) {
		_, _, err := slicePhoto([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0})
		assert.ErrorContains(t, err, "decode")
	})
}

func TestSliceImageTransparentPNG(t *testing.T) {
	// 透明 PNG 经 encodeChunkJPEG 后左上角应为白底（JPEG 无 alpha）
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{}) // 透明
		}
	}
	chunks, _, err := sliceImage(img)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	parsed, err := jpeg.Decode(bytes.NewReader(chunks[0]))
	require.NoError(t, err)
	r, g, b, _ := parsed.At(10, 10).RGBA()
	assert.Equal(t, uint32(0xFFFF), r, "透明区应填白")
	assert.Equal(t, uint32(0xFFFF), g)
	assert.Equal(t, uint32(0xFFFF), b)
}
