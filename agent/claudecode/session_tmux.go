//go:build !windows

package claudecode

import (
	"fmt"
	"os/exec"
	"strings"
)

const tmuxSidecarPrefix = "cc-connect-"

func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func createSidecarPane(sessionID string) (string, error) {
	name := tmuxSidecarPrefix + sessionID
	if len(sessionID) > 12 {
		name = tmuxSidecarPrefix + sessionID[:12]
	}
	cmd := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "50", "tail", "-f", "/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w: %s", err, strings.TrimSpace(string(out)))
	}
	setCmd := exec.Command("tmux", "set-option", "-t", name, "history-limit", "5000")
	if out, err := setCmd.CombinedOutput(); err != nil {
		_ = destroySidecarPane(name)
		return "", fmt.Errorf("tmux set-option history-limit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return name, nil
}

func destroySidecarPane(name string) error {
	return exec.Command("tmux", "kill-session", "-t", name).Run()
}

func captureSidecarPane(name string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", name, "-p", "-e").Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return string(out), nil
}

func reapStaleSidecars() {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, tmuxSidecarPrefix) {
			_ = exec.Command("tmux", "kill-session", "-t", line).Run()
		}
	}
}
