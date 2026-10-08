//go:build darwin

package window

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include "cocoa/zgwindow.h"
*/
import "C"

import (
	"unsafe"

	"zen-gate/internal/tray"
	_ "zen-gate/internal/window/cocoa"
)

// macOS has no HWND. Callers only ever compare the handle against zero to ask
// "does a window exist yet?", so the NSWindow's identity is irrelevant and a
// non-zero sentinel keeps that contract intact.
const hwndToken = uintptr(1)

var onCloseButton func() bool

// pending is the Run call's options, waiting for the main thread to pick up.
var pending Options

// zgGoCreate is invoked by zg_dispatch_create, on the main thread.
//
//export zgGoCreate
func zgGoCreate() { create(pending) }

// zgGoClose is invoked by AppKit for both the dashboard's × and ⌘W. It runs on
// the main thread, which is why the getters below can be read synchronously
// without a deadlock.
//
//export zgGoClose
func zgGoClose() {
	if onCloseButton == nil || onCloseButton() {
		C.zg_do_terminate()
		return
	}
	// Close-to-tray: the callback has already hidden the window; hide again so
	// a future callback that forgets to cannot strand a visible window.
	C.zg_do_hide()
}

// SetProcessDPIAwareness is a no-op: macOS picks the backing scale per display
// for the whole process, so there is nothing to opt into.
func SetProcessDPIAwareness() {}

// SetAppUserModelID is the Windows taskbar-identity call; on macOS the Dock
// icon and app name come from the bundle's Info.plist instead.
func SetAppUserModelID(id string) { C.zg_noop_string(nil) }

// Run builds the window on the main thread and blocks until the app quits.
//
// macOS wants every AppKit object created on the main thread, so unlike
// Windows — where the tray loop gets its own locked goroutine while the
// WebView2 message pump owns main — the tray event loop is what we run *into*,
// and the window is created from its ready hook.
func Run(o Options) {
	onCloseButton = o.OnCloseButton
	pending = o

	opts := tray.Options{OnReady: func() { C.zg_dispatch_create() }}
	if o.Tray != nil {
		opts.DashboardURL = o.Tray.DashboardURL
		opts.OnQuit = o.Tray.OnQuit
		opts.OnReprobe = o.Tray.OnReprobe
		opts.OnShow = o.Tray.OnShow
	}
	tray.Register(opts)
	tray.Loop()
}

// create runs on the main thread, inside NSApp.run.
func create(o Options) {
	title := C.CString(o.Title)
	url := C.CString(o.URL)
	defer C.zg_free(unsafe.Pointer(title))
	defer C.zg_free(unsafe.Pointer(url))
	C.zg_run(title, C.int(o.Width), C.int(o.Height), url)

	if o.Bounds != nil && o.Bounds.W > 0 {
		w, h := o.Bounds.W, o.Bounds.H
		// A save that happened while minimized can persist the collapsed
		// titlebar size; fall back to the defaults rather than opening as a
		// tiny sliver.
		if w < 400 || h < 300 {
			w, h = o.Width, o.Height
		}
		ApplyBounds(hwndToken, o.Bounds.X, o.Bounds.Y, w, h)
		if o.Bounds.Maximized {
			MaximizeWindow(hwndToken)
		}
	}
	// Show only after the restored rectangle is in place, so the window never
	// flashes at the default size on its way to the saved spot.
	C.zg_do_show()

	if o.OnReady != nil {
		o.OnReady(hwndToken)
	}
}

// MaximizeWindow animates the window to its maximized state.
func MaximizeWindow(hwnd uintptr) { C.zg_do_toggle_max() }

func minimizeWindow(hwnd uintptr) { C.zg_do_minimize() }

// IsVisible reports whether the main window is currently shown.
func IsVisible(hwnd uintptr) bool { return C.zg_do_is_visible() != 0 }

// ShowWindowWin shows/restores the main window (used by close-to-tray).
func ShowWindowWin(hwnd uintptr) { C.zg_do_show() }

// HideWindow hides to tray.
func HideWindow(hwnd uintptr) { C.zg_do_hide() }

// IsMaximized reports the zoomed state.
func IsMaximized(hwnd uintptr) bool { return C.zg_do_is_maximized() != 0 }

// IsMinimized reports whether the window is in the Dock. A miniaturized
// window's frame is not meaningful, so its rectangle must never be persisted.
func IsMinimized(hwnd uintptr) bool { return C.zg_do_is_minimized() != 0 }

// GetBounds returns the window rect as left, top, right, bottom — the same
// shape the Windows build reports, so callers can share their arithmetic.
func GetBounds(hwnd uintptr) (int, int, int, int) {
	C.zg_do_get_bounds()
	b := unsafe.Slice(C.zg_bounds(), 4)
	x, y, w, h := int(b[0]), int(b[1]), int(b[2]), int(b[3])
	return x, y, x + w, y + h
}

// ApplyBounds positions the window. A persisted rect that lands (almost)
// fully off-screen — saved while minimized, or on a display that has since
// gone away — is re-centered instead of opening invisibly.
func ApplyBounds(hwnd uintptr, x, y, w, h int) {
	C.zg_do_get_screen_frame()
	s := unsafe.Slice(C.zg_screen(), 4)
	sx, sy, sw, sh := int(s[0]), int(s[1]), int(s[2]), int(s[3])
	if sw <= 0 || sh <= 0 {
		sx, sy, sw, sh = 0, 0, 1440, 900
	}
	if w <= 0 || h <= 0 ||
		minInt(x+w, sx+sw)-maxInt(x, sx) < 120 || minInt(y+h, sy+sh)-maxInt(y, sy) < 120 {
		if sw > 200 && w > sw {
			w = sw
		}
		if sh > 200 && h > sh {
			h = sh
		}
		x, y = sx+(sw-w)/2, sy+(sh-h)/2
		if x < sx {
			x = sx
		}
		if y < sy {
			y = sy
		}
	}
	C.zg_do_set_bounds(C.int(x), C.int(y), C.int(w), C.int(h))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
