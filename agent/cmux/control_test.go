package cmux

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type controlRPCStep struct {
	method string
	result json.RawMessage
	rpcErr *rpcError
}

type controlRPCCall struct {
	method string
	params map[string]any
}

type controlRPCScript struct {
	t     *testing.T
	mu    sync.Mutex
	steps []controlRPCStep
	calls []controlRPCCall
	next  int
}

func newControlRPCScript(t *testing.T, mapper *sessionMapper, steps ...controlRPCStep) (*Controller, *controlRPCScript) {
	t.Helper()
	script := &controlRPCScript{t: t, steps: steps}
	server := newMockCmuxServer(t, script.respond)
	t.Cleanup(func() {
		script.mu.Lock()
		defer script.mu.Unlock()
		if len(script.calls) != len(script.steps) {
			t.Errorf("RPC methods = %v, want exactly %v", controlCallMethods(script.calls), controlStepMethods(script.steps))
		}
	})
	return &Controller{client: server.client(), mapper: mapper}, script
}

func (s *controlRPCScript) respond(method string, raw json.RawMessage) (json.RawMessage, *rpcError) {
	params := make(map[string]any)
	if err := json.Unmarshal(raw, &params); err != nil {
		s.t.Errorf("decode %s params: %v", method, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, controlRPCCall{method: method, params: params})
	if s.next >= len(s.steps) {
		s.t.Errorf("unexpected RPC %q with params %#v", method, params)
		return nil, &rpcError{Code: "unexpected_rpc", Message: method}
	}
	step := s.steps[s.next]
	s.next++
	if method != step.method {
		s.t.Errorf("RPC %d method = %q, want %q", s.next, method, step.method)
	}
	if step.rpcErr != nil {
		return nil, step.rpcErr
	}
	if len(step.result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return step.result, nil
}

func (s *controlRPCScript) call(t *testing.T, index int) controlRPCCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.calls) {
		t.Fatalf("RPC call %d missing; methods = %v", index, controlCallMethods(s.calls))
	}
	return s.calls[index]
}

func controlCallMethods(calls []controlRPCCall) []string {
	methods := make([]string, len(calls))
	for index, call := range calls {
		methods[index] = call.method
	}
	return methods
}

func controlStepMethods(steps []controlRPCStep) []string {
	methods := make([]string, len(steps))
	for index, step := range steps {
		methods[index] = step.method
	}
	return methods
}

func controlWorkspaceList(workspaces string) json.RawMessage {
	return json.RawMessage(`{"workspaces":` + workspaces + `}`)
}

func controlFeedList(t *testing.T, items ...feedItem) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(struct {
		Items []feedItem `json:"items"`
	}{Items: items})
	if err != nil {
		t.Fatalf("marshal feed.list result: %v", err)
	}
	return data
}

func writeControlHookStore(t *testing.T, dir, source string, store hookSessionStore) {
	t.Helper()
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal %s hook store: %v", source, err)
	}
	if err := os.WriteFile(filepath.Join(dir, source+"-hook-sessions.json"), data, 0o600); err != nil {
		t.Fatalf("write %s hook store: %v", source, err)
	}
}

func controlMapper(t *testing.T) *sessionMapper {
	t.Helper()
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{
			"WORKSPACE-TARGET": {SessionID: "SESSION-TARGET", UpdatedAt: 20},
			"WORKSPACE-OTHER":  {SessionID: "SESSION-OTHER", UpdatedAt: 10},
		},
		Sessions: map[string]hookSession{
			"SESSION-TARGET": {SessionID: "SESSION-TARGET", SurfaceID: "SURFACE-TARGET", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "needsInput", UpdatedAt: 20},
			"SESSION-OTHER":  {SessionID: "SESSION-OTHER", SurfaceID: "SURFACE-OTHER", WorkspaceID: "WORKSPACE-OTHER", AgentLifecycle: "running", UpdatedAt: 10},
		},
	})
	return newSessionMapper(dir)
}

func controlMapperWithSession(t *testing.T, source, sessionID, surfaceID, lifecycle string) *sessionMapper {
	t.Helper()
	dir := t.TempDir()
	writeControlHookStore(t, dir, source, hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: sessionID, UpdatedAt: 20}},
		Sessions: map[string]hookSession{
			sessionID: {SessionID: sessionID, SurfaceID: surfaceID, WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: lifecycle, UpdatedAt: 20},
		},
	})
	return newSessionMapper(dir)
}

func controlTarget(t *testing.T, targets []core.AgentControlTarget, id string) core.AgentControlTarget {
	t.Helper()
	for _, target := range targets {
		if target.ID == id {
			return target
		}
	}
	t.Fatalf("target %q missing from %#v", id, targets)
	return core.AgentControlTarget{}
}

func controlRequest(t *testing.T, requests []core.AgentControlRequest, id string) core.AgentControlRequest {
	t.Helper()
	for _, request := range requests {
		if request.ID == id {
			return request
		}
	}
	t.Fatalf("request %q missing from %#v", id, requests)
	return core.AgentControlRequest{}
}

func assertOpaqueRevision(t *testing.T, revision string) {
	t.Helper()
	decoded, err := hex.DecodeString(revision)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("revision = %q, want a SHA-256 token", revision)
	}
}

