package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentListSessions_ExcludesSubagentRollouts(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "08", "03")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}
	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}

	writeRollout := func(name, sessionID, source string) {
		t.Helper()
		body := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":` + string(workDirJSON) + `,"source":` + source + `}}` + "\n" +
			`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"fix the login bug"}]}}` + "\n"
		if err := os.WriteFile(filepath.Join(sessionsDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write rollout %s: %v", name, err)
		}
	}

	writeRollout("rollout-top-level.jsonl", "top-level", `"vscode"`)
	writeRollout(
		"rollout-subagent.jsonl",
		"subagent",
		`{"subagent":{"thread_spawn":{"parent_thread_id":"top-level"}}}`,
	)

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1 top-level session", len(sessions))
	}
	if sessions[0].ID != "top-level" {
		t.Fatalf("ListSessions()[0].ID = %q, want %q", sessions[0].ID, "top-level")
	}
}

func TestAgentListSessions_ExcludesSubagentRolloutWithCopiedParentMeta(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "08", "04")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}

	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}
	parentMeta := `{"type":"session_meta","payload":{"id":"parent","cwd":` + string(workDirJSON) + `,"source":"vscode"}}`
	parentRollout := parentMeta + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"top-level prompt"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "rollout-parent.jsonl"), []byte(parentRollout), 0o644); err != nil {
		t.Fatalf("write parent rollout: %v", err)
	}

	childMeta := `{"type":"session_meta","payload":{"id":"child","cwd":` + string(workDirJSON) + `,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}}}`
	childRollout := childMeta + "\n" + parentMeta + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"copied parent prompt"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "rollout-child.jsonl"), []byte(childRollout), 0o644); err != nil {
		t.Fatalf("write child rollout: %v", err)
	}

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want only the parent session", len(sessions))
	}
	if sessions[0].ID != "parent" {
		t.Fatalf("ListSessions()[0].ID = %q, want parent", sessions[0].ID)
	}
}

func TestAgentListSessions_UsesSessionIndexThreadName(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "08", "04")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}

	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}
	const sessionID = "019fc636-3567-76e3-a4d6-b223545f7e71"
	rollout := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":` + string(workDirJSON) + `,"source":"vscode"}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"这是很长的具体需求正文，不应该覆盖 Codex 生成的会话名称"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "rollout-session.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	indexEntry := `{"id":"` + sessionID + `","thread_name":"设计简易基础管理模块","updated_at":"2026-08-03T06:01:25Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(indexEntry), 0o644); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].Summary != "设计简易基础管理模块" {
		t.Fatalf("ListSessions()[0].Summary = %q, want Codex thread name", sessions[0].Summary)
	}
}

func TestAgentListSessions_LongThreadNameTruncated(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "08", "15")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}

	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}
	const sessionID = "019fc636-3567-76e3-a4d6-b223545f7e72"
	rollout := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":` + string(workDirJSON) + `,"source":"vscode"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "rollout-session.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	longTitle := strings.Repeat("会", 61)
	indexEntry := `{"id":"` + sessionID + `","thread_name":"` + longTitle + `","updated_at":"2026-08-15T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(indexEntry), 0o644); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1", len(sessions))
	}
	want := strings.Repeat("会", 60) + "..."
	if sessions[0].Summary != want {
		t.Fatalf("ListSessions()[0].Summary = %q, want %q", sessions[0].Summary, want)
	}
}

func TestListSessionsAndListAllSessions(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex-home")
	workDir := filepath.Join(tempDir, "project-a")
	otherWorkDir := filepath.Join(tempDir, "project-b")
	baseTime := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)

	writeCodexSessionFile(t, codexHome, "2026/07/28/a.jsonl", "session-a", workDir, baseTime, "prompt a")
	writeCodexSessionFile(t, codexHome, "2026/07/28/b.jsonl", "session-b", otherWorkDir, baseTime.Add(time.Minute), "prompt b")

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	codexSessionDeepParseCount.Store(0)
	projectSessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(projectSessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1: %+v", len(projectSessions), projectSessions)
	}
	if got := projectSessions[0]; got.ID != "session-a" || got.ProjectPath != workDir {
		t.Fatalf("ListSessions()[0] = %+v, want session-a with ProjectPath %q", got, workDir)
	}
	if got := codexSessionDeepParseCount.Load(); got != 1 {
		t.Fatalf("ListSessions() deeply parsed %d files, want only the cwd match", got)
	}

	allSessions, err := agent.ListAllSessions(context.Background())
	if err != nil {
		t.Fatalf("ListAllSessions() error = %v", err)
	}
	if len(allSessions) != 2 {
		t.Fatalf("ListAllSessions() returned %d sessions, want 2: %+v", len(allSessions), allSessions)
	}
	wantProjectPaths := map[string]string{
		"session-a": workDir,
		"session-b": otherWorkDir,
	}
	for _, session := range allSessions {
		if want := wantProjectPaths[session.ID]; session.ProjectPath != want {
			t.Errorf("session %q ProjectPath = %q, want %q", session.ID, session.ProjectPath, want)
		}
		delete(wantProjectPaths, session.ID)
	}
	if len(wantProjectPaths) != 0 {
		t.Fatalf("ListAllSessions() missing sessions: %v", wantProjectPaths)
	}
}

