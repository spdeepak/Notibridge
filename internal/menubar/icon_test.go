package menubar

import (
	"bytes"
	"image/png"
	"testing"
)

func TestMenuBarIconIsDecodablePNG(t *testing.T) {
	data := menuBarIcon()
	if len(data) == 0 {
		t.Fatal("empty icon")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode icon png: %v", err)
	}
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
		t.Errorf("icon size = %v", img.Bounds())
	}
	// A template image must contain both inked and transparent pixels (a
	// glyph, not a solid square) and no colour: macOS tints the alpha channel.
	var inked, clear int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if r != 0 || g != 0 || bl != 0 {
				t.Fatalf("pixel (%d,%d) is not black: %d,%d,%d", x, y, r>>8, g>>8, bl>>8)
			}
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
