//go:build !windows

package claudecode

import (
	"strings"
	"testing"
)

func TestTmuxAvailable(t *testing.T) {
	// tmuxAvailable must not panic; result depends on environment
	_ = tmuxAvailable()
}

func TestCreateDestroySidecarPane(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not installed")
	}

	name, err := createSidecarPane("abc123456789xyz")
	if err != nil {
		t.Fatalf("createSidecarPane: %v", err)
	}
	if !strings.HasPrefix(name, tmuxSidecarPrefix) {
		t.Errorf("session name %q missing prefix %q", name, tmuxSidecarPrefix)
	}
	// name should use first 12 chars of the session ID
	if name != tmuxSidecarPrefix+"abc123456789" {
		t.Errorf("unexpected session name %q", name)
	}

	if err := destroySidecarPane(name); err != nil {
		t.Errorf("destroySidecarPane: %v", err)
	}
}

func TestCreateSidecarPane_ShortID(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not installed")
	}

	name, err := createSidecarPane("short")
	if err != nil {
		t.Fatalf("createSidecarPane with short id: %v", err)
	}
	if name != tmuxSidecarPrefix+"short" {
		t.Errorf("unexpected session name %q", name)
	}
	_ = destroySidecarPane(name)
}

func TestCaptureSidecarPane(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not installed")
	}

	name, err := createSidecarPane("capturetest01")
	if err != nil {
		t.Fatalf("createSidecarPane: %v", err)
	}
	defer destroySidecarPane(name)

	out, err := captureSidecarPane(name)
	if err != nil {
		t.Fatalf("captureSidecarPane: %v", err)
	}
	// capture-pane output may be empty or whitespace for a fresh pane — just ensure no error
	_ = out
}

func TestDestroyNonexistentPane(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not installed")
	}
	// Destroying a non-existent session should return an error (tmux exits non-zero)
	err := destroySidecarPane("cc-connect-doesnotexist99")
	if err == nil {
		t.Error("expected error destroying non-existent session, got nil")
	}
}

func TestSidecarNamePrefix(t *testing.T) {
	// Name derivation is pure logic — no tmux required
	tests := []struct {
		id   string
		want string
	}{
		{"abcdefghijklmnop", tmuxSidecarPrefix + "abcdefghijkl"},
		{"short", tmuxSidecarPrefix + "short"},
		{"exactly12345", tmuxSidecarPrefix + "exactly12345"},
	}
	for _, tc := range tests {
		name := tmuxSidecarPrefix + tc.id
		if len(tc.id) > 12 {
			name = tmuxSidecarPrefix + tc.id[:12]
		}
		if name != tc.want {
			t.Errorf("id=%q: got %q, want %q", tc.id, name, tc.want)
		}
	}
}