func terminalControlCapabilities() []core.AgentControlCapability {
	return []core.AgentControlCapability{
		core.AgentControlCapabilityTail,
		core.AgentControlCapabilityKey,
		core.AgentControlCapabilityRequests,
		core.AgentControlCapabilityPermission,
		core.AgentControlCapabilityQuestion,
	}
}

func requestControlCapabilities() []core.AgentControlCapability {
	return []core.AgentControlCapability{core.AgentControlCapabilityRequests, core.AgentControlCapabilityPermission, core.AgentControlCapabilityQuestion}
}

func TestCmuxControllerRegistered(t *testing.T) {
	for _, name := range core.ListRegisteredAgentControllers() {
		if name == "cmux" {
			return
		}
	}
	t.Fatal("cmux controller is not registered")
}

func TestNewControllerResolvesOnlyTransportConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var gotExplicit, gotPassword string
	controller, err := newControllerWithResolver(map[string]any{
		"socket_path": "/tmp/cmux-control.sock", "password": "secret", "rpc_timeout_ms": 2500,
	}, func(_ context.Context, explicit, password string) (string, error) {
		gotExplicit, gotPassword = explicit, password
		return explicit, nil
	})
	if err != nil {
		t.Fatalf("newControllerWithResolver: %v", err)
	}
	if gotExplicit != "/tmp/cmux-control.sock" || gotPassword != "secret" {
		t.Fatalf("resolver args = %q/%q", gotExplicit, gotPassword)
	}
	if controller.client.socketPath != gotExplicit || controller.client.rpcTimeout != 2500*time.Millisecond {
		t.Fatalf("controller client = %#v", controller.client)
	}
	if controller.mapper.dir != filepath.Join(home, ".cmuxterm") {
		t.Fatalf("mapper dir = %q", controller.mapper.dir)
	}
}

func TestCmuxControllerListUsesOnlyWorkspaceListAndMapperTruth(t *testing.T) {
	listing := controlWorkspaceList(`[
		{"id":"WORKSPACE-TARGET","title":"live title","current_directory":"/target"},
		{"id":"WORKSPACE-UNKNOWN","title":"Claude-looking title","current_directory":"/unknown","command":"claude"},
		{"id":"WORKSPACE-DIRECT","current_directory":"/direct","surface_id":"SURFACE-DIRECT"},
		{"id":"","current_directory":"/missing-id","surface_id":"SURFACE-NO-ID"}
	]`)
	controller, _ := newControlRPCScript(t, controlMapper(t), controlRPCStep{method: methodListWorkspaces, result: listing})

	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %#v, want three exact workspace IDs", targets)
	}
	mapped := controlTarget(t, targets, "WORKSPACE-TARGET")
	assertOpaqueRevision(t, mapped.Revision)
	if mapped.Backend != "cmux" || mapped.Kind != "claude" || mapped.Directory != "/target" || mapped.Status != "needsInput" {
		t.Fatalf("mapped target = %#v", mapped)
	}
	if !reflect.DeepEqual(mapped.Capabilities, requestControlCapabilities()) {
		t.Fatalf("mapped capabilities = %v", mapped.Capabilities)
	}
	unknown := controlTarget(t, targets, "WORKSPACE-UNKNOWN")
	if unknown.Kind != "unknown" || unknown.Status != "" || len(unknown.Capabilities) != 0 {
		t.Fatalf("unproven target = %#v", unknown)
	}
	direct := controlTarget(t, targets, "WORKSPACE-DIRECT")
	if direct.Kind != "unknown" || direct.Status != "" || !reflect.DeepEqual(direct.Capabilities, terminalControlCapabilities()[:2]) {
		t.Fatalf("direct-surface target = %#v", direct)
	}
	if !mapped.Supports(core.AgentControlCapabilityQuestion) || unknown.Supports(core.AgentControlCapabilityQuestion) || direct.Supports(core.AgentControlCapabilityQuestion) {
		t.Fatal("cmux request capabilities did not require a strict active hook")
	}
}

func TestCmuxTargetRevisionIsStableAndIncludesHookSessionIdentity(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","current_directory":"/target"}]`)
	controller, _ := newControlRPCScript(t, controlMapperWithSession(t, "claude", "SESSION-A", "SURFACE-TARGET", "running"),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
	)
	first, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstRevision := controlTarget(t, first, "WORKSPACE-TARGET").Revision
	if got := controlTarget(t, second, "WORKSPACE-TARGET").Revision; got != firstRevision {
		t.Fatalf("unchanged target revision changed from %q to %q", firstRevision, got)
	}

	changed, _ := newControlRPCScript(t, controlMapperWithSession(t, "claude", "SESSION-B", "SURFACE-TARGET", "running"),
		controlRPCStep{method: methodListWorkspaces, result: listing})
	changedTargets, err := changed.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := controlTarget(t, changedTargets, "WORKSPACE-TARGET").Revision; got == firstRevision {
		t.Fatal("target revision ignored a changed hook-session identity")
	}
}

func TestCmuxTargetRevisionChangesWhenWorkspaceCWDChanges(t *testing.T) {
	before := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET","current_directory":"/before"}]`)
	after := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET","current_directory":"/after"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: before},
		controlRPCStep{method: methodListWorkspaces, result: after},
	)
	first, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTarget := controlTarget(t, first, "WORKSPACE-TARGET")
	secondTarget := controlTarget(t, second, "WORKSPACE-TARGET")
	if firstTarget.Revision == secondTarget.Revision {
		t.Fatal("target revision ignored a changed workspace CWD")
	}
	if secondTarget.Directory != "/after" {
		t.Fatalf("target directory = %q, want /after", secondTarget.Directory)
	}
}

