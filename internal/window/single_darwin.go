//go:build darwin && !headless

package window

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"zen-gate/internal/store"
)

// Single instance on macOS is an advisory flock on a lock file rather than a
// named mutex: the kernel releases it when the process dies, so a crash can
// never leave a stale lock behind — the failure mode a lock file needs to be
// careful about and a PID file does not have.
const lockFileName = "zen-gate.lock"

// bundleID must match CFBundleIdentifier in the .app Info.plist written by
// tools/build-macos.sh; it is how the second launch finds its predecessor.
const bundleID = "com.lagcomcom.zen-gate"

// lockFile is held open for the life of the process: closing it would drop the
// flock and let a second instance through.
var lockFile *os.File

// AcquireSingleInstance takes the app lock. Returns false when another
// instance already holds it — in that case the running app is brought forward
// and the caller should exit.
func AcquireSingleInstance(name, windowTitle string) bool {
	path := filepath.Join(store.Home(), lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true // can't tell; don't block the app over this
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Someone else holds it. Activating the bundle raises their window
		// without touching the gateway, which keeps the port single-owner.
		_ = f.Close()
		activateRunningApp()
		return false
	}
	lockFile = f
	return true
}

// activateRunningApp best-effort brings the already-running instance forward.
func activateRunningApp() {
	script := `tell application id "` + bundleID + `" to activate`
	if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
		dlogf("activate running instance: %v: %s", err, out)
	}
}
