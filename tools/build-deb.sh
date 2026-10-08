#!/usr/bin/env bash
# Build zen-gate for Linux and package it as a Debian .deb
# (system libs are used; Depends: libgtk-3-0, libayatana-appindicator3-1).
#
# Requirements:
#   - Go >= 1.26, gcc, pkg-config
#   - dpkg-deb (Debian/Ubuntu/Deepin etc.)
#
# Usage:
#   tools/build-deb.sh [VERSION]      # e.g. tools/build-deb.sh 1.2.2
set -euo pipefail

VERSION="${1:-1.2.1-linux}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$ROOT/build"
PKGDIR="$BUILD/deb-root/zen-gate_${VERSION}_amd64"
OUT="$DIST/zen-gate_${VERSION}_amd64.deb"

echo ">> building binary"
mkdir -p "$DIST"
(cd "$ROOT" && go build -trimpath \
  -ldflags "-s -w \
    -X zen-gate/internal/gateway.Version=$VERSION \
    -X zen-gate/internal/update.Current=$VERSION" \
  -o "$DIST/zen-gate-linux" ./cmd/zen-gate)

echo ">> assembling package tree"
rm -rf "$(dirname "$PKGDIR")"
mkdir -p "$PKGDIR/DEBIAN" \
         "$PKGDIR/usr/bin" \
         "$PKGDIR/usr/share/applications" \
         "$PKGDIR/usr/share/icons/hicolor/256x256/apps" \
         "$PKGDIR/usr/share/doc/zen-gate" \
         "$PKGDIR/usr/share/metainfo"
cp "$DIST/zen-gate-linux" "$PKGDIR/usr/bin/zen-gate"
cp "$ROOT/assets/icon-256.png" "$PKGDIR/usr/share/icons/hicolor/256x256/apps/zen-gate.png"

# --- desktop entry ----------------------------------------------------------
cat > "$PKGDIR/usr/share/applications/zen-gate.desktop" <<'EOF'
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

# --- control -----------------------------------------------------------------
SIZE_KB=$(( $(stat -c%s "$DIST/zen-gate-linux") / 1024 + 200 ))
cat > "$PKGDIR/DEBIAN/control" <<EOF
Package: zen-gate
Version: $VERSION
Architecture: amd64
Maintainer: Zen Gate Linux Port <zen-gate@localhost>
Installed-Size: $SIZE_KB
Depends: libc6 (>= 2.34), libgtk-3-0 (>= 3.24), libayatana-appindicator3-1 (>= 0.5)
Section: net
Priority: optional
Homepage: https://github.com/LAGcomcom/zen-gate
Description: Local free-model gateway with a tray icon
 Zen Gate wraps the OpenCode Zen free model lane into standard
 OpenAI/Anthropic-compatible APIs on 127.0.0.1, auto-detects and injects
 configuration into installed AI agents (OpenCode, Codex, Claude Code,
 Aider, Qwen Code, etc.), and stays resident in the system tray. The
 management dashboard opens in the default browser.
 .
 Linux features: XDG autostart, libnotify notifications, system proxy
 via environment variables, pgrep-based agent detection.
EOF

# --- postinst ----------------------------------------------------------------
cat > "$PKGDIR/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database /usr/share/applications >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -f -t /usr/share/icons/hicolor >/dev/null 2>&1 || true
fi
exit 0
EOF
chmod 755 "$PKGDIR/DEBIAN/postinst"

# --- docs --------------------------------------------------------------------
cat > "$PKGDIR/usr/share/doc/zen-gate/copyright" <<'EOF'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: zen-gate
Upstream-Contact: https://github.com/LAGcomcom/zen-gate
Source: https://github.com/LAGcomcom/zen-gate

Files: *
Copyright: 2024-2026 LAGcomcom
License: MIT
 Permission is hereby granted, free of charge, to any person obtaining a copy
 of this software and associated documentation files (the "Software"), to deal
 in the Software without restriction, including without limitation the rights
 to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 copies of the Software, and to permit persons to whom the Software is
 furnished to do so, subject to the following conditions.
 .
 The above copyright notice and this permission notice shall be included in
 all copies or substantial portions of the Software.
 .
 THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 SOFTWARE.
EOF

cat > "$PKGDIR/usr/share/doc/zen-gate/changelog" <<EOF
zen-gate ($VERSION) unstable; urgency=medium

  * Linux 适配：托盘常驻、浏览器打开管理页、XDG 开机自启、
    libnotify 通知、系统代理环境变量、pgrep 进程检测。
  * 打包为自包含 AppImage 与 Debian .deb。

 -- Zen Gate Linux Port <zen-gate@localhost>  $(date -R)
EOF

cat > "$PKGDIR/usr/share/metainfo/zen-gate.metainfo.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<component type="desktop-application">
  <id>com.lagcomcom.zen-gate</id>
  <name>Zen Gate</name>
  <summary>本地免费模型网关（托盘常驻）</summary>
  <metadata_license>MIT</metadata_license>
  <project_license>MIT</project_license>
  <description>
    <p>把 OpenCode Zen 免费模型封装成标准 OpenAI / Anthropic 兼容接口，自动注入已安装的 AI Agent 配置，托盘常驻。</p>
  </description>
  <url type="homepage">https://github.com/LAGcomcom/zen-gate</url>
  <launchable type="desktop-id">zen-gate.desktop</launchable>
  <provides>
    <binary>zen-gate</binary>
  </provides>
</component>
EOF

# --- build -------------------------------------------------------------------
echo ">> building $OUT"
dpkg-deb --build --root-owner-group "$PKGDIR" "$OUT"
echo ">> done: $OUT"
