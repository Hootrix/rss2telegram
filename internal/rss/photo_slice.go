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

	// 解码位图上限 40MP = 40_000_000：10MB JPEG 可声称任意尺寸，解码前按头部拦截，
	// 防超大位图打爆常驻内存。16-bit PNG 解码 NRGBA64 8B/px，100MP 即 800MB，
	// 双 feed 并发可破 1.5GB（CR）；40MP 覆盖实测案例 1080×33634=36.3MP，最坏单图 320MB，
	// 配合 sliceSem 串行化把峰值钳到单图水平
	maxPhotoPixels = 40_000_000

	// readableChunkHeight [issue #16 CR] Telegram sendPhoto 服务端压缩至长边 ~1280（Bot API 无 HD 档，
	// 查证 2026-10）；片高 ≤1280 保证片的长边不被二次缩放、文字零损失
	readableChunkHeight = 1280

	// 重编码质量：实测长截图 q85 单片 ≤1MB，远低于 10MB 上限
	sliceJPEGQuality = 85
)

// chunkBounds [issue #16 CR] 给定宽度返回合法片高上下限（同时满足 w+h、比例、可读压缩三项）；
// ok=false 表示该宽度不存在合法片高（过宽图不可切片）。
// sliceImage 与 fitsReadableAlbum 预检共用，防两处数学漂移
func chunkBounds(w int) (chunkMax, chunkMin int, ok bool) {
	chunkMax = min(min(maxPhotoSideSum-w, maxPhotoRatio*w), readableChunkHeight)
	chunkMin = (w + maxPhotoRatio - 1) / maxPhotoRatio
	return chunkMax, chunkMin, chunkMax > 0 && chunkMax >= chunkMin
}

// fitsReadableAlbum [issue #16 CR] 用头部尺寸预判能否切成 ≤maxAlbumPhotos 张零压缩可读片
// （不做解码，photoForItem 以此决定相册 vs 整图文件，避免切完再丢弃）
func fitsReadableAlbum(w, h int) bool {
	chunkMax, chunkMin, ok := chunkBounds(w)
	if !ok {
		return false
	}
	if w*h > maxPhotoPixels {
		return false
	}
	n := (h + chunkMax - 1) / chunkMax
	if n > maxAlbumPhotos {
		return false
	}
	return h/n >= chunkMin // 均匀切分后最低片仍须合法（如 9500×600）
}

// sliceSem 切片全程串行：解码位图峰值大（40MP 16bit PNG ≈ 320MB），
// ProcessFeeds 双 feed 并发叠加会击穿内存（CR），串行化把峰值钳到单图水平
var sliceSem = make(chan struct{}, 1)

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
// 解码失败（webp/损坏/未注册格式）返回 error，由调用方回退下一候选。
// 全程持 sliceSem 串行（DecodeConfig+Decode+sliceImage）：解码位图峰值内存是
// 串行化的原因，见 sliceSem 注释
func slicePhoto(data []byte) ([][]byte, bool, error) {
	sliceSem <- struct{}{}
	defer func() { <-sliceSem }()
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
// 片高合法区间 [⌈w/20⌉, min(10000−w, 20w, readableChunkHeight)]（同时满足 w+h、
// 比例与可读压缩三项，由 chunkBounds 统一给出，issue #16 CR）；
// 切片触发（调用方经 validatePhoto 判定超限）时 n ≥ 2；限内图直接调用则自然产出单片，
// 由调用方决定单图/相册路径（TestSliceImageTransparentPNG 即单片例证）。
// 超过 maxAlbumPhotos 片时截尾（truncated=true）。CR 裁决：handler 预检
// （fitsReadableAlbum）已把切不出可读相册的图分派为整图文件，此截尾路径
// 仅作防御保留，正常流不可达
func sliceImage(img image.Image) ([][]byte, bool, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// 入口像素守卫：slicePhoto 已预检过，这里兜底直接传位图的调用方
	if err := checkPixelBudget(w, h); err != nil {
		return nil, false, err
	}
	chunkMax, chunkMin, ok := chunkBounds(w)
	if !ok {
		return nil, false, fmt.Errorf("slice photo: width %d has no valid chunk height (range [%d,%d])", w, chunkMin, chunkMax)
	}
	// [issue #16 CR] 旧内联计算（readableChunkHeight 引入前）保留备查：
	// chunkMax := min(maxPhotoSideSum-w, maxPhotoRatio*w)
	// chunkMin := (w + maxPhotoRatio - 1) / maxPhotoRatio
	// if chunkMax <= 0 || chunkMax < chunkMin {
	// 	return nil, false, fmt.Errorf("slice photo: width %d has no valid chunk height (range [%d,%d])", w, chunkMin, chunkMax)
	// }
	// [外部 CR] 短边守卫：切片是纵向切，片宽=图宽，w < 50 的图切出来仍低于 Telegram
	// 短边下限，不可修复；validatePhoto 已在 fetch 路径拦截，此处兜底直接传位图的调用方
	if w < minPhotoSide {
		return nil, false, fmt.Errorf("slice photo: width %d below minimum side %d", w, minPhotoSide)
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
// 单片超 maxPhotoBytes 视为整图不可发（极端纹理图，TODO 自适应降质 issue #16）。
// 上限检查暂无直接单测（构造 >10MB 编码成本过高），如后续引入自适应降质需一并补测
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
