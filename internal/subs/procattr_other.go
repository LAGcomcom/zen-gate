//go:build !windows

package subs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// singBoxProcAttr is a no-op off Windows: a daemonised background process
// needs no window-hiding attributes there.
func singBoxProcAttr() *syscall.SysProcAttr { return nil }

// tieToParent is a no-op off Windows; the sidecar there is reaped by the
// process group the app shares with it.
func tieToParent(p *os.Process) error {
	_ = p
	return nil
}

// runningImage reports the executable a pid is running. Only Linux has
// /proc/<pid>/exe; darwin and the BSDs have no procfs, so the image is read
// from ps. When nothing can be established the caller refuses to kill — an
// unknown process is never treated as our own sidecar.
func runningImage(pid int) (string, error) {
	if exe, err := filepath.EvalSymlinks("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return exe, nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", fmt.Errorf("进程 %d 不存在", pid)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("进程 %d 的映像读不到", pid)
	}
	return name, nil
}

func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
