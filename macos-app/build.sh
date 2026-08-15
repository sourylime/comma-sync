#!/usr/bin/env bash
#
# Build "Comma Sync.app" — the SwiftUI macOS UI wired to the bundled Go core.
# This is THE macOS build of Comma Sync. (The legacy comma-sync.sh engine is retired
# but kept in the repo for anyone who still wants it.) Requires macOS + Xcode CLT + Go.
#
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DIR/.." && pwd)"
CORE="$ROOT/core"
# APP_NAME lets a test build install alongside the real app (e.g. "Comma Sync Beta").
# It keeps the same bundle identifier on purpose, so settings/folders carry over.
APP_NAME="${APP_NAME:-Comma Sync}"
APP="$ROOT/${APP_NAME}.app"
APP_VERSION="${APP_VERSION:-1.3.0}"

echo "==> Building universal Go core (arm64 + x86_64)"
( cd "$CORE"
  GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$DIR/core-arm64" .
  GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$DIR/core-amd64" . )
lipo -create -output "$DIR/comma-sync" "$DIR/core-arm64" "$DIR/core-amd64"
rm -f "$DIR/core-arm64" "$DIR/core-amd64"

echo "==> Compiling App.swift"
swiftc -parse-as-library -O "$DIR/App.swift" -o "$DIR/CommaSync" \
  -framework SwiftUI -framework AppKit

echo "==> Building app icon"
rm -rf "$DIR/icon.iconset" "$DIR/icon.icns"; mkdir "$DIR/icon.iconset"
for sz in 16 32 128 256 512; do
  sips -z $sz $sz "$DIR/icon_1024.png" --out "$DIR/icon.iconset/icon_${sz}x${sz}.png" >/dev/null
  d=$((sz*2)); sips -z $d $d "$DIR/icon_1024.png" --out "$DIR/icon.iconset/icon_${sz}x${sz}@2x.png" >/dev/null
done
iconutil -c icns "$DIR/icon.iconset" -o "$DIR/icon.icns"

echo "==> Assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
mv "$DIR/CommaSync" "$APP/Contents/MacOS/CommaSync"
mv "$DIR/comma-sync" "$APP/Contents/Resources/comma-sync"
chmod +x "$APP/Contents/Resources/comma-sync"
cp "$DIR/icon.icns" "$APP/Contents/Resources/icon.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>${APP_NAME}</string>
  <key>CFBundleDisplayName</key><string>${APP_NAME}</string>
  <key>CFBundleExecutable</key><string>CommaSync</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundleIdentifier</key><string>com.example.commasync</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${APP_VERSION}</string>
  <key>CFBundleVersion</key><string>${APP_VERSION}</string>
  <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

# Ad-hoc sign (covers the bundled core too) so Gatekeeper allows it to launch.
codesign --force --deep -s - "$APP" 2>/dev/null || true

echo "==> Done. Built: $APP"
echo "    Needs ffmpeg/ffprobe on PATH (brew install ffmpeg)."
