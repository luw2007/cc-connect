package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExtractNew is ported verbatim from agent/tmux/tmux_test.go — extractNew
// is generic "diff two scrollback snapshots" logic with no tmux/herdr calls,
// so the same behavioral spec applies unchanged to this backend.
func TestExtractNew(t *testing.T) {
	tests := []struct {
		name     string
		baseline string
		current  string
		want     string
	}{
		{
			name:     "no change",
			baseline: "foo\nbar",
			current:  "foo\nbar",
			want:     "",
		},
		{
			name:     "empty baseline",
			baseline: "",
			current:  "hello",
			want:     "hello",
		},
		{
			name:     "content grew (fast path)",
			baseline: "foo\nbar",
			current:  "foo\nbar\nbaz",
			want:     "baz",
		},
		{
			name:     "new line after prompt",
			baseline: "user@host:~$ ",
			current:  "user@host:~$ ls\nfile1\nfile2\nuser@host:~$ ",
			want:     "ls\nfile1\nfile2\nuser@host:~$ ",
		},
		{
			name:     "anchor overlap",
			baseline: "line1\nline2\nline3\nline4\nline5",
			current:  "line3\nline4\nline5\nnew1\nnew2",
			want:     "new1\nnew2",
		},
		{
			name:     "fully scrolled - return all current",
			baseline: "old1\nold2\nold3",
			current:  "new1\nnew2\nnew3",
			want:     "new1\nnew2\nnew3",
		},
		{
			name:     "TUI redrawn - shared frame, response replaces prompt",
			baseline: "╭─ Claude ─╮\n\n>",
			current:  "╭─ Claude ─╮\n\nThe answer is 42.\n\n>",
			want:     "The answer is 42.",
		},
		{
			name:     "TUI redrawn - multi-line response",
			baseline: "header\n\n>",
			current:  "header\n\nLine one.\nLine two.\n\n>",
			want:     "Line one.\nLine two.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractNew(tt.baseline, tt.current)
			if got != tt.want {
				t.Errorf("extractNew() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNormalizeCapture only covers trailing-whitespace trimming — unlike
// tmux's version, this package's normalizeCapture does not strip ANSI codes
// (herdr's agent.read already does that server-side with strip_ansi=true).
func TestNormalizeCapture(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "strip trailing spaces per line",
			raw:  "hello   \nworld   \n",
			want: "hello\nworld",
		},
		{
			name: "strip trailing tabs and carriage returns",
			raw:  "foo\t\r\nbar\r\n",
			want: "foo\nbar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCapture(tt.raw)
			if got != tt.want {
				t.Errorf("normalizeCapture() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	// Missing init_command should fail before any socket access.
	if _, err := New(map[string]any{}); err == nil {
		t.Error("expected error when init_command is empty")
	}
}

func TestNewHerdrSessionWorkDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient("/nonexistent.sock") // not dialed by this test
	s := newHerdrSession(ctx, c, "cc-test1", "/tmp/workspace", 200*time.Millisecond)
	defer s.Close()

	if s.workDir != "/tmp/workspace" {
		t.Errorf("workDir = %q, want /tmp/workspace", s.workDir)
	}
	if s.CurrentSessionID() != "cc-test1" {
		t.Errorf("CurrentSessionID() = %q, want cc-test1", s.CurrentSessionID())
	}
	if !s.Alive() {
		t.Error("expected new session to be alive")
	}
}

// mockHerdrServer starts a minimal one-request-per-connection Unix socket
// server that mirrors herdr's real behavior (see client.go's protocol
// notes), driven by a caller-supplied responder function.
//
// Uses a short-lived temp dir with a random suffix rather than t.TempDir():
// t.TempDir() embeds the full test name in the path, and Unix socket paths
// are capped at ~104 bytes on macOS (sockaddr_un.sun_path) — long test
// names like TestClientCallOneRequestPerConnection blow that limit and fail
// with "bind: invalid argument".
func mockHerdrServer(t *testing.T, respond func(method string, params json.RawMessage) rpcResponse) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil && line == "" {
					return
				}
				var req struct {
					ID     string          `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal([]byte(line), &req); err != nil {
					return
				}
				resp := respond(req.Method, req.Params)
				resp.ID = req.ID
				b, _ := json.Marshal(resp)
				_, _ = c.Write(append(b, '\n'))
			}(conn)
		}
	}()
	return sockPath
}

func TestClientCallSuccess(t *testing.T) {
	sockPath := mockHerdrServer(t, func(method string, _ json.RawMessage) rpcResponse {
		if method != "agent.get" {
			t.Errorf("unexpected method %q", method)
		}
		return rpcResponse{Result: json.RawMessage(`{"agent":{"terminal_id":"t1","pane_id":"wA:p1","agent_status":"idle"}}`)}
	})

	c := newClient(sockPath)
	info, err := c.agentGet(context.Background(), "cc-test")
	if err != nil {
		t.Fatalf("agentGet: %v", err)
	}
	if info.PaneID != "wA:p1" || info.AgentStatus != "idle" {
		t.Errorf("agentGet() = %+v, want pane_id=wA:p1 agent_status=idle", info)
	}
}

func TestClientCallError(t *testing.T) {
	sockPath := mockHerdrServer(t, func(_ string, _ json.RawMessage) rpcResponse {
		return rpcResponse{Error: &rpcError{Code: "agent_not_found", Message: "boom"}}
	})

	c := newClient(sockPath)
	_, err := c.agentGet(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestClientCallOneRequestPerConnection guards the documented herdr
// behavior this package relies on: the server answers exactly one request
// per connection. If a future herdr version changes this, client.go's
// "open a fresh connection per call" design would need revisiting.
func TestClientCallOneRequestPerConnection(t *testing.T) {
	var connCount int
	sockPath := mockHerdrServer(t, func(_ string, _ json.RawMessage) rpcResponse {
		connCount++
		return rpcResponse{Result: json.RawMessage(`{}`)}
	})

	c := newClient(sockPath)
	for i := 0; i < 3; i++ {
		if err := c.call(context.Background(), "ping", nil, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if connCount != 3 {
		t.Errorf("connCount = %d, want 3 (one connection per call)", connCount)
	}
}