func TestCmuxTailRejectsChangedWorkspaceCWDRevisionBeforeRead(t *testing.T) {
	before := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET","current_directory":"/before"}]`)
	after := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET","current_directory":"/after"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: before},
		controlRPCStep{method: methodListWorkspaces, result: after},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.TailAgent(context.Background(), targets[0].Ref(), 2); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("TailAgent error = %v, want stale target after CWD change", err)
	}
}

func TestCmuxMapperNewestSourceOwnsKindLifecycleAndRevision(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{Sessions: map[string]hookSession{
		"old": {SessionID: "old", SurfaceID: "SURFACE-OLD", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "needsInput", UpdatedAt: 10},
	}})
	writeControlHookStore(t, dir, "codex", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "new", UpdatedAt: 20}},
		Sessions: map[string]hookSession{
			"new": {SessionID: "new", SurfaceID: "SURFACE-NEW", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 20},
		},
	})
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","title":"Claude","current_directory":"/target"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(dir), controlRPCStep{method: methodListWorkspaces, result: listing})

	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := controlTarget(t, targets, "WORKSPACE-TARGET")
	if target.Kind != "codex" || target.Status != "running" {
		t.Fatalf("target kind/status = %q/%q, want newest hook truth", target.Kind, target.Status)
	}
	assertOpaqueRevision(t, target.Revision)
}

func TestCmuxControllerRefreshesUnchangedHookStoreAndKeepsLastGoodMapping(t *testing.T) {
	dir := t.TempDir()
	store := hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "SESSION-A", UpdatedAt: 20}},
		Sessions: map[string]hookSession{
			"SESSION-A": {SessionID: "SESSION-A", SurfaceID: "SURFACE-A", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 20},
		},
	}
	writeControlHookStore(t, dir, "claude", store)
	path := filepath.Join(dir, "claude-hook-sessions.json")
	initialInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","current_directory":"/target"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(dir),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
	)
	first, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTarget := controlTarget(t, first, "WORKSPACE-TARGET")

	store.ActiveByWorkspace["WORKSPACE-TARGET"] = hookSessionRef{SessionID: "SESSION-B", UpdatedAt: 20}
	store.Sessions = map[string]hookSession{
		"SESSION-B": {SessionID: "SESSION-B", SurfaceID: "SURFACE-B", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 20},
	}
	writeControlHookStore(t, dir, "claude", store)
	updatedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if updatedInfo.Size() != initialInfo.Size() {
		t.Fatalf("regression fixture size changed from %d to %d", initialInfo.Size(), updatedInfo.Size())
	}
	if err := os.Chtimes(path, initialInfo.ModTime(), initialInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	updatedInfo, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !updatedInfo.ModTime().Equal(initialInfo.ModTime()) {
		t.Fatalf("regression fixture mtime = %v, want %v", updatedInfo.ModTime(), initialInfo.ModTime())
	}

	second, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondTarget := controlTarget(t, second, "WORKSPACE-TARGET")
	if secondTarget.Revision == firstTarget.Revision {
		t.Fatal("controller snapshot ignored same-size, same-mtime hook-store content")
	}
	if surfaceID, ok := controller.mapper.surfaceIDForWorkspace("WORKSPACE-TARGET"); !ok || surfaceID != "SURFACE-B" {
		t.Fatalf("refreshed surface = %q, %v; want SURFACE-B", surfaceID, ok)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	thirdTarget := controlTarget(t, third, "WORKSPACE-TARGET")
	if thirdTarget.Revision == secondTarget.Revision {
		t.Fatal("direct controller reused last-good hook mapping after malformed reload")
	}
	if len(thirdTarget.Capabilities) != 0 || thirdTarget.Kind != "unknown" || thirdTarget.Status != "" {
		t.Fatalf("direct target after failed refresh = %#v, want no hook-derived authority", thirdTarget)
	}
	if surfaceID, ok := controller.mapper.surfaceIDForWorkspace("WORKSPACE-TARGET"); !ok || surfaceID != "SURFACE-B" {
		t.Fatalf("surface after failed refresh = %q, %v; want retained SURFACE-B", surfaceID, ok)
	}
}

func TestCmuxMapperActiveIndexOverridesNewerHistoricalSession(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "SESSION-ACTIVE", UpdatedAt: 10}},
		Sessions: map[string]hookSession{
			"SESSION-ACTIVE":  {SessionID: "SESSION-ACTIVE", SurfaceID: "SURFACE-ACTIVE", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "needsInput", UpdatedAt: 10},
			"SESSION-HISTORY": {SessionID: "SESSION-HISTORY", SurfaceID: "SURFACE-HISTORY", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 100},
		},
	})
	writeControlHookStore(t, dir, "codex", hookSessionStore{Sessions: map[string]hookSession{
		"CODEX-HISTORY": {SessionID: "CODEX-HISTORY", SurfaceID: "SURFACE-CODEX-HISTORY", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 200},
	}})
	mapper := newSessionMapper(dir)
	hook := mapper.workspaceHook("WORKSPACE-TARGET")
	if hook.Source != "claude" || hook.SessionID != "SESSION-ACTIVE" || hook.SurfaceID != "SURFACE-ACTIVE" || hook.Lifecycle != "needsInput" {
		t.Fatalf("active hook identity = %#v, want indexed active session", hook)
	}
}

