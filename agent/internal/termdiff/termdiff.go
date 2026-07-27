// Package termdiff provides terminal snapshot normalization and diffing for
// polling-based agent backends. It deliberately knows nothing about any
// backend's transport or RPC schema.
package termdiff

import (
	"regexp"
	"strings"
)

var ansiRe = regexp.MustCompile(
	`\x1b\][^\x07\x1b]*\x07` +
		`|\x1b\[[0-9;]*[a-zA-Z]` +
		`|\x1b.`,
)

// Normalize strips ANSI sequences and trims trailing whitespace per line.
func Normalize(raw string) string {
	return NormalizeCapture(raw, true)
}

// NormalizeCapture trims trailing whitespace per line. stripANSI is false
// for transports such as herdr agent.read that already return plain text.
func NormalizeCapture(raw string, stripANSI bool) string {
	if stripANSI {
		raw = ansiRe.ReplaceAllString(raw, "")
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ExtractNew returns content that appeared in current after baseline. It
// handles append-only output, in-place TUI redraws, and scroll-off overlap.
func ExtractNew(baseline, current string) string {
	if current == baseline {
		return ""
	}
	if baseline == "" {
		return current
	}
	if strings.HasPrefix(current, baseline) {
		return strings.TrimLeft(current[len(baseline):], "\n")
	}

	baseLines := strings.Split(baseline, "\n")
	curLines := strings.Split(current, "\n")
	commonLen := 0
	for i := 0; i < len(baseLines) && i < len(curLines); i++ {
		if baseLines[i] != curLines[i] {
			break
		}
		commonLen = i + 1
	}
	if commonLen > 0 && commonLen < len(curLines) {
		newLines := curLines[commonLen:]
		baselineTail := baseLines
		for len(newLines) > 0 && len(baselineTail) > 0 && newLines[len(newLines)-1] == baselineTail[len(baselineTail)-1] {
			newLines = newLines[:len(newLines)-1]
			baselineTail = baselineTail[:len(baselineTail)-1]
		}
		if result := strings.TrimRight(strings.Join(newLines, "\n"), "\n"); result != "" {
			return result
		}
	}

	maxAnchor := min(5, len(baseLines))
	for n := maxAnchor; n >= 1; n-- {
		anchor := strings.Join(baseLines[len(baseLines)-n:], "\n")
		if idx := strings.Index(current, anchor); idx >= 0 {
			if rest := strings.TrimLeft(current[idx+len(anchor):], "\n"); rest != "" {
				return rest
			}
		}
	}
	return current
}
