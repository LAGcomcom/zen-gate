package subs

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestPortHolder is not a test — it is the process the lifecycle tests re-exec,
// so "the inbound port is still bound" has a real owner instead of a mock. It
// returns immediately when the marker variable is absent.
func TestPortHolder(t *testing.T) {
	port := os.Getenv("ZENGATE_HOLD_PORT")
	if port == "" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		os.Exit(1)
	}
	_ = ln
	time.Sleep(90 * time.Second)
}

func holdPort(t *testing.T, port string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestPortHolder")
	cmd.Env = append(os.Environ(), "ZENGATE_HOLD_PORT="+port)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	for i := 0; i < 40; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err == nil {
			_ = c.Close()
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("holder never bound 127.0.0.1:%s", port)
	return nil
}

func reservedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe a free port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// writePidfile records what startProcess records: the child's pid and the
// binary it was launched from.
func writePidfile(t *testing.T, pid int, exe string) {
	t.Helper()
	if err := writePidfileAt(pid, exe); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	t.Cleanup(func() { _ = removePidfile() })
}

// Kill() only signals: the socket stays bound while the child dies, so the next
// sidecar instance fails to bind, exits, and the supervisor restarts it forever
// (72 crash events in one day in the reporter's log).
func TestStopAndWaitReleasesThePort(t *testing.T) {
	port := reservedPort(t)
	holder := holdPort(t, port)
	addr := "127.0.0.1:" + port

	if err := stopAndWait(holder.Process, []string{addr}, 5*time.Second); err != nil {
		t.Fatalf("stopAndWait: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still held after stopAndWait — a restarted sidecar would crash-loop: %v", err)
	}
	_ = ln.Close()
}

// A sidecar we never started (an orphan from a pre-fix zen-gate) has no
// *os.Process to kill, so the restart guard has to reap it by the pid we
// recorded when we launched it. The pid must be verified against the binary it
// actually runs, or a recycled pid gets murdered.
func TestReapOrphanTerminatesOurOwnSidecarOnly(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	port := reservedPort(t)
	addr := "127.0.0.1:" + port
	holder := holdPort(t, port)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writePidfile(t, holder.Process.Pid, exe)

	if err := reapOrphan([]string{addr}, 5*time.Second); err != nil {
		t.Fatalf("reapOrphan: %v", err)
	}
	if !portsFree([]string{addr}, time.Second) {
		t.Errorf("%s is still bound — the orphan from the previous run blocks every restart", addr)
	}
}

// A stale pid file that now points at somebody else's process must never be a
// licence to kill: reapOrphan leaves the stranger alone.
func TestReapOrphanSkipsARecycledPid(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	port := reservedPort(t)
	addr := "127.0.0.1:" + port
	holder := holdPort(t, port)
	writePidfile(t, holder.Process.Pid, `C:\somewhere\else\not-sing-box.exe`)

	err := reapOrphan([]string{addr}, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("reapOrphan killed a process that is not our sidecar")
	}
	if portsFree([]string{addr}, 300*time.Millisecond) {
		t.Errorf("the port went free, so the wrong process was terminated")
	}
}

func TestPortsFreeSeesAHeldPort(t *testing.T) {
	port := reservedPort(t)
	holdPort(t, port)

	addr := "127.0.0.1:" + port
	if portsFree([]string{addr}, 400*time.Millisecond) {
		t.Fatalf("%s was busy but portsFree said otherwise", addr)
	}
	if portsFree([]string{"127.0.0.1:" + reservedPort(t)}, time.Second) != true {
		t.Errorf("a free port reported busy")
	}
}

// Two paths to one binary must read as one image, or the reaper refuses to kill
// its own sidecar — on macOS ps reports /var/folders/… while the recorded path
// is /private/var/folders/….
func TestSameImageResolvesSymlinkedPaths(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "sing-box")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "sb-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this machine cannot create symlinks: %v", err)
	}
	if !sameImage(link, real) {
		t.Errorf("the same binary reached by two paths read as different images")
	}
	if sameImage(real, filepath.Join(dir, "other-name")) {
		t.Errorf("two different binaries read as the same image")
	}
}

// The sidecar writes one inbound per node, so the restart guard must wait for
// the whole range — checking only the first port would still crash-loop the
// rest.
func TestInboundPortsCoverEveryNode(t *testing.T) {
	got := inboundPorts(3)
	want := []string{
		fmt.Sprintf("127.0.0.1:%d", PortBase),
		fmt.Sprintf("127.0.0.1:%d", PortBase+1),
		fmt.Sprintf("127.0.0.1:%d", PortBase+2),
	}
	if len(got) != len(want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port %d = %s, want %s", i, got[i], want[i])
		}
	}
	if len(inboundPorts(0)) != 1 {
		t.Errorf("an empty pool must still guard the first inbound")
	}
}
