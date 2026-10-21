#!/bin/zsh
# Build NotiBridge and assemble an unsigned (ad-hoc signed) NotiBridge.app.
#
# Usage: scripts/package_app.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-0.1.0}"
DIST="dist"
APP="$DIST/NotiBridge.app"

echo "==> Building release binary (darwin/arm64, CGO for the menu bar)"
mkdir -p "$DIST"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
  go build -trimpath -ldflags "-s -w -X github.com/spdeepak/notibridge/internal/app.Version=$VERSION" \
  -o "$DIST/NotiBridge" ./cmd/notifyrelay

echo "==> Assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$DIST/NotiBridge" "$APP/Contents/MacOS/NotiBridge"
sed "s/<string>0.1.0<\/string>/<string>$VERSION<\/string>/" packaging/Info.plist > "$APP/Contents/Info.plist"

# No Dock icon is needed: Info.plist sets LSUIElement, so NotiBridge lives
# only in the menu bar (with a runtime-generated template icon).

echo "==> Ad-hoc code signing (no Developer ID; Gatekeeper will still warn on other Macs)"
codesign --force --sign - "$APP" 2>/dev/null || true
codesign --verify "$APP" 2>/dev/null && echo "    ad-hoc signature valid" || echo "    (codesign unavailable or verification skipped)"

echo "==> Done: $APP"
echo "    Launch: open '$APP'"
echo "    Quit from the menu bar icon, or: pkill -f NotiBridge.app"
