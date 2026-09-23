package cmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("cmux", New)
}

type Agent struct {
	socketPath       string
	password         string
	cursorPath       string
	workspaceFilter  string
	createWorkspaces bool
	workDir          string
	pollInterval     time.Duration
	feedReplyMode    string
	feedHookTimeout  time.Duration
	rpcTimeout       time.Duration

	client     *client
	mapper     *sessionMapper
	bus        *eventBus
	releaseBus func()

	mu       sync.RWMutex
	sessions map[string]*cmuxSession
	stopOnce sync.Once
}

func New(opts map[string]any) (core.Agent, error) {
	password, _ := opts["password"].(string)
	explicitSocket, _ := opts["socket_path"].(string)
	probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socketPath, err := resolveSocket(probeCtx, explicitSocket, password)
	if err != nil {
		return nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cmux: resolve home directory: %w", err)
	}
	cursorPath, _ := opts["events_cursor_file"].(string)
	if cursorPath == "" {
		cursorPath = filepath.Join(home, ".cc-connect", "cmux_events_cursor.json")
	} else {
		cursorPath = expandHome(cursorPath, home)
	}
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	workDir, err = absoluteWorkDir(workDir)
	if err != nil {
		return nil, err
	}
	workspaceFilter, _ := opts["workspace_filter"].(string)
	createWorkspaces := boolOpt(opts["create_workspaces"], true)
	pollMs := intOpt(opts["poll_interval_ms"], 3000)
	if pollMs < 100 {
		pollMs = 100
	}
	feedHookTimeoutMs := intOpt(opts["feed_hook_timeout_ms"], 120000)
	if feedHookTimeoutMs < 100 {
		feedHookTimeoutMs = 100
	}
	rpcTimeoutMs := intOpt(opts["rpc_timeout_ms"], 10000)
	if rpcTimeoutMs < 1000 {
		rpcTimeoutMs = 1000
	}
	feedReplyMode, _ := opts["feed_reply_default_mode"].(string)
	if feedReplyMode == "" {
		feedReplyMode = "once"
	}
	if !validReplyMode(feedReplyMode) {
		return nil, fmt.Errorf("cmux: invalid feed_reply_default_mode %q (want once, always, all, bypass, or deny)", feedReplyMode)
	}

	c := newClient(socketPath, password)
	c.rpcTimeout = time.Duration(rpcTimeoutMs) * time.Millisecond
	mapper := newSessionMapper(filepath.Join(home, ".cmuxterm"))
	bus, releaseBus := acquireEventBus(c, cursorPath, time.Duration(feedHookTimeoutMs)*time.Millisecond, mapper)
	return &Agent{
		socketPath:       socketPath,
		password:         password,
		cursorPath:       cursorPath,
		workspaceFilter:  workspaceFilter,
		createWorkspaces: createWorkspaces,
		workDir:          workDir,
		pollInterval:     time.Duration(pollMs) * time.Millisecond,
		feedReplyMode:    feedReplyMode,
		feedHookTimeout:  time.Duration(feedHookTimeoutMs) * time.Millisecond,
		rpcTimeout:       time.Duration(rpcTimeoutMs) * time.Millisecond,
		client:           c,
		mapper:           mapper,
		bus:              bus,
		releaseBus:       releaseBus,
		sessions:         make(map[string]*cmuxSession),
	}, nil
}

func intOpt(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return fallback
	}
}

func boolOpt(value any, fallback bool) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return fallback
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if len(path) >= 2 && path[:2] == "~/" {
		return filepath.Join(home, path[2:])
	}
	return path
}

func absoluteWorkDir(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cmux: resolve work_dir %q: %w", dir, err)
	}
	return filepath.Clean(absolute), nil
}

func (a *Agent) Name() string { return "cmux" }

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.RLock()
	workDir := a.workDir
	createWorkspaces := a.createWorkspaces
	pollInterval := a.pollInterval
	feedReplyMode := a.feedReplyMode
	a.mu.RUnlock()

	workspaces, err := a.client.listWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("cmux: list workspaces: %w", err)
	}
	workspace := matchWorkspace(workspaces, sessionID, workDir)
	if workspace == nil {
		if !createWorkspaces {
			return nil, fmt.Errorf("cmux: workspace %q not found and create_workspaces is false", sessionID)
		}
		name := sessionID
		if name == "" {
			name = "cc-" + core.GenerateToken(8)
		}
		created, err := a.client.newWorkspace(ctx, name, workDir, "")
		if err != nil {
			return nil, fmt.Errorf("cmux: create workspace %q: %w", name, err)
		}
		workspace = &created
		slog.Info("cmux: created workspace", "workspace_id", workspace.ID, "name", workspace.stableName(), "cwd", workspace.CWD)
	} else {
		slog.Info("cmux: attached workspace", "workspace_id", workspace.ID, "name", workspace.stableName(), "cwd", workspace.CWD)
	}
	if workspace.ID == "" {
		return nil, fmt.Errorf("cmux: workspace response omitted id")
	}
	surfaceID := workspace.SurfaceID
	if surfaceID == "" {
		surfaceID, _ = a.mapper.surfaceIDForWorkspace(workspace.ID)
	}
	if surfaceID == "" {
		// surface.read_text/surface.send_text ignore workspace_id and silently
		// fall back to an unrelated ambient surface when surface_id is blank
		// (verified live, PROBES.md) — refuse rather than risk reading from or
		// sending to the wrong pane.
		return nil, fmt.Errorf("cmux: could not resolve a terminal surface for workspace %q; no active agent hook session found in ~/.cmuxterm", workspace.ID)
	}
	sessionWorkDir := workspace.CWD
	if sessionWorkDir == "" {
		sessionWorkDir = workDir
	}
	session := newCmuxSession(ctx, a.client, a.bus, workspace.ID, surfaceID, sessionWorkDir, feedReplyMode, pollInterval)
	a.bus.register(session)
	a.bus.feed.register(session)
	a.mu.Lock()
	if previous := a.sessions[workspace.ID]; previous != nil {
		_ = previous.Close()
	}
	a.sessions[workspace.ID] = session
	a.mu.Unlock()
	return session, nil
}