func TestListAllSessionsKeepsNewest100(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex-home")
	baseTime := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("session-%03d", i)
		writeCodexSessionFile(
			t,
			codexHome,
			fmt.Sprintf("2026/07/28/%03d.jsonl", i),
			id,
			fmt.Sprintf("/project/%03d", i),
			baseTime.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("prompt %d", i),
		)
	}

	agent := &Agent{codexHome: codexHome}
	codexSessionDeepParseCount.Store(0)
	sessions, err := agent.ListAllSessions(context.Background())
	if err != nil {
		t.Fatalf("ListAllSessions() error = %v", err)
	}
	if len(sessions) != 100 {
		t.Fatalf("ListAllSessions() returned %d sessions, want 100", len(sessions))
	}
	if got := codexSessionDeepParseCount.Load(); got > 100 {
		t.Fatalf("ListAllSessions() deeply parsed %d session files, want at most 100", got)
	}
	if got := sessions[0].ID; got != "session-104" {
		t.Errorf("newest session ID = %q, want session-104", got)
	}
	if got := sessions[len(sessions)-1].ID; got != "session-005" {
		t.Errorf("oldest retained session ID = %q, want session-005", got)
	}
	for _, session := range sessions {
		if session.ID == "session-004" {
			t.Fatalf("ListAllSessions() retained an older session: %+v", session)
		}
	}
}

func TestListCodexSessionsDeepParsesAtMostLimit(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex-home")
	baseTime := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)

	const (
		fileCount = 8
		limit     = 3
	)
	for i := 0; i < fileCount; i++ {
		writeCodexSessionFile(
			t,
			codexHome,
			fmt.Sprintf("session-%d.jsonl", i),
			fmt.Sprintf("session-%d", i),
			"/project/shared",
			baseTime.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("prompt %d", i),
		)
	}

	codexSessionDeepParseCount.Store(0)
	sessions, err := listCodexSessions("", codexHome, limit)
	if err != nil {
		t.Fatalf("listCodexSessions() error = %v", err)
	}
	if got := codexSessionDeepParseCount.Load(); got > limit {
		t.Fatalf("listCodexSessions() deeply parsed %d of %d files, want at most %d", got, fileCount, limit)
	}
	if len(sessions) != limit {
		t.Fatalf("listCodexSessions() returned %d sessions, want %d", len(sessions), limit)
	}
	if got := sessions[0].ID; got != "session-7" {
		t.Fatalf("newest session ID = %q, want session-7", got)
	}
}

func TestListAllSessionsFindsSessionMetaAfterFirstLine(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex-home")
	modifiedAt := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	contents := `{"type":"turn_context","payload":{"turn_id":"turn-1"}}` + "\n" +
		`{"type":"session_meta","payload":{"id":"session-late-meta","cwd":"/project/late-meta"}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"late meta prompt"}]}}` + "\n"
	writeRawSessionFile(t, codexHome, "late-meta.jsonl", contents, modifiedAt)

	agent := &Agent{codexHome: codexHome}
	sessions, err := agent.ListAllSessions(context.Background())
	if err != nil {
		t.Fatalf("ListAllSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListAllSessions() returned %d sessions, want 1: %+v", len(sessions), sessions)
	}
	if got := sessions[0]; got.ID != "session-late-meta" || got.ProjectPath != "/project/late-meta" {
		t.Fatalf("ListAllSessions()[0] = %+v, want late session_meta values", got)
	}
	if got := sessions[0]; got.Summary != "late meta prompt" || got.MessageCount != 1 {
		t.Fatalf("ListAllSessions()[0] = %+v, want summary and message count from full parse", got)
	}
}

