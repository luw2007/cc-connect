package cmux

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newFeedSafetyTestSession(t *testing.T, items ...feedItem) (*feedBridge, *cmuxSession) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Items []feedItem `json:"items"`
	}{Items: items})
	if err != nil {
		t.Fatalf("marshal feed items: %v", err)
	}
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedList {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		return payload, nil
	})
	return newTestFeedSession(t, server.client(), fixtureMapper(t))
}

func assertNoFeedEvent(t *testing.T, session *cmuxSession) {
	t.Helper()
	select {
	case event := <-session.Events():
		t.Fatalf("unsafe feed item emitted event: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFeedResyncRoutesUniqueStrictWorkstreamPermission(t *testing.T) {
	item := feedItem{
		ID:           "ITEM-UNIQUE",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-UNIQUE",
		ToolName:     "Bash",
		ToolInput:    `{"command":"go test ./agent/cmux"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	feed, session := newFeedSafetyTestSession(t, item)

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	select {
	case event := <-session.Events():
		if event.Type != core.EventPermissionRequest || event.RequestID != item.RequestID || event.ToolName != item.ToolName {
			t.Fatalf("permission event = %#v", event)
		}
		if command, ok := event.ToolInputRaw["command"].(string); !ok || command != "go test ./agent/cmux" {
			t.Fatalf("tool input = %#v", event.ToolInputRaw)
		}
	case <-time.After(time.Second):
		t.Fatal("unique strict workstream permission was not emitted")
	}
}

func TestFeedResyncRejectsWorkstreamWithMismatchedCanonicalWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-FIXTURE": {SessionID: "SESSION-FIXTURE"}},
		Sessions: map[string]hookSession{
			"SESSION-FIXTURE": {SessionID: "SESSION-FIXTURE", SurfaceID: "SURFACE-FIXTURE", WorkspaceID: "WORKSPACE-OTHER"},
		},
	})
	item := feedItem{
		ID:           "ITEM-MISMATCHED-WORKSPACE",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-MISMATCHED-WORKSPACE",
		ToolName:     "Bash",
		ToolInput:    `{"command":"unsafe"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	feed, session := newFeedSafetyTestSession(t, item)
	feed.mapper = newSessionMapper(dir)
	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}
	assertNoFeedEvent(t, session)
	feed.mu.Lock()
	_, outstanding := feed.outstanding[item.RequestID]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("mismatched canonical workspace became outstanding")
	}
}

func TestFeedResyncOmitsGloballyDuplicateRequestIDs(t *testing.T) {
	items := []feedItem{
		{ID: "ITEM-FIRST", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-FIXTURE"},
		{ID: "ITEM-SECOND", Kind: "permissionRequest", Status: "resolved", RequestID: "REQ-DUPLICATE", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Other?"}]}`, WorkstreamID: "claude-SESSION-OTHER"},
	}
	feed, session := newFeedSafetyTestSession(t, items...)

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	assertNoFeedEvent(t, session)
	feed.mu.Lock()
	_, outstanding := feed.outstanding["REQ-DUPLICATE"]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("globally duplicated request became outstanding")
	}
}

func TestFeedResyncDropsOutstandingGloballyDuplicateRequestWithoutResolution(t *testing.T) {
	items := []feedItem{
		{ID: "ITEM-FIRST", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-FIXTURE"},
		{ID: "ITEM-SECOND", Kind: "permissionRequest", Status: "resolved", RequestID: "REQ-DUPLICATE", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Other?"}]}`, WorkstreamID: "claude-SESSION-OTHER"},
	}
	feed, session := newFeedSafetyTestSession(t, items...)
	feed.mu.Lock()
	feed.outstanding["REQ-DUPLICATE"] = &pendingRef{
		item:     items[0],
		session:  session,
		deadline: time.Now().Add(time.Minute),
	}
	feed.mu.Unlock()

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	select {
	case event := <-session.Events():
		if event.Type != core.EventPermissionResolved || event.RequestID != "REQ-DUPLICATE" {
			t.Fatalf("duplicate request event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not resolve outstanding permission")
	}
	feed.mu.Lock()
	_, outstanding := feed.outstanding["REQ-DUPLICATE"]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("globally duplicated request remained addressable by request ID")
	}
}

func TestFeedResyncDoesNotRouteByCWD(t *testing.T) {
	item := feedItem{
		ID:        "ITEM-CWD",
		Kind:      "permissionRequest",
		Status:    "pending",
		RequestID: "REQ-CWD",
		ToolName:  "Bash",
		ToolInput: `{"command":"pwd"}`,
		CWD:       "/redacted/project",
	}
	feed, session := newFeedSafetyTestSession(t, item)

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	assertNoFeedEvent(t, session)
	feed.mu.Lock()
	_, outstanding := feed.outstanding[item.RequestID]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("CWD-only item became outstanding")
	}
}

func TestFeedResyncOmitsInvalidPermissionToolInput(t *testing.T) {
	tests := []struct {
		name      string
		toolInput string
	}{
		{name: "malformed", toolInput: `{`},
		{name: "array", toolInput: `[]`},
		{name: "scalar", toolInput: `"command"`},
		{name: "null", toolInput: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := feedItem{
				ID:           "ITEM-INVALID",
				Kind:         "permissionRequest",
				Status:       "pending",
				RequestID:    "REQ-INVALID",
				ToolName:     "Bash",
				ToolInput:    tt.toolInput,
				WorkstreamID: "claude-SESSION-FIXTURE",
			}
			feed, session := newFeedSafetyTestSession(t, item)

			if err := feed.resync(context.Background()); err != nil {
				t.Fatalf("resync: %v", err)
			}

			assertNoFeedEvent(t, session)
			feed.mu.Lock()
			_, outstanding := feed.outstanding[item.RequestID]
			feed.mu.Unlock()
			if outstanding {
				t.Fatal("invalid tool_input became outstanding")
			}
		})
	}
}

func TestFeedResyncOmitsPartiallyMalformedAskUserQuestionInput(t *testing.T) {
	tests := []struct {
		name      string
		toolInput string
	}{
		{name: "missing questions", toolInput: `{}`},
		{name: "empty questions", toolInput: `{"questions":[]}`},
		{name: "partial question", toolInput: `{"questions":[{"question":"First?"},{"header":"Missing question"}]}`},
		{name: "wrong question type", toolInput: `{"questions":[{"question":42}]}`},
		{name: "wrong header type", toolInput: `{"questions":[{"question":"Continue?","header":false}]}`},
		{name: "wrong multi-select type", toolInput: `{"questions":[{"question":"Continue?","multiSelect":"false"}]}`},
		{name: "partial option", toolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"description":"Missing label"}]}]}`},
		{name: "wrong description type", toolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod","description":false}]}]}`},
		{name: "duplicate option labels", toolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"prod"}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := feedItem{
				ID:           "ITEM-QUESTION",
				Kind:         "permissionRequest",
				Status:       "pending",
				RequestID:    "REQ-QUESTION",
				ToolName:     "AskUserQuestion",
				ToolInput:    tt.toolInput,
				WorkstreamID: "claude-SESSION-FIXTURE",
			}
			feed, session := newFeedSafetyTestSession(t, item)

			if err := feed.resync(context.Background()); err != nil {
				t.Fatalf("resync: %v", err)
			}

			assertNoFeedEvent(t, session)
			feed.mu.Lock()
			_, outstanding := feed.outstanding[item.RequestID]
			feed.mu.Unlock()
			if outstanding {
				t.Fatal("partially malformed question became outstanding")
			}
		})
	}
}

