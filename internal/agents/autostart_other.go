//go:build !windows && !darwin && !linux

package agents

// AutostartEnabled reports whether the app is registered to start at login.
// No supported implementation exists for this platform yet.
func AutostartEnabled() bool { return false }

// AutostartSet is a no-op on platforms without a per-user autostart registry.
func AutostartSet(enable bool) error { return nil }
