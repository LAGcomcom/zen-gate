//go:build windows

package window

import (
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")

var (
	procReleaseCapture   = user32.NewProc("ReleaseCapture")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procIsZoomed         = user32.NewProc("IsZoomed")
	procIsIconic         = user32.NewProc("IsIconic")
	procShowWindow       = user32.NewProc("ShowWindow")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procGetWindowLongPtr = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procIsWindowVisible  = user32.NewProc("IsWindowVisible")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
)

const (
	gwlStyle        = -16
	wsCaption       = 0x00C00000
	wmNCLButtonDown = 0x00A1
	htCaption       = 2
	wmClose         = 0x0010
	wmSysCommand    = 0x0112
	scMinimize      = 0xF020
	scMaximize      = 0xF030
	scRestore       = 0xF120
	swMinimize      = 6
	swMaximize      = 3
	swRestore       = 9
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
)

type rect struct {
	Left, Top, Right, Bottom int32
}

func getWindowLong(hwnd uintptr, index int32) uintptr {
	ret, _, _ := procGetWindowLongPtr.Call(hwnd, uintptr(index))
	return ret
}

func setWindowLong(hwnd uintptr, index int32, value uintptr) uintptr {
	ret, _, _ := procSetWindowLongPtr.Call(hwnd, uintptr(index), value)
	return ret
}

// stripCaption removes the native title bar while keeping WS_THICKFRAME, so
// the invisible resize borders (and Win11 rounded corners) keep working; the
// client area — our webview with the HTML title bar — extends to the top.
func stripCaption(hwnd uintptr) {
	style := getWindowLong(hwnd, gwlStyle)
	setWindowLong(hwnd, gwlStyle, style&^uintptr(wsCaption))
	// Force frame recalculation and make the webview child refill the grown
	// client area: nudge the height by 1px so WM_SIZE fires.
	var r rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	w, h := r.Right-r.Left, r.Bottom-r.Top
	procSetWindowPos.Call(hwnd, 0, 0, 0, uintptr(w), uintptr(h+1), swpNoZOrder|swpNoActivate)
	procSetWindowPos.Call(hwnd, 0, 0, 0, uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate|swpFrameChanged)
}

// startDrag hands the mouse gesture to the native move loop via the classic
// ReleaseCapture + WM_NCLBUTTONDOWN(HTCAPTION) trick.
func startDrag(hwnd uintptr) {
	procReleaseCapture.Call()
	procSendMessageW.Call(hwnd, wmNCLButtonDown, htCaption, 0)
}

func minimizeWindow(hwnd uintptr) {
	// Via WM_SYSCOMMAND, not ShowWindow: the system-command path is the one
	// that plays the DWM min/max/restore animation — ShowWindow on a
	// caption-less window snaps instantly.
	procPostMessageW.Call(hwnd, wmSysCommand, scMinimize, 0)
}

// MaximizeWindow animates the window to its maximized state.
func MaximizeWindow(hwnd uintptr) {
	procPostMessageW.Call(hwnd, wmSysCommand, scMaximize, 0)
}

func toggleMaximize(hwnd uintptr) {
	zoomed, _, _ := procIsZoomed.Call(hwnd)
	if zoomed != 0 {
		procPostMessageW.Call(hwnd, wmSysCommand, scRestore, 0)
	} else {
		procPostMessageW.Call(hwnd, wmSysCommand, scMaximize, 0)
	}
}

func closeWindow(hwnd uintptr) {
	procPostMessageW.Call(hwnd, wmClose, 0, 0)
}

// IsVisible reports whether the main window is currently shown.
func IsVisible(hwnd uintptr) bool {
	v, _, _ := procIsWindowVisible.Call(hwnd)
	return v != 0
}

// ShowWindowWin shows/restores the main window (used by close-to-tray).
func ShowWindowWin(hwnd uintptr) {
	procShowWindow.Call(hwnd, swRestore)
	procSetForeground.Call(hwnd)
}

// GetBounds returns the window rect (left, top, right, bottom).
func GetBounds(hwnd uintptr) (int, int, int, int) {
	var r rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return int(r.Left), int(r.Top), int(r.Right), int(r.Bottom)
}

// IsMaximized reports the zoomed state.
func IsMaximized(hwnd uintptr) bool {
	z, _, _ := procIsZoomed.Call(hwnd)
	return z != 0
}

// IsMinimized reports the iconic state. A minimized window's GetWindowRect is
// the sentinel off-screen rect, which must never be persisted or restored.
func IsMinimized(hwnd uintptr) bool {
	z, _, _ := procIsIconic.Call(hwnd)
	return z != 0
}

func screenMetric(index int) int {
	v, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(v))
}

// ApplyBounds positions the window before it is shown. A persisted rect that
// lands (almost) fully outside the virtual screen — e.g. saved while
// minimized, or on a monitor that since went away — is re-centered on the
// primary display instead of opening invisibly.
func ApplyBounds(hwnd uintptr, x, y, w, h int) {
	vx, vy := screenMetric(76), screenMetric(77)
	vw, vh := screenMetric(78), screenMetric(79)
	if vw <= 0 || vh <= 0 {
		vx, vy, vw, vh = 0, 0, screenMetric(0), screenMetric(1)
	}
	ix1, iy1 := maxInt(x, vx), maxInt(y, vy)
	ix2, iy2 := minInt(x+w, vx+vw), minInt(y+h, vy+vh)
	if w <= 0 || h <= 0 || ix2-ix1 < 120 || iy2-iy1 < 120 {
		cw, ch := screenMetric(0), screenMetric(1)
		if cw > 200 && w > cw {
			w = cw
		}
		if ch > 200 && h > ch {
			h = ch
		}
		x, y = (cw-w)/2, (ch-h)/2
		if x < 0 {
			x = 0
		}
		if y < 0 {
			y = 0
		}
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x0014) // NOZORDER|NOACTIVATE
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

// RestoreWindow unhides without forcing foreground.
func RestoreWindow(hwnd uintptr) {
	procShowWindow.Call(hwnd, swRestore)
}

// HideWindow hides to tray.
func HideWindow(hwnd uintptr) {
	procShowWindow.Call(hwnd, 0) // SW_HIDE
}
