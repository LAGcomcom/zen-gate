//go:build windows && !headless

package tray

import (
	_ "embed"
	"fmt"
	"os/exec"
	"syscall"

	"github.com/getlantern/systray"
)

//go:embed assets/tray.ico
var iconBytes []byte

func setIcon() { systray.SetIcon(iconBytes) }

func openURL(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func copyToClipboard(text string) {
	cmd := exec.Command("cmd", "/c", fmt.Sprintf("echo %s| clip", text))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = cmd.Start()
}
