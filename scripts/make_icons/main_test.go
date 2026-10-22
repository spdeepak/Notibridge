package main

import (
	"image"
	"image/color"
	"testing"
)

func TestCropToContentTrimsTheBackdrop(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, color.RGBA{235, 235, 235, 255})
		}
	}
	for y := 20; y < 80; y++ {
		for x := 20; x < 80; x++ {
			src.Set(x, y, color.RGBA{90, 90, 90, 255})
		}
	}
	got := cropToContent(src).Bounds()
	want := image.Rect(0, 0, 60, 60)
	if got != want {
		t.Fatalf("crop = %v, want %v", got, want)
	}
	if c := cropToContent(src).At(0, 0); c != (color.RGBA{90, 90, 90, 255}) {
		t.Errorf("top-left pixel = %v, want the tile colour", c)
	}
}

func TestCropToContentKeepsFullBleedImages(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, color.RGBA{60, 60, 60, 255})
		}
	}
	if got := cropToContent(src).Bounds(); got != src.Bounds() {
		t.Fatalf("crop = %v, want the full %v", got, src.Bounds())
	}
}