func TestCmuxMapperCrossSourceActiveTieIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "CLAUDE-ACTIVE", UpdatedAt: 20}},
		Sessions: map[string]hookSession{
			"CLAUDE-ACTIVE": {SessionID: "CLAUDE-ACTIVE", SurfaceID: "SURFACE-CLAUDE", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "needsInput", UpdatedAt: 20},
		},
	})
	writeControlHookStore(t, dir, "codex", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "CODEX-ACTIVE", UpdatedAt: 20}},
		Sessions: map[string]hookSession{
			"CODEX-ACTIVE": {SessionID: "CODEX-ACTIVE", SurfaceID: "SURFACE-CODEX", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running", UpdatedAt: 20},
		},
	})
	for iteration := 0; iteration < 100; iteration++ {
		hook := newSessionMapper(dir).workspaceHook("WORKSPACE-TARGET")
		if hook.Source != "claude" || hook.SessionID != "CLAUDE-ACTIVE" || hook.SurfaceID != "SURFACE-CLAUDE" || hook.Lifecycle != "needsInput" {
			t.Fatalf("iteration %d: active hook identity = %#v, want deterministic claude tie-break", iteration, hook)
		}
	}
}

func TestCmuxMissingSurfaceMakesTailAndKeyDoOnlyVerificationRPCs(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-NO-SURFACE","current_directory":"/target"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref := targets[0].Ref()
	if _, err := controller.TailAgent(context.Background(), ref, 20); !errors.Is(err, core.ErrAgentControlUnsupported) {
		t.Fatalf("TailAgent error = %v, want unsupported", err)
	}
	if err := controller.SendAgentKey(context.Background(), ref, "Enter"); !errors.Is(err, core.ErrAgentControlUnsupported) {
		t.Fatalf("SendAgentKey error = %v, want unsupported", err)
	}
}

func TestCmuxInvalidTargetRefsStopBeforeBackendOperations(t *testing.T) {
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()))
	invalid := core.AgentControlTargetRef{ID: "WORKSPACE-TARGET"}
	if _, err := controller.TailAgent(context.Background(), invalid, 20); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("TailAgent error = %v, want stale target", err)
	}
	if _, err := controller.ListAgentRequests(context.Background(), invalid); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("ListAgentRequests error = %v, want stale target", err)
	}
}

func TestCmuxTailRejectsChangedSurfaceRevisionBeforeRead(t *testing.T) {
	first := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-A"}]`)
	changed := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-B"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: first},
		controlRPCStep{method: methodListWorkspaces, result: changed},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.TailAgent(context.Background(), targets[0].Ref(), 2); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("TailAgent error = %v, want stale target", err)
	}
}

func TestCmuxTailNormalizesTerminalEscapesAndClipsLines(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	controller, script := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodReadScreen, result: json.RawMessage(`{"text":"one\n\u001b[31mtwo\u001b[0m  \n\u001b[32mthree\u001b[0m\n"}`)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text, err := controller.TailAgent(context.Background(), targets[0].Ref(), 2)
	if err != nil {
		t.Fatalf("TailAgent: %v", err)
	}
	if text != "two\nthree" {
		t.Fatalf("tail = %q, want normalized last two lines", text)
	}
	want := map[string]any{"workspace_id": "WORKSPACE-TARGET", "surface_id": "SURFACE-TARGET"}
	if got := script.call(t, 2).params; !reflect.DeepEqual(got, want) {
		t.Fatalf("surface.read_text params = %#v, want %#v", got, want)
	}
}

func TestCmuxKeySendsExactRequestedKeyAfterRevalidation(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	controller, script := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodSendKey},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SendAgentKey(context.Background(), targets[0].Ref(), "Ctrl-Shift-P"); err != nil {
		t.Fatalf("SendAgentKey: %v", err)
	}
	want := map[string]any{"workspace_id": "WORKSPACE-TARGET", "surface_id": "SURFACE-TARGET", "key": "Ctrl-Shift-P"}
	if got := script.call(t, 2).params; !reflect.DeepEqual(got, want) {
		t.Fatalf("surface.send_key params = %#v, want %#v", got, want)
	}
}

func TestCmuxControllerOmitsUnsafeDirectCapabilities(t *testing.T) {
	if _, ok := any(&Controller{}).(core.AgentControlPrompter); ok {
		t.Fatal("cmux controller must not implement non-atomic prompt submission")
	}

	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "SESSION-TARGET"}},
		Sessions: map[string]hookSession{
			"SESSION-TARGET": {SessionID: "SESSION-TARGET", SurfaceID: "SURFACE-HOOK", WorkspaceID: "WORKSPACE-TARGET", AgentLifecycle: "running"},
		},
	})
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-LIVE"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(dir), controlRPCStep{method: methodListWorkspaces, result: listing})
	target := mustControlTarget(t, controller, "WORKSPACE-TARGET")
	if target.Supports(core.AgentControlCapabilityPrompt) || target.Supports(core.AgentControlCapabilityRequests) || !target.Supports(core.AgentControlCapabilityTail) || !target.Supports(core.AgentControlCapabilityKey) {
		t.Fatalf("conflicting direct target capabilities = %#v", target.Capabilities)
	}
}

