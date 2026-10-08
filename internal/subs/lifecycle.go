package subs

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zen-gate/internal/store"
)

// stopAndWait terminates the tracked sidecar and does not return until its
// inbound ports can be bound again. Kill() only signals; without the port wait
// the next sing-box starts while the dying one still owns the sockets, exits on
// "address already in use", and the supervisor restarts it forever.
//
// It deliberately does not call p.Wait(): the supervisor goroutine owns that
// child's exit status, and two waiters on one process hand back a bogus error.
func stopAndWait(p *os.Process, addrs []string, timeout time.Duration) error {
	if p != nil {
		_ = p.Kill()
	}
	return waitForPorts(addrs, timeout)
}

func waitForPorts(addrs []string, timeout time.Duration) error {
	if len(addrs) == 0 {
		return nil
	}
	if portsFree(addrs, timeout) {
		return nil
	}
	return fmt.Errorf("端口仍被占用: %s", strings.Join(busyPorts(addrs), ", "))
}

// reapOrphan terminates a sidecar left behind by an earlier zen-gate — one this
// process has no handle to, because its parent died. Only a pid the manager
// recorded itself, still running the recorded binary, is touched; anything else
// holding the port belongs to the user and is left alone.
func reapOrphan(addrs []string, timeout time.Duration) error {
	if len(addrs) == 0 {
		return nil
	}
	if portsFree(addrs, 0) {
		_ = removePidfile()
		return nil
	}
	rec, err := readPidfile()
	if err != nil || rec == nil {
		return waitForPorts(addrs, timeout)
	}
	exe, err := runningImage(rec.PID)
	if err != nil {
		_ = removePidfile()
		return waitForPorts(addrs, timeout)
	}
	if !sameImage(exe, rec.Exe) {
		return fmt.Errorf("端口被进程 %d 占着，但它运行的不是 %s（是 %s），不会替你杀掉它",
			rec.PID, rec.Exe, exe)
	}
	if err := terminatePID(rec.PID); err != nil {
		return err
	}
	_ = removePidfile()
	return waitForPorts(addrs, timeout)
}

// portsFree reports whether every address can be bound right now.
func portsFree(addrs []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(busyPorts(addrs)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func busyPorts(addrs []string) []string {
	var busy []string
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			busy = append(busy, a)
			continue
		}
		_ = ln.Close()
	}
	return busy
}

// inboundPorts lists the sidecar's local socks inbound addresses for n nodes.
func inboundPorts(n int) []string {
	if n < 1 {
		n = 1
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("127.0.0.1:%d", PortBase+i))
	}
	return out
}

func sameImage(a, b string) bool {
	return strings.EqualFold(canonicalImage(a), canonicalImage(b))
}

// canonicalImage resolves symlinks before comparing. macOS hands the same
// binary back as /var/folders/… from ps and /private/var/folders/… from
// os.Executable(), and a false "that is not our sidecar" would leave the
// orphan holding the port and crash-looping every restart.
func canonicalImage(p string) string {
	p = strings.TrimSpace(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// pidRecord is what the manager needs to recognise its own sidecar again after
// a restart: a recycled pid must not be enough to authorise a kill.
type pidRecord struct {
	PID int    `json:"pid"`
	Exe string `json:"exe"`
}

func pidfilePath() string { return filepath.Join(store.SubsDir(), "sing-box.pid") }

func writePidfileAt(pid int, exe string) error {
	if err := os.MkdirAll(store.SubsDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(pidRecord{PID: pid, Exe: exe})
	if err != nil {
		return err
	}
	return os.WriteFile(pidfilePath(), b, 0o600)
}

func readPidfile() (*pidRecord, error) {
	b, err := os.ReadFile(pidfilePath())
	if err != nil {
		return nil, err
	}
	var rec pidRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if rec.PID <= 0 || rec.Exe == "" {
		return nil, fmt.Errorf("pid 文件残缺")
	}
	return &rec, nil
}

func removePidfile() error {
	err := os.Remove(pidfilePath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
