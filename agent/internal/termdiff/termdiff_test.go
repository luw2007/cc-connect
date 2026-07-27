package termdiff

import "testing"

func TestExtractNew(t *testing.T) {
	tests := []struct {
		name     string
		baseline string
		current  string
		want     string
	}{
		{name: "no change", baseline: "foo\nbar", current: "foo\nbar", want: ""},
		{name: "empty baseline", baseline: "", current: "hello", want: "hello"},
		{name: "content grew", baseline: "foo\nbar", current: "foo\nbar\nbaz", want: "baz"},
		{name: "new line after prompt", baseline: "user@host:~$ ", current: "user@host:~$ ls\nfile1\nfile2\nuser@host:~$ ", want: "ls\nfile1\nfile2\nuser@host:~$ "},
		{name: "anchor overlap", baseline: "line1\nline2\nline3\nline4\nline5", current: "line3\nline4\nline5\nnew1\nnew2", want: "new1\nnew2"},
		{name: "fully scrolled", baseline: "old1\nold2\nold3", current: "new1\nnew2\nnew3", want: "new1\nnew2\nnew3"},
		{name: "TUI redrawn - shared frame, response replaces prompt", baseline: "╭─ Claude ─╮\n\n>", current: "╭─ Claude ─╮\n\nThe answer is 42.\n\n>", want: "The answer is 42."},
		{name: "TUI redraw", baseline: "header\n\n>", current: "header\n\nLine one.\nLine two.\n\n>", want: "Line one.\nLine two."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractNew(tt.baseline, tt.current); got != tt.want {
				t.Fatalf("ExtractNew() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeCapture(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		stripANSI bool
		want      string
	}{
		{name: "trailing whitespace", raw: "hello   \nworld\t\r\n", want: "hello\nworld"},
		{name: "ANSI stripped", raw: "\x1b[32mgreen\x1b[0m normal", stripANSI: true, want: "green normal"},
		{name: "OSC stripped", raw: "\x1b]0;title\x07prompt$ ", stripANSI: true, want: "prompt$"},
		{name: "ANSI retained", raw: "\x1b[32mgreen\x1b[0m", stripANSI: false, want: "\x1b[32mgreen\x1b[0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCapture(tt.raw, tt.stripANSI); got != tt.want {
				t.Fatalf("NormalizeCapture() = %q, want %q", got, tt.want)
			}
		})
	}
}
