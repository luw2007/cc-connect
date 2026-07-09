package herdr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// recentLines bounds how much scrollback is fetched per read call.
const recentLines = 2000

type herdrSession struct {
	client  *client
	target  string // herdr pane name; also the cc-connect session ID (see herdr.go)
	workDir string
	pollInt time.Duration

	events    chan core.Event
	ctx       context.Context
	cancel    context.CancelFunc
	alive     atomic.Bool
	closeOnce sync.Once

	mu              sync.Mutex
	pollCancel      context.CancelFunc
	baselineCapture string
}

func newHerdrSession(ctx context.Context, c *client, target, workDir string, pollInt time.Duration) *herdrSession {
	sessCtx, cancel := context.WithCancel(ctx)
	s := &herdrSession{
		client:  c,
		target:  target,
		workDir: workDir,
		pollInt: pollInt,
		events:  make(chan core.Event, 128),
		ctx:     sessCtx,
		cancel:  cancel,
	}
	s.alive.Store(true)
	return s
}

func (s *herdrSession) Send(prompt string, _ []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.alive.Load() {
		return fmt.Errorf("herdr: session closed")
	}

	if len(files) > 0 {
		paths := core.SaveFilesToDisk(s.workDir, files)
		if len(paths) > 0 {
			prompt = prompt + "\n# files: " + strings.Join(paths, ", ")
		}
	}

	// Cancel any running poll from a previous Send.
	s.mu.Lock()
	if s.pollCancel != nil {
		s.pollCancel()
		s.pollCancel = nil
	}

	// Snapshot scrollback before sending; extractResponse diffs against this
	// to find exactly what the agent added (see agent/tmux's extractNew,
	// ported below — same TUI-redraw/scroll edge cases apply here).
	baseline, err := s.client.agentRead(s.ctx, s.target, "recent", recentLines)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("herdr: capture baseline: %w", err)
	}
	s.baselineCapture = baseline

	pollCtx, pollCancel := context.WithCancel(s.ctx)
	s.pollCancel = pollCancel
	s.mu.Unlock()

	// Enter is \r (13), not \n (10) — see client.go's agentSend doc comment.
	// Sending \n types a literal newline into the input box without
	// submitting it, which looks like the agent silently ignored the message.
	if err := s.client.agentSend(s.ctx, s.target, prompt+"\r"); err != nil {
		pollCancel()
		return fmt.Errorf("herdr: send: %w", err)
	}

	go s.poll(pollCtx)
	return nil
}

// poll waits for the turn to finish using two signals, whichever fires first:
//
//  1. Fast path — herdr's own agent_status (idle/working/blocked/done),
//     purpose-built detection for CLIs herdr recognizes (claude, codex, ...):
//     once the pane has been seen "working" or "blocked" at least once after
//     Send, a transition back to "idle" or "done" means the turn is over.
//
//  2. Slow path — screen-content stability, for CLIs herdr doesn't recognize
//     (agent_status stays "unknown" throughout). Mirrors agent/tmux's
//     poll()/extractNew() heuristic: content unchanged for several
//     consecutive polls means the agent is done producing output.
func (s *herdrSession) poll(ctx context.Context) {
	ticker := time.NewTicker(s.pollInt)
	defer ticker.Stop()

	sawWorking := false
	var prevContent string
	stable := 0
	idleThreshold := max(10, 5000/int(s.pollInt.Milliseconds()))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := s.client.agentGet(ctx, s.target)
			if err != nil {
				slog.Warn("herdr: agent.get error", "target", s.target, "err", err)
				continue
			}

			switch info.AgentStatus {
			case "working", "blocked":
				sawWorking = true
				continue
			case "idle", "done":
				if sawWorking {
					s.finish(ctx)
					return
				}
				// Status was already idle/done before we ever saw "working" —
				// detection may just be lagging. Fall through to the stability
				// check below so this doesn't wait forever if the wrapped CLI
				// never reports "working" (agent_status stuck "unknown").
			}

			current, err := s.client.agentRead(ctx, s.target, "visible", 0)
			if err != nil {
				slog.Warn("herdr: agent.read error", "target", s.target, "err", err)
				continue
			}
			if current == prevContent {
				stable++
			} else {
				stable = 0
				prevContent = current
			}
			if stable >= idleThreshold {
				s.finish(ctx)
				return
			}
		}
	}
}

func (s *herdrSession) finish(ctx context.Context) {
	// Guard against the race where Send() cancelled this poll just as we
	// were about to emit — avoids duplicate responses.
	select {
	case <-ctx.Done():
		return
	default:
	}
	response, err := s.extractResponse(ctx)
	if err != nil {
		slog.Warn("herdr: extract response failed", "target", s.target, "err", err)
	}
	s.safeSend(core.Event{Type: core.EventResult, Content: response, Done: true})
}

