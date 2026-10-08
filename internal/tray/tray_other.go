//go:build !windows && !darwin && !linux

package tray

import (
	_ "embed"
	"os/exec"
	"strings"

	"github.com/getlantern/systray"
)

//go:embed assets/trayTemplate.png
var iconBytes []byte

func setIcon() { systray.SetIcon(iconBytes) }

func openURL(url string) { _ = exec.Command("xdg-open", url).Start() }

func copyToClipboard(text string) {
	cmd := exec.Command("xclip", "-selection", "clipboard")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}
