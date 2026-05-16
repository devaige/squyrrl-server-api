// 缩略图生成。
//
// 输入：图片字节流（jpeg / png / gif / webp）。
// 输出：JPEG 字节，长边不超过 MaxEdge，quality 80。
//
// 设计取舍：
//   - 不输出 webp：webp encoding 在纯 Go 生态里成熟度差（需 CGO 或第三方 native impl），
//     运维成本远超 JPEG 节省的几 KB。JPEG quality 80 在 256×256 量级下视觉无损。
//   - 不区分明文 / 混淆字节：服务端不持有客户端混淆密钥，直接 image.Decode；
//     混淆字节会在头几个字节就 decode 失败，开销 ~µs。失败 → ErrUnsupported → 调用方
//     把 thumbnail_key 留 NULL，客户端走「无缩略图」分支。
//   - 不做 EXIF rotation：Phase 1.5 简化。后续可换 disintegration/imaging 等库一行替换。
package thumb

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"

	// 注册解码器
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"golang.org/x/image/draw"
)

var ErrUnsupported = errors.New("thumb: unsupported or unreadable bytes")

// MaxEdge 缩略图长边（短边按比例缩放）
const MaxEdge = 256

// JPEG 输出质量
const jpegQuality = 80

// Generate 解码并生成缩略图字节。成功返回 JPEG bytes（mime image/jpeg）。
func Generate(src io.Reader) ([]byte, error) {
	img, _, err := image.Decode(src)
	if err != nil {
		return nil, ErrUnsupported
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, ErrUnsupported
	}

	dw, dh := scaledDimensions(w, h, MaxEdge)
	if dw == w && dh == h {
		return encodeJPEG(img)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	return encodeJPEG(dst)
}

func scaledDimensions(w, h, maxEdge int) (int, int) {
	if w <= maxEdge && h <= maxEdge {
		return w, h
	}
	if w >= h {
		return maxEdge, max(1, (h*maxEdge+w/2)/w)
	}
	return max(1, (w*maxEdge+h/2)/h), maxEdge
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
