package menubar

import _ "embed"

// iconPNG is generated from the project artwork by scripts/make_icons
// (`make icons`). It is a black-with-alpha template image, so macOS renders it
// black on a light menu bar and white on a dark one. systray sizes every menu
// bar image to 16 points, so 32 pixels is the Retina rendering.
//
//go:embed icon.png
var iconPNG []byte

// menuBarIcon returns the template image bytes handed to systray.
func menuBarIcon() []byte {
	return iconPNG
}
