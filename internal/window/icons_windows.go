//go:build windows && !headless

package window

import (
	_ "embed"
	"os"
	"path/filepath"
	"syscall"
)

//go:embed assets/icon.ico
var windowIconICO []byte

var (
	procLoadImageW       = user32.NewProc("LoadImageW")
	procSendMessageW2    = user32.NewProc("SendMessageW")
	procSetClassLongPtrW = user32.NewProc("SetClassLongPtrW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procSetExplicitAUMID = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
)

const (
	gclpHIcon      = 0xFFFFFFF2
	gclpHIconSm    = 0xFFFFFFDE
	wmSetIcon      = 0x0080
	iconBigW       = 1
	iconSmallW     = 0
	smCXIcon       = 11
	smCYIcon       = 12
	smCXSmIcon     = 49
	smCYSmIcon     = 50
	lrLoadFromFile = 0x0010
	imageIconW     = 1
)

// SetAppUserModelID gives the process an explicit taskbar identity so Windows
// doesn't reuse cached icon groups from older builds.
func SetAppUserModelID(id string) {
	procSetExplicitAUMID.Call(toUTF16(id))
}

func winMetric(index int) int {
	v, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(v)
}

// setWindowIcons assigns the embedded icon to the running window + class via
// LoadImage from a temp .ico file at DPI-correct sizes. This is the simplest
// approach that reliably works on Windows.
func setWindowIcons(hwnd uintptr) {
	cxIcon := winMetric(smCXIcon)
	cyIcon := winMetric(smCYIcon)
	cxSm := winMetric(smCXSmIcon)
	cySm := winMetric(smCYSmIcon)
	dlogf("icons: DPI sizes big=%dx%d small=%dx%d", cxIcon, cyIcon, cxSm, cySm)

	tmp := filepath.Join(os.TempDir(), "zen-gate-icon.ico")
	if err := os.WriteFile(tmp, windowIconICO, 0o644); err != nil {
		dlogf("write ico: %v", err)
		return
	}
	defer os.Remove(tmp)

	u := toUTF16(tmp)
	hBig, _, _ := procLoadImageW.Call(0, u, uintptr(imageIconW), uintptr(cxIcon), uintptr(cyIcon), uintptr(lrLoadFromFile))
	hSm, _, _ := procLoadImageW.Call(0, u, uintptr(imageIconW), uintptr(cxSm), uintptr(cySm), uintptr(lrLoadFromFile))
	dlogf("icons: loadimage big=%#x small=%#x", hBig, hSm)

	if hBig != 0 {
		procSendMessageW2.Call(hwnd, wmSetIcon, uintptr(iconBigW), hBig)
		procSetClassLongPtrW.Call(hwnd, uintptr(gclpHIcon), hBig)
	}
	if hSm != 0 {
		procSendMessageW2.Call(hwnd, wmSetIcon, uintptr(iconSmallW), hSm)
		procSetClassLongPtrW.Call(hwnd, uintptr(gclpHIconSm), hSm)
	}
}
