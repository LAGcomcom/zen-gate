//go:build linux

package tray

import (
	_ "embed"
	"os/exec"
	"strings"

	"github.com/getlantern/systray"
)

//go:embed assets/icon-256.png
var iconBytes []byte

func setIcon() { systray.SetIcon(iconBytes) }

func openURL(url string) { _ = exec.Command("xdg-open", url).Start() }

// copyToClipboard writes text to the X11/Wayland clipboard, trying the common
// helpers in order: xclip → xsel → wl-copy (Wayland). Best-effort: a missing
// helper is silently ignored.
func copyToClipboard(text string) {
	for _, c := range [][]string{
		{"xclip", "-selection", "clipboard"},
		{"xsel", "-i", "-b"},
		{"wl-copy"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}
}
