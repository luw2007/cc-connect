package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type agentSessionTestAgent struct {
	name      string
	sessions  []AgentSessionInfo
	listCalls int
	listErr   error
}

func (a *agentSessionTestAgent) Name() string { return a.name }
func (a *agentSessionTestAgent) StartSession(context.Context, string) (AgentSession, error) {
	return nil, nil
}
func (a *agentSessionTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	a.listCalls++
	return a.sessions, a.listErr
}
func (a *agentSessionTestAgent) Stop() error { return nil }

type agentSessionAllTestAgent struct {
	*agentSessionTestAgent
	allSessions  []AgentSessionInfo
	allCalls     int
	listAllError error
}

func (a *agentSessionAllTestAgent) ListAllSessions(context.Context) ([]AgentSessionInfo, error) {
	a.allCalls++
	return a.allSessions, a.listAllError
}

type agentSessionHistoryTestAgent struct {
	*agentSessionTestAgent
	history      []HistoryEntry
	historyCalls int
	historyID    string
	historyLimit int
	historyErr   error
}

func (a *agentSessionHistoryTestAgent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]HistoryEntry, error) {
	a.historyCalls++
	a.historyID = sessionID
	a.historyLimit = limit
	return a.history, a.historyErr
}

type agentSessionPlainPlatform struct {
	name string
}

func (p *agentSessionPlainPlatform) Name() string                             { return p.name }
func (p *agentSessionPlainPlatform) Start(MessageHandler) error               { return nil }
func (p *agentSessionPlainPlatform) Reply(context.Context, any, string) error { return nil }
func (p *agentSessionPlainPlatform) Send(context.Context, any, string) error  { return nil }
func (p *agentSessionPlainPlatform) Stop() error                              { return nil }

type agentSessionGroupPlatform struct {
	agentSessionPlainPlatform
	createCalls int
	nameArg     string
	descArg     string
	ownerArg    string
	chatID      string
	createErr   error
}

func (p *agentSessionGroupPlatform) CreateGroupChat(_ context.Context, name, description, ownerUserID string) (string, error) {
	p.createCalls++
	p.nameArg = name
	p.descArg = description
	p.ownerArg = ownerUserID
	return p.chatID, p.createErr
}

func TestAgentSessionList_TimeBoundsUnknownAndLimit(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	agent := &agentSessionTestAgent{
		name: "native",
		sessions: []AgentSessionInfo{
			{ID: "unknown"},
			{ID: "before", ModifiedAt: base.Add(-time.Second)},
			{ID: "since", ModifiedAt: base},
			{ID: "middle", ModifiedAt: base.Add(time.Hour)},
			{ID: "until", ModifiedAt: base.Add(2 * time.Hour)},
			{ID: "after", ModifiedAt: base.Add(2*time.Hour + time.Second)},
		},
	}
	e := NewEngine("project-a", agent, nil, "", LangEnglish)

	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{
		Since: base,
		Until: base.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	if result.FilteredUnknownTime != 1 {
		t.Fatalf("FilteredUnknownTime = %d, want 1", result.FilteredUnknownTime)
	}
	if result.Count != 3 || len(result.Sessions) != 3 {
		t.Fatalf("Count/len = %d/%d, want 3/3", result.Count, len(result.Sessions))
	}
	wantIDs := []string{"until", "middle", "since"}
	for i, want := range wantIDs {
		if got := result.Sessions[i].ID; got != want {
			t.Fatalf("Sessions[%d].ID = %q, want %q", i, got, want)
		}
		if result.Sessions[i].ModifiedAt == nil || result.Sessions[i].TimeSource != "record" {
			t.Fatalf("Sessions[%d] timestamp = %v/%q, want non-nil/record", i, result.Sessions[i].ModifiedAt, result.Sessions[i].TimeSource)
		}
	}

	result, err = e.ListAgentSessions(context.Background(), AgentSessionQuery{Limit: 2})
	if err != nil {
		t.Fatalf("ListAgentSessions(limit) error = %v", err)
	}
	if result.Count != 2 || result.Sessions[0].ID != "after" || result.Sessions[1].ID != "until" {
		t.Fatalf("limited sessions = %#v, want after, until", result.Sessions)
	}
}

func TestAgentSessionList_UnknownTimeIsNilAndLast(t *testing.T) {
	t.Parallel()

	now := time.Now()
	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{
		{ID: "unknown-a"},
		{ID: "known", ModifiedAt: now},
		{ID: "unknown-b"},
	}}
	e := NewEngine("project-a", agent, nil, "", LangEnglish)
	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	if result.Sessions[0].ID != "known" {
		t.Fatalf("first session = %q, want known", result.Sessions[0].ID)
	}
	for _, view := range result.Sessions[1:] {
		if view.ModifiedAt != nil || view.TimeSource != "unknown" {
			t.Fatalf("unknown session timestamp = %v/%q, want nil/unknown", view.ModifiedAt, view.TimeSource)
		}
	}
}

