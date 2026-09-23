package herdr

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var numberedMenuLineRe = regexp.MustCompile(`^[\s❯>*]*(\d{1,2})[.)\-:]\s+(.+?)\s*$`)

const (
	maxMenuOptions   = 9
	menuScanWindow   = 20
	blockedTailLines = 30
	blockedTailRunes = 1200
)

func buildBlockedQuestion(screen string) core.UserQuestion {
	tail := clipBlockedTail(screen)
	q := core.UserQuestion{Question: tail}
	if options := parseNumberedMenu(tail); len(options) > 0 {
		q.Options = options
		return q
	}
	q.Options = []core.UserQuestionOption{
		{Label: "Enter"},
		{Label: "Escape"},
		{Label: "Key1"},
		{Label: "Key2"},
		{Label: "Key3"},
		{Label: "Up"},
		{Label: "Down"},
	}
	return q
}

func parseNumberedMenu(tail string) []core.UserQuestionOption {
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if len(lines) > menuScanWindow {
		lines = lines[len(lines)-menuScanWindow:]
	}

	var best, run []core.UserQuestionOption
	next := 1
	flush := func() {
		if len(run) > len(best) {
			best = append([]core.UserQuestionOption(nil), run...)
		}
		run = nil
		next = 1
	}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		match := numberedMenuLineRe.FindStringSubmatch(line)
		if match == nil {
			flush()
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err != nil {
			flush()
			continue
		}
		if n != next {
			flush()
			if n != 1 {
				continue
			}
		}
		run = append(run, core.UserQuestionOption{Label: "Key" + match[1]})
		next = n + 1
	}
	flush()
	if len(best) < 2 || len(best) > maxMenuOptions {
		return nil
	}
	return best
}

func clipBlockedTail(screen string) string {
	lines := strings.Split(strings.TrimSpace(screen), "\n")
	if len(lines) > blockedTailLines {
		lines = lines[len(lines)-blockedTailLines:]
	}
	tail := strings.Join(lines, "\n")
	runes := []rune(tail)
	if len(runes) > blockedTailRunes {
		tail = "…" + string(runes[len(runes)-blockedTailRunes:])
	}
	return tail
}

var blockedOptionKeys = map[string]string{
	"Enter": "Enter", "Escape": "Escape", "Up": "Up", "Down": "Down",
	"Key1": "1", "Key2": "2", "Key3": "3", "Key4": "4", "Key5": "5",
	"Key6": "6", "Key7": "7", "Key8": "8", "Key9": "9",
}

func blockedKeyForLabel(label string) (string, bool) {
	key, ok := blockedOptionKeys[label]
	return key, ok
}
