# RFC-001: Tmux Sidecar for Claude Code Terminal Access

## Problem

`/terminal` and `/screenshot` commands fail for Claude Code sessions:
- `claudeSession` starts CLI via `exec.Command` + `StdinPipe`/`StdoutPipe` (JSON pipe mode)
- No terminal exists → `TerminalAttacher` never satisfied → web terminal and screenshot are dead paths

## Design: Sidecar Renderer

Keep existing direct-pipe spawn unchanged. Create a **sidecar tmux session** where a Go renderer writes decoded event summaries.

```
┌──────────────────────────────────────────────────────────────┐
│  claudeSession                                               │
│                                                              │
│  ┌─────────────────────────────┐                             │
│  │ claude CLI (exec.Command)   │  ← existing, UNCHANGED     │
│  │  stdin ← JSON pipe          │                             │
│  │  stdout → JSON pipe          │                             │
│  └─────────────┬───────────────┘                             │
│                │ events channel                               │
│                ▼                                              │
│  ┌─────────────────────────────┐                             │
│  │ readLoop() → Event stream   │  ← existing, UNCHANGED     │
│  └─────────────┬───────────────┘                             │
│                │                                              │
│       ┌────────┴────────┐                                    │
│       ▼                 ▼                                    │
│  Engine (existing)   renderToPane() [NEW]                    │
│                         │                                    │
│                         ▼                                    │
│  ┌─────────────────────────────┐                             │
│  │ tmux session "cc-<id>"      │  ← NEW sidecar             │
│  │  Shows: tool calls, edits,  │                             │
│  │  assistant text, errors     │                             │
│  └─────────────────────────────┘                             │
│                                                              │
│  /terminal  → AttachTerminal() → newTmuxPipe("cc-<id>")     │
│  /screenshot → tmux capture-pane -t "cc-<id>" -p            │
└──────────────────────────────────────────────────────────────┘
```

### Why This Works

- **Zero deadlock risk**: No FIFOs, no pipe reordering
- **Zero regression**: Existing pipe spawn completely unchanged
- **Useful pane content**: Renderer formats events as human-readable terminal output (tool calls, file edits, assistant responses, errors)
- **TerminalAttacher satisfied**: `tmux attach-session -t cc-<id>` via existing `newTmuxPipe()`
- **Screenshot works**: `tmux capture-pane -p` produces ANSI content for `RenderANSIToPNG()`

### Limitation

The tmux pane is a *rendered view*, not the "real" CLI terminal. Interactive typing in web terminal goes to the pane's `cat` process, not Claude's stdin. This is acceptable because cc-connect controls Claude via JSON pipe — there's no interactive terminal use case.

## Implementation

### Phase 1: Tmux Sidecar Lifecycle

```go
// agent/claudecode/session_tmux.go (NEW)

const tmuxPrefix = "cc-connect-"

func tmuxAvailable() bool {
    _, err := exec.LookPath("tmux")
    return err == nil
}

func createSidecarPane(sessionID string) (string, error) {
    name := tmuxPrefix + sessionID[:12]
    cmd := exec.Command("tmux", "new-session", "-d",
        "-s", name, "-x", "200", "-y", "50", "--", "cat")
    if err := cmd.Run(); err != nil {
        return "", err
    }
    return name, nil
}

func destroySidecarPane(name string) {
    exec.Command("tmux", "kill-session", "-t", name).Run()
}
```

### Phase 2: Event Renderer

```go
// agent/claudecode/pane_renderer.go (NEW)

func (cs *claudeSession) renderToPane(ctx context.Context) {
    // Subscribe to a copy of the event stream
    // Format each event type as human-readable ANSI text:
    //   - AssistantText → white text with word wrap
    //   - ToolUse       → cyan "[Tool] name: input summary"
    //   - ToolResult    → dim "  → result summary (truncated)"  
    //   - Error         → red "ERROR: message"
    //   - Permission    → yellow "⚠ Permission: tool_name"
    // Write to pane via: tmux send-keys -t <name> -l "line\n"
}
```

### Phase 3: Wire Into Session

