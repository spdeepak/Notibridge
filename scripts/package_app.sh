#!/bin/zsh
# Build NotiBridge and assemble an unsigned (ad-hoc signed) NotiBridge.app,
# then zip it for distribution.
#
# Usage: scripts/package_app.sh [version] [arch]
#   version: release version, e.g. 0.2.0 (default: the Makefile's VERSION)
#   arch:    arm64 | amd64 | universal (default: arm64)
#
# Output:
#   dist/NotiBridge.app                    the bundle (for local installation)
#   dist/NotiBridge_<version>_<arch>.zip   the release archive
#
# No Apple Developer account is required: the bundle is signed ad-hoc, so
# Gatekeeper still shows an "unverified developer" warning on other Macs.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(make -s version)}"
ARCH="${2:-arm64}"
DIST="dist"
APP="$DIST/NotiBridge.app"
ICNS="assets/icons/NotiBridge.icns"
MODULE="github.com/spdeepak/notibridge"

case "$ARCH" in
  arm64|amd64|universal) ;;
  *) echo "unknown architecture: $ARCH (expected arm64, amd64 or universal)" >&2; exit 2 ;;
esac

# The Ed25519 public key that verifies release checksum files. Empty means
# checksum-only verification (see docs/UPDATING.md).
PUBLIC_KEY="${NOTIBRIDGE_UPDATE_PUBLIC_KEY:-}"

LDFLAGS="-s -w -X ${MODULE}/internal/app.Version=${VERSION}"
if [[ -n "$PUBLIC_KEY" ]]; then
  LDFLAGS="${LDFLAGS} -X ${MODULE}/internal/update.PublicKey=${PUBLIC_KEY}"
fi

echo "==> Icon"
if [[ ! -f "$ICNS" ]]; then
  echo "    $ICNS missing — generating it from image.png"
  # Build the generator for the host, not for the target architecture.
  GOARCH="$(go env GOHOSTARCH)" go run ./scripts/make_icons
fi
[[ -f "$ICNS" ]] || { echo "could not generate $ICNS" >&2; exit 1; }

echo "==> Building release binary (darwin/$ARCH, CGO for the menu bar)"
mkdir -p "$DIST"
build() { # build <goarch> <output>
  # go build refuses to overwrite an existing file that is not a Go archive.
  rm -f "$2"
  GOOS=darwin GOARCH="$1" CGO_ENABLED=1 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$2" ./cmd/notifyrelay
}

case "$ARCH" in
  arm64|amd64)
    build "$ARCH" "$DIST/NotiBridge"
    ;;
  universal)
    build arm64 "$DIST/NotiBridge.arm64"
    build amd64 "$DIST/NotiBridge.amd64"
    rm -f "$DIST/NotiBridge"
    lipo -create -output "$DIST/NotiBridge" "$DIST/NotiBridge.arm64" "$DIST/NotiBridge.amd64"
    rm -f "$DIST/NotiBridge.arm64" "$DIST/NotiBridge.amd64"
    ;;
esac
file "$DIST/NotiBridge"

echo "==> Assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$DIST/NotiBridge" "$APP/Contents/MacOS/NotiBridge"
cp "$ICNS" "$APP/Contents/Resources/NotiBridge.icns"
cp packaging/Info.plist "$APP/Contents/Info.plist"

# PlistBuddy edits the plist structurally; sed on XML is fragile.
PLIST="$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$PLIST"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $VERSION" "$PLIST"

# No Dock icon is needed: Info.plist sets LSUIElement, so NotiBridge lives
# only in the menu bar (with a runtime-generated template icon). The .icns is
# the Finder/bundle icon.

echo "==> Ad-hoc code signing (no Developer ID; Gatekeeper will still warn on other Macs)"
codesign --force --sign - "$APP"
codesign --verify --deep --strict "$APP" && echo "    ad-hoc signature valid"

ZIP="$DIST/NotiBridge_${VERSION}_${ARCH}.zip"
rm -f "$ZIP"
echo "==> Archiving $ZIP"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$ZIP"

echo "==> Done"
echo "    Bundle:   $APP"
echo "    Archive:  $ZIP"
echo "    Checksum: shasum -a 256 \"$ZIP\""
echo "    Launch:   open '$APP'"
echo "    Quit from the menu bar icon, or: pkill -f NotiBridge.app"
