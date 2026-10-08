//go:build linux

package agents

import (
	"os/exec"
	"strings"
)

// processRunningAny checks whether any of the given process names are
// currently running, using pgrep with a case-insensitive substring match on
// the command line. Best-effort: missing pgrep reports nothing running.
func processRunningAny(names []string) bool {
	if len(names) == 0 {
		return false
	}
	args := []string{"-f", "-i"}
	for _, n := range names {
		args = append(args, "-e", n)
	}
	out, err := exec.Command("pgrep", args...).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}