func (s *herdrSession) extractResponse(ctx context.Context) (string, error) {
	current, err := s.client.agentRead(ctx, s.target, "recent", recentLines)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	baseline := s.baselineCapture
	s.mu.Unlock()

	response := extractNew(normalizeCapture(baseline), normalizeCapture(current))
	if response != "" {
		response = "```\n" + response + "\n```"
	}
	return response, nil
}

func (s *herdrSession) safeSend(ev core.Event) {
	defer func() { _ = recover() }() // channel may be closed on session teardown
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

func (s *herdrSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return fmt.Errorf("herdr: permission requests are not supported")
}

func (s *herdrSession) Events() <-chan core.Event { return s.events }

func (s *herdrSession) CurrentSessionID() string { return s.target }

func (s *herdrSession) Alive() bool { return s.alive.Load() }

func (s *herdrSession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.mu.Lock()
		if s.pollCancel != nil {
			s.pollCancel()
			s.pollCancel = nil
		}
		s.mu.Unlock()
		s.cancel()
		close(s.events)
	})
	return nil
}

// InjectKey sends a named key (e.g. "C-c", "Escape", "Up", "Enter") to the
// pane, letting herdr translate it to the right terminal escape sequence.
// Unlike agentSend (which needs only a target name), pane.send_keys requires
// a resolved pane_id, so this looks it up via agent.get first.
func (s *herdrSession) InjectKey(key string) error {
	if !s.alive.Load() {
		return fmt.Errorf("herdr: session not alive")
	}
	info, err := s.client.agentGet(s.ctx, s.target)
	if err != nil {
		return fmt.Errorf("herdr: resolve pane for inject key: %w", err)
	}
	return s.client.paneSendKeys(s.ctx, info.PaneID, []string{key})
}

// CaptureBuffer returns the full scrollback + visible pane content.
func (s *herdrSession) CaptureBuffer() (string, error) {
	if !s.alive.Load() {
		return "", fmt.Errorf("herdr: session not alive")
	}
	return s.client.agentRead(s.ctx, s.target, "recent", recentLines)
}

// ── pure text helpers, ported from agent/tmux/session.go ──────────────────
//
// These don't call tmux or herdr — they're generic "diff two scrollback
// snapshots" logic that applies equally to any polling-based backend, so
// they're reused as-is rather than reinvented. See agent/tmux/tmux_test.go's
// TestExtractNew for the behavioral spec these satisfy.

// normalizeCapture trims trailing whitespace per line. Unlike tmux's version,
// this does not strip ANSI codes — herdr's agent.read already does that
// server-side when format="text" (used throughout this package).
func normalizeCapture(raw string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// extractNew returns the response text that appeared in current after the
// baseline. It handles three cases:
//  1. Linear output — current is baseline + new lines (HasPrefix fast path).
//  2. TUI redraws — terminal overwrites lines in place; find the longest
//     common line prefix shared by both snapshots, then return the new lines
//     that follow it in current, stripping the repeated trailing prompt lines.
//  3. Terminal scrolled — baseline has partially scrolled off; use a
//     shrinking anchor.
func extractNew(baseline, current string) string {
	if current == baseline {
		return ""
	}
	if baseline == "" {
		return current
	}

	// Fast path: linear output, content only grew.
	if strings.HasPrefix(current, baseline) {
		return strings.TrimLeft(current[len(baseline):], "\n")
	}

	baseLines := strings.Split(baseline, "\n")
	curLines := strings.Split(current, "\n")

	// TUI path: find how many leading lines the two snapshots share (the
	// static frame/header), then return the new lines that follow in current.
	commonLen := 0
	for i := 0; i < len(baseLines) && i < len(curLines); i++ {
		if baseLines[i] != curLines[i] {
			break
		}
		commonLen = i + 1
	}
	if commonLen > 0 && commonLen < len(curLines) {
		newLines := curLines[commonLen:]
		// Strip trailing lines that duplicate the baseline's suffix (e.g. the prompt).
		bl := baseLines
		for len(newLines) > 0 && len(bl) > 0 && newLines[len(newLines)-1] == bl[len(bl)-1] {
			newLines = newLines[:len(newLines)-1]
			bl = bl[:len(bl)-1]
		}
		result := strings.TrimRight(strings.Join(newLines, "\n"), "\n")
		if result != "" {
			return result
		}
	}

	// Scroll path: baseline has partially scrolled off the top; try
	// progressively shorter anchors from the end of baseline to find where
	// new content begins.
	maxAnchor := 5
	if len(baseLines) < maxAnchor {
		maxAnchor = len(baseLines)
	}
	for n := maxAnchor; n >= 1; n-- {
		anchor := strings.Join(baseLines[len(baseLines)-n:], "\n")
		if idx := strings.Index(current, anchor); idx >= 0 {
			rest := strings.TrimLeft(current[idx+len(anchor):], "\n")
			if rest != "" {
				return rest
			}
		}
	}

	return current
}
