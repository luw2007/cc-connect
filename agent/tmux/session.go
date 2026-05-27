package tmux

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type tmuxSession struct {
	target          string // e.g., "mywork:0"
	sessionID       string
	workDir         string
	promptPat       *regexp.Regexp
	pollInt         time.Duration
	stripInputBlock bool
	stripPatterns   []*regexp.Regexp
	events          chan core.Event
	ctx             context.Context
	cancel          context.CancelFunc
	alive           atomic.Bool
	closeOnce       sync.Once

	mu              sync.Mutex
	pollCancel      context.CancelFunc
	baselineCapture string // full captureScrollback output at the time of the last Send()
}

func newTmuxSession(ctx context.Context, target, sessionID, promptPattern string, pollInt time.Duration, stripInputBlock bool, stripPatternStrs []string, workDir string) (*tmuxSession, error) {
	sessCtx, cancel := context.WithCancel(ctx)

	var pat *regexp.Regexp
	if promptPattern != "" {
		var err error
		pat, err = regexp.Compile(promptPattern)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("tmux: invalid prompt_pattern %q: %w", promptPattern, err)
		}
	}

	var stripPats []*regexp.Regexp
	for _, s := range stripPatternStrs {
		re, err := regexp.Compile(s)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("tmux: invalid strip_pattern %q: %w", s, err)
		}
		stripPats = append(stripPats, re)
	}

	s := &tmuxSession{
		target:          target,
		sessionID:       sessionID,
		workDir:         workDir,
		promptPat:       pat,
		pollInt:         pollInt,
		stripInputBlock: stripInputBlock,
		stripPatterns:   stripPats,
		events:          make(chan core.Event, 128),
		ctx:       sessCtx,
		cancel:    cancel,
	}
	s.alive.Store(true)
	return s, nil
}

func (s *tmuxSession) Send(prompt string, _ []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.alive.Load() {
		return fmt.Errorf("tmux: session closed")
	}

	// Save attached files and append their paths to the prompt
	if len(files) > 0 {
		paths := core.SaveFilesToDisk(s.workDir, files)
		if len(paths) > 0 {
			prompt = prompt + "\n# files: " + strings.Join(paths, ", ")
		}
	}

	// Cancel any running poll from a previous Send
	s.mu.Lock()
	if s.pollCancel != nil {
		s.pollCancel()
		s.pollCancel = nil
	}

	// Snapshot the full scrollback (history + visible pane) before sending.
	// extractResponse diffs against this to find exactly what the agent added,
	// regardless of whether the TUI rewrites lines in-place or scrolls them.
	baseline, _ := captureScrollback(s.target)
	visibleBase, _ := capturePane(s.target) // for poll stability comparison
	s.baselineCapture = baseline

	pollCtx, pollCancel := context.WithCancel(s.ctx)
	s.pollCancel = pollCancel
	s.mu.Unlock()

	if err := sendKeys(s.target, prompt); err != nil {
		pollCancel()
		return fmt.Errorf("tmux: send-keys: %w", err)
	}

	go s.poll(pollCtx, visibleBase)
	return nil
}

func (s *tmuxSession) poll(ctx context.Context, baseline string) {
	ticker := time.NewTicker(s.pollInt)
	defer ticker.Stop()

	prev := baseline
	stable := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := capturePane(s.target)
			if err != nil {
				slog.Warn("tmux: capture-pane error", "target", s.target, "err", err)
				continue
			}

			if current == prev {
				stable++
			} else {
				stable = 0
				prev = current
			}

			// Done: pane stable AND changed from baseline.
			// fast path — prompt pattern matched; slow path — 5 s idle fallback.
			if stable >= 2 && current != baseline {
				trimmed := strings.TrimRight(current, " \t\n")
				promptOK := s.promptPat == nil || s.promptPat.MatchString(trimmed)
				idleN := max(10, 5000/int(s.pollInt.Milliseconds()))
				if promptOK || stable >= idleN {
					// Guard against the race where Send() cancelled this poll
					// just as we were about to emit — avoids duplicate responses.
					select {
					case <-ctx.Done():
						return
					default:
					}
					if !promptOK {
						lastLine := trimmed
						if nl := strings.LastIndex(trimmed, "\n"); nl >= 0 {
							lastLine = trimmed[nl+1:]
						}
						slog.Info("tmux: idle-done (prompt pattern did not match)", "target", s.target, "last_line", lastLine)
					}
					response := s.extractResponse()
					s.safeSend(core.Event{Type: core.EventResult, Content: response, Done: true})
					return
				}
			}
		}
	}
}

