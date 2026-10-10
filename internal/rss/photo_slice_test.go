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
	// [issue #16 CR 可读性重构] 片高被 readableChunkHeight=1280 封顶（sendPhoto 服务端
	// 压缩至长边 ~1280，片高 ≤1280 保证零二次缩放、文字零损失），所有期望按新数学重算。
	// [issue #16 用户反馈] slicePhoto/sliceImage 参数化 maxPieces（相册切片上限）
	t.Run("w+h 超限长图切成多片且逐片合规", func(t *testing.T) {
		// 200×9900：w+h=10100 触发；chunkMax=min(9800, 4000, 1280)=1280；n=⌈9900/1280⌉=8；
		// base=1237 rem=4 → 4×1238 + 4×1237
		chunks, truncated, err := slicePhoto(encodeGrad(t, 200, 9900), maxAlbumPhotos)
		require.NoError(t, err)
		assert.False(t, truncated)
		require.Len(t, chunks, 8)
		for i, c := range chunks {
			w, h := dims(t, c)
			assert.Equal(t, 200, w)
			assert.LessOrEqual(t, h, readableChunkHeight, "片 %d 超可读片高上限", i)
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
		// 100×5000：比例 50 > 20 触发；chunkMax=min(9900, 2000, 1280)=1280；n=4；base=1250
		chunks, _, err := slicePhoto(encodeGrad(t, 100, 5000), maxAlbumPhotos)
		require.NoError(t, err)
		require.Len(t, chunks, 4)
		for i, c := range chunks {
			_, h := dims(t, c)
			assert.Equal(t, 1250, h, "片 %d 应为均分 1250", i)
		}
	})

	t.Run("恰好 10 片不截断", func(t *testing.T) {
		// 100×12800：chunkMax=1280 → n=⌈12800/1280⌉=10，恰等 maxAlbumPhotos：
		// truncated := n > maxPieces 的 off-by-one 守护（误写 >= 此用例必红）
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 12800), maxAlbumPhotos)
		require.NoError(t, err)
		assert.False(t, truncated)
		require.Len(t, chunks, maxAlbumPhotos)
		total := 0
		for i, c := range chunks {
			w, h := dims(t, c)
			assert.Equal(t, 100, w)
			assert.Equal(t, 1280, h, "片 %d 高度应为满额 chunkMax", i)
			total += h
		}
		assert.Equal(t, 12800, total, "各片拼回总高")
	})

	t.Run("超过 10 片截尾并置 truncated", func(t *testing.T) {
		// 100×25000：chunkMax=1280；n=⌈25000/1280⌉=20 > 10 → 10×1280，尾部 12200 丢弃
		// （issue #16 用户反馈起：该截尾路径由 photo_overlimit=crop 正式启用，
		// 不再仅是防御保留）
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 25000), maxAlbumPhotos)
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.Len(t, chunks, maxAlbumPhotos)
		for _, c := range chunks {
			_, h := dims(t, c)
			assert.Equal(t, 1280, h)
		}
	})

	t.Run("maxPieces=3 压制原 4 片场景为 3 片并截断", func(t *testing.T) {
		// [issue #16 用户反馈] 100×5000 本可切 4 片（n=4）；上限压到 3 →
		// truncated=true，取顶部 3×1280（截尾片高固定 chunkMax），尾部 1160 丢弃
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 5000), 3)
		require.NoError(t, err)
		assert.True(t, truncated)
		require.Len(t, chunks, 3)
		for i, c := range chunks {
			_, h := dims(t, c)
			assert.Equal(t, 1280, h, "截尾片 %d 高度应固定 chunkMax", i)
		}
	})

	t.Run("maxPieces=4 恰等于 needed 不截断", func(t *testing.T) {
		// 100×5000：n=4 恰等上限 → 走均匀切分（非截尾），4×1250 拼回原高
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 5000), 4)
		require.NoError(t, err)
		assert.False(t, truncated)
		require.Len(t, chunks, 4)
		total := 0
		for _, c := range chunks {
			_, h := dims(t, c)
			total += h
		}
		assert.Equal(t, 5000, total)
	})

	t.Run("maxPieces 上界钳制与下界防御", func(t *testing.T) {
		// 上界：传 >10 被钳回 maxAlbumPhotos（100×12800 恰 10 片不截断）
		chunks, truncated, err := slicePhoto(encodeGrad(t, 100, 12800), 99)
		require.NoError(t, err)
		assert.False(t, truncated)
		assert.Len(t, chunks, maxAlbumPhotos)
		// 下界：<1 防御性报错（handler 侧 config 校验已保证 2-10，此处兜底直接调用的调用方）
		_, _, err = slicePhoto(encodeGrad(t, 100, 5000), 0)
		assert.ErrorContains(t, err, "maxPieces")
	})

	t.Run("w 过大无合法片高区间降级", func(t *testing.T) {
		// 9600×1000：chunkMax=min(400, 192000, 1280)=400 < chunkMin=480 → 区间空
		_, _, err := slicePhoto(encodeGrad(t, 9600, 1000), maxAlbumPhotos)
		assert.ErrorContains(t, err, "chunk")
	})

	t.Run("均匀切分后片高低于下限不可分区", func(t *testing.T) {
		// 9500×600：区间 [475,500] 非空但 600 无法分为合法段（n=2 → base=300 < 475）
		_, _, err := slicePhoto(encodeGrad(t, 9500, 600), maxAlbumPhotos)
		assert.ErrorContains(t, err, "partition")
	})

	t.Run("像素超 40MP 拒绝", func(t *testing.T) {
		// 直接测 sliceImage：无需真解码 40MP 字节（10000×10001 = 100,010,000 > 40MP）
		_, _, err := sliceImage(image.NewGray(image.Rect(0, 0, 10000, 10001)), maxAlbumPhotos)
		assert.ErrorContains(t, err, "pixels")
	})

	t.Run("宽低于短边下限拒绝（切片只会更窄）", func(t *testing.T) {
		// 外部 CR：切片是纵向切，片宽=图宽，w < 50 的图切出来仍不可用；
		// validatePhoto 已在 fetch 路径拦截，此处兜底直接传位图的调用方
		_, _, err := sliceImage(image.NewGray(image.Rect(0, 0, 40, 200)), maxAlbumPhotos)
		assert.ErrorContains(t, err, "below minimum side")
	})

	t.Run("损坏数据解码失败", func(t *testing.T) {
		_, _, err := slicePhoto([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}, maxAlbumPhotos)
		assert.ErrorContains(t, err, "decode")
	})
}

