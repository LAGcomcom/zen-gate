//go:build darwin && !headless

package tray

import (
	_ "embed"
	"os/exec"
	"strings"

	"github.com/getlantern/systray"
)

//go:embed assets/trayTemplate.png
var iconBytes []byte

// macOS menu bar icons are template images: the system masks them to the menu
// bar's foreground colour, so the same glyph reads correctly in a light or a
// dark bar and inverts while the menu is open.
func setIcon() { systray.SetTemplateIcon(iconBytes, iconBytes) }

func openURL(url string) { _ = exec.Command("open", url).Start() }

func copyToClipboard(text string) {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}
