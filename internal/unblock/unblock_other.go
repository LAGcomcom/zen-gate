//go:build !windows

package unblock

// On non-Windows platforms there is no Zone.Identifier alternative data stream
// and no Attachment Manager gate, so the pass is a no-op that keeps the call
// sites in main.go platform-free.

// ZoneIDFor always returns "" off Windows.
func ZoneIDFor(string) string { return "" }

// Has always reports false off Windows.
func Has(string) bool { return false }

// ClearOne reports that there was nothing to clear.
func ClearOne(string) (cleared bool, zone string, err error) { return false, "", nil }

// Dir returns an empty result off Windows.
func Dir(string, int) Result { return Result{} }

// Near returns an empty result off Windows.
func Near() Result { return Result{} }