func TestAgentSessionList_DefaultLimitIsOneHundred(t *testing.T) {
	t.Parallel()

	sessions := make([]AgentSessionInfo, 101)
	for i := range sessions {
		sessions[i] = AgentSessionInfo{ID: time.Unix(int64(i), 0).String(), ModifiedAt: time.Unix(int64(i+1), 0)}
	}
	e := NewEngine("project-a", &agentSessionTestAgent{name: "native", sessions: sessions}, nil, "", LangEnglish)
	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{Limit: 0})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	if result.Count != 100 || len(result.Sessions) != 100 {
		t.Fatalf("Count/len = %d/%d, want 100/100", result.Count, len(result.Sessions))
	}
}

func TestAgentSessionList_AllScopeFallbackIsTruthful(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "project-session"}}}
	e := NewEngine("project-a", agent, nil, "", LangEnglish)
	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{Scope: "all"})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	if result.Scope != "all" || result.AllSessionsSupported {
		t.Fatalf("scope/support = %q/%v, want all/false", result.Scope, result.AllSessionsSupported)
	}
	if agent.listCalls != 1 || len(result.Sessions) != 1 || result.Sessions[0].ID != "project-session" {
		t.Fatalf("fallback calls/sessions = %d/%#v", agent.listCalls, result.Sessions)
	}
}

func TestAgentSessionList_AllScopeUsesCapability(t *testing.T) {
	t.Parallel()

	base := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "project-session"}}}
	agent := &agentSessionAllTestAgent{
		agentSessionTestAgent: base,
		allSessions:           []AgentSessionInfo{{ID: "global-session"}},
	}
	e := NewEngine("project-a", agent, nil, "", LangEnglish)
	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{Scope: "ALL"})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	if !result.AllSessionsSupported || agent.allCalls != 1 || base.listCalls != 0 {
		t.Fatalf("support/all/project calls = %v/%d/%d", result.AllSessionsSupported, agent.allCalls, base.listCalls)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].ID != "global-session" {
		t.Fatalf("sessions = %#v, want global-session", result.Sessions)
	}
}

func TestAgentSessionList_BindingMetadataUsesCCSessionAndManualNamespace(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "agent-1"}, {ID: "agent-2"}}}
	e := NewEngine("project-a", agent, nil, "", LangEnglish)
	ccSession := e.sessions.SwitchToAgentSession("user", "agent-1", "native", "one")
	e.workspaceBindings = NewWorkspaceBindingManager(filepath.Join(t.TempDir(), "bindings.json"))
	e.workspaceBindings.BindSession("manual:project-a", "feishu:chat-1", "one", "/repo", "agent-1")
	e.workspaceBindings.BindSession("manual:project-a", "feishu:chat-2", "two", "/repo", "agent-2")
	e.workspaceBindings.BindSession("autogroup:project-a", "feishu:wrong-chat", "wrong", "/repo", "agent-1")

	result, err := e.ListAgentSessions(context.Background(), AgentSessionQuery{})
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v", err)
	}
	byID := make(map[string]AgentSessionView)
	for _, view := range result.Sessions {
		byID[view.ID] = view
	}
	if got := byID["agent-1"]; !got.Bound || got.CCSessionID != ccSession.ID || got.BoundChatID != "chat-1" {
		t.Fatalf("agent-1 binding = %#v", got)
	}
	if got := byID["agent-2"]; got.Bound || got.CCSessionID != "" || got.BoundChatID != "chat-2" {
		t.Fatalf("agent-2 binding = %#v", got)
	}
}

