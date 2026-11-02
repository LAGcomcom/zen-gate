//go:build windows && !headless

package window

import "syscall"

var (
	shcore    = syscall.NewLazyDLL("shcore.dll")
	user32dll = syscall.NewLazyDLL("user32.dll")
)

// setProcessDPIAwareness declares Per-Monitor-V2 DPI awareness (falling back
// to system awareness on older Windows). Must run before any window is
// created: without it the process renders at 96 DPI and Windows
// bitmap-stretches the whole window, which is exactly the "everything looks
// blurry" effect on a scaled display.
// SetProcessDPIAwareness declares DPI awareness; call once at process start,
// before any window (main window or tray) is created. Idempotent.
func SetProcessDPIAwareness() { setProcessDPIAwareness() }

func setProcessDPIAwareness() {
	if p := shcore.NewProc("SetProcessDpiAwareness"); p.Find() == nil {
		// PROCESS_PER_MONITOR_DPI_AWARE = 2
		_, _, _ = p.Call(2)
		return
	}
	if p := user32dll.NewProc("SetProcessDPIAware"); p.Find() == nil {
		_, _, _ = p.Call()
	}
}
