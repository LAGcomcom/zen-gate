//go:build linux

package notify

import (
	"os/exec"
	"strings"
)

// Toast delivers a desktop notification through libnotify's notify-send CLI.
// Best-effort: missing notify-send or a non-notifying session is ignored.
func Toast(title, body string) {
	title, body, ok := want(title, body)
	if !ok {
		return
	}
	args := []string{"-a", "zen-gate"}
	if strings.TrimSpace(body) != "" {
		args = append(args, title, body)
	} else {
		args = append(args, title)
	}
	_ = exec.Command("notify-send", args...).Start()
}