func TestAgentSessionHistory_DefaultLimitAndUnsupported(t *testing.T) {
	t.Parallel()

	unsupported := NewEngine("project-a", &agentSessionTestAgent{name: "plain"}, nil, "", LangEnglish)
	if _, err := unsupported.AgentSessionHistory(context.Background(), "session-1", 10); !errors.Is(err, ErrAgentSessionHistoryUnsupported) {
		t.Fatalf("AgentSessionHistory() error = %v, want ErrAgentSessionHistoryUnsupported", err)
	}

	want := []HistoryEntry{{Role: "user", Content: "hello"}}
	historyAgent := &agentSessionHistoryTestAgent{
		agentSessionTestAgent: &agentSessionTestAgent{name: "history"},
		history:               want,
	}
	supported := NewEngine("project-a", historyAgent, nil, "", LangEnglish)
	got, err := supported.AgentSessionHistory(context.Background(), "session-2", 0)
	if err != nil {
		t.Fatalf("AgentSessionHistory() error = %v", err)
	}
	if len(got) != 1 || got[0].Content != "hello" || historyAgent.historyID != "session-2" || historyAgent.historyLimit != 50 {
		t.Fatalf("history/id/limit = %#v/%q/%d", got, historyAgent.historyID, historyAgent.historyLimit)
	}
}

func TestAgentSessionGroup_IdempotentManualBinding(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "session-1", ProjectPath: "/repo"}}}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", agent, []Platform{platform}, "", LangEnglish)
	e.SetDataDir(t.TempDir())

	first, err := e.CreateAgentSessionGroup(context.Background(), "session-1", "  owner-1  ", "Session One")
	if err != nil {
		t.Fatalf("first CreateAgentSessionGroup() error = %v", err)
	}
	second, err := e.CreateAgentSessionGroup(context.Background(), "session-1", "owner-1", "Ignored Retry Name")
	if err != nil {
		t.Fatalf("second CreateAgentSessionGroup() error = %v", err)
	}
	if !first.Created || second.Created || first.ChatID != "chat-1" || second.ChatID != "chat-1" {
		t.Fatalf("first/second = %#v/%#v", first, second)
	}
	if platform.createCalls != 1 || platform.nameArg != "Session One" || platform.descArg != "" || platform.ownerArg != "owner-1" {
		t.Fatalf("create calls/args = %d/%q/%q/%q", platform.createCalls, platform.nameArg, platform.descArg, platform.ownerArg)
	}
	if first.BindingNamespace != "manual:project-a" || second.BindingNamespace != "manual:project-a" {
		t.Fatalf("binding namespaces = %q/%q", first.BindingNamespace, second.BindingNamespace)
	}
	if _, binding, found := e.workspaceBindings.LookupBySessionID("manual:project-a", "session-1"); !found || binding.Workspace != "/repo" {
		t.Fatalf("manual binding = %#v, found=%v", binding, found)
	}
	if _, _, found := e.workspaceBindings.LookupBySessionID("autogroup:project-a", "session-1"); found {
		t.Fatal("manual binding leaked into autogroup namespace")
	}
	if _, _, found := e.workspaceBindings.LookupBySessionID("project:project-a", "session-1"); found {
		t.Fatal("manual binding leaked into project namespace")
	}
}

func TestAgentSessionGroup_OwnerRequiredBeforeSideEffects(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "session-1"}}}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", agent, []Platform{platform}, "", LangEnglish)
	_, err := e.CreateAgentSessionGroup(context.Background(), "session-1", " \t ", "Session One")
	if !errors.Is(err, ErrAgentSessionOwnerRequired) {
		t.Fatalf("error = %v, want ErrAgentSessionOwnerRequired", err)
	}
	if agent.listCalls != 0 || platform.createCalls != 0 || e.workspaceBindings != nil {
		t.Fatalf("side effects: list=%d create=%d bindings=%v", agent.listCalls, platform.createCalls, e.workspaceBindings)
	}
}

func TestAgentSessionGroup_NotFoundBeforeCreate(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "other-session"}}}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", agent, []Platform{platform}, "", LangEnglish)
	_, err := e.CreateAgentSessionGroup(context.Background(), "missing", "owner-1", "Missing")
	if !errors.Is(err, ErrAgentSessionNotFound) {
		t.Fatalf("error = %v, want ErrAgentSessionNotFound", err)
	}
	if agent.listCalls != 1 || platform.createCalls != 0 || e.workspaceBindings != nil {
		t.Fatalf("side effects: list=%d create=%d bindings=%v", agent.listCalls, platform.createCalls, e.workspaceBindings)
	}
}

