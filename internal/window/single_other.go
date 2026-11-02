//go:build !windows && !darwin && !headless

package window

// SetProcessDPIAwareness is a no-op on platforms with no per-process DPI
// opt-in (the OS scales the app).
func SetProcessDPIAwareness() {}

// SetAppUserModelID is Windows-only; elsewhere the taskbar identity comes from
// the desktop entry.
func SetAppUserModelID(id string) {}

// AcquireSingleInstance has no cross-process lock on this platform, so the app
// always starts. Guard the shared listen port at the caller instead.
func AcquireSingleInstance(name, windowTitle string) bool { return true }
