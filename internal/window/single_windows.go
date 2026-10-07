//go:build windows

package window

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex   = kernel32.NewProc("CreateMutexW")
	procGetLastError  = kernel32.NewProc("GetLastError")
	procFindWindow    = user32.NewProc("FindWindowW")
	procSetForeground = user32.NewProc("SetForegroundWindow")
)

const errorAlreadyExists = 183

// toUTF16 builds a NUL-terminated UTF-16 pointer.
func toUTF16(s string) uintptr {
	runes := []uint16{}
	for _, r := range s {
		runes = append(runes, uint16(r))
	}
	runes = append(runes, 0)
	return uintptr(unsafe.Pointer(&runes[0]))
}

// AcquireSingleInstance creates the app mutex. Returns false when another
// instance is already running — in that case the existing main window is
// restored and focused, and the caller should exit.
func AcquireSingleInstance(name, windowTitle string) bool {
	h, _, err := procCreateMutex.Call(0, 0, toUTF16(name))
	if h == 0 {
		return true // can't tell; don't block the app over this
	}
	if err.(syscall.Errno) == errorAlreadyExists {
		// Focus the running instance's window, if visible.
		if hwnd, _, _ := procFindWindow.Call(0, toUTF16(windowTitle)); hwnd != 0 {
			procShowWindow.Call(hwnd, swRestore)
			procSetForeground.Call(hwnd)
		}
		return false
	}
	return true
}

var _ = os.Getpid