func TestCmuxControllerRejectsDuplicateWorkspaceSurfaces(t *testing.T) {
	listing := controlWorkspaceList(`[
		{"id":"WORKSPACE-A","surface_id":"SURFACE-DUPLICATE"},
		{"id":"WORKSPACE-B","surface_id":"SURFACE-DUPLICATE"}
	]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()), controlRPCStep{method: methodListWorkspaces, result: listing})
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Supports(core.AgentControlCapabilityTail) || target.Supports(core.AgentControlCapabilityKey) {
			t.Fatalf("duplicate surface target advertised terminal capability: %#v", target)
		}
	}
}

func TestCmuxControllerRejectsUnsafeStrictHookJoins(t *testing.T) {
	tests := []struct {
		name  string
		store hookSessionStore
	}{
		{
			name: "workspace mismatch",
			store: hookSessionStore{
				ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "SESSION"}},
				Sessions:          map[string]hookSession{"SESSION": {SessionID: "SESSION", SurfaceID: "SURFACE", WorkspaceID: "WORKSPACE-OTHER"}},
			},
		},
		{
			name: "multiple fallback surfaces",
			store: hookSessionStore{
				ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-TARGET": {SessionID: "SESSION"}},
				ActiveBySurface:   map[string]hookSessionRef{"SURFACE-A": {SessionID: "SESSION"}, "SURFACE-B": {SessionID: "SESSION"}},
				Sessions:          map[string]hookSession{"SESSION": {SessionID: "SESSION", WorkspaceID: "WORKSPACE-TARGET"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeControlHookStore(t, dir, "claude", test.store)
			listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET"}]`)
			controller, _ := newControlRPCScript(t, newSessionMapper(dir), controlRPCStep{method: methodListWorkspaces, result: listing})
			target := mustControlTarget(t, controller, "WORKSPACE-TARGET")
			if target.Kind != "unknown" || target.Status != "" || len(target.Capabilities) != 0 {
				t.Fatalf("unsafe hook join produced direct authority: %#v", target)
			}
		})
	}
}

func mustControlTarget(t *testing.T, controller *Controller, id string) core.AgentControlTarget {
	t.Helper()
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return controlTarget(t, targets, id)
}

func TestCmuxRequestListerUsesOnlyStableWorkstreamMapping(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","current_directory":"/target"}]`)
	items := []feedItem{
		{ID: "ITEM-PERM", Kind: "permissionRequest", Source: "claude", Status: "pending", RequestID: "REQ-PERM", ToolName: "Bash", ToolInput: `{"command":"go test"}`, WorkstreamID: "claude-SESSION-TARGET", UpdatedAt: "2026-07-27T00:00:00Z"},
		{ID: "ITEM-QUESTION", Kind: "permissionRequest", Source: "claude", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Environment?","header":"Env","multiSelect":false,"options":[{"label":"prod","description":"Production"},{"label":"staging"}]}]}`, WorkstreamID: "claude-SESSION-TARGET"},
		{ID: "ITEM-OTHER", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-OTHER", ToolName: "Bash", WorkstreamID: "claude-SESSION-OTHER"},
		{ID: "ITEM-CWD", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-CWD", ToolName: "Bash", WorkstreamID: "claude-MISSING", CWD: "/target"},
		{ID: "ITEM-RESOLVED", Kind: "permissionRequest", Status: "resolved", RequestID: "REQ-RESOLVED", ToolName: "Bash", WorkstreamID: "claude-SESSION-TARGET"},
		{ID: "ITEM-NOTICE", Kind: "notice", Status: "pending", RequestID: "REQ-NOTICE", WorkstreamID: "claude-SESSION-TARGET"},
	}
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, items...)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatalf("ListAgentRequests: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want two exact mapped pending requests", requests)
	}
	permission := controlRequest(t, requests, "REQ-PERM")
	assertOpaqueRevision(t, permission.Revision)
	if permission.Kind != core.AgentControlRequestPermission || permission.ToolName != "Bash" || permission.Input["command"] != "go test" {
		t.Fatalf("permission request = %#v", permission)
	}
	wantDecisions := []string{"once", "always", "all", "bypass", "deny"}
	if !reflect.DeepEqual(permission.AllowedDecisions, wantDecisions) {
		t.Fatalf("allowed decisions = %v, want %v", permission.AllowedDecisions, wantDecisions)
	}
	question := controlRequest(t, requests, "REQ-QUESTION")
	assertOpaqueRevision(t, question.Revision)
	if question.Kind != core.AgentControlRequestQuestion || question.ToolName != "AskUserQuestion" || len(question.AllowedDecisions) != 0 {
		t.Fatalf("question request = %#v", question)
	}
	if len(question.Questions) != 1 || question.Questions[0].Question != "Environment?" || len(question.Questions[0].Options) != 2 || question.Questions[0].Options[0].Label != "prod" {
		t.Fatalf("parsed questions = %#v", question.Questions)
	}
	if permission.Revision == question.Revision {
		t.Fatal("different requests received the same revision")
	}
}

func TestCmuxRequestListerRejectsStaleTargetBeforeFeedList(t *testing.T) {
	first := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-A"}]`)
	changed := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-B"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: first},
		controlRPCStep{method: methodListWorkspaces, result: changed},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ListAgentRequests(context.Background(), targets[0].Ref()); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("ListAgentRequests error = %v, want stale target", err)
	}
}