func matchWorkspace(workspaces []workspaceInfo, sessionID, workDir string) *workspaceInfo {
	for index := range workspaces {
		if sessionID != "" && workspaces[index].ID == sessionID {
			workspace := workspaces[index]
			return &workspace
		}
	}
	if sessionID != "" {
		var matches []workspaceInfo
		for _, workspace := range workspaces {
			if workspace.stableName() == sessionID {
				matches = append(matches, workspace)
			}
		}
		if len(matches) == 1 {
			return &matches[0]
		}
	}
	if workDir != "" {
		var matches []workspaceInfo
		for _, workspace := range workspaces {
			if filepath.Clean(workspace.CWD) == filepath.Clean(workDir) {
				matches = append(matches, workspace)
			}
		}
		if len(matches) == 1 {
			return &matches[0]
		}
	}
	return nil
}

func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	a.mu.RLock()
	filter := a.workspaceFilter
	a.mu.RUnlock()
	workspaces, err := a.client.listWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("cmux: list workspaces: %w", err)
	}
	infos := make([]core.AgentSessionInfo, 0, len(workspaces))
	for _, workspace := range workspaces {
		if filter != "" {
			nameMatch, _ := filepath.Match(filter, workspace.stableName())
			cwdMatch, _ := filepath.Match(filter, workspace.CWD)
			if !nameMatch && !cwdMatch {
				continue
			}
		}
		modifiedAt := parseTimestamp(workspace.UpdatedAt)
		if activity := a.bus.activity(workspace.ID); activity.After(modifiedAt) {
			modifiedAt = activity
		}
		if activity := a.bus.feed.activity(workspace.ID); activity.After(modifiedAt) {
			modifiedAt = activity
		}
		if activity := a.mapper.workspaceUpdatedAt(workspace.ID); activity.After(modifiedAt) {
			modifiedAt = activity
		}
		summary := workspace.displayName()
		if lifecycle := a.mapper.workspaceLifecycle(workspace.ID); lifecycle != "" {
			summary += " [" + lifecycle + "]"
		}
		infos = append(infos, core.AgentSessionInfo{ID: workspace.ID, Summary: summary, ModifiedAt: modifiedAt, ProjectPath: workspace.CWD})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

func (a *Agent) Stop() error {
	var stopErr error
	a.stopOnce.Do(func() {
		a.mu.Lock()
		sessions := make([]*cmuxSession, 0, len(a.sessions))
		for _, session := range a.sessions {
			sessions = append(sessions, session)
		}
		a.sessions = make(map[string]*cmuxSession)
		a.mu.Unlock()
		for _, session := range sessions {
			if err := session.Close(); err != nil {
				stopErr = errors.Join(stopErr, err)
			}
		}
		a.releaseBus()
	})
	return stopErr
}

func (a *Agent) SetWorkDir(dir string) {
	absolute, err := absoluteWorkDir(dir)
	if err != nil {
		slog.Warn("cmux: ignoring invalid work_dir", "work_dir", dir, "err", err)
		return
	}
	a.mu.Lock()
	a.workDir = absolute
	a.mu.Unlock()
	slog.Info("cmux: work_dir changed", "work_dir", absolute)
}

func (a *Agent) GetWorkDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workDir
}

func (a *Agent) WorkspaceAgentOptions() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return map[string]any{
		"socket_path":             a.socketPath,
		"password":                a.password,
		"events_cursor_file":      a.cursorPath,
		"workspace_filter":        a.workspaceFilter,
		"create_workspaces":       a.createWorkspaces,
		"poll_interval_ms":        int(a.pollInterval / time.Millisecond),
		"feed_reply_default_mode": a.feedReplyMode,
		"feed_hook_timeout_ms":    int(a.feedHookTimeout / time.Millisecond),
		"rpc_timeout_ms":          int(a.rpcTimeout / time.Millisecond),
	}
}

func (a *Agent) CLIBinaryName() string  { return "cmux" }
func (a *Agent) CLIDisplayName() string { return "cmux" }

func (a *Agent) DoctorChecks(ctx context.Context) []core.DoctorCheckResult {
	start := time.Now()
	err := a.client.ping(ctx)
	latency := time.Since(start)
	results := make([]core.DoctorCheckResult, 0, 2)
	if err != nil {
		results = append(results, core.DoctorCheckResult{Name: "cmux socket", Status: core.DoctorFail, Detail: err.Error(), Latency: latency})
	} else {
		results = append(results, core.DoctorCheckResult{Name: "cmux socket", Status: core.DoctorPass, Detail: a.socketPath, Latency: latency})
	}
	if a.mapper.hasHookStore() {
		results = append(results, core.DoctorCheckResult{Name: "cmux agent hooks", Status: core.DoctorPass, Detail: "hook session mapping detected"})
	} else {
		results = append(results, core.DoctorCheckResult{Name: "cmux agent hooks", Status: core.DoctorWarn, Detail: "no hook session mapping found; enable cmux Claude Code integration"})
	}
	return results
}