func TestListAllSessionsSkipsInvalidSessionFiles(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex-home")
	baseTime := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)

	writeCodexSessionFile(t, codexHome, "valid.jsonl", "valid-session", "/project/valid", baseTime, "valid prompt")
	writeRawSessionFile(t, codexHome, "corrupt.jsonl", "not json\n", baseTime.Add(time.Minute))
	writeRawSessionFile(
		t,
		codexHome,
		"no-meta.jsonl",
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"missing meta"}]}}`+"\n",
		baseTime.Add(2*time.Minute),
	)

	agent := &Agent{codexHome: codexHome}
	sessions, err := agent.ListAllSessions(context.Background())
	if err != nil {
		t.Fatalf("ListAllSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListAllSessions() returned %d sessions, want only the valid session: %+v", len(sessions), sessions)
	}
	if got := sessions[0].ID; got != "valid-session" {
		t.Fatalf("ListAllSessions()[0].ID = %q, want valid-session", got)
	}
}

func writeCodexSessionFile(t *testing.T, codexHome, relativePath, id, cwd string, modifiedAt time.Time, prompt string) {
	t.Helper()
	contents := fmt.Sprintf(
		"%s\n%s\n%s\n",
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`, id, cwd),
		fmt.Sprintf(`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":%q}]}}`, prompt),
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
	)
	writeRawSessionFile(t, codexHome, relativePath, contents, modifiedAt)
}

func writeRawSessionFile(t *testing.T, codexHome, relativePath, contents string, modifiedAt time.Time) {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	if err := os.Chtimes(path, modifiedAt, modifiedAt); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}

// Regression: Codex JSONL records can exceed the old 256 KiB scanner limit.
// Both /list and /history must read through such a record, and /history must
// still return the last N eligible messages in chronological order.
func TestGetSessionHistory_LargeJSONLLineAndLastEntries(t *testing.T) {
	codexHome := t.TempDir()
	sessionID := "session-large-line"
	sessionsDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	largePrompt := strings.Repeat("x", 300*1024)
	data := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"/project"}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"first"}]}}` + "\n" +
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"first reply"}]}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"` + largePrompt + `"}]}}` + "\n" +
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"last reply"}]}}` + "\n"
	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	info := parseCodexSessionFile(path, "/project")
	if info == nil || info.MessageCount != 4 {
		t.Fatalf("session info = %+v, want 4 messages", info)
	}
	entries, err := getSessionHistory(sessionID, codexHome, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Content != largePrompt || entries[1].Content != "last reply" {
		t.Fatalf("last 2 entries = %d entries; want large prompt and last reply", len(entries))
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadCodexSessionHistory_ReportsScanError(t *testing.T) {
	want := errors.New("injected read failure")
	line := `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	reader := io.MultiReader(strings.NewReader(line), errorReader{err: want})
	entries, err := readCodexSessionHistory(reader, 10)
	if !errors.Is(err, want) || entries != nil {
		t.Fatalf("entries = %+v, error = %v; want nil entries and injected error", entries, err)
	}
}

func TestReadCodexSessionHistory_RejectsCorruptJSONAfterValidPrefix(t *testing.T) {
	valid := `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}`
	for _, bad := range []string{
		`{"type":"response_item",`,
		`{"type":"response_item","payload":{"role":`,
	} {
		entries, err := readCodexSessionHistory(strings.NewReader(valid+"\n"+bad+"\n"), 10)
		if err == nil || entries != nil {
			t.Fatalf("corrupt record %q: entries = %+v, error = %v; want nil entries and error", bad, entries, err)
		}
	}

	entries, err := readCodexSessionHistory(strings.NewReader(valid+"\n\n  \n"+`{"type":"unknown_event","payload":{}}`+"\n"), 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("blank and unknown records: entries = %+v, error = %v; want one entry", entries, err)
	}
}

