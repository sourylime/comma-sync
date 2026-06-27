#!/usr/bin/env bash
#
# Build "Comma Sync Go.app" — the SwiftUI macOS UI wired to the bundled Go core
# (instead of comma-sync.sh). Installs alongside the script-based "Comma Sync.app".
# Requires macOS + Xcode CLT + Go.
#
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DIR/.." && pwd)"
CORE="$ROOT/core"
APP="$ROOT/Comma Sync Go.app"
APP_VERSION="${APP_VERSION:-1.0.3}"

echo "==> Building universal Go core (arm64 + x86_64)"
( cd "$CORE"
  GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$DIR/core-arm64" .
  GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$DIR/core-amd64" . )
lipo -create -output "$DIR/comma-sync" "$DIR/core-arm64" "$DIR/core-amd64"
rm -f "$DIR/core-arm64" "$DIR/core-amd64"

echo "==> Compiling App.swift"
swiftc -parse-as-library -O "$DIR/App.swift" -o "$DIR/CommaSyncGo" \
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
mv "$DIR/CommaSyncGo" "$APP/Contents/MacOS/CommaSyncGo"
mv "$DIR/comma-sync" "$APP/Contents/Resources/comma-sync"
chmod +x "$APP/Contents/Resources/comma-sync"
cp "$DIR/icon.icns" "$APP/Contents/Resources/icon.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Comma Sync Go</string>
  <key>CFBundleDisplayName</key><string>Comma Sync Go</string>
  <key>CFBundleExecutable</key><string>CommaSyncGo</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundleIdentifier</key><string>com.example.commasyncgo</string>
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
