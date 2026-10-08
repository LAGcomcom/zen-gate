//go:build !windows && !darwin && !linux

package agents

// processRunningAny has no supported process-table lookup on this platform, so
// the dashboard just omits the "restart the agent" warnings.
func processRunningAny(names []string) bool { return false }
