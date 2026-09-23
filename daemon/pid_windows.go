//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"strconv"
)

// pidAlive checks whether a process with the given PID is still running.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH")
	out, err := cmd.Output()
	if err != nil {
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			return false
		}
		// On Windows, FindProcess always succeeds; signal test is best-effort.
		_ = proc
		return false
	}
	return len(out) > 0 && !containsNoTasks(out)
}

func containsNoTasks(out []byte) bool {
	for i := 0; i+17 <= len(out); i++ {
		if string(out[i:i+17]) == "INFO: No tasks ar" {
			return true
		}
	}
	return false
}