// issue #16 二轮评审：可判别错误 unsliceableError——「图片完好但切不动」
// （均分不可分 / 单片超 10MB），与「数据损坏」（解码失败）区分：
// handler 的 crop 分支前者兜底原文件、后者回退下一候选
func TestUnsliceableError(t *testing.T) {
	t.Run("均分不可分区属 unsliceable", func(t *testing.T) {
		// 9500×600：区间 [475,500] 非空但 n=2 均分 base=300 < 475 → partition 报错
		_, _, err := slicePhoto(encodeGrad(t, 9500, 600), maxAlbumPhotos)
		require.Error(t, err)
		assert.ErrorContains(t, err, "partition")
		assert.True(t, isUnsliceableError(err), "图片完好切不动，应可判别为 unsliceable")
	})

	t.Run("解码损坏不属 unsliceable", func(t *testing.T) {
		// 截断 JPEG：DecodeConfig 头部可读但全量解码失败 → 数据损坏，普通错误
		full := encodeGrad(t, 200, 9900)
		_, _, err := slicePhoto(full[:len(full)*3/5], maxAlbumPhotos)
		require.Error(t, err)
		assert.False(t, isUnsliceableError(err), "数据损坏应回退候选而非兜底原文件")
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
	chunks, _, err := sliceImage(img, maxAlbumPhotos)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	parsed, err := jpeg.Decode(bytes.NewReader(chunks[0]))
	require.NoError(t, err)
	r, g, b, _ := parsed.At(10, 10).RGBA()
	assert.Equal(t, uint32(0xFFFF), r, "透明区应填白")
	assert.Equal(t, uint32(0xFFFF), g)
	assert.Equal(t, uint32(0xFFFF), b)
}

// issue #16 CR 可读性重构：fitsReadableAlbum 用头部尺寸预判「能否切成 ≤maxPieces 张
// 零压缩可读片」，photoForItem 以此决定相册 vs 整图文件（避免切完再丢弃）；
// [issue #16 用户反馈] maxPieces 参数化（photo_slices 配置）
func TestFitsReadableAlbum(t *testing.T) {
	// 1080×12112：chunkMax=1280，n=ceil(12112/1280)=10 恰好 ≤10；末片 1211 ≥ 片高下限 54
	assert.True(t, fitsReadableAlbum(1080, 12112, maxAlbumPhotos))
	// 1080×3363：n=3，每片 1121
	assert.True(t, fitsReadableAlbum(1080, 3363, maxAlbumPhotos))
	// 4000×10000：像素恰 40MP 限内；chunkMax=1280，n=8；末片 1250 ≥ 下限 200
	assert.True(t, fitsReadableAlbum(4000, 10000, maxAlbumPhotos))
	// 1080×33634：像素 36.3MP ≤ 40MP 限内，但 n=27 > 10 → 不可读相册，走整图文件
	assert.False(t, fitsReadableAlbum(1080, 33634, maxAlbumPhotos))
	// 9600×1000：chunkMax=400 < chunkMin=480，区间空（过宽图不可切片）
	assert.False(t, fitsReadableAlbum(9600, 1000, maxAlbumPhotos))
	// 9500×600：区间 [475,500] 非空但 n=2 均分后 base=300 < 475
	assert.False(t, fitsReadableAlbum(9500, 600, maxAlbumPhotos))
	// 4000×15000：n=12 > 10
	assert.False(t, fitsReadableAlbum(4000, 15000, maxAlbumPhotos))

	// [issue #16 用户反馈] maxPieces 生效：同一尺寸随上限收紧而翻转
	// 1080×12112：n=10 ≤ 10 → 默认上限可读；压到 3 → 10 > 3 不可读
	assert.True(t, fitsReadableAlbum(1080, 12112, 10))
	assert.False(t, fitsReadableAlbum(1080, 12112, 3))
	// 1080×33634：n=27 连默认上限 10 都超
	assert.False(t, fitsReadableAlbum(1080, 33634, 10))
}