func (s *tmuxSession) safeSend(ev core.Event) {
	defer func() { _ = recover() }() // channel may be closed on session teardown
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

func (s *tmuxSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return fmt.Errorf("tmux: permission requests are not supported")
}

func (s *tmuxSession) Events() <-chan core.Event { return s.events }

func (s *tmuxSession) CurrentSessionID() string { return s.sessionID }

func (s *tmuxSession) Alive() bool { return s.alive.Load() }

func (s *tmuxSession) Close() error {
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

// ── tmux helpers ──────────────────────────────────────────────────────────────

func tmuxSessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// tmuxWindowExists checks whether a window or pane target (e.g. "sess:win") exists.
func tmuxWindowExists(target string) bool {
	return exec.Command("tmux", "has-session", "-t", target).Run() == nil
}

// createTmuxSession creates a new detached tmux session with the given window name.
func createTmuxSession(name, windowName, workDir, shell string) error {
	args := []string{"new-session", "-d", "-s", name, "-n", windowName}
	if workDir != "" && workDir != "." {
		args = append(args, "-c", workDir)
	}
	if shell != "" {
		args = append(args, shell)
	}
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	// Enable focus events so Claude Code doesn't warn about them being off.
	_ = exec.Command("tmux", "set-option", "-t", name, "-g", "focus-events", "on").Run()
	return nil
}

// createTmuxWindow adds a new window to an existing session.
// Using "session:" (trailing colon) tells tmux to pick the next free index,
// avoiding index collisions when multiple windows are created concurrently.
func createTmuxWindow(session, windowName, workDir string) error {
	args := []string{"new-window", "-d", "-t", session + ":", "-n", windowName}
	if workDir != "" && workDir != "." {
		args = append(args, "-c", workDir)
	}
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func capturePane(target string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return "", err
	}
	return normalizeCapture(string(out)), nil
}

func sendKeys(target, keys string) error {
	// -l (literal) prevents tmux from interpreting key names (C-c, Enter, Up, …)
	// embedded in the user's text. Enter is sent as a separate keystroke afterwards.
	out, err := exec.Command("tmux", "send-keys", "-t", target, "-l", keys).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err = exec.Command("tmux", "send-keys", "-t", target, "Enter").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractResponse diffs the current full scrollback against the snapshot taken
// in Send() and returns only what the agent added.
//
// Using extractNew on full captures (history + visible pane) handles both TUI
// rendering modes correctly:
//
//   - Append mode (agent scrolls content up): the baseline is a prefix of the
//     new capture, so the fast-path HasPrefix strips it and returns only the
//     new lines — no old visible-pane content leaks in.
//
//   - In-place rewrite mode (agent overwrites pane rows then scrolls): the
//     first divergence is at line N (the first pane row, now containing the
//     first response line), so extractNew returns everything from that point —
//     no lines are skipped.
func (s *tmuxSession) extractResponse() string {
	current, err := captureScrollback(s.target)
	if err != nil {
		slog.Warn("tmux: captureScrollback failed", "err", err)
		pane, _ := capturePane(s.target)
		return s.cleanTUIContent(pane)
	}

	s.mu.Lock()
	baseline := s.baselineCapture
	s.mu.Unlock()

	response := s.cleanTUIContent(extractNew(baseline, current))
	if response != "" {
		response = "```\n" + response + "\n```"
	}
	return response
}

// captureScrollback captures the full scrollback history plus the visible pane.
// Using "-S -" (start of history) instead of a fixed line count avoids the bug
// where a long response pushes the capture window past the response start,
// causing the first N lines of the response to be silently dropped.
func captureScrollback(target string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", target, "-p", "-S", "-").Output()
	if err != nil {
		return "", err
	}
	return normalizeCapture(string(out)), nil
}

// shellQuote wraps a path in single quotes and escapes any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// cleanTUIContent removes Claude Code TUI frame lines from captured output:
//   - horizontal separator lines made of ─ (U+2500)
//   - bare prompt lines (❯, >, $, #, %)
// tuiInputBlockRe matches Claude Code's 3-line input area:
//   ────────────────   (U+2500 separator line)
//   ❯ …               (U+276F prompt, any trailing chars)
//   ────────────────
// Uses explicit Unicode codepoints and [^\n]* to be immune to invisible
// trailing characters on the prompt line.
var tuiInputBlockRe = regexp.MustCompile("(?m)^─+\n❯[^\n]*\n─+")

func (s *tmuxSession) cleanTUIContent(text string) string {
	if s.stripInputBlock {
		text = tuiInputBlockRe.ReplaceAllString(text, "")
	}
	if len(s.stripPatterns) == 0 {
		return strings.TrimRight(text, "\n")
	}
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, line := range lines {
		drop := false
		for _, re := range s.stripPatterns {
			if re.MatchString(line) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// normalizeCapture trims trailing whitespace per line and strips ANSI codes.
func normalizeCapture(raw string) string {
	raw = ansiRe.ReplaceAllString(raw, "")
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ansiRe matches common ANSI/VT escape sequences.
// OSC must come first so "\x1b]" is consumed fully (not as a generic two-char sequence).
var ansiRe = regexp.MustCompile(
	`\x1b\][^\x07\x1b]*\x07` + // OSC: ESC ] ... BEL
		`|\x1b\[[0-9;]*[a-zA-Z]` + // CSI: ESC [ params letter
		`|\x1b.`, // Other two-char escape sequences
)

// extractNew returns the response text that appeared in current after the baseline.
// It handles three cases:
//  1. Linear shell output — current is baseline + new lines (HasPrefix fast path).
//  2. TUI redraws (e.g. Claude Code) — terminal overwrites lines in place; find the
//     longest common line prefix shared by both snapshots, then return the new lines
//     that follow it in current, stripping the repeated trailing prompt lines.
//  3. Terminal scrolled — baseline has partially scrolled off; use a shrinking anchor.
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

	// TUI path: find how many leading lines the two snapshots share (the static
	// frame/header), then return the new lines that follow in current.
	commonLen := 0
	for i := 0; i < len(baseLines) && i < len(curLines); i++ {
		if baseLines[i] != curLines[i] {
			break
		}
		commonLen = i + 1
	}
	if commonLen > 0 && commonLen < len(curLines) {
		newLines := curLines[commonLen:]
		// Strip trailing lines that duplicate the baseline's suffix (e.g. the prompt ">").
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

	// Scroll path: baseline has partially scrolled off the top; try progressively
	// shorter anchors from the end of baseline to find where new content begins.
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

// InjectKey sends a raw key name to the tmux pane without literal mode,
// allowing tmux to interpret it (e.g., "C-c", "Escape", "Up", "Enter").
func (s *tmuxSession) InjectKey(key string) error {
	if !s.alive.Load() {
		return fmt.Errorf("tmux: session not alive")
	}
	out, err := exec.Command("tmux", "send-keys", "-t", s.target, key).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux: inject key %q: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CaptureBuffer returns the full terminal buffer (scrollback + visible pane).
func (s *tmuxSession) CaptureBuffer() (string, error) {
	if !s.alive.Load() {
		return "", fmt.Errorf("tmux: session not alive")
	}
	return captureScrollback(s.target)
}

// AttachTerminal opens a raw pty pipe to the tmux pane via `tmux attach-session`
// and returns it as an io.ReadWriteCloser for WebSocket proxying.
func (s *tmuxSession) AttachTerminal() (io.ReadWriteCloser, error) {
	if !s.alive.Load() {
		return nil, fmt.Errorf("tmux: session not alive")
	}
	// Use `tmux pipe-pane` to stream pane output; pair with a named pipe for input.
	// Simpler approach: run `tmux attach-session -t <target>` inside a pty.
	// We use creack/pty if available; fall back to a pipe-based approach.
	return newTmuxPipe(s.target)
}

// TerminalSize returns the current rows/cols of the tmux pane.
func (s *tmuxSession) TerminalSize() (rows, cols int) {
	out, err := exec.Command("tmux", "display-message", "-t", s.target, "-p", "#{pane_height} #{pane_width}").Output()
	if err != nil {
		return 24, 80
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) == 2 {
		_, _ = fmt.Sscan(parts[0], &rows)
		_, _ = fmt.Sscan(parts[1], &cols)
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	return rows, cols
}

// ResizeTerminal resizes the tmux window/pane.
func (s *tmuxSession) ResizeTerminal(rows, cols int) error {
	if !s.alive.Load() {
		return fmt.Errorf("tmux: session not alive")
	}
	out, err := exec.Command("tmux", "resize-window", "-t", s.target,
		"-x", fmt.Sprintf("%d", cols), "-y", fmt.Sprintf("%d", rows)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux: resize %dx%d: %w: %s", cols, rows, err, strings.TrimSpace(string(out)))
	}
	return nil
}
