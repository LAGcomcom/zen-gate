package update

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Apply downloads the new binary from a release asset and swaps it with the
// running one. Windows refuses to overwrite a running exe but allows renaming
// it, so the swap is: current → .bak, .new → current, spawn the new exe,
// exit. The caller exits right after Apply returns success; the .bak is
// cleaned up on the next boot (CleanupBackup).
//
// Only Windows reaches this path — see SelfUpdateSupported. Everywhere else
// the release is a bundle the user installs by hand, because replacing a
// signed .app's inner binary in place would break the signature.
//
// download is the release asset URL (https only — the whole point is that the
// payload comes from the GitHub release this update check just authenticated
// against indirectly).

const minExeSize = 1 << 20 // 1 MiB sanity floor

// Apply performs the download-and-swap. After a true return the caller should
// start the new exe (Relaunch) and exit.
func Apply(downloadURL string, client *http.Client) (bool, error) {
	if downloadURL == "" {
		return false, fmt.Errorf("no download url")
	}
	if len(downloadURL) < 8 || downloadURL[:8] != "https://" {
		return false, fmt.Errorf("update asset url must be https")
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	exePath, err := filepath.Abs(exe)
	if err != nil {
		return false, err
	}
	newPath := exePath + ".new"
	bakPath := exePath + ".bak"

	// Download to <exe>.new with a generous timeout (exe ≈ 9 MB).
	timeout := 10 * time.Minute
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	resp, err := c.Get(downloadURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("update download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(newPath)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(newPath)
		return false, err
	}
	if closeErr != nil {
		os.Remove(newPath)
		return false, closeErr
	}
	if n < minExeSize {
		os.Remove(newPath)
		return false, fmt.Errorf("downloaded update is suspiciously small (%d bytes)", n)
	}
	// A proxy or a half-closed connection can truncate the body without io.Copy
	// reporting an error. Compare against the declared length when the server
	// sent one, so a partial binary never replaces a working install.
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		os.Remove(newPath)
		return false, fmt.Errorf("downloaded update is %d bytes, server declared %d", n, resp.ContentLength)
	}

	// Swap: running exe → .bak, .new → exe. Windows permits renaming a
	// running binary, which is what makes this work without a helper.
	if err := os.Rename(exePath, bakPath); err != nil {
		os.Remove(newPath)
		return false, fmt.Errorf("could not move the running exe aside: %w", err)
	}
	if err := os.Rename(newPath, exePath); err != nil {
		// Roll the backup back so the install stays bootable.
		_ = os.Rename(bakPath, exePath)
		os.Remove(newPath)
		return false, fmt.Errorf("could not place the new exe: %w", err)
	}
	return true, nil
}

// Relaunch starts the freshly installed exe detached and returns an error if
// it could not be started. The caller exits afterwards.
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe).Start()
}

// Rollback restores the binary that Apply moved to .bak. It is called when the
// freshly installed exe cannot be relaunched (antivirus block, missing runtime),
// where leaving the unverified binary in place would brick the next start.
func Rollback() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	p, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		return fmt.Errorf("no previous binary to restore: %w", err)
	}
	// The new binary is not running yet, so it can be removed; on Unix the
	// rename below would replace it anyway, but Windows refuses that.
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(p+".bak", p)
}

// CleanupBackup removes the previous binary left by a prior Apply.
func CleanupBackup() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := filepath.Abs(exe)
	if err != nil {
		return
	}
	_ = os.Remove(p + ".bak")
	_ = os.Remove(p + ".new")
}
