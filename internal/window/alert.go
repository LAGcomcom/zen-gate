//go:build !windows

package window

import "zen-gate/internal/notify"

// FatalAlert is the cross-platform last-resort channel for a startup failure
// the user must see: on a -H=windowsgui build there is no console, so a log
// line and an exit(1) look like "double-click did nothing" (issue #44).
// Everywhere else the tray's notification backend carries the same message.
func FatalAlert(title, body string) { notify.Toast(title, body) }
