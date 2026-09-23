package claudecode

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

const (
	paneWrapCols    = 180
	paneTruncLen    = 160
	paneFlushSize   = 4 * 1024
	paneFlushTick   = 100 * time.Millisecond
)

func (cs *claudeSession) renderToPane(ctx context.Context) {
	var buf strings.Builder
	ticker := time.NewTicker(paneFlushTick)
	defer ticker.Stop()

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		text := buf.String()
		buf.Reset()
		if err := exec.Command("tmux", "send-keys", "-t", cs.tmuxSession, "-l", text).Run(); err != nil {
			slog.Warn("pane_renderer: tmux send-keys failed", "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case evt, ok := <-cs.paneEvents:
			if !ok {
				flush()
				return
			}
			line := formatPaneEvent(evt)
			if line == "" {
				continue
			}
			buf.WriteString(line)
			buf.WriteByte('\n')
			if buf.Len() >= paneFlushSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func formatPaneEvent(evt core.Event) string {
	switch evt.Type {
	case core.EventText:
		return "\033[37m" + wordWrap(evt.Content, paneWrapCols) + "\033[0m"
	case core.EventToolUse:
		return fmt.Sprintf("\033[36m[Tool] \033[1m%s\033[0m: %s", evt.ToolName, truncate(evt.ToolInput, paneTruncLen))
	case core.EventToolResult:
		return fmt.Sprintf("\033[2m  → %s\033[0m", truncate(evt.ToolResult, paneTruncLen))
	case core.EventError:
		msg := ""
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return fmt.Sprintf("\033[31mERROR: %s\033[0m", msg)
	case core.EventPermissionRequest:
		return fmt.Sprintf("\033[33m⚠ Permission: %s\033[0m", evt.ToolName)
	default:
		return ""
	}
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

func wordWrap(s string, cols int) string {
	if utf8.RuneCountInString(s) <= cols {
		return s
	}
	var sb strings.Builder
	lineLen := 0
	for _, word := range strings.Fields(s) {
		wlen := utf8.RuneCountInString(word)
		if lineLen > 0 && lineLen+1+wlen > cols {
			sb.WriteByte('\n')
			lineLen = 0
		} else if lineLen > 0 {
			sb.WriteByte(' ')
			lineLen++
		}
		sb.WriteString(word)
		lineLen += wlen
	}
	return sb.String()
}
