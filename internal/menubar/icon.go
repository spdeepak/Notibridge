package menubar

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// iconSize is the generated menu bar icon size in points. macOS scales it for
// Retina displays.
const iconSize = 22

// generateIcon renders a simple bell glyph as a black-with-alpha PNG suitable
// for use as a macOS menu bar template image. Generating the icon at runtime
// keeps the repository free of binary assets.
func generateIcon() []byte {
	const ss = 4 // supersampling factor for smoother edges
	big := image.NewGray(image.Rect(0, 0, iconSize*ss, iconSize*ss))

	fill := func(x0, y0, x1, y1 float64) {
		for y := y0 * ss; y < y1*ss; y++ {
			for x := x0 * ss; x < x1*ss; x++ {
				big.SetGray(int(x), int(y), color.Gray{Y: 0})
			}
		}
	}
	fillEllipse := func(cx, cy, rx, ry float64) {
		for y := 0; y < iconSize*ss; y++ {
			for x := 0; x < iconSize*ss; x++ {
				dx := (float64(x)+0.5)/ss - cx
				dy := (float64(y)+0.5)/ss - cy
				if (dx*dx)/(rx*rx)+(dy*dy)/(ry*ry) <= 1 {
					big.SetGray(x, y, color.Gray{Y: 0})
				}
			}
		}
	}

	// Bell dome (upper part of a large ellipse).
	fillEllipse(11, 13, 7.5, 9)
	// Clear everything below the dome's shoulder; the rim and clapper are
	// drawn afterwards.
	for y := 16 * ss; y < iconSize*ss; y++ {
		for x := 0; x < iconSize*ss; x++ {
			big.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	// Bell rim.
	fill(3.5, 15.5, 18.5, 17.5)
	// Clapper.
	fillEllipse(11, 19.5, 2.1, 2.1)
	// Small handle on top.
	fill(9.8, 2.5, 12.2, 4.5)

	// Downsample to the final size with alpha.
	out := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			var sum float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					if big.GrayAt(x*ss+sx, y*ss+sy).Y == 0 {
						sum++
					}
				}
			}
			a := sum / float64(ss*ss)
			out.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: uint8(a * 255)})
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, out)
	return buf.Bytes()
}
