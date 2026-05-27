//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// pidAlive checks whether a process with the given PID is still running.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return os.IsPermission(err)
}