func TestFeedResyncOmitsExitPlanMode(t *testing.T) {
	item := feedItem{
		ID:           "ITEM-EXIT-PLAN",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-EXIT-PLAN",
		ToolName:     "ExitPlanMode",
		ToolInput:    `{"plan":"ship it"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	feed, session := newFeedSafetyTestSession(t, item)

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	assertNoFeedEvent(t, session)
	feed.mu.Lock()
	_, outstanding := feed.outstanding[item.RequestID]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("ExitPlanMode became outstanding")
	}
}

func TestFeedResyncDropsOutstandingWhenCurrentPendingRowBecomesUnsafe(t *testing.T) {
	previous := feedItem{
		ID:           "ITEM-CHANGED",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-CHANGED",
		ToolName:     "Bash",
		ToolInput:    `{"command":"safe"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	tests := []struct {
		name    string
		current feedItem
	}{
		{
			name: "malformed generic input",
			current: feedItem{
				ID:           previous.ID,
				Kind:         previous.Kind,
				Status:       previous.Status,
				RequestID:    previous.RequestID,
				ToolName:     previous.ToolName,
				ToolInput:    `{`,
				WorkstreamID: previous.WorkstreamID,
			},
		},
		{
			name: "ExitPlanMode",
			current: feedItem{
				ID:           previous.ID,
				Kind:         previous.Kind,
				Status:       previous.Status,
				RequestID:    previous.RequestID,
				ToolName:     "ExitPlanMode",
				ToolInput:    `{"plan":"unsafe generic reply"}`,
				WorkstreamID: previous.WorkstreamID,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			feed, session := newFeedSafetyTestSession(t, tt.current)
			feed.mu.Lock()
			feed.outstanding[previous.RequestID] = &pendingRef{
				item:     previous,
				session:  session,
				deadline: time.Now().Add(time.Minute),
			}
			feed.mu.Unlock()

			if err := feed.resync(context.Background()); err != nil {
				t.Fatalf("resync: %v", err)
			}

			select {
			case event := <-session.Events():
				if event.Type != core.EventPermissionResolved || event.RequestID != previous.RequestID {
					t.Fatalf("unsafe current row event = %#v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe current row did not resolve outstanding permission")
			}
			feed.mu.Lock()
			_, outstanding := feed.outstanding[previous.RequestID]
			feed.mu.Unlock()
			if outstanding {
				t.Fatal("unsafe current row retained stale outstanding permission")
			}
			if err := feed.replyPermission(context.Background(), previous.RequestID, "once", nil); err == nil {
				t.Fatal("unsafe current row remained replyable")
			}
		})
	}
}

func TestFeedResyncDropsOutstandingWhenStrictRouteChangesSession(t *testing.T) {
	item := feedItem{
		ID:           "ITEM-ROUTE-SHIFT",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-ROUTE-SHIFT",
		ToolName:     "Bash",
		ToolInput:    `{"command":"safe"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	feed, original := newFeedSafetyTestSession(t, item)
	other := newCmuxSession(context.Background(), original.client, original.bus, "WORKSPACE-OTHER", "SURFACE-OTHER", "/other", "once", time.Second)
	original.bus.register(other)
	feed.mu.Lock()
	feed.sessions[other.workspaceID] = other
	feed.outstanding[item.RequestID] = &pendingRef{
		item:     item,
		session:  original,
		deadline: time.Now().Add(time.Minute),
	}
	feed.mu.Unlock()
	feed.mapper.mu.Lock()
	feed.mapper.workstreamToWorkspace[item.WorkstreamID] = other.workspaceID
	feed.mapper.initialized = true
	feed.mapper.lastRefresh = time.Now().Add(time.Hour)
	feed.mapper.mu.Unlock()
	t.Cleanup(func() { _ = other.Close() })

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	select {
	case event := <-original.Events():
		if event.Type != core.EventPermissionResolved || event.RequestID != item.RequestID {
			t.Fatalf("route-shift event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("route-shift did not resolve original outstanding permission")
	}
	assertNoFeedEvent(t, other)
	feed.mu.Lock()
	_, outstanding := feed.outstanding[item.RequestID]
	feed.mu.Unlock()
	if outstanding {
		t.Fatal("route-shifted row retained permission for original session")
	}
	if err := feed.replyPermission(context.Background(), item.RequestID, "once", nil); err == nil {
		t.Fatal("route-shifted row remained replyable")
	}
}

func TestFeedResyncRetainsOutstandingWhenUnchangedRouteIsTemporarilyUnresolved(t *testing.T) {
	previous := feedItem{
		ID:           "ITEM-UNAVAILABLE",
		Kind:         "permissionRequest",
		Status:       "pending",
		RequestID:    "REQ-UNAVAILABLE",
		ToolName:     "Bash",
		ToolInput:    `{"command":"before"}`,
		WorkstreamID: "claude-SESSION-FIXTURE",
	}
	current := previous
	current.ToolInput = `{"command":"after"}`
	feed, session := newFeedSafetyTestSession(t, current)
	ref := &pendingRef{item: previous, session: session, deadline: time.Now().Add(time.Minute)}
	feed.mu.Lock()
	feed.outstanding[previous.RequestID] = ref
	feed.mu.Unlock()
	feed.mapper.mu.Lock()
	delete(feed.mapper.workstreamToWorkspace, previous.WorkstreamID)
	feed.mapper.initialized = true
	feed.mapper.lastRefresh = time.Now().Add(time.Hour)
	feed.mapper.mu.Unlock()

	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	assertNoFeedEvent(t, session)
	feed.mu.Lock()
	retained := feed.outstanding[previous.RequestID]
	feed.mu.Unlock()
	if retained != ref {
		t.Fatal("temporarily unresolved unchanged route did not retain outstanding permission")
	}
	if retained.item.ToolInput != current.ToolInput {
		t.Fatalf("retained tool_input = %q, want current %q", retained.item.ToolInput, current.ToolInput)
	}
}
