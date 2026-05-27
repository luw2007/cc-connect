package core

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWatchSnapshot_Empty(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)
	snap := e.WatchSnapshot()
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %d", len(snap))
	}
}

func TestWatchSnapshot_WithSession(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)

	e.interactiveMu.Lock()
	e.interactiveStates["feishu:chat1:user1"] = &interactiveState{
		platform:     &stubPlatformWatch{},
		workspaceDir: "/tmp/project",
	}
	e.interactiveMu.Unlock()

	snap := e.WatchSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].SessionKey != "feishu:chat1:user1" {
		t.Errorf("unexpected session key: %s", snap[0].SessionKey)
	}
	if snap[0].Workspace != "/tmp/project" {
		t.Errorf("unexpected workspace: %s", snap[0].Workspace)
	}
}

func TestHandleWatch_SSE(t *testing.T) {
	e := NewEngine("proj1", nil, nil, "", LangEnglish)
	e.interactiveMu.Lock()
	e.interactiveStates["tg:chat1:user1"] = &interactiveState{
		platform:     &stubPlatformWatch{},
		workspaceDir: "/tmp/work",
	}
	e.interactiveMu.Unlock()

	m := NewManagementServer(0, "", nil)
	m.RegisterEngine("proj1", e)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/watch", nil).WithContext(ctx)

	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		m.handleWatch(rec, req)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: sessions") {
		t.Fatalf("expected SSE event, got: %s", body)
	}
	if !strings.Contains(body, `"total":1`) {
		t.Fatalf("expected total:1, got: %s", body)
	}
	if !strings.Contains(body, "tg:chat1:user1") {
		t.Fatalf("expected session key in output, got: %s", body)
	}
}

func TestHandleWatch_OnlyPushesOnChange(t *testing.T) {
	e := NewEngine("proj1", nil, nil, "", LangEnglish)
	e.interactiveMu.Lock()
	e.interactiveStates["key1"] = &interactiveState{
		platform:     &stubPlatformWatch{},
		workspaceDir: "/tmp",
	}
	e.interactiveMu.Unlock()

	m := NewManagementServer(0, "", nil)
	m.RegisterEngine("proj1", e)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/watch", nil).WithContext(ctx)

	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		m.handleWatch(rec, req)
		close(done)
	}()

	// Wait enough for initial + one tick (2s)
	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	eventCount := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: sessions") {
			eventCount++
		}
	}
	if eventCount != 1 {
		t.Errorf("expected 1 SSE event (no change = no re-push), got %d", eventCount)
	}
}

// stubPlatformWatch implements Platform interface for testing.
type stubPlatformWatch struct{}

func (s *stubPlatformWatch) Name() string                                               { return "stub" }
func (s *stubPlatformWatch) Start(MessageHandler) error                                 { return nil }
func (s *stubPlatformWatch) Stop() error                                                { return nil }
func (s *stubPlatformWatch) Reply(context.Context, any, string) error                   { return nil }
func (s *stubPlatformWatch) Send(context.Context, any, string) error                    { return nil }

func TestRenderWatchCard(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)
	card := e.renderWatchCard("key1")
	if card == nil {
		t.Fatal("expected non-nil card")
	}
	text := card.RenderText()
	if !strings.Contains(text, "Session Monitor") {
		t.Errorf("expected title in card text, got: %s", text)
	}
}

func TestWatchStartStop(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)
	p := &stubPlatformWatch{}

	e.startWatchLoop(p, nil, "sess1")

	e.watchMu.Lock()
	_, exists := e.watchStates["sess1"]
	e.watchMu.Unlock()
	if !exists {
		t.Fatal("expected watch state to be registered")
	}

	e.stopWatch("sess1")

	e.watchMu.Lock()
	_, exists = e.watchStates["sess1"]
	e.watchMu.Unlock()
	if exists {
		t.Fatal("expected watch state to be removed after stop")
	}
}

func TestGroupByProject(t *testing.T) {
	sessions := []AgentSessionInfo{
		{ID: "a1", ProjectPath: "/home/user/proj1", ModifiedAt: time.Now().Add(-1 * time.Hour)},
		{ID: "a2", ProjectPath: "/home/user/proj1", ModifiedAt: time.Now()},
		{ID: "b1", ProjectPath: "/home/user/proj2", ModifiedAt: time.Now()},
	}
	groups := groupByProject(sessions)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	// proj1 group should have newest first
	for _, g := range groups {
		if g.path == "/home/user/proj1" {
			if len(g.sessions) != 2 {
				t.Fatalf("expected 2 sessions in proj1, got %d", len(g.sessions))
			}
			if g.sessions[0].ID != "a2" {
				t.Errorf("expected newest session first, got %s", g.sessions[0].ID)
			}
		}
	}
}

func TestClassifySession(t *testing.T) {
	recent := AgentSessionInfo{ModifiedAt: time.Now().Add(-30 * time.Second)}
	if classifySession(recent) != sessionStatusWorking {
		t.Error("expected working for 30s old session")
	}

	awaiting := AgentSessionInfo{ModifiedAt: time.Now().Add(-2 * time.Minute)}
	if classifySession(awaiting) != sessionStatusAwaiting {
		t.Error("expected awaiting for 2min old session")
	}

	completed := AgentSessionInfo{ModifiedAt: time.Now().Add(-10 * time.Minute)}
	if classifySession(completed) != sessionStatusCompleted {
		t.Error("expected completed for 10min old session")
	}
}
