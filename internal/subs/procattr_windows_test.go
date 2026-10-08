//go:build windows

package subs

import (
	"os/exec"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 > nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

var kernel32IsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// inJob asks kernel32 whether the process handle is enrolled in the job. The
// Go binding does not export IsProcessInJob, so it is loaded by name.
func inJob(t *testing.T, pid int, job windows.Handle) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("open child %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)
	var ok byte
	ret, _, err := kernel32IsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&ok)))
	if ret == 0 {
		t.Fatalf("IsProcessInJob: %v", err)
	}
	return ok != 0
}

// A sidecar that outlives zen-gate is the whole crash loop: it keeps the
// inbound ports, m.cmd is gone so nothing can kill it, and every later start
// binds a busy port. Tying the child to a kill-on-close job makes the orphan
// structurally impossible — the OS reaps it however zen-gate died.
func TestTieToParentEnrollsTheChildInTheKillJob(t *testing.T) {
	cmd := startSleeper(t)
	if err := tieToParent(cmd.Process); err != nil {
		t.Fatalf("tieToParent: %v", err)
	}
	job, ok := killJobHandle()
	if !ok {
		t.Fatalf("no kill job exists — the child can outlive zen-gate and hold the ports")
	}
	if !inJob(t, cmd.Process.Pid, job) {
		t.Errorf("child pid %d is not in zen-gate's kill job", cmd.Process.Pid)
	}

	// The handle must be created once and kept open: closing it is what kills
	// everything inside, which would take a healthy sidecar down on refresh.
	second := startSleeper(t)
	if err := tieToParent(second.Process); err != nil {
		t.Fatalf("tieToParent (second): %v", err)
	}
	job2, _ := killJobHandle()
	if job2 != job {
		t.Errorf("a second job was created: %v vs %v", job2, job)
	}
	if !inJob(t, second.Process.Pid, job) {
		t.Errorf("second child is not in the shared job")
	}
}
