//go:build linux

// Package window's Linux host: there is no dedicated native webview in this
// port, so the dashboard is served by the local gateway and opened in the
// user's default browser, while the app stays resident in the system tray
// (GTK/appindicator via getlantern/systray).
package window

import (
	"os/exec"

	"zen-gate/internal/tray"
)

// Linux has no HWND. Callers only ever compare the handle against zero to ask
// "does a window exist yet?", so a non-zero sentinel keeps that contract
// intact (matching the macOS port).
const hwndToken = uintptr(1)

// pending holds the Run call's options, read back by the window getters.
var pending Options

// Run hosts the tray event loop (blocking) and opens the dashboard in the
// default browser once the tray is ready. The gateway itself is started by
// the caller before Run, so the URL is already live by the time it opens.
func Run(o Options) {
	pending = o

	opts := tray.Options{OnReady: func() {
		if o.OnReady != nil {
			o.OnReady(hwndToken)
		}
		// First-launch UX mirrors the native ports: the dashboard appears
		// automatically instead of hiding behind a tray menu click.
		if o.URL != "" {
			tray.Open(o.URL)
		}
	}}
	if o.Tray != nil {
		opts.DashboardURL = o.Tray.DashboardURL
		opts.OnQuit = o.Tray.OnQuit
		opts.OnReprobe = o.Tray.OnReprobe
		opts.OnShow = o.Tray.OnShow
	}
	tray.Register(opts)
	tray.Loop()
}

// ShowWindowWin has no native window to show on Linux; it re-opens the
// dashboard in the default browser (tray "打开管理页" / close-to-tray
// restore).
func ShowWindowWin(hwnd uintptr) {
	if pending.URL != "" {
		_ = exec.Command("xdg-open", pending.URL).Start()
	}
}

// HideWindow is a no-op: there is no native window to hide.
func HideWindow(hwnd uintptr) {}

// IsVisible reports whether the main window is currently shown. Linux keeps
// no persistent window, so this always reports false.
func IsVisible(hwnd uintptr) bool { return false }

// MaximizeWindow is a no-op on Linux.
func MaximizeWindow(hwnd uintptr) {}

func minimizeWindow(hwnd uintptr) {}

// IsMaximized always reports false: the browser tab is not tracked.
func IsMaximized(hwnd uintptr) bool { return false }

// IsMinimized always reports false so saveWindowState keeps persisting the
// last known (default) bounds instead of skipping the save.
func IsMinimized(hwnd uintptr) bool { return false }

// GetBounds returns the configured default rectangle, so saveWindowState
// persists a sane window state across launches even without a native window.
func GetBounds(hwnd uintptr) (left, top, right, bottom int) {
	w, h := pending.Width, pending.Height
	if w <= 0 {
		w = 1160
	}
	if h <= 0 {
		h = 820
	}
	return 0, 0, w, h
}
