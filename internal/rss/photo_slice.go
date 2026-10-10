package rss

// issue #16：超限长图切片为 sendPhoto 合规片，纯函数、无状态

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

const (
	// sendMediaGroup 单次相册上限
	maxAlbumPhotos = 10

	// 解码位图上限（字面 100MP = 100_000_000）：10MB JPEG 可声称任意尺寸，解码前按头部拦截，
	// 防超大位图打爆常驻内存。最坏瞬时 250–400MB（4:4:4 JPEG 解码 ~300MB + 每片 RGBA 画布
	// 峰值，w=5000 时 ~100MB），逐片分配可回收，可接受。
	// 注意：测试用 10000×10001 = 100_010,000 像素恰好超此阈值，故不能写成 100<<20（≈104.9MP）
	maxPhotoPixels = 100_000_000

	// 重编码质量：实测长截图 q85 单片 ≤1MB，远低于 10MB 上限
	sliceJPEGQuality = 85
)

// checkPixelBudget 像素预算守卫，slicePhoto 与 sliceImage 共用：
// 前者在 DecodeConfig 后、全量解码前调用（内存保护的承重点），
// 后者在入口调用（覆盖直接传位图的调用方，如测试与未来复用）
func checkPixelBudget(w, h int) error {
	// int64 乘积防 32 位平台溢出：JPEG 尺寸上限 65535² ≈ 4.29e9 超 int32，
	// int 为 32 位的平台上 w*h 会回绕成负数绕过守卫
	if int64(w)*int64(h) > maxPhotoPixels {
		return fmt.Errorf("slice photo: %dx%d pixels exceed %d", w, h, maxPhotoPixels)
	}
	return nil
}

// slicePhoto 解码原始图片字节并切片。
// DecodeConfig 头部先行像素预检（全量解码前拦截超大位图），
// 解码失败（webp/损坏/未注册格式）返回 error，由调用方回退下一候选
func slicePhoto(data []byte) ([][]byte, bool, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, false, fmt.Errorf("slice photo: decode config: %w", err)
	}
	if err := checkPixelBudget(cfg.Width, cfg.Height); err != nil {
		return nil, false, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false, fmt.Errorf("slice photo: decode: %w", err)
	}
	return sliceImage(img)
}

// sliceImage 将已解码位图按片高均匀切段重编码。
// 片高合法区间 [⌈w/20⌉, min(10000−w, 20w)]（同时满足 w+h 与比例）；
// 切片触发（调用方经 validatePhoto 判定超限）时 n ≥ 2；限内图直接调用则自然产出单片，
// 由调用方决定单图/相册路径（TestSliceImageTransparentPNG 即单片例证）。
// 超过 maxAlbumPhotos 片时截尾（truncated=true，尾部像素丢弃由 caption 披露）
func sliceImage(img image.Image) ([][]byte, bool, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// 入口像素守卫：slicePhoto 已预检过，这里兜底直接传位图的调用方
	if err := checkPixelBudget(w, h); err != nil {
		return nil, false, err
	}
	chunkMax := min(maxPhotoSideSum-w, maxPhotoRatio*w)
	chunkMin := (w + maxPhotoRatio - 1) / maxPhotoRatio
	if chunkMax <= 0 || chunkMax < chunkMin {
		return nil, false, fmt.Errorf("slice photo: width %d has no valid chunk height (range [%d,%d])", w, chunkMin, chunkMax)
	}
	// h=0 → n=⌈0/chunkMax⌉=0 → 下方 base := h/n 整型除零 panic；h<0 同落 n=0，一并拦截。
	// w<=0 理论上已被上方区间守卫拦截（w=0 → 20w=0；w<0 → 20w<0，均使 chunkMax<=0），
	// 此处对称防御，防上游构造出倒置 Rect（Bounds.Dx() 可为负）
	if h <= 0 || w <= 0 {
		return nil, false, fmt.Errorf("slice photo: invalid dimensions %dx%d", w, h)
	}

	n := (h + chunkMax - 1) / chunkMax
	truncated := n > maxAlbumPhotos
	if truncated {
		n = maxAlbumPhotos
	}
	// 均匀切分：n 段，前 rem 段 +1px；base=⌊h/n⌋
	base := h / n
	if !truncated && base < chunkMin {
		// 区间非空但 h 无法分成合法段（如 9500×600），不可修复
		return nil, false, fmt.Errorf("slice photo: height %d cannot partition into valid chunks (base %d < min %d)", h, base, chunkMin)
	}

	type span struct{ y0, y1 int }
	var spans []span
	if truncated {
		// 截尾路径：片高固定取 chunkMax（不是 base），保证逐片 w+h 合规，尾部像素直接丢弃
		for i := 0; i < maxAlbumPhotos; i++ {
			spans = append(spans, span{b.Min.Y + i*chunkMax, b.Min.Y + (i+1)*chunkMax})
		}
	} else {
		rem := h % n
		y := b.Min.Y
		for i := 0; i < n; i++ {
			ph := base
			if i < rem {
				ph++
			}
			spans = append(spans, span{y, y + ph})
			y += ph
		}
	}

	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok { // 标准库解码产物均实现 SubImage，防御性分支
		return nil, false, fmt.Errorf("slice photo: image type %T lacks SubImage", img)
	}
	var chunks [][]byte
	for _, s := range spans {
		piece := sub.SubImage(image.Rect(b.Min.X, s.y0, b.Max.X, s.y1))
		chunk, err := encodeChunkJPEG(piece)
		if err != nil {
			return nil, false, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, truncated, nil
}

// encodeChunkJPEG 铺白底画布后 q85 重编码（JPEG 无 alpha，透明区直编会变黑），
// 单片超 maxPhotoBytes 视为整图不可发（极端纹理图，TODO 自适应降质 issue #16）
func encodeChunkJPEG(img image.Image) ([]byte, error) {
	b := img.Bounds()
	canvas := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(canvas, canvas.Bounds(), img, b.Min, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: sliceJPEGQuality}); err != nil {
		return nil, fmt.Errorf("slice photo: encode: %w", err)
	}
	if buf.Len() > maxPhotoBytes {
		return nil, fmt.Errorf("slice photo: chunk %d bytes exceeds %d", buf.Len(), maxPhotoBytes)
	}
	return buf.Bytes(), nil
}
