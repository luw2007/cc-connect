package claudecode

import (
	"errors"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFormatPaneEvent_Text(t *testing.T) {
	evt := core.Event{Type: core.EventText, Content: "hello world"}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "hello world") {
		t.Errorf("expected content in output, got %q", got)
	}
	if !strings.HasPrefix(got, "\033[37m") {
		t.Errorf("expected white ANSI prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "\033[0m") {
		t.Errorf("expected reset ANSI suffix, got %q", got)
	}
}

func TestFormatPaneEvent_ToolUse(t *testing.T) {
	evt := core.Event{Type: core.EventToolUse, ToolName: "Bash", ToolInput: "ls -la"}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "[Tool]") {
		t.Errorf("expected [Tool] in output, got %q", got)
	}
	if !strings.Contains(got, "Bash") {
		t.Errorf("expected tool name in output, got %q", got)
	}
	if !strings.Contains(got, "ls -la") {
		t.Errorf("expected tool input in output, got %q", got)
	}
}

func TestFormatPaneEvent_ToolResult(t *testing.T) {
	evt := core.Event{Type: core.EventToolResult, ToolResult: "ok"}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "→") {
		t.Errorf("expected arrow in output, got %q", got)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("expected result in output, got %q", got)
	}
}

func TestFormatPaneEvent_Error(t *testing.T) {
	evt := core.Event{Type: core.EventError, Error: errors.New("boom")}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "ERROR:") {
		t.Errorf("expected ERROR: prefix, got %q", got)
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("expected error message, got %q", got)
	}
	if !strings.HasPrefix(got, "\033[31m") {
		t.Errorf("expected red ANSI prefix, got %q", got)
	}
}

func TestFormatPaneEvent_ErrorNil(t *testing.T) {
	evt := core.Event{Type: core.EventError}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "ERROR:") {
		t.Errorf("expected ERROR: even with nil error, got %q", got)
	}
}

func TestFormatPaneEvent_Permission(t *testing.T) {
	evt := core.Event{Type: core.EventPermissionRequest, ToolName: "Write"}
	got := formatPaneEvent(evt)
	if !strings.Contains(got, "Permission:") {
		t.Errorf("expected Permission: in output, got %q", got)
	}
	if !strings.Contains(got, "Write") {
		t.Errorf("expected tool name in output, got %q", got)
	}
	if !strings.HasPrefix(got, "\033[33m") {
		t.Errorf("expected yellow ANSI prefix, got %q", got)
	}
}

func TestFormatPaneEvent_Skipped(t *testing.T) {
	for _, typ := range []core.EventType{core.EventResult, core.EventThinking} {
		evt := core.Event{Type: typ, Content: "ignore me"}
		got := formatPaneEvent(evt)
		if got != "" {
			t.Errorf("event type %q should produce empty string, got %q", typ, got)
		}
	}
}

func TestTruncate_Short(t *testing.T) {
	s := "hello"
	if got := truncate(s, 10); got != s {
		t.Errorf("expected %q unchanged, got %q", s, got)
	}
}

func TestTruncate_Long(t *testing.T) {
	s := strings.Repeat("a", 200)
	got := truncate(s, 160)
	runes := []rune(got)
	// 160 chars + ellipsis (1 rune)
	if len(runes) != 161 {
		t.Errorf("expected 161 runes, got %d", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got[len(got)-4:])
	}
}

func TestTruncate_Unicode(t *testing.T) {
	s := strings.Repeat("中", 200)
	got := truncate(s, 160)
	runes := []rune(got)
	if len(runes) != 161 {
		t.Errorf("expected 161 runes for unicode truncation, got %d", len(runes))
	}
}

func TestWordWrap_Short(t *testing.T) {
	s := "hello world"
	if got := wordWrap(s, 180); got != s {
		t.Errorf("short string should be unchanged, got %q", got)
	}
}

func TestWordWrap_Long(t *testing.T) {
	// Build a string with words that will exceed 180 cols
	words := make([]string, 30)
	for i := range words {
		words[i] = strings.Repeat("x", 10)
	}
	s := strings.Join(words, " ")
	got := wordWrap(s, 180)
	for _, line := range strings.Split(got, "\n") {
		if utf8RuneCount(line) > 180 {
			t.Errorf("line exceeds 180 cols: %q (len=%d)", line, utf8RuneCount(line))
		}
	}
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