```go
// agent/claudecode/session.go — additions

type claudeSession struct {
    // ... existing fields ...
    tmuxSession string // sidecar tmux session name; empty = unavailable
    paneEvents  chan core.Event // forked event channel for renderer
}

func newClaudeSession(...) (*claudeSession, error) {
    // ... existing spawn logic (UNCHANGED) ...

    cs := &claudeSession{
        cmd:   cmd,
        stdin: stdin,
        // ...
    }

    // NEW: create sidecar if tmux available and not isolated
    if !spawnOpts.IsolationMode() && tmuxAvailable() {
        if name, err := createSidecarPane(sessionID); err == nil {
            cs.tmuxSession = name
            cs.paneEvents = make(chan core.Event, 64)
            go cs.renderToPane(cs.ctx)
        }
    }

    go cs.readLoop(stdout, &stderrBuf) // existing
    return cs, nil
}

// Implement TerminalAttacher
func (cs *claudeSession) AttachTerminal() (io.ReadWriteCloser, error) {
    if cs.tmuxSession == "" {
        return nil, fmt.Errorf("no tmux session")
    }
    return newTmuxPipe(cs.tmuxSession)
}

// Cleanup in Close()
func (cs *claudeSession) Close() error {
    // ... existing graceful shutdown ...
    if cs.tmuxSession != "" {
        destroySidecarPane(cs.tmuxSession)
    }
    return nil
}
```

### Phase 4: Event Forking in readLoop

The renderer needs events without interfering with the engine's consumption. Fork events in `handleReadLoopLine`:

```go
func (cs *claudeSession) handleReadLoopLine(line string) {
    // ... existing parse logic ...
    evt := // parsed event

    // Fork to pane renderer (non-blocking)
    if cs.paneEvents != nil {
        select {
        case cs.paneEvents <- evt:
        default: // drop if renderer is slow
        }
    }

    // Existing: send to engine
    cs.events <- evt
}
```

### Phase 5: Screenshot via capture-pane

```go
// agent/claudecode/session_tmux.go

func (cs *claudeSession) CapturePane() (string, error) {
    if cs.tmuxSession == "" {
        return "", fmt.Errorf("no tmux session")
    }
    out, err := exec.Command("tmux", "capture-pane",
        "-t", cs.tmuxSession, "-p", "-e").Output()
    return string(out), err
}
```

This feeds directly into the existing `RenderANSIToPNG()` in `core/terminal_screenshot.go`.

## Config

```toml
[projects.agent.options]
# Enable tmux sidecar for /terminal and /screenshot
# "auto" = use if tmux available, "never" = disable
terminal_backend = "auto"
```

## Files Changed

| File | Change |
|------|--------|
| `agent/claudecode/session_tmux.go` | NEW: tmux lifecycle (create/destroy/check) |
| `agent/claudecode/pane_renderer.go` | NEW: event→ANSI renderer, tmux send-keys writer |
| `agent/claudecode/session.go` | Add tmuxSession field, event forking, AttachTerminal(), cleanup |
| `agent/claudecode/claudecode.go` | Pass terminal_backend config |
| `config/config.go` | Add TerminalBackend option |
| `core/terminal_screenshot.go` | Add CapturePane-based path (optional) |

## Edge Cases

| Case | Handling |
|------|---------|
| tmux not installed | Graceful fallback: tmuxSession stays empty, /terminal reports unavailable |
| Session crash | `Close()` always calls `destroySidecarPane()`; startup reap stale `cc-connect-*` sessions |
| run_as_user mode | Skip tmux sidecar (tmux session ownership mismatch) |
| Windows | Skip entirely (tmux_pty_windows.go stub) |
| Session naming collision | Use 12-char prefix of UUID (48-bit space) + check `tmux has-session` |
| Long-running session | Pane scrollback grows; set `set-option -t <name> history-limit 5000` |
| Renderer goroutine leak | Exits when `cs.ctx` is cancelled (session close) |

## Test Plan

1. Unit: `createSidecarPane` / `destroySidecarPane` lifecycle
2. Unit: `renderToPane` formatting for each event type
3. Integration: Start session → verify `AttachTerminal()` returns working RWC
4. Integration: Start session → verify `capture-pane` produces non-empty ANSI
5. Regression: `terminal_backend = "never"` → all existing tests unchanged
6. Regression: Tests pass on systems without tmux installed
