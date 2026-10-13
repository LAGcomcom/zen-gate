//go:build windows

package window

import (
	"syscall"
	"unsafe"
)

// FatalAlert shows a modal native dialog and blocks until dismissed.
//
// A tray Toast is the wrong shape for a startup failure: -Toast is fired from
// the WinRT path through PowerShell, which needs a moment to spin up, and the
// process is about to os.Exit(1) — the notification races its own host dying
// and lands silently in a muted action centre. The symptom issue #44 reports
// is exactly that: the port is taken, the bind fails, the process exits, and
// the user double-clicks an .exe that shows "no reaction, no process". A
// blocking MessageBox cannot be missed, cannot be muted, and does not need the
// notification backend or a window to exist yet.
func FatalAlert(title, body string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	procMessageBoxW := user32.NewProc("MessageBoxW")
	// MB_OK | MB_ICONERROR (0x10): an error icon, single OK button.
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	bodyPtr, _ := syscall.UTF16PtrFromString(body)
	_, _, _ = procMessageBoxW.Call(
		uintptr(0),
		uintptr(unsafe.Pointer(bodyPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(0x00000010|0x00000000),
	)
}