func TestCmuxPermissionResponseRevalidatesCanonicalRequestThenUsesPermissionVerb(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	item := feedItem{ID: "ITEM", Kind: "permissionRequest", Source: "claude", Status: "pending", RequestID: "REQ-PERM", ToolName: "Bash", ToolInput: `{"command":"go test"}`, WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, item)
	controller, script := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodFeedPermissionReply, result: json.RawMessage(`{"delivered":true}`)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	request := controlRequest(t, requests, "REQ-PERM")
	request.Input["command"] = "forged display value"
	request.AllowedDecisions = []string{"forged"}
	if err := controller.RespondAgentPermission(context.Background(), targets[0].Ref(), request.Ref(), "always"); err != nil {
		t.Fatalf("RespondAgentPermission: %v", err)
	}
	want := map[string]any{"request_id": "REQ-PERM", "mode": "always"}
	if got := script.call(t, 5).params; !reflect.DeepEqual(got, want) {
		t.Fatalf("feed.permission.reply params = %#v, want %#v", got, want)
	}
}

func TestCmuxPermissionResponseRejectsReusedChangedRequestBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	before := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-PERM", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-TARGET"}
	after := before
	after.ToolInput = `{"command":"changed"}`
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, before)},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, after)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	request := controlRequest(t, requests, "REQ-PERM")
	if err := controller.RespondAgentPermission(context.Background(), targets[0].Ref(), request.Ref(), "once"); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("RespondAgentPermission error = %v, want stale request", err)
	}
}

func TestCmuxPermissionResponseRejectsChangedFeedCWDRevisionBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	before := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-PERM", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-TARGET", CWD: "/before"}
	after := before
	after.CWD = "/after"
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, before)},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, after)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	request := controlRequest(t, requests, "REQ-PERM")
	if err := controller.RespondAgentPermission(context.Background(), targets[0].Ref(), request.Ref(), "once"); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("RespondAgentPermission error = %v, want stale request after feed CWD change", err)
	}
}

func TestCmuxPermissionResponseRejectsWrongWorkspaceAndKindBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[
		{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"},
		{"id":"WORKSPACE-OTHER","surface_id":"SURFACE-OTHER"}
	]`)
	question := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Continue?"}]}`, WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, question)
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := controlTarget(t, targets, "WORKSPACE-TARGET")
	other := controlTarget(t, targets, "WORKSPACE-OTHER")
	requests, err := controller.ListAgentRequests(context.Background(), target.Ref())
	if err != nil {
		t.Fatal(err)
	}
	request := controlRequest(t, requests, "REQ")
	if err := controller.RespondAgentPermission(context.Background(), target.Ref(), request.Ref(), "once"); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("question response error = %v, want stale request kind", err)
	}
	if err := controller.RespondAgentPermission(context.Background(), other.Ref(), request.Ref(), "once"); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("wrong-workspace response error = %v, want stale request", err)
	}
}

func TestCmuxPermissionResponseRejectsInvalidRefsAndDecisionWithoutWrite(t *testing.T) {
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()))
	validTarget := core.AgentControlTargetRef{ID: "WORKSPACE-TARGET", Revision: "target-revision"}
	validRequest := core.AgentControlRequestRef{ID: "REQ", Revision: "request-revision"}
	if err := controller.RespondAgentPermission(context.Background(), core.AgentControlTargetRef{}, validRequest, "once"); !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("invalid target error = %v", err)
	}
	if err := controller.RespondAgentPermission(context.Background(), validTarget, core.AgentControlRequestRef{}, "once"); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("invalid request error = %v", err)
	}

	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	item := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "Bash", WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, item)
	controller, _ = newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.RespondAgentPermission(context.Background(), targets[0].Ref(), requests[0].Ref(), "ONCE"); !errors.Is(err, core.ErrAgentControlInvalidAnswer) {
		t.Fatalf("invalid decision error = %v, want invalid answer", err)
	}
}

func TestCmuxQuestionResponseOrdersCanonicalSingleSelections(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	item := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"staging"}]},{"question":"Regions?","multiSelect":true,"options":[{"label":"us"},{"label":"eu"}]}]}`, WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, item)
	controller, script := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodFeedQuestionReply, result: json.RawMessage(`{"delivered":true}`)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	answers := []core.AgentControlQuestionAnswer{
		{Index: 1, Selections: []string{"eu"}},
		{Index: 0, Selections: []string{"staging"}},
	}
	if err := controller.AnswerAgentQuestions(context.Background(), targets[0].Ref(), requests[0].Ref(), answers); err != nil {
		t.Fatalf("AnswerAgentQuestions: %v", err)
	}
	want := map[string]any{"request_id": "REQ-QUESTION", "selections": []any{"staging", "eu"}}
	if got := script.call(t, 5).params; !reflect.DeepEqual(got, want) {
		t.Fatalf("feed.question.reply params = %#v, want %#v", got, want)
	}
}

func TestCmuxQuestionResponseRejectsMultipleSelectionsBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	item := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"staging"}]},{"question":"Regions?","multiSelect":true,"options":[{"label":"us"},{"label":"eu"}]}]}`, WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, item)
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: feed},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	answers := []core.AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"staging"}},
		{Index: 1, Selections: []string{"us", "eu"}},
	}
	if err := controller.AnswerAgentQuestions(context.Background(), targets[0].Ref(), requests[0].Ref(), answers); !errors.Is(err, core.ErrAgentControlInvalidAnswer) {
		t.Fatalf("AnswerAgentQuestions error = %v, want invalid answer for unproven multi-select wire encoding", err)
	}
}

