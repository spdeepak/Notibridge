package menubar

import (
	"bytes"
	"image/png"
	"testing"
)

func TestGenerateIconIsDecodablePNG(t *testing.T) {
	data := generateIcon()
	if len(data) == 0 {
		t.Fatal("empty icon")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode icon png: %v", err)
	}
	if img.Bounds().Dx() != iconSize || img.Bounds().Dy() != iconSize {
		t.Errorf("icon size = %v", img.Bounds())
	}
	// The icon must contain both inked and transparent pixels (a glyph, not
	// a solid square).
	var inked, clear int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a > 128 {
				inked++
			} else {
				clear++
			}
		}
	}
	if inked == 0 || clear == 0 {
		t.Errorf("icon looks blank or solid: inked=%d clear=%d", inked, clear)
	}
}
