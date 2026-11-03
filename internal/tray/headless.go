//go:build headless

// Package tray in the headless build: there is no notification area to put an
// icon in. The exported surface is the same so the shared call sites (main's
// tray status line and the tray's 打开管理页) still compile.
package tray

// Options mirrors the desktop build's configuration type so callers do not
// need a build tag of their own.
type Options struct {
	DashboardURL string
	OnQuit       func()
	OnReprobe    func()
	OnShow       func()
	OnReady      func()
}

// Register accepts the configuration and keeps nothing: no icon will ever be
// created out of it.
func Register(o Options) {}

// Loop would block on a UI event loop that does not exist here, so it returns
// immediately.
func Loop() {}

// Quit is a no-op.
func Quit() {}

// SetStatus is a no-op: nothing displays it.
func SetStatus(text string) {}

// Open still works — on a server it is how you get at the dashboard from a
// terminal — but the headless host is expected to be reached over the network
// instead, so this stays a plain no-op to keep the icon-free promise.
func Open(url string) {}