func TestAgentListSessions_BadFileDoesNotHideHealthySession(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	dir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := json.Marshal(workDir)
	if err != nil {
		t.Fatal(err)
	}
	healthy := `{"type":"session_meta","payload":{"id":"healthy","cwd":` + string(cwd) + `}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-healthy.jsonl"), []byte(healthy), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := `{"type":"session_meta","payload":{"id":"bad","cwd":` + string(cwd) + `}}` + "\n" + `{"type":"response_item","payload":`
	if err := os.WriteFile(filepath.Join(dir, "rollout-bad.jsonl"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := a.ListSessions(context.Background())
	if err != nil || len(sessions) != 1 || sessions[0].ID != "healthy" {
		t.Fatalf("ListSessions() = %+v, %v; want only healthy session", sessions, err)
	}
}

func TestGetSessionHistory_ConvertsTimestampToLocal(t *testing.T) {
	codexHome := t.TempDir()
	sessionID := "session-local-time"
	sessionsDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}

	const timestamp = "2026-09-24T12:34:56.123456789Z"
	data := `{"type":"response_item","timestamp":"` + timestamp + `","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write session history: %v", err)
	}

	entries, err := getSessionHistory(sessionID, codexHome, 0)
	if err != nil {
		t.Fatalf("getSessionHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	want, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatalf("parse expected timestamp: %v", err)
	}
	got := entries[0].Timestamp
	if !got.Equal(want) {
		t.Fatalf("timestamp instant = %v, want %v", got, want)
	}
	if got.Location() != time.Local {
		t.Fatalf("timestamp location = %v, want time.Local (%v)", got.Location(), time.Local)
	}
}

func TestParseCodexTimestamp_NonUTCOffsetConvertsToLocal(t *testing.T) {
	const timestamp = "2026-09-24T20:34:56.123456789+08:00"
	want, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	got := parseCodexTimestamp(timestamp)
	if !got.Equal(want) || got.Location() != time.Local {
		t.Fatalf("parseCodexTimestamp(%q) = %v (%v), want instant %v in time.Local", timestamp, got, got.Location(), want)
	}
}

func TestParseCodexTimestamp_PreservesZeroForInvalidInput(t *testing.T) {
	if got := parseCodexTimestamp(""); !got.IsZero() {
		t.Fatalf("empty timestamp = %v, want zero", got)
	}
	if got := parseCodexTimestamp("not-a-timestamp"); !got.IsZero() {
		t.Fatalf("invalid timestamp = %v, want zero", got)
	}
}

func TestAgentListSessions_DeduplicatesRolloutSegmentsBySessionID(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "10", "19")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}

	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}

	const sessionID = "01a14f62-b1f9-75a0-9c37-058b03ddc788"
	writeRollout := func(name, prompt string, modifiedAt time.Time) {
		t.Helper()
		path := filepath.Join(sessionsDir, name)
		body := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":` + string(workDirJSON) + `,"source":"vscode"}}` + "\n" +
			`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write rollout %s: %v", name, err)
		}
		if err := os.Chtimes(path, modifiedAt, modifiedAt); err != nil {
			t.Fatalf("set rollout time %s: %v", name, err)
		}
	}

	older := time.Date(2026, 10, 18, 22, 20, 30, 0, time.Local)
	newer := time.Date(2026, 10, 19, 10, 25, 44, 0, time.Local)
	writeRollout("rollout-old.jsonl", "older segment prompt", older)
	writeRollout("rollout-new.jsonl", "latest segment prompt", newer)

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want one logical session", len(sessions))
	}
	if sessions[0].ID != sessionID {
		t.Fatalf("ListSessions()[0].ID = %q, want %q", sessions[0].ID, sessionID)
	}
	if sessions[0].Summary != "older segment prompt" {
		t.Fatalf("ListSessions()[0].Summary = %q, want earliest segment prompt", sessions[0].Summary)
	}
	if !sessions[0].ModifiedAt.Equal(newer) {
		t.Fatalf("ListSessions()[0].ModifiedAt = %v, want %v", sessions[0].ModifiedAt, newer)
	}
}

func TestAgentListSessions_UsesFirstUserPromptWhenIndexTitleMissing(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "10", "19")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}

	workDirJSON, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("encode work directory: %v", err)
	}

	const sessionID = "01a15201-8ac5-7e41-8433-faa725ed555e"
	rollout := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":` + string(workDirJSON) + `,"source":"vscode"}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"第一句稳定标题"}]}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"后来补充的问题不应覆盖标题"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "rollout-session.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	agent := &Agent{workDir: workDir, codexHome: codexHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].Summary != "第一句稳定标题" {
		t.Fatalf("ListSessions()[0].Summary = %q, want first user prompt", sessions[0].Summary)
	}
}
