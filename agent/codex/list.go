package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// codexSessionDeepParseCount exposes full transcript parse work to package tests.
var codexSessionDeepParseCount atomic.Int64

// resolveCodexHomeDir returns the effective CODEX_HOME directory.
// Priority: explicit config value > CODEX_HOME env > ~/.codex
func resolveCodexHomeDir(explicit string) string {
	if h := strings.TrimSpace(explicit); h != "" {
		return h
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".codex")
}

type codexSessionCandidate struct {
	path       string
	id         string
	cwd        string
	modifiedAt time.Time
}

// listCodexSessions scans the codex sessions directory for JSONL transcript
// files whose cwd matches workDir. An empty workDir disables cwd filtering.
// maxResults limits how many matching transcripts receive a full parse; values
// less than or equal to zero return all matches.
func listCodexSessions(workDir, codexHome string, maxResults int) ([]core.AgentSessionInfo, error) {
	absWorkDir := ""
	if workDir != "" {
		var err error
		absWorkDir, err = filepath.Abs(workDir)
		if err != nil {
			absWorkDir = workDir
		}
	}

	sessionsDir := filepath.Join(resolveCodexHomeDir(codexHome), "sessions")

	var candidates []codexSessionCandidate
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		candidate := readCodexSessionCandidate(path)
		if candidate == nil {
			return nil
		}
		if absWorkDir != "" && candidate.cwd != absWorkDir {
			return nil
		}
		candidates = append(candidates, *candidate)
		return nil
	})

	if len(candidates) == 0 {
		return nil, nil
	}

	sessionTitles := loadCodexSessionTitles(codexHome)

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modifiedAt.After(candidates[j].modifiedAt)
	})
	if maxResults > 0 && len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}

	var sessions []core.AgentSessionInfo
	for _, candidate := range candidates {
		info := parseCodexSessionFile(candidate.path, absWorkDir)
		if info != nil {
			if title := sessionTitles[info.ID]; title != "" {
				if titleRunes := []rune(title); len(titleRunes) > 60 {
					title = string(titleRunes[:60]) + "..."
				}
				info.Summary = title
			}
			patchSessionSourceFile(candidate.path)
			sessions = append(sessions, *info)
		}
	}

	return sessions, nil
}

// loadCodexSessionTitles reads the same generated thread names that Codex uses
// in its session picker. Later entries win because renames append a new record.
func loadCodexSessionTitles(codexHome string) map[string]string {
	path := filepath.Join(resolveCodexHomeDir(codexHome), "session_index.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("codex: failed to close session index", "path", path, "error", err)
		}
	}()

	titles := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		var entry struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.ID != "" && strings.TrimSpace(entry.ThreadName) != "" {
			titles[entry.ID] = entry.ThreadName
		}
	}
	return titles
}

// parseCodexSessionFile reads a Codex JSONL transcript.
// Returns nil if the session's cwd doesn't match filterCwd.

// readCodexSessionCandidate performs the cheap first pass: stat the file and
// stop scanning as soon as a valid session_meta entry is found.
func readCodexSessionCandidate(path string) *codexSessionCandidate {
	stat, err := os.Stat(path)
	if err != nil {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Session metadata is normally the first short line. Keep the initial
	// allocation small while retaining compatibility with larger JSONL lines.
	scanner.Buffer(make([]byte, 4*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "session_meta" {
			continue
		}

		var meta struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(entry.Payload, &meta) != nil || meta.ID == "" {
			continue
		}
		return &codexSessionCandidate{
			path:       path,
			id:         meta.ID,
			cwd:        meta.Cwd,
			modifiedAt: stat.ModTime(),
		}
	}

	return nil
}

// parseCodexSessionFile reads a Codex JSONL transcript.
// Returns nil if the session's cwd doesn't match filterCwd.
func parseCodexSessionFile(path, filterCwd string) *core.AgentSessionInfo {
	codexSessionDeepParseCount.Add(1)

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil
	}

	var sessionID string
	var sessionCwd string
	var summary string
	var msgCount int
	userMsgSeen := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		switch entry.Type {
		case "session_meta":
			var meta struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(entry.Payload, &meta) == nil {
				sessionID = meta.ID
				sessionCwd = meta.Cwd
			}

		case "response_item":
			var item struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil {
				if item.Role == "user" {
					userMsgSeen++
					msgCount++
					// The actual user prompt is the last user response_item
					// (earlier ones are system/AGENTS.md instructions).
					// Pick the last content block that looks like a real prompt.
					for _, c := range item.Content {
						if c.Type == "input_text" && c.Text != "" && isUserPrompt(c.Text) {
							summary = c.Text
						}
					}
				} else if item.Role == "assistant" {
					msgCount++
				}
			}
		}
	}

	// Filter by cwd
	if filterCwd != "" && sessionCwd != "" && sessionCwd != filterCwd {
		return nil
	}

	if sessionID == "" {
		return nil
	}

	if len([]rune(summary)) > 60 {
		summary = string([]rune(summary)[:60]) + "..."
	}

	return &core.AgentSessionInfo{
		ID:           sessionID,
		Summary:      summary,
		MessageCount: msgCount,
		ModifiedAt:   stat.ModTime(),
		ProjectPath:  sessionCwd,
	}
}

