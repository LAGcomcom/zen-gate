//go:build headless

// Package window's headless host: no native window, no tray, no WebView.
//
// Everything the desktop build does here is a UI concern, so the headless
// build replaces the whole platform file set with this one stub. The point is
// that "runs in a container" stops being a runtime flag on a binary that still
// links GTK, WebView2 and the tray: a -tags headless build has no GUI
// dependencies left to link, which is what makes a scratch/alpine image and a
// CGO_ENABLED=0 cross-compile possible at all (issue #79, PR #108).
//
// main's -no-tray path never calls these, so behaviour is identical; the stub
// only has to exist so the one call site compiles.
package window

// Run never blocks in headless mode: there is no UI thread to own. If the
// caller reaches this, the process would otherwise exit immediately, so the
// stub waits on nothing and returns — main's headless branch is the real path.
func Run(o Options) {}

// ShowWindowWin, HideWindow, MaximizeWindow and IsVisible describe a window
// that does not exist.
func ShowWindowWin(hwnd uintptr) {}
func HideWindow(hwnd uintptr)     {}
func MaximizeWindow(hwnd uintptr) {}
func IsVisible(hwnd uintptr) bool { return false }

func minimizeWindow(hwnd uintptr) {}

func IsMaximized(hwnd uintptr) bool { return false }
func IsMinimized(hwnd uintptr) bool { return false }

// GetBounds returns zeros: there is no rectangle to persist, and
// saveWindowState treats anything under 400x300 as garbage and keeps the last
// good value.
func GetBounds(hwnd uintptr) (left, top, right, bottom int) { return 0, 0, 0, 0 }

// SetProcessDPIAwareness / SetAppUserModelID are desktop-shell concerns.
func SetProcessDPIAwareness()          {}
func SetAppUserModelID(id string)      {}
func AcquireSingleInstance(name, windowTitle string) bool { return true }

// FatalAlert must not try to raise a toast in a process with no desktop.
func FatalAlert(title, body string) {}
