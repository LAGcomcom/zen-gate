//go:build windows && !headless

package window

import (
	"syscall"
	"unsafe"
)

var dwmapi = syscall.NewLazyDLL("dwmapi.dll")
var procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")

// DWM window attributes (Win11 22000+ for the color ones; older builds just
// no-op with an error we ignore).
const (
	dwmUseImmersiveDarkMode = 20
	dwmBorderColor          = 34
	dwmCaptionColor         = 35
	dwmTextColor            = 36
)

// colorref packs 0x00BBGGRR.
func colorref(r, g, b byte) uint32 {
	return uint32(r) | uint32(g)<<8 | uint32(b)<<16
}

func dwmSetU32(hwnd uintptr, attr uint32, value uint32) {
	v := value
	_, _, _ = procDwmSetWindowAttribute.Call(
		hwnd, uintptr(attr), uintptr(unsafe.Pointer(&v)), 4)
}

// themeTitleBar paints the native caption in the UI's own colors so the
// title bar melts into the dark dashboard instead of showing the system's
// default light strip.
func themeTitleBar(hwnd uintptr) {
	dwmSetU32(hwnd, dwmUseImmersiveDarkMode, 1)
	// --bg: #101215; caption text: --ink #f0f1ee
	dwmSetU32(hwnd, dwmCaptionColor, colorref(0x17, 0x17, 0x17))
	dwmSetU32(hwnd, dwmBorderColor, colorref(0x17, 0x17, 0x17))
	dwmSetU32(hwnd, dwmTextColor, colorref(0xf0, 0xf1, 0xee))
}
