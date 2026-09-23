package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubPermPlatform struct {
	stubPlatformEngine
}

func (p *stubPermPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

func TestHandlePermission_MethodNotAllowed(t *testing.T) {
	ws := NewWebhookServer(0, "", "/hook")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hook/permission", nil)
	ws.handlePermission(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
}

func TestHandlePermission_Unauthorized(t *testing.T) {
	ws := NewWebhookServer(0, "secret", "/hook")
	body := `{"tool_name":"Bash","cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", strings.NewReader(body))
	ws.handlePermission(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestHandlePermission_ValidationMissingToolName(t *testing.T) {
	ws := NewWebhookServer(0, "", "/hook")
	body := `{"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", strings.NewReader(body))
	ws.handlePermission(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestHandlePermission_ValidationMissingCwdAndSessionKey(t *testing.T) {
	ws := NewWebhookServer(0, "", "/hook")
	body := `{"tool_name":"Bash"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", strings.NewReader(body))
	ws.handlePermission(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestHandlePermission_ResolvedAllow(t *testing.T) {
	p := &stubPermPlatform{stubPlatformEngine{n: "telegram"}}
	agent := &stubAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	ws := NewWebhookServer(0, "", "/hook")
	ws.RegisterEngine("test", e)

	reqBody := ExternalPermissionRequest{
		ToolName:   "Bash",
		ToolInput:  map[string]any{"command": "ls"},
		SessionKey: "telegram:chat1:user1",
		Timeout:    2,
	}
	body, _ := json.Marshal(reqBody)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", bytes.NewReader(body))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.handlePermission(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	e.externalPermMu.Lock()
	var ep *externalPendingPermission
	for _, v := range e.externalPermissions {
		ep = v
		break
	}
	e.externalPermMu.Unlock()

	if ep == nil {
		t.Fatal("no external permission registered")
	}
	ep.resolve(&ExternalPermDecision{Behavior: "allow"})
	wg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp ExternalPermissionResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "resolved" {
		t.Fatalf("status = %q, want resolved", resp.Status)
	}
	if resp.Decision == nil || resp.Decision.Behavior != "allow" {
		t.Fatalf("decision = %+v, want allow", resp.Decision)
	}
}

func TestHandlePermission_ResolvedDeny(t *testing.T) {
	p := &stubPermPlatform{stubPlatformEngine{n: "telegram"}}
	agent := &stubAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	ws := NewWebhookServer(0, "", "/hook")
	ws.RegisterEngine("test", e)

	reqBody := ExternalPermissionRequest{
		ToolName:   "Write",
		ToolInput:  map[string]any{"file_path": "/etc/passwd"},
		SessionKey: "telegram:chat1:user1",
		Timeout:    2,
	}
	body, _ := json.Marshal(reqBody)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", bytes.NewReader(body))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.handlePermission(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	e.externalPermMu.Lock()
	var ep *externalPendingPermission
	for _, v := range e.externalPermissions {
		ep = v
		break
	}
	e.externalPermMu.Unlock()

	if ep == nil {
		t.Fatal("no external permission registered")
	}
	ep.resolve(&ExternalPermDecision{Behavior: "deny", Message: "dangerous"})
	wg.Wait()

	var resp ExternalPermissionResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "resolved" || resp.Decision.Behavior != "deny" {
		t.Fatalf("got %+v, want resolved/deny", resp)
	}
	if resp.Decision.Message != "dangerous" {
		t.Fatalf("message = %q, want dangerous", resp.Decision.Message)
	}
}

func TestHandlePermission_Timeout(t *testing.T) {
	p := &stubPermPlatform{stubPlatformEngine{n: "telegram"}}
	agent := &stubAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	ws := NewWebhookServer(0, "", "/hook")
	ws.RegisterEngine("test", e)

	reqBody := ExternalPermissionRequest{
		ToolName:   "Bash",
		ToolInput:  map[string]any{"command": "rm -rf /"},
		SessionKey: "telegram:chat1:user1",
		Timeout:    1,
	}
	body, _ := json.Marshal(reqBody)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", bytes.NewReader(body))

	ws.handlePermission(rec, req)

	var resp ExternalPermissionResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", resp.Status)
	}

	e.externalPermMu.Lock()
	count := len(e.externalPermissions)
	e.externalPermMu.Unlock()
	if count != 0 {
		t.Fatalf("expected cleanup, got %d pending", count)
	}
}

func TestHandlePermission_ClientDisconnect(t *testing.T) {
	p := &stubPermPlatform{stubPlatformEngine{n: "telegram"}}
	agent := &stubAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	ws := NewWebhookServer(0, "", "/hook")
	ws.RegisterEngine("test", e)

	reqBody := ExternalPermissionRequest{
		ToolName:   "Bash",
		ToolInput:  map[string]any{"command": "sleep 100"},
		SessionKey: "telegram:chat1:user1",
		Timeout:    60,
	}
	body, _ := json.Marshal(reqBody)

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/permission", bytes.NewReader(body)).WithContext(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.handlePermission(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	e.externalPermMu.Lock()
	count := len(e.externalPermissions)
	e.externalPermMu.Unlock()
	if count != 0 {
		t.Fatalf("expected cleanup after disconnect, got %d pending", count)
	}
}

func TestHandleNotify_Accepted(t *testing.T) {
	p := &stubPermPlatform{stubPlatformEngine{n: "telegram"}}
	agent := &stubAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	ws := NewWebhookServer(0, "", "/hook")
	ws.RegisterEngine("test", e)

	reqBody := ExternalNotifyRequest{
		Event:      "stop",
		Message:    "Task completed successfully",
		SessionKey: "telegram:chat1:user1",
	}
	body, _ := json.Marshal(reqBody)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/notify", bytes.NewReader(body))
	ws.handleNotify(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "accepted" {
		t.Fatalf("status = %q, want accepted", resp["status"])
	}
}

func TestResolveExternalPermission(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, []Platform{&stubPlatformEngine{n: "tg"}}, "", LangEnglish)

	ep := &externalPendingPermission{
		RequestID:  "ext-test-001",
		ToolName:   "Bash",
		ChannelKey: "tg:chat",
		Resolved:   make(chan struct{}),
	}
	e.externalPermMu.Lock()
	e.externalPermissions["ext-test-001"] = ep
	e.externalPermMu.Unlock()

	if e.resolveExternalPermission("tg:other:user", "allow") {
		t.Fatal("should not resolve for wrong channel")
	}
	if e.resolveExternalPermission("tg:chat:user", "hello") {
		t.Fatal("should not resolve for non-decision text")
	}
	// Should match: "tg:chat:user" has prefix "tg:chat"
	if !e.resolveExternalPermission("tg:chat:user", "allow") {
		t.Fatal("should resolve for allow with session key matching channel prefix")
	}

	select {
	case <-ep.Resolved:
	default:
		t.Fatal("Resolved channel not closed")
	}
	if ep.Decision.Behavior != "allow" {
		t.Fatalf("behavior = %q, want allow", ep.Decision.Behavior)
	}
}

func TestLookupByWorkspace(t *testing.T) {
	m := NewWorkspaceBindingManager("")
	m.Bind("project:test", "telegram:chat1", "Chat 1", "/home/user/project")
	m.Bind("project:test", "feishu:chat2", "Chat 2", "/home/user/other")
	m.Bind("project:other", "telegram:chat3", "Chat 3", "/home/user/project")

	matches := m.LookupByWorkspace("/home/user/project")
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}

	matches = m.LookupByWorkspace("/home/user/other")
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}

	matches = m.LookupByWorkspace("/nonexistent")
	if len(matches) != 0 {
		t.Fatalf("got %d matches, want 0", len(matches))
	}
}