func TestCmuxQuestionResponseRejectsStaleCanonicalQuestionBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	before := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"staging"}]}]}`, WorkstreamID: "claude-SESSION-TARGET"}
	after := before
	after.ToolInput = `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"dev"}]}]}`
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, before)},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, after)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
	if err != nil {
		t.Fatal(err)
	}
	answers := []core.AgentControlQuestionAnswer{{Index: 0, Selections: []string{"staging"}}}
	if err := controller.AnswerAgentQuestions(context.Background(), targets[0].Ref(), requests[0].Ref(), answers); !errors.Is(err, core.ErrAgentControlRequestStale) {
		t.Fatalf("AnswerAgentQuestions error = %v, want stale request", err)
	}
}

func TestCmuxQuestionResponseRejectsInvalidCanonicalAnswersBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	item := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Environment?","options":[{"label":"prod"},{"label":"staging"}]},{"question":"Reason?"}]}`, WorkstreamID: "claude-SESSION-TARGET"}
	feed := controlFeedList(t, item)
	invalid := [][]core.AgentControlQuestionAnswer{
		{{Index: 0, Selections: []string{"prod"}}},
		{{Index: 0, Selections: []string{"unknown"}}, {Index: 1, Text: "why"}},
		{{Index: 0, Text: "prod"}, {Index: 1, Text: "why"}},
		{{Index: 0, Selections: []string{"prod"}}, {Index: 0, Selections: []string{"staging"}}},
	}
	for index, answers := range invalid {
		controller, _ := newControlRPCScript(t, controlMapper(t),
			controlRPCStep{method: methodListWorkspaces, result: listing},
			controlRPCStep{method: methodListWorkspaces, result: listing},
			controlRPCStep{method: methodFeedList, result: feed},
			controlRPCStep{method: methodListWorkspaces, result: listing},
			controlRPCStep{method: methodFeedList, result: feed},
		)
		targets, err := controller.ListAgents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
		if err != nil {
			t.Fatal(err)
		}
		if err := controller.AnswerAgentQuestions(context.Background(), targets[0].Ref(), requests[0].Ref(), answers); !errors.Is(err, core.ErrAgentControlInvalidAnswer) {
			t.Fatalf("case %d error = %v, want invalid answer", index, err)
		}
	}
}

func TestCmuxTailRemovesRemainingTerminalControls(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	controller, _ := newControlRPCScript(t, newSessionMapper(t.TempDir()),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodReadScreen, result: json.RawMessage(`{"text":"safe\rFORGED\u0007\b\u007f\u009b2J\u001b]0;x\u001b\\\nkeep\tcolumn"}`)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text, err := controller.TailAgent(context.Background(), targets[0].Ref(), 20)
	if err != nil {
		t.Fatalf("TailAgent: %v", err)
	}
	if want := "safeFORGED2J0;x\nkeep\tcolumn"; text != want {
		t.Fatalf("tail = %q, want control-safe %q", text, want)
	}
}

func TestCmuxRequestListerOmitsGloballyDuplicatePendingRequestIDs(t *testing.T) {
	listing := controlWorkspaceList(`[
		{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"},
		{"id":"WORKSPACE-OTHER","surface_id":"SURFACE-OTHER"}
	]`)
	items := []feedItem{
		{ID: "ITEM-PERM-TARGET", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-PERM", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-TARGET"},
		{ID: "ITEM-PERM-OTHER", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-PERM", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Other?"}]}`, WorkstreamID: "claude-SESSION-OTHER"},
		{ID: "ITEM-QUESTION-TARGET", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Target?"}]}`, WorkstreamID: "claude-SESSION-TARGET"},
		{ID: "ITEM-QUESTION-OTHER", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-QUESTION", ToolName: "Bash", ToolInput: `{"command":"other"}`, WorkstreamID: "claude-SESSION-OTHER"},
	}
	controller, _ := newControlRPCScript(t, controlMapper(t),
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodListWorkspaces, result: listing},
		controlRPCStep{method: methodFeedList, result: controlFeedList(t, items...)},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requests, err := controller.ListAgentRequests(context.Background(), controlTarget(t, targets, "WORKSPACE-TARGET").Ref())
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("globally duplicated requests = %#v, want none", requests)
	}
}

