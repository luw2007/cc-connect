package herdr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("herdr", New)
}

// Agent drives a CLI process hosted in a herdr-managed pane. Unlike the tmux
// agent (which shares one long-lived session/window across cc-connect
// sessions by default), this always creates one herdr pane per cc-connect
// session — herdr panes are cheap to create/destroy programmatically via
// agent.start, so there's no equivalent to tmux's "attach to my existing
// manually-set-up session" use case to preserve.
type Agent struct {
	socketPath    string
	workDir       string
	initCmd       string // shell command run in the pane, e.g. "claude" or "codex"
	namePrefix    string
	startupWaitMs int
	pollMs        int
	mu            sync.RWMutex
}

func New(opts map[string]any) (core.Agent, error) {
	initCmd, _ := opts["init_command"].(string)
	if initCmd == "" {
		return nil, fmt.Errorf("herdr: 'init_command' option is required (e.g. \"claude\")")
	}

	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}

	socketPath, _ := opts["socket_path"].(string)
	if socketPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("herdr: resolve home dir for default socket_path: %w", err)
		}
		socketPath = filepath.Join(home, ".config", "herdr", "herdr.sock")
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("herdr: socket not found at %s (is `herdr` running?): %w", socketPath, err)
	}

	namePrefix, _ := opts["name_prefix"].(string)
	if namePrefix == "" {
		namePrefix = "cc-"
	}

	startupWaitMs := intOpt(opts["startup_wait_ms"], 2000)
	pollMs := intOpt(opts["poll_interval_ms"], 200)
	if pollMs <= 0 {
		pollMs = 200
	}

	return &Agent{
		socketPath:    socketPath,
		workDir:       workDir,
		initCmd:       initCmd,
		namePrefix:    namePrefix,
		startupWaitMs: startupWaitMs,
		pollMs:        pollMs,
	}, nil
}

// intOpt reads an option that may arrive as int64, int, or float64 depending
// on the TOML/JSON decoder (mirrors agent/tmux's same pattern).
func intOpt(v any, def int) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	default:
		return def
	}
}

func (a *Agent) Name() string { return "herdr" }

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.RLock()
	workDir := a.workDir
	initCmd := a.initCmd
	startupWaitMs := a.startupWaitMs
	pollMs := a.pollMs
	a.mu.RUnlock()

	c := newClient(a.socketPath)

	// The herdr pane's `name` field doubles as the cc-connect session ID
	// (mirrors tmux's window-per-session naming) so resumes reattach to the
	// same pane instead of spawning a new process each time.
	name := sessionID
	if name == "" {
		name = a.namePrefix + core.GenerateToken(8)
	}

	info, exists, err := c.findByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("herdr: list agents: %w", err)
	}

	if !exists {
		argv := []string{"sh", "-c", initCmd}
		if _, err := c.agentStart(ctx, name, workDir, argv, nil); err != nil {
			return nil, fmt.Errorf("herdr: start pane %q: %w", name, err)
		}
		slog.Info("herdr: created pane", "name", name, "work_dir", workDir, "init_command", initCmd)
		if startupWaitMs > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(startupWaitMs) * time.Millisecond):
			}
		}
	} else {
		slog.Info("herdr: resumed pane", "name", name, "pane_id", info.PaneID)
	}

	return newHerdrSession(ctx, c, name, workDir, time.Duration(pollMs)*time.Millisecond), nil
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func (a *Agent) Stop() error { return nil }

// WorkspaceAgentOptions implements core.WorkspaceAgentOptionSnapshotter so the
// engine can seed per-workspace agent instances with herdr-specific options
// in multi-workspace mode. work_dir is intentionally omitted; the engine
// sets it on the cloned instance.
func (a *Agent) WorkspaceAgentOptions() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return map[string]any{
		"init_command":     a.initCmd,
		"socket_path":      a.socketPath,
		"name_prefix":      a.namePrefix,
		"startup_wait_ms":  a.startupWaitMs,
		"poll_interval_ms": a.pollMs,
	}
}

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("herdr: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workDir
}
