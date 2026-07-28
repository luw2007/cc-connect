package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
