#!/usr/bin/env bash
# Build zen-gate for Linux and package it as a self-contained .AppImage
# (GTK3 + libayatana-appindicator bundled).
#
# Requirements:
#   - Go >= 1.26 (https://go.dev/dl)
#   - gcc, pkg-config
#   - dev packages: libgtk-3-dev, libayatana-appindicator3-dev
#   - linuxdeploy + appimagetool AppImages (auto-downloaded into build/tools)
#
# Usage:
#   tools/build-linux.sh [VERSION]      # e.g. tools/build-linux.sh 1.2.2
#   GOPROXY=https://goproxy.cn,direct tools/build-linux.sh   # mirror for CN
set -euo pipefail

VERSION="${1:-1.2.1-linux}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$ROOT/build"
APPDIR="$BUILD/AppDir"
TOOLS="$BUILD/tools"
NAME="zen-gate-linux"
APPIMAGE_OUT="$DIST/Zen_Gate-x86_64.AppImage"

# --- 1. compile -----------------------------------------------------------
mkdir -p "$DIST"
echo ">> building $NAME (version $VERSION)"
(cd "$ROOT" && go build -trimpath \
  -ldflags "-s -w \
    -X zen-gate/internal/gateway.Version=$VERSION \
    -X zen-gate/internal/update.Current=$VERSION" \
  -o "$DIST/$NAME" ./cmd/zen-gate)

# --- 2. AppDir skeleton -----------------------------------------------------
rm -rf "$APPDIR"
mkdir -p "$APPDIR/usr/bin" \
         "$APPDIR/usr/share/applications" \
         "$APPDIR/usr/share/icons/hicolor/256x256/apps"
cp "$DIST/$NAME" "$APPDIR/usr/bin/zen-gate"
cp "$ROOT/assets/icon-256.png" "$APPDIR/usr/share/icons/hicolor/256x256/apps/zen-gate.png"
cp "$ROOT/assets/icon-256.png" "$APPDIR/zen-gate.png"

cat > "$APPDIR/usr/share/applications/zen-gate.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=Zen Gate
Comment=本地免费模型网关
Exec=zen-gate
Icon=zen-gate
Terminal=false
Categories=Network;Utility;
StartupWMClass=zen-gate
X-GNOME-Autostart-enabled=false
EOF
cp "$APPDIR/usr/share/applications/zen-gate.desktop" "$APPDIR/zen-gate.desktop"

cat > "$APPDIR/AppRun" <<'EOF'
#!/bin/sh
SELF="$0"
HERE="$(dirname "$(readlink -f "$SELF")")"
export GSETTINGS_SCHEMA_DIR="$HERE/usr/share/glib-2.0/schemas"
export LD_LIBRARY_PATH="$HERE/usr/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec "$HERE/usr/bin/zen-gate" "$@"
EOF
chmod +x "$APPDIR/AppRun" "$APPDIR/usr/bin/zen-gate"

# --- 3. bundle dynamic libs with linuxdeploy ---------------------------------
mkdir -p "$TOOLS"
run_appimage() {
  local file="$1"; shift
  if "$file" "$@" 2>/dev/null; then
    return 0
  fi
  "$file" --appimage-extract-and-run "$@"
}
if [ ! -x "$TOOLS/linuxdeploy-x86_64.AppImage" ]; then
  echo ">> downloading linuxdeploy"
  curl -sL -o "$TOOLS/linuxdeploy-x86_64.AppImage" \
    https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-x86_64.AppImage
  chmod +x "$TOOLS/linuxdeploy-x86_64.AppImage"
fi
echo ">> bundling libraries with linuxdeploy"
run_appimage "$TOOLS/linuxdeploy-x86_64.AppImage" \
  --appdir "$APPDIR" \
  --executable "$APPDIR/usr/bin/zen-gate" \
  --desktop-file "$APPDIR/usr/share/applications/zen-gate.desktop" \
  --icon-file "$APPDIR/zen-gate.png"

# --- 4. GTK runtime extras (schemas + pixbuf loaders) -------------------------
mkdir -p "$APPDIR/usr/share/glib-2.0/schemas" \
         "$APPDIR/usr/lib/gdk-pixbuf-2.0/2.10.0/loaders"
if [ -f /usr/share/glib-2.0/schemas/gschemas.compiled ]; then
  cp /usr/share/glib-2.0/schemas/gschemas.compiled \
     "$APPDIR/usr/share/glib-2.0/schemas/"
fi
if ls /usr/lib/x86_64-linux-gnu/gdk-pixbuf-2.0/*/loaders/*.so >/dev/null 2>&1; then
  cp /usr/lib/x86_64-linux-gnu/gdk-pixbuf-2.0/*/loaders/*.so \
     "$APPDIR/usr/lib/gdk-pixbuf-2.0/2.10.0/loaders/"
fi

# --- 5. squash into AppImage with appimagetool ---------------------------------
if [ ! -x "$TOOLS/appimagetool-x86_64.AppImage" ]; then
  echo ">> downloading appimagetool"
  curl -sL -o "$TOOLS/appimagetool-x86_64.AppImage" \
    https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage
  chmod +x "$TOOLS/appimagetool-x86_64.AppImage"
fi
echo ">> generating $APPIMAGE_OUT"
run_appimage "$TOOLS/appimagetool-x86_64.AppImage" "$APPDIR" "$APPIMAGE_OUT"

echo ">> done: $APPIMAGE_OUT"
