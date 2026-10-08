//go:build windows

package subs

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// singBoxProcAttr hides the sidecar's console window: sing-box runs for the
// whole app lifetime and a stray terminal would sit in the taskbar.
func singBoxProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// kernel32 does export QueryFullProcessImageNameW but the Go binding does not,
// so it is resolved by name.
var procQueryFullProcessImageName = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

// killJob holds every sidecar this process launched. The last handle to a job
// with KILL_ON_JOB_CLOSE belongs to zen-gate, so the OS terminates the child
// when this process dies — killed, crashed, or closed by the window manager.
// Without it an orphan keeps the inbound ports, every later start fails to
// bind, and the supervisor restarts a process that can never come up.
var (
	killJobMu  sync.Mutex
	killJobHnd windows.Handle
)

func ensureKillJob() (windows.Handle, error) {
	killJobMu.Lock()
	defer killJobMu.Unlock()
	if killJobHnd != 0 {
		return killJobHnd, nil
	}
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("创建回收作业: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("设置回收作业: %w", err)
	}
	killJobHnd = h
	return h, nil
}

// tieToParent enrols the sidecar in the kill job. The handle is deliberately
// never closed: closing it would kill a healthy sidecar on every refresh.
func tieToParent(p *os.Process) error {
	if p == nil {
		return nil
	}
	job, err := ensureKillJob()
	if err != nil {
		return err
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_CREATE_PROCESS|windows.PROCESS_TERMINATE,
		false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("打开侧车进程: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("把侧车并入回收作业: %w", err)
	}
	return nil
}

func killJobHandle() (windows.Handle, bool) {
	killJobMu.Lock()
	defer killJobMu.Unlock()
	return killJobHnd, killJobHnd != 0
}

// runningImage is the executable the pid actually runs, used to prove a
// recorded pid still belongs to the sidecar before killing it.
func runningImage(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("进程 %d 不存在", pid)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH+1)
	size := uint32(len(buf))
	ret, _, errno := procQueryFullProcessImageName.Call(
		uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return "", fmt.Errorf("查询进程 %d 的可执行路径: %v", pid, errno)
	}
	return windows.UTF16ToString(buf[:size]), nil
}

func terminatePID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("打开进程 %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("终止进程 %d: %w", pid, err)
	}
	return nil
}
