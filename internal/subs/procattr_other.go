//go:build !windows

package subs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func runningImage(pid int) (string, error) {
	exe, err := filepath.EvalSymlinks("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", fmt.Errorf("进程 %d 不存在", pid)
	}
	return exe, nil
}

func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
