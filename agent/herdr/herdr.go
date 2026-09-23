package herdr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("herdr", New)
	core.RegisterAgentController("herdr", NewController)
}

// Agent drives a CLI process hosted in a herdr-managed pane. Unlike the tmux
// agent (which shares one long-lived session/window across cc-connect
// sessions by default), this always creates one herdr pane per cc-connect
// session — herdr panes are cheap to create/destroy programmatically via
// agent.start, so there's no equivalent to tmux's "attach to my existing
// manually-set-up session" use case to preserve.
type Agent struct {
	socketPath      string
	workDir         string
	initCmd         string // shell command run in the pane, e.g. "claude" or "codex"
	namePrefix      string
	startupWaitMs   int
	pollMs          int
	attachExisting  bool
	blockedCard     bool
	streamOutput    bool
	subscribeEvents bool
	rpcTimeoutMs    int
	maxReadFailures int
	clientFactory   func() *client
	mu              sync.RWMutex
}

const defaultMaxReadFailures = 5

var protocol17AgentKinds = map[string]struct{}{
	"pi": {}, "claude": {}, "codex": {}, "gemini": {}, "cursor": {},
	"devin": {}, "agy": {}, "cline": {}, "omp": {}, "mastracode": {},
	"opencode": {}, "copilot": {}, "kimi": {}, "kiro": {}, "droid": {},
	"amp": {}, "grok": {}, "hermes": {}, "kilo": {}, "qodercli": {}, "maki": {},
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
	if pollMs < int(minPollInterval/time.Millisecond) {
		pollMs = int(minPollInterval / time.Millisecond)
	}
	attachExisting := boolOpt(opts["attach_existing"], true)
	blockedCard := boolOpt(opts["blocked_card"], true)
	streamOutput := boolOpt(opts["stream_output"], true)
	subscribeEvents := boolOpt(opts["subscribe_events"], true)
	rpcTimeoutMs := intOpt(opts["rpc_timeout_ms"], int(defaultRPCTimeout/time.Millisecond))
	if rpcTimeoutMs < 1000 {
		rpcTimeoutMs = 1000
	}
	maxReadFailures := intOpt(opts["max_read_failures"], defaultMaxReadFailures)
	if maxReadFailures < 1 {
		maxReadFailures = defaultMaxReadFailures
	}

	return &Agent{
		socketPath:      socketPath,
		workDir:         workDir,
		initCmd:         initCmd,
		namePrefix:      namePrefix,
		startupWaitMs:   startupWaitMs,
		pollMs:          pollMs,
		attachExisting:  attachExisting,
		blockedCard:     blockedCard,
		streamOutput:    streamOutput,
		subscribeEvents: subscribeEvents,
		rpcTimeoutMs:    rpcTimeoutMs,
		maxReadFailures: maxReadFailures,
	}, nil
}

func boolOpt(v any, def bool) bool {
	if value, ok := v.(bool); ok {
		return value
	}
	return def
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
	blockedCard := a.blockedCard
	streamOutput := a.streamOutput
	subscribeEvents := a.subscribeEvents
	rpcTimeoutMs := a.rpcTimeoutMs
	maxReadFailures := a.maxReadFailures
	a.mu.RUnlock()

	c := a.makeClient()
	c.rpcTimeout = time.Duration(rpcTimeoutMs) * time.Millisecond

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
		kind, args, ok := protocol17Kind(initCmd)
		if !ok {
			return nil, fmt.Errorf("herdr: init_command %q is not a recognized protocol-17 agent kind; creation is unavailable, attach an existing named herdr agent instead", initCmd)
		}
		tabID, paneID, err := c.tabCreate(ctx, workDir, name)
		if err != nil {
			return nil, fmt.Errorf("herdr: create tab for %q: %w", name, err)
		}
		started, startErr := c.agentStart(ctx, name, kind, paneID, args)
		if startErr != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), c.rpcTimeout)
			cleanupErr := c.tabClose(cleanupCtx, tabID)
			cleanupCancel()
			if cleanupErr != nil {
				return nil, fmt.Errorf("herdr: start agent %q: %w", name, errors.Join(startErr, fmt.Errorf("cleanup tab %s: %w", tabID, cleanupErr)))
			}
			return nil, fmt.Errorf("herdr: start agent %q: %w", name, startErr)
		}
		info = started
		slog.Info("herdr: created agent", "name", name, "kind", kind, "pane_id", paneID, "tab_id", tabID, "work_dir", workDir)
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

	session := newHerdrSession(ctx, c, name, workDir, time.Duration(pollMs)*time.Millisecond, subscribeEvents, blockedCard, streamOutput, maxReadFailures)
	if exists && info.AgentStatus == "blocked" && blockedCard {
		// Cold recovery is synchronous so the buffered permission event exists
		// before StartSession returns and no untracked goroutine can outlive Close.
		session.handleBlocked(nil)
	}
	return session, nil
}

func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	a.mu.RLock()
	attachExisting := a.attachExisting
	namePrefix := a.namePrefix
	a.mu.RUnlock()
	c := a.makeClient()
	a.mu.RLock()
	c.rpcTimeout = time.Duration(a.rpcTimeoutMs) * time.Millisecond
	a.mu.RUnlock()
	agents, err := c.agentList(ctx)
	if err != nil {
		return nil, fmt.Errorf("herdr: list agents: %w", err)
	}
	result := make([]core.AgentSessionInfo, 0, len(agents))
	for _, info := range agents {
		if strings.TrimSpace(info.Name) == "" {
			slog.Debug("herdr: skipping unnamed agent", "pane_id", info.PaneID)
			continue
		}
		if !attachExisting && !strings.HasPrefix(info.Name, namePrefix) {
			continue
		}
		parts := make([]string, 0, 2)
		if title := strings.TrimSpace(info.TerminalTitle); title != "" {
			parts = append(parts, title)
		}
		if cwd := strings.TrimSpace(info.CWD); cwd != "" {
			parts = append(parts, cwd)
		}
		summary := strings.Join(parts, " · ")
		if summary == "" {
			summary = info.Name
		}
		result = append(result, core.AgentSessionInfo{
			ID:          info.Name,
			Summary:     summary,
			ProjectPath: info.CWD,
		})
	}
	return result, nil
}

func (a *Agent) makeClient() *client {
	a.mu.RLock()
	factory := a.clientFactory
	socketPath := a.socketPath
	a.mu.RUnlock()
	if factory != nil {
		return factory()
	}
	return newClient(socketPath)
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
		"init_command":      a.initCmd,
		"socket_path":       a.socketPath,
		"name_prefix":       a.namePrefix,
		"startup_wait_ms":   a.startupWaitMs,
		"poll_interval_ms":  a.pollMs,
		"attach_existing":   a.attachExisting,
		"blocked_card":      a.blockedCard,
		"stream_output":     a.streamOutput,
		"subscribe_events":  a.subscribeEvents,
		"rpc_timeout_ms":    a.rpcTimeoutMs,
		"max_read_failures": a.maxReadFailures,
	}
}

func protocol17Kind(command string) (kind string, args []string, ok bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", nil, false
	}
	kind = filepath.Base(fields[0])
	if _, ok := protocol17AgentKinds[kind]; !ok {
		return "", nil, false
	}
	return kind, fields[1:], true
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
