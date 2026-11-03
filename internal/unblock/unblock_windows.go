//go:build windows

package unblock

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// zoneStream is the alternate data stream Explorer's Attachment Manager reads.
const zoneStream = ":Zone.Identifier"

// maxBytes bounds how much of the stream we read; the real payload is a handful
// of ASCII lines.
const maxBytes = 4096

// ZoneIDFor returns the ZoneId recorded on path, or "" when there is no stream
// (or its contents cannot be read).
func ZoneIDFor(path string) (zone string) {
	f, err := os.Open(path + zoneStream)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ZoneId="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Has returns true when path carries a zone stream at all.
func Has(path string) bool {
	f, err := os.Open(path + zoneStream)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// ClearOne deletes the zone stream from a single file.
//
// A file without the stream is not an error: it reports cleared=false, err=nil,
// which is what makes repeated runs over an already-clean directory free.
func ClearOne(path string) (cleared bool, zone string, err error) {
	f, openErr := os.Open(path + zoneStream)
	if openErr != nil {
		if missing(openErr) {
			return false, "", nil
		}
		// The stream exists in a form we cannot open (locked by an AV scanner,
		// for instance). Report it: the caller logs and moves on.
		return false, "", openErr
	}
	zone = ""
	if b, rerr := io.ReadAll(io.LimitReader(f, maxBytes)); rerr == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "ZoneId="); ok {
				zone = strings.TrimSpace(v)
			}
		}
	}
	_ = f.Close()

	// os.Remove on "<file>:<stream>" deletes just that stream; the file itself
	// is untouched. No elevation is needed — the stream is owned by the file's
	// owner, same as the file.
	if rerr := os.Remove(path + zoneStream); rerr != nil {
		return false, zone, rerr
	}
	return true, zone, nil
}

// Dir clears every file at or below root.
//
// depth bounds the walk: a release folder is flat, and an unbounded walk over a
// directory a user happens to point us at (or a deep tree) would make startup
// latency depend on filesystem size, which is the wrong trade for a tray app.
// Missing or unreadable entries are skipped rather than failing the pass —
// clearing zone streams is best-effort hygiene, never a startup precondition.
func Dir(root string, depth int) Result {
	var res Result
	if depth < 0 {
		depth = 0
	}
	root = filepath.Clean(root)

	baseDepth := strings.Count(root, string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtree: not fatal, and definitely not worth aborting
			// the whole pass over.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// depth is measured in directories between root and the entry, so a
		// file directly in root is depth 0 and root/lib/helper.dll is depth 1.
		// The entry name itself adds one separator, hence the -1 on files.
		level := strings.Count(path, string(os.PathSeparator)) - baseDepth
		if !d.IsDir() {
			level--
		}
		if path != root && level > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if in, ierr := d.Info(); ierr == nil && in != nil && !in.Mode().IsRegular() {
			return nil
		}

		cleared, zone, cerr := ClearOne(path)
		if !cleared && cerr == nil {
			return nil // no stream: already clean
		}
		t := Target{Path: path, ZoneID: zone, Cleared: cleared, Err: cerr}
		res.Scanned = append(res.Scanned, t)
		if cleared {
			res.Cleared++
		} else {
			res.Failed++
		}
		return nil
	})
	return res
}

// Near clears the directory this executable lives in, plus one level below it.
//
// One executable-relative anchor rather than cwd: Explorer starts a double-click
// with the working directory set to the exe's folder, but a shell launch or a
// shortcut can start it anywhere, and the files that carry a downloaded zone
// stream are always the ones that arrived next to the binary.
func Near() Result {
	exe, err := os.Executable()
	if err != nil {
		return Result{}
	}
	// Resolve symlinks/8.3 names so the walk covers the real folder.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	res := Dir(filepath.Dir(exe), 1)

	// Clearing the running image's own stream is worth doing — Windows lets a
	// process delete the alternate data stream of its own locked image, which is
	// what makes the fix stick on the very first launch instead of waiting for
	// the user to run an unlock script. But if some AV scanner holds it, that
	// must not read as a failure: the image is already past the gate, and the
	// next launch will retry. So: keep the entry, drop only the error claim.
	res.Failed = 0
	self := exe
	for i := range res.Scanned {
		if !res.Scanned[i].Cleared && sameFile(res.Scanned[i].Path, self) {
			res.Scanned[i].Err = nil
			res.Scanned = append(res.Scanned[:i], res.Scanned[i+1:]...)
			continue
		}
		if !res.Scanned[i].Cleared {
			res.Failed++
		}
	}
	return res
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, os.ErrNotExist)
}
