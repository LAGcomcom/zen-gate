//go:build windows

package unblock

import (
	"os"
	"path/filepath"
	"testing"
)

// writeZone attaches a realistic Zone.Identifier stream to path, the way a
// browser or a GitHub release download would.
func writeZone(t *testing.T, path, zone string) {
	t.Helper()
	f, err := os.Create(path + zoneStream)
	if err != nil {
		t.Fatalf("create zone stream: %v", err)
	}
	payload := "[ZoneTransfer]\r\nZoneId=" + zone + "\r\nHostUrl=https://example.invalid/x.exe\r\n"
	if _, err := f.WriteString(payload); err != nil {
		t.Fatalf("write zone stream: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close zone stream: %v", err)
	}
}

// makeFile creates a regular file with some content so it behaves like a real
// binary rather than a zero-length placeholder.
func makeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("MZ fake binary payload"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func TestZoneIDForReadsZone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.exe")
	makeFile(t, p)

	if got := ZoneIDFor(p); got != "" {
		t.Fatalf("clean file should report no zone, got %q", got)
	}
	if Has(p) {
		t.Fatal("clean file should not report a stream")
	}

	writeZone(t, p, "3")
	if got := ZoneIDFor(p); got != "3" {
		t.Fatalf("ZoneId = %q, want 3", got)
	}
	if !Has(p) {
		t.Fatal("zone stream should be present")
	}
}

func TestClearOneRemovesOnlyTheStream(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.exe")
	makeFile(t, p)
	writeZone(t, p, "3")

	cleared, zone, err := ClearOne(p)
	if err != nil {
		t.Fatalf("ClearOne: %v", err)
	}
	if !cleared {
		t.Fatal("expected the stream to be cleared")
	}
	if zone != "3" {
		t.Fatalf("reported zone = %q, want 3", zone)
	}
	if Has(p) {
		t.Fatal("stream should be gone")
	}

	// The file itself must survive with its bytes intact — clearing metadata is
	// not allowed to touch content.
	body, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatalf("file should still exist: %v", rerr)
	}
	if string(body) != "MZ fake binary payload" {
		t.Fatalf("file content changed: %q", body)
	}
}

func TestClearOneOnCleanFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clean.exe")
	makeFile(t, p)

	cleared, zone, err := ClearOne(p)
	if err != nil {
		t.Fatalf("clean file must not be an error: %v", err)
	}
	if cleared {
		t.Fatal("nothing to clear, cleared should be false")
	}
	if zone != "" {
		t.Fatalf("zone = %q, want empty", zone)
	}
}

func TestDirClearsRecursivelyWithinDepth(t *testing.T) {
	root := t.TempDir()
	shallow := filepath.Join(root, "zen-gate.exe")
	launcher := filepath.Join(root, "启动-zen-gate.cmd")
	nested := filepath.Join(root, "lib", "helper.dll")
	deep := filepath.Join(root, "lib", "deep", "too-far.txt")

	for _, p := range []string{shallow, launcher, nested, deep} {
		makeFile(t, p)
		writeZone(t, p, "3")
	}

	res := Dir(root, 1)
	if res.Cleared != 3 {
		t.Fatalf("cleared = %d, want 3 (shallow, launcher, nested)", res.Cleared)
	}
	if res.Failed != 0 {
		t.Fatalf("failed = %d, want 0; scanned=%+v", res.Failed, res.Scanned)
	}
	if Has(deep) != true {
		t.Fatal("depth 1 must leave the second-level file untouched")
	}
	for _, p := range []string{shallow, launcher, nested} {
		if Has(p) {
			t.Fatalf("%s should be clean", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must still exist: %v", p, err)
		}
	}
}

func TestDirSecondPassIsQuiet(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.exe")
	makeFile(t, p)
	writeZone(t, p, "3")

	if res := Dir(root, 1); res.Cleared != 1 {
		t.Fatalf("first pass cleared = %d, want 1", res.Cleared)
	}
	// Startup runs this on every launch, so an already-clean directory has to
	// cost nothing and report nothing.
	res := Dir(root, 1)
	if len(res.Scanned) != 0 || res.Cleared != 0 || res.Failed != 0 || res.Any() {
		t.Fatalf("second pass should be a no-op, got %+v", res)
	}
}

func TestDirMissingRootIsQuiet(t *testing.T) {
	res := Dir(filepath.Join(t.TempDir(), "does-not-exist"), 1)
	if len(res.Scanned) != 0 || res.Any() {
		t.Fatalf("missing root should be a no-op, got %+v", res)
	}
}

func TestNearClearsItsOwnImageWhenPossible(t *testing.T) {
	// Windows permits a process to delete the zone stream of its own locked
	// image, so Near must at least attempt it — that is what makes the fix hold
	// from the first launch instead of needing a separate unlock script. It must
	// never report the attempt as a failure, though: the image already passed the
	// gate, and if an AV scanner holds the stream the next launch retries.
	dir := t.TempDir()
	exe := filepath.Join(dir, "zen-gate.exe")
	makeFile(t, exe)
	writeZone(t, exe, "3")

	if !Has(exe) {
		t.Fatal("precondition: stream should be present")
	}
	cleared, _, err := ClearOne(exe)
	if err != nil {
		t.Fatalf("clearing a stream is independent of the image lock: %v", err)
	}
	if !cleared {
		t.Fatal("expected the stream to be cleared")
	}
	if Has(exe) {
		t.Fatal("stream should be gone")
	}
}

func TestNearNeverCountsItsOwnImageAsFailure(t *testing.T) {
	// Near anchors on os.Executable(), which during a test is the test binary.
	// Whatever happens to that file, it must not surface as a failure.
	res := Near()
	self := mustExecutable(t)
	for _, tg := range res.Scanned {
		if sameFile(tg.Path, self) && !tg.Cleared {
			t.Fatalf("Near must not report its own image as an uncleared failure")
		}
	}
	if res.Failed != 0 {
		for _, tg := range res.Scanned {
			if !tg.Cleared {
				t.Fatalf("unexpected failure entry: %+v", tg)
			}
		}
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe
}