func TestCmuxResponsesRejectGloballyDuplicatePendingRequestIDsBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[
		{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"},
		{"id":"WORKSPACE-OTHER","surface_id":"SURFACE-OTHER"}
	]`)
	tests := []struct {
		name      string
		target    feedItem
		duplicate feedItem
		respond   func(*Controller, core.AgentControlTargetRef, core.AgentControlRequestRef) error
	}{
		{
			name:      "permission",
			target:    feedItem{ID: "ITEM-TARGET", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-TARGET"},
			duplicate: feedItem{ID: "ITEM-OTHER", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Other?"}]}`, WorkstreamID: "claude-SESSION-OTHER"},
			respond: func(controller *Controller, target core.AgentControlTargetRef, request core.AgentControlRequestRef) error {
				return controller.RespondAgentPermission(context.Background(), target, request, "once")
			},
		},
		{
			name:      "question",
			target:    feedItem{ID: "ITEM-TARGET", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Continue?","options":[{"label":"yes"}]}]}`, WorkstreamID: "claude-SESSION-TARGET"},
			duplicate: feedItem{ID: "ITEM-OTHER", Kind: "permissionRequest", Status: "pending", RequestID: "REQ-DUPLICATE", ToolName: "Bash", ToolInput: `{"command":"other"}`, WorkstreamID: "claude-SESSION-OTHER"},
			respond: func(controller *Controller, target core.AgentControlTargetRef, request core.AgentControlRequestRef) error {
				return controller.AnswerAgentQuestions(context.Background(), target, request, []core.AgentControlQuestionAnswer{{Index: 0, Selections: []string{"yes"}}})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _ := newControlRPCScript(t, controlMapper(t),
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodFeedList, result: controlFeedList(t, tt.target)},
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodFeedList, result: controlFeedList(t, tt.target, tt.duplicate)},
			)
			targets, err := controller.ListAgents(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			target := controlTarget(t, targets, "WORKSPACE-TARGET")
			requests, err := controller.ListAgentRequests(context.Background(), target.Ref())
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 1 {
				t.Fatalf("initial requests = %#v, want one", requests)
			}
			if err := tt.respond(controller, target.Ref(), requests[0].Ref()); !errors.Is(err, core.ErrAgentControlRequestStale) {
				t.Fatalf("response error = %v, want stale globally duplicated request", err)
			}
		})
	}
}

func TestCmuxRequestListerOmitsInvalidToolInput(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	tests := []struct {
		name      string
		toolName  string
		toolInput string
	}{
		{name: "malformed permission object", toolName: "Bash", toolInput: `{`},
		{name: "permission array", toolName: "Bash", toolInput: `[]`},
		{name: "permission scalar", toolName: "Bash", toolInput: `"command"`},
		{name: "permission null", toolName: "Bash", toolInput: `null`},
		{name: "question empty object", toolName: "AskUserQuestion", toolInput: `{}`},
		{name: "question empty list", toolName: "AskUserQuestion", toolInput: `{"questions":[]}`},
		{name: "question without canonical entries", toolName: "AskUserQuestion", toolInput: `{"questions":[null,{}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: tt.toolName, ToolInput: tt.toolInput, WorkstreamID: "claude-SESSION-TARGET"}
			controller, _ := newControlRPCScript(t, controlMapper(t),
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodFeedList, result: controlFeedList(t, item)},
			)
			targets, err := controller.ListAgents(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			requests, err := controller.ListAgentRequests(context.Background(), targets[0].Ref())
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 0 {
				t.Fatalf("requests = %#v, want invalid tool_input omitted", requests)
			}
		})
	}
}

func TestCmuxResponsesRejectInvalidReloadedToolInputBeforeWrite(t *testing.T) {
	listing := controlWorkspaceList(`[{"id":"WORKSPACE-TARGET","surface_id":"SURFACE-TARGET"}]`)
	tests := []struct {
		name    string
		before  feedItem
		after   feedItem
		respond func(*Controller, core.AgentControlTargetRef, core.AgentControlRequestRef) error
	}{
		{
			name:   "malformed permission",
			before: feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "Bash", ToolInput: `{"command":"safe"}`, WorkstreamID: "claude-SESSION-TARGET"},
			after:  feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "Bash", ToolInput: `{`, WorkstreamID: "claude-SESSION-TARGET"},
			respond: func(controller *Controller, target core.AgentControlTargetRef, request core.AgentControlRequestRef) error {
				return controller.RespondAgentPermission(context.Background(), target, request, "once")
			},
		},
		{
			name:   "question without canonical questions",
			before: feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "AskUserQuestion", ToolInput: `{"questions":[{"question":"Continue?"}]}`, WorkstreamID: "claude-SESSION-TARGET"},
			after:  feedItem{ID: "ITEM", Kind: "permissionRequest", Status: "pending", RequestID: "REQ", ToolName: "AskUserQuestion", ToolInput: `{}`, WorkstreamID: "claude-SESSION-TARGET"},
			respond: func(controller *Controller, target core.AgentControlTargetRef, request core.AgentControlRequestRef) error {
				return controller.AnswerAgentQuestions(context.Background(), target, request, []core.AgentControlQuestionAnswer{{Index: 0, Text: "yes"}})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _ := newControlRPCScript(t, controlMapper(t),
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodFeedList, result: controlFeedList(t, tt.before)},
				controlRPCStep{method: methodListWorkspaces, result: listing},
				controlRPCStep{method: methodFeedList, result: controlFeedList(t, tt.after)},
			)
			targets, err := controller.ListAgents(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			target := targets[0]
			requests, err := controller.ListAgentRequests(context.Background(), target.Ref())
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 1 {
				t.Fatalf("initial requests = %#v, want one", requests)
			}
			if err := tt.respond(controller, target.Ref(), requests[0].Ref()); !errors.Is(err, core.ErrAgentControlRequestStale) {
				t.Fatalf("response error = %v, want stale invalid canonical request", err)
			}
		})
	}
}
