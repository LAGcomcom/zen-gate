// Package unblock removes the NTFS "Mark of the Web" (MOTW) from files that
// shipped alongside this binary.
//
// # Why this exists
//
// When a file is downloaded — browser, GitHub release, zip extracted by
// Explorer — Windows attaches an alternate data stream to it:
//
//	<file>:Zone.Identifier  with  ZoneId=3
//
// ZoneId=3 means "came from the Internet". Explorer Hands that flag to the
// Attachment Manager before the process is ever created, so a double-click on a
// ZoneId=3 executable goes through SmartScreen instead of a plain CreateProcess.
// A self-signed (or absent) signature has no cloud reputation, so the result is
// the "Windows protected your PC / 无法验证发布者" wall. Nothing about the code
// changed, and nothing about which folder the file sits in changes it either —
// the zone stream is the whole trigger.
//
// The denial happens before CreateProcess, so the binary cannot prevent the
// very first launch. What it *can* do is delete the stream from itself and its
// siblings the first time it does run. From then on the files carry no zone
// stream, Explorer starts them directly, and the wall never appears again.
//
// This is ordinary file metadata, not a signature and not a permission: the
// stream is writable by the owning user, so os.Remove on "<file>:Zone.Identifier"
// deletes it without elevation.
package unblock

// Target is one file considered for clearing plus whether it actually carried a
// zone stream.
type Target struct {
	Path    string
	ZoneID  string // "" when unknown
	Cleared bool
	Err     error
}

// Result summarises one pass over a directory.
type Result struct {
	Scanned []Target
	Cleared int
	Failed  int
}

// SuspiciousOrSelf reports whether the path is worth clearing. Everything with a
// zone stream qualifies; this predicate only decides what the caller passed in.
func (r Result) Any() bool { return r.Cleared > 0 || r.Failed > 0 }