// findSessionFile locates the JSONL transcript for a given session ID.
func findSessionFile(sessionID, codexHome string) string {
	sessionsDir := filepath.Join(resolveCodexHomeDir(codexHome), "sessions")

	var found string
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if strings.Contains(filepath.Base(path), sessionID) {
			found = path
		}
		return nil
	})
	return found
}

// getSessionHistory reads the JSONL transcript and returns user/assistant messages.
func getSessionHistory(sessionID, codexHome string, limit int) ([]core.HistoryEntry, error) {
	path := findSessionFile(sessionID, codexHome)
	if path == "" {
		return nil, fmt.Errorf("session file not found for %s", sessionID)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []core.HistoryEntry

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw.Type != "response_item" {
			continue
		}

		var item struct {
			Role    string `json:"role"`
			Type    string `json:"type"`
			Text    string `json:"text"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw.Payload, &item) != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)

		switch {
		case item.Role == "user" && len(item.Content) > 0:
			for _, c := range item.Content {
				if c.Type == "input_text" && c.Text != "" && isUserPrompt(c.Text) {
					entries = append(entries, core.HistoryEntry{
						Role: "user", Content: c.Text, Timestamp: ts,
					})
				}
			}
		case item.Role == "assistant" && len(item.Content) > 0:
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					entries = append(entries, core.HistoryEntry{
						Role: "assistant", Content: c.Text, Timestamp: ts,
					})
				}
			}
		case item.Type == "reasoning" && item.Text != "":
			// skip reasoning items
		}
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// patchSessionSource rewrites the session_meta line in a Codex JSONL transcript
// so that source="cli" and originator="codex_cli_rs", making the session visible
// in the interactive `codex` terminal.
func patchSessionSource(sessionID, codexHome string) {
	path := findSessionFile(sessionID, codexHome)
	if path == "" {
		return
	}
	patchSessionSourceFile(path)
}

func patchSessionSourceFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		return
	}
	firstLine := data[:idx]

	// Only patch if it's actually an exec-sourced session
	if !bytes.Contains(firstLine, []byte(`"source":"exec"`)) {
		return
	}

	patched := bytes.Replace(firstLine, []byte(`"source":"exec"`), []byte(`"source":"cli"`), 1)
	patched = bytes.Replace(patched, []byte(`"originator":"codex_exec"`), []byte(`"originator":"codex_cli_rs"`), 1)

	if bytes.Equal(patched, firstLine) {
		return
	}

	out := make([]byte, 0, len(patched)+len(data)-idx)
	out = append(out, patched...)
	out = append(out, data[idx:]...)

	_ = os.WriteFile(path, out, 0o644)
}

// isUserPrompt returns true if the text looks like an actual user prompt
// rather than system context (AGENTS.md, environment_context, permissions, etc.)
func isUserPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// Skip XML-style system context
	if strings.HasPrefix(t, "<") {
		return false
	}
	// Skip AGENTS.md instructions injected by Codex
	if strings.HasPrefix(t, "# AGENTS.md") || strings.HasPrefix(t, "#AGENTS.md") {
		return false
	}
	return true
}
