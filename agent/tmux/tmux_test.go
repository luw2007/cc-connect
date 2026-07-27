package tmux

import (
	"context"
	"testing"
	"time"
)

func TestNewAgentValidation(t *testing.T) {
	// Missing session name should fail
	_, err := New(map[string]any{})
	if err == nil {
		t.Error("expected error when session is empty")
	}

	// With session name but tmux not in PATH - may fail on systems without tmux,
	// so we just verify the session check happens before the tmux PATH check.
}

// TestResolveTargetUniquePerWorkDir verifies that two workDirs sharing the same
// basename but different parent paths never map to the same tmux window target.
func TestResolveTargetUniquePerWorkDir(t *testing.T) {
	a := &Agent{sessionName: "mywork", pane: "0"}

	target1, win1 := a.resolveTarget("mywork", "0", "/repo/a/app")
	target2, win2 := a.resolveTarget("mywork", "0", "/repo/b/app")

	if target1 == target2 {
		t.Errorf("resolveTarget: collision — /repo/a/app and /repo/b/app both produced %q", target1)
	}
	if win1 == win2 {
		t.Errorf("uniqueWindowName: collision — /repo/a/app and /repo/b/app both produced %q", win1)
	}
}

// TestResolveTargetStable verifies that the same workDir always yields the same target.
func TestResolveTargetStable(t *testing.T) {
	a := &Agent{sessionName: "mywork", pane: "0"}

	t1, w1 := a.resolveTarget("mywork", "0", "/repo/a/app")
	t2, w2 := a.resolveTarget("mywork", "0", "/repo/a/app")

	if t1 != t2 || w1 != w2 {
		t.Errorf("resolveTarget not deterministic: got %q/%q then %q/%q", t1, w1, t2, w2)
	}
}

// TestNewTmuxSessionWorkDir verifies that the workDir is stored in the session so
// that file attachments are saved relative to the workspace, not to ".".
func TestNewTmuxSessionWorkDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := newTmuxSession(ctx, "sess:win", "sid1", "", 200*time.Millisecond, false, nil, "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.workDir != "/tmp/workspace" {
		t.Errorf("workDir = %q, want /tmp/workspace", s.workDir)
	}
}
