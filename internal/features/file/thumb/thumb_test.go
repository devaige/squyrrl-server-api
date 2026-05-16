package thumb

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestGenerate_PNGShrink(t *testing.T) {
	src := makePNG(t, 1024, 512)
	out, err := Generate(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode out: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != MaxEdge || b.Dy() != MaxEdge/2 {
		t.Fatalf("unexpected size: %dx%d (want %dx%d)", b.Dx(), b.Dy(), MaxEdge, MaxEdge/2)
	}
}

func TestGenerate_SmallPassThrough(t *testing.T) {
	src := makePNG(t, 50, 30)
	out, err := Generate(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 50 || b.Dy() != 30 {
		t.Fatalf("small image was resized: got %dx%d", b.Dx(), b.Dy())
	}
}

func TestGenerate_GarbageBytesUnsupported(t *testing.T) {
	if _, err := Generate(strings.NewReader("not an image")); err != ErrUnsupported {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode src: %v", err)
	}
	return buf.Bytes()
}
