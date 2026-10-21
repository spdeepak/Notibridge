// Command make_icons derives NotiBridge's macOS icon assets from the project
// icon in the repository root.
//
// The source file is named image.png but is inspected by content, not by
// extension: image.Decode picks the right decoder, so a JPEG payload under a
// .png name works. The source file is never modified.
//
// Usage:
//
//	go run ./scripts/make_icons [source] [output-dir]
//
// Defaults: source "image.png", output-dir "assets/icons".
//
// It writes an .iconset (the intermediate format iconutil consumes) and the
// final NotiBridge.icns. Both use only the Go standard library, so the
// release build needs no extra tooling.
package main

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // register the JPEG decoder (the source file is JPEG data)
	"image/png"    // PNG encoder and decoder
	"math"
	"os"
	"os/exec"
	"path/filepath"
)

// iconSizes are the pixel sizes iconutil expects inside an .iconset. Each
// entry is the logical macOS icon size; "@2x" entries are the Retina
// variants.
var iconSizes = []struct {
	name string
	size int
}{
	{"icon_16x16.png", 16},
	{"icon_16x16@2x.png", 32},
	{"icon_32x32.png", 32},
	{"icon_32x32@2x.png", 64},
	{"icon_128x128.png", 128},
	{"icon_128x128@2x.png", 256},
	{"icon_256x256.png", 256},
	{"icon_256x256@2x.png", 512},
	{"icon_512x512.png", 512},
	{"icon_512x512@2x.png", 1024},
}

// cornerRadiusRatio is the corner radius of the macOS app icon shape as a
// fraction of the icon edge. 0.2237 is the widely used macOS value; it makes
// the icon render correctly in the Dock and Finder, which do not mask
// .icns files themselves.
const cornerRadiusRatio = 0.2237

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "make_icons:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	source := "image.png"
	outDir := filepath.Join("assets", "icons")
	if len(args) > 0 {
		source = args[0]
	}
	if len(args) > 1 {
		outDir = args[1]
	}

	src, err := loadSource(source)
	if err != nil {
		return err
	}
	bounds := src.Bounds()
	fmt.Printf("source %s: %dx%d\n", source, bounds.Dx(), bounds.Dy())

	iconset := filepath.Join(outDir, "NotiBridge.iconset")
	if err := os.RemoveAll(iconset); err != nil {
		return fmt.Errorf("clear iconset: %w", err)
	}
	if err := os.MkdirAll(iconset, 0o755); err != nil {
		return fmt.Errorf("create iconset dir: %w", err)
	}

	for _, s := range iconSizes {
		img := renderIcon(src, s.size)
		path := filepath.Join(iconset, s.name)
		if err := writePNG(path, img); err != nil {
			return err
		}
	}

	icns := filepath.Join(outDir, "NotiBridge.icns")
	if err := buildICNS(iconset, icns); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", icns, fileSize(icns))
	return nil
}

// loadSource decodes the source image, choosing the decoder by content so
// the .png extension is not authoritative.
func loadSource(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open source image: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode source image %s: %w", path, err)
	}
	return img, nil
}

// renderIcon resizes the source to size x size and applies the rounded
// macOS icon shape, producing an image with transparent corners.
func renderIcon(src image.Image, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	resampleArea(src, dst)
	applyRoundedMask(dst, float64(size)*cornerRadiusRatio)
	return dst
}

// resampleArea fills dst with a box-filtered (area average) downscale of src.
// It handles both up- and down-scaling; for upscaling it degenerates to
// nearest-neighbour sampling of the source region.
func resampleArea(src image.Image, dst *image.RGBA) {
	sb, db := src.Bounds(), dst.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return
	}
	for y := 0; y < db.Dy(); y++ {
		y0 := sb.Min.Y + y*sh/db.Dy()
		y1 := sb.Min.Y + (y+1)*sh/db.Dy()
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < db.Dx(); x++ {
			x0 := sb.Min.X + x*sw/db.Dx()
			x1 := sb.Min.X + (x+1)*sw/db.Dx()
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(b / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
}

// applyRoundedMask sets the alpha of every pixel outside the rounded
// rectangle to zero, with a one-pixel antialiased edge.
func applyRoundedMask(dst *image.RGBA, radius float64) {
	b := dst.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	cx, cy := w/2, h/2
	halfW, halfH := w/2, h/2
	if radius > halfW {
		radius = halfW
	}
	if radius > halfH {
		radius = halfH
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			dx := abs(px-cx) - (halfW - radius)
			dy := abs(py-cy) - (halfH - radius)
			if dx < 0 {
				dx = 0
			}
			if dy < 0 {
				dy = 0
			}
			dist := math.Sqrt(dx*dx + dy*dy)
			// dist < radius is inside; fade out over the last pixel.
			a := uint8(0)
			if dist <= radius-0.5 {
				a = 255
			} else if dist < radius+0.5 {
				frac := (radius + 0.5 - dist)
				a = uint8(frac * 255)
			}
			c := dst.RGBAAt(x, y)
			c.A = uint8(uint32(c.A) * uint32(a) / 255)
			dst.SetRGBA(x, y, c)
		}
	}
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}

// buildICNS converts the iconset into a .icns file using macOS's iconutil.
func buildICNS(iconset, out string) error {
	if _, err := exec.LookPath("iconutil"); err != nil {
		return fmt.Errorf("iconutil not found (this step requires macOS): %w", err)
	}
	if err := os.RemoveAll(out); err != nil {
		return fmt.Errorf("remove old icns: %w", err)
	}
	cmd := exec.Command("iconutil", "-c", "icns", iconset, "-o", out)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iconutil failed: %w: %s", err, out)
	}
	return nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