func TestAgentSessionGroup_UnsupportedPlatform(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "session-1"}}}
	e := NewEngine("project-a", agent, []Platform{&agentSessionPlainPlatform{name: "plain"}}, "", LangEnglish)
	_, err := e.CreateAgentSessionGroup(context.Background(), "session-1", "owner-1", "Session One")
	if !errors.Is(err, ErrAgentSessionGroupUnsupported) {
		t.Fatalf("error = %v, want ErrAgentSessionGroupUnsupported", err)
	}
}

func TestAgentSessionGroup_PersistenceFailureIsReturned(t *testing.T) {
	t.Parallel()

	agent := &agentSessionTestAgent{name: "native", sessions: []AgentSessionInfo{{ID: "session-1"}}}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", agent, []Platform{platform}, "", LangEnglish)
	// A directory cannot be replaced by AtomicWriteFile as a JSON file.
	e.workspaceBindings = NewWorkspaceBindingManager(t.TempDir())
	if _, err := e.CreateAgentSessionGroup(context.Background(), "session-1", "owner-1", "Session One"); err == nil {
		t.Fatal("CreateAgentSessionGroup() error = nil, want persistence failure")
	}
	if platform.createCalls != 1 {
		t.Fatalf("CreateGroupChat calls = %d, want 1", platform.createCalls)
	}
}

// agentSessionRaceAgent is the mutex-guarded counterpart of
// agentSessionTestAgent: the concurrency regression drives ListSessions from
// two goroutines, so an unguarded counter would trip -race for the wrong
// reason and mask the invariant under test.
type agentSessionRaceAgent struct {
	sessions []AgentSessionInfo

	mu        sync.Mutex
	listCalls int
}

func (a *agentSessionRaceAgent) Name() string { return "native" }
func (a *agentSessionRaceAgent) StartSession(context.Context, string) (AgentSession, error) {
	return nil, nil
}
func (a *agentSessionRaceAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listCalls++
	return a.sessions, nil
}
func (a *agentSessionRaceAgent) Stop() error { return nil }

type agentSessionRaceGroupPlatform struct {
	agentSessionPlainPlatform

	// createDelay is slept before the call is recorded so the
	// lookup->create->bind window is wide enough for two callers to overlap
	// deterministically instead of racing to finish first.
	createDelay time.Duration

	mu          sync.Mutex
	createCalls int
}

func (p *agentSessionRaceGroupPlatform) CreateGroupChat(context.Context, string, string, string) (string, error) {
	if p.createDelay > 0 {
		time.Sleep(p.createDelay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls++
	// A distinct ID per call keeps a duplicate create from collapsing into the
	// same binding key, which would hide the exact bug this test defends.
	return fmt.Sprintf("chat-%d", p.createCalls), nil
}

// TestAgentSessionGroup_ConcurrentCreateNeverDuplicates guards the lock that
// spans lookup -> CreateGroupChat -> BindSession in CreateAgentSessionGroup.
// Without it, a double-clicked "建群" button creates two real chats and the
// first one is orphaned. Removing the mutex must fail this test.
func TestAgentSessionGroup_ConcurrentCreateNeverDuplicates(t *testing.T) {
	t.Parallel()

	agent := &agentSessionRaceAgent{sessions: []AgentSessionInfo{{ID: "session-1", ProjectPath: "/repo"}}}
	platform := &agentSessionRaceGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		createDelay:               50 * time.Millisecond,
	}
	e := NewEngine("project-a", agent, []Platform{platform}, "", LangEnglish)
	e.SetDataDir(t.TempDir())

	var wg sync.WaitGroup
	results := make([]AgentSessionGroupResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = e.CreateAgentSessionGroup(context.Background(), "session-1", "owner-1", "Session One")
		}(i)
	}
	wg.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("CreateAgentSessionGroup() call %d error = %v", index, err)
		}
	}
	platform.mu.Lock()
	createCalls := platform.createCalls
	platform.mu.Unlock()
	if createCalls != 1 {
		t.Fatalf("CreateGroupChat calls = %d, want exactly 1", createCalls)
	}
	if results[0].ChatID == "" || results[0].ChatID != results[1].ChatID {
		t.Fatalf("chat IDs = %q/%q, want one shared non-empty chat", results[0].ChatID, results[1].ChatID)
	}
	if results[0].Created == results[1].Created {
		t.Fatalf("created flags = %v/%v, want exactly one creator", results[0].Created, results[1].Created)
	}
}
