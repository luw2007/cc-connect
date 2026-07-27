package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type mockRPCHandler func(method string, params json.RawMessage) (json.RawMessage, *rpcError)

type mockCmuxServer struct {
	path    string
	handler mockRPCHandler
	wg      sync.WaitGroup
}

func newMockCmuxServer(t *testing.T, handler mockRPCHandler) *mockCmuxServer {
	t.Helper()
	server := &mockCmuxServer{path: "mock://" + t.Name(), handler: handler}
	t.Cleanup(server.wg.Wait)
	return server
}

// shortSocketPath returns a unix socket path outside t.TempDir(). Its
// per-test nesting (test name + subtest counter) routinely pushes the full
// path past macOS's ~104-byte sockaddr_un limit for our longer test names,
// failing bind with "invalid argument".
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmux")
	if err != nil {
		t.Fatalf("create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "cmux.sock")
}

func (s *mockCmuxServer) client() *client {
	client := newClient(s.path, "")
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(serverConn)
		}()
		return clientConn, nil
	}
	return client
}

func (s *mockCmuxServer) handle(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &request) != nil {
		return
	}
	result, rpcErr := s.handler(request.Method, request.Params)
	response := struct {
		ID     string          `json:"id"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *rpcError       `json:"error,omitempty"`
	}{ID: request.ID, OK: rpcErr == nil, Result: result, Error: rpcErr}
	_ = json.NewEncoder(conn).Encode(response)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func fixtureMapper(t *testing.T) *sessionMapper {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-hook-sessions.json"), fixture(t, "claude-hook-sessions.json"), 0o600); err != nil {
		t.Fatalf("write hook fixture: %v", err)
	}
	return newSessionMapper(dir)
}

func newTestFeedSession(t *testing.T, client *client, mapper *sessionMapper) (*feedBridge, *cmuxSession) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	feed := newFeedBridge(ctx, client, time.Minute, mapper)
	bus := &eventBus{client: client, feed: feed, sessions: make(map[string]*cmuxSession), lastActivity: make(map[string]time.Time)}
	session := newCmuxSession(ctx, client, bus, "WORKSPACE-FIXTURE", "SURFACE-FIXTURE", "/redacted/project", "once", time.Second)
	bus.register(session)
	feed.mu.Lock()
	feed.sessions[session.workspaceID] = session
	feed.mu.Unlock()
	t.Cleanup(func() {
		_ = session.Close()
		feed.close()
		cancel()
	})
	return feed, session
}

func TestFeedDiff_PendingToResolved_EmitsResolved(t *testing.T) {
	responses := [][]byte{fixture(t, "feed_list_pending.json"), fixture(t, "feed_list_resolved.json")}
	var mu sync.Mutex
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedList {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		mu.Lock()
		defer mu.Unlock()
		response := responses[len(responses)-1]
		if len(responses) > 1 {
			response = responses[0]
			responses = responses[1:]
		}
		return response, nil
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("pending resync: %v", err)
	}
	request := <-session.Events()
	if request.Type != core.EventPermissionRequest {
		t.Fatalf("first event type = %q, want permission request", request.Type)
	}
	notified := make(chan string, 1)
	cancelNotify := session.OnExternalResolution("REQ-PENDING", func(note string) { notified <- note })
	defer cancelNotify()
	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("resolved resync: %v", err)
	}
	resolved := <-session.Events()
	if resolved.Type != core.EventPermissionResolved || resolved.RequestID != "REQ-PENDING" {
		t.Fatalf("resolved event = %#v", resolved)
	}
	if resolved.Content != "" {
		t.Fatalf("resolved note = %q, want empty so core localizes it", resolved.Content)
	}
	select {
	case note := <-notified:
		if note != "" {
			t.Fatalf("notification note = %q, want empty so core localizes it", note)
		}
	case <-time.After(time.Second):
		t.Fatal("external resolution notifier was not called")
	}
}

func TestFeedDiff_MissingTwiceUsesGenericLocalizedNote(t *testing.T) {
	responses := [][]byte{
		fixture(t, "feed_list_pending.json"),
		fixture(t, "feed_list_empty.json"),
		fixture(t, "feed_list_empty.json"),
	}
	var mu sync.Mutex
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedList {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		mu.Lock()
		defer mu.Unlock()
		response := responses[0]
		if len(responses) > 1 {
			responses = responses[1:]
		}
		return response, nil
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	if err := feed.resync(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-session.Events()
	if err := feed.resync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := feed.resync(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-session.Events():
		if event.Type != core.EventPermissionResolved || event.Content != "" {
			t.Fatalf("missing-item resolution = %#v, want empty localized note", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing item did not resolve after two confirmed misses")
	}
}

func TestFeedDiff_CompletedFrameIsNotResolution(t *testing.T) {
	pending := fixture(t, "feed_list_pending.json")
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method == methodFeedList {
			return pending, nil
		}
		return nil, &rpcError{Code: "unexpected", Message: method}
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	bus := session.bus
	var received streamFrame
	if err := json.Unmarshal(fixture(t, "events_feed_item_received.jsonl"), &received); err != nil {
		t.Fatalf("decode received frame: %v", err)
	}
	bus.dispatch(received)
	select {
	case event := <-session.Events():
		if event.Type != core.EventPermissionRequest {
			t.Fatalf("event type = %q, want permission request", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("permission request not emitted")
	}
	var completed streamFrame
	if err := json.Unmarshal(fixture(t, "events_feed_item_completed.jsonl"), &completed); err != nil {
		t.Fatalf("decode completed frame: %v", err)
	}
	bus.dispatch(completed)
	select {
	case event := <-session.Events():
		if event.Type == core.EventPermissionResolved {
			t.Fatalf("completed frame incorrectly resolved request: %#v", event)
		}
	case <-time.After(450 * time.Millisecond):
	}
	feed.mu.Lock()
	_, outstanding := feed.outstanding["REQ-PENDING"]
	feed.mu.Unlock()
	if !outstanding {
		t.Fatal("completed frame removed outstanding permission")
	}
}

func TestGapResetsCursorSeq(t *testing.T) {
	var ack streamAck
	if err := json.Unmarshal(fixture(t, "events_stream_ack.json"), &ack); err != nil {
		t.Fatalf("decode ack fixture: %v", err)
	}
	bus := &eventBus{client: newClient("mock://gap", ""), cursor: eventCursor{BootID: "OLD-BOOT", Seq: 24610}}
	if !bus.applyAck(ack) {
		t.Fatal("gap ack was not detected")
	}
	if bus.cursor.Seq != ack.Resume.AfterSeq {
		t.Fatalf("cursor seq = %d, want %d", bus.cursor.Seq, ack.Resume.AfterSeq)
	}
	if bus.cursor.BootID != ack.BootID {
		t.Fatalf("cursor boot = %q, want %q", bus.cursor.BootID, ack.BootID)
	}
}

func TestResync_NeverClearsOnError(t *testing.T) {
	pending := fixture(t, "feed_list_pending.json")
	calls := 0
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		calls++
		if calls == 1 {
			return pending, nil
		}
		return nil, &rpcError{Code: "unavailable", Message: "temporary feed failure"}
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("first resync: %v", err)
	}
	<-session.Events()
	if err := feed.resync(context.Background()); err == nil {
		t.Fatal("failed feed.list returned nil error")
	}
	feed.mu.Lock()
	_, outstanding := feed.outstanding["REQ-PENDING"]
	feed.mu.Unlock()
	if !outstanding {
		t.Fatal("outstanding permission cleared after failed feed.list")
	}
}

func TestResync_DedupsOutstanding(t *testing.T) {
	pending := fixture(t, "feed_list_pending.json")
	server := newMockCmuxServer(t, func(string, json.RawMessage) (json.RawMessage, *rpcError) { return pending, nil })
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	for index := 0; index < 2; index++ {
		if err := feed.resync(context.Background()); err != nil {
			t.Fatalf("resync %d: %v", index, err)
		}
	}
	first := <-session.Events()
	if first.Type != core.EventPermissionRequest {
		t.Fatalf("event type = %q", first.Type)
	}
	select {
	case duplicate := <-session.Events():
		t.Fatalf("duplicate event emitted: %#v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
	feed.mu.Lock()
	count := len(feed.outstanding)
	feed.mu.Unlock()
	if count != 1 {
		t.Fatalf("outstanding count = %d, want 1", count)
	}
}

func TestCorrelation_WorkstreamToWorkspace(t *testing.T) {
	mapper := fixtureMapper(t)
	workspaceID, ok := mapper.workspaceIDForWorkstream("claude-SESSION-FIXTURE")
	if !ok || workspaceID != "WORKSPACE-FIXTURE" {
		t.Fatalf("correlation = %q, %v", workspaceID, ok)
	}
	if surfaceID, ok := mapper.surfaceIDForWorkspace(workspaceID); !ok || surfaceID != "SURFACE-FIXTURE" {
		t.Fatalf("surface correlation = %q, %v", surfaceID, ok)
	}
}

func TestSessionMapperAmbiguousWorkstreamFailsClosedAcrossSources(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{
			"WORKSPACE-A": {SessionID: "SESSION-SHARED", UpdatedAt: 10},
			"WORKSPACE-B": {SessionID: "SESSION-SHARED", UpdatedAt: 20},
		},
		Sessions: map[string]hookSession{
			"SESSION-A": {SessionID: "SESSION-SHARED", WorkspaceID: "WORKSPACE-A"},
			"SESSION-B": {SessionID: "SESSION-SHARED", WorkspaceID: "WORKSPACE-B"},
		},
	})
	writeControlHookStore(t, dir, "codex", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{
			"WORKSPACE-C": {SessionID: "SESSION-SHARED", UpdatedAt: 30},
		},
		Sessions: map[string]hookSession{
			"SESSION-SHARED": {SessionID: "SESSION-SHARED", WorkspaceID: "WORKSPACE-C"},
		},
	})

	for iteration := 0; iteration < 100; iteration++ {
		mapper := newSessionMapper(dir)
		if workspaceID, ok := mapper.workspaceIDForWorkstream("claude-SESSION-SHARED"); ok {
			t.Fatalf("iteration %d: ambiguous claude workstream routed to %q", iteration, workspaceID)
		}
		if workspaceID, ok := mapper.workspaceIDForWorkstream("codex-SESSION-SHARED"); !ok || workspaceID != "WORKSPACE-C" {
			t.Fatalf("iteration %d: distinct codex workstream = %q, %v; want WORKSPACE-C", iteration, workspaceID, ok)
		}
	}
}

func TestSessionMapperRejectsMismatchedActiveWorkspaceSurface(t *testing.T) {
	dir := t.TempDir()
	writeControlHookStore(t, dir, "claude", hookSessionStore{
		ActiveByWorkspace: map[string]hookSessionRef{"WORKSPACE-A": {SessionID: "SESSION"}},
		Sessions: map[string]hookSession{
			"SESSION": {SessionID: "SESSION", SurfaceID: "SURFACE-B", WorkspaceID: "WORKSPACE-B"},
		},
	})
	mapper := newSessionMapper(dir)
	if workspaceID, ok := mapper.workspaceIDForSurface("SURFACE-B"); !ok || workspaceID != "WORKSPACE-B" {
		t.Fatalf("mismatched active session surface = %q, %v; want canonical WORKSPACE-B", workspaceID, ok)
	}
}

func TestSessionMapper_NewestSessionWinsDeterministically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-hook-sessions.json")
	if err := os.WriteFile(path, fixture(t, "codex-hook-sessions-multi.json"), 0o600); err != nil {
		t.Fatalf("write multi-session fixture: %v", err)
	}
	for iteration := 0; iteration < 100; iteration++ {
		mapper := newSessionMapper(dir)
		if surfaceID, ok := mapper.surfaceIDForWorkspace("WORKSPACE-MULTI"); !ok || surfaceID != "SURFACE-NEWEST" {
			t.Fatalf("iteration %d: surface = %q, %v; want newest SURFACE-NEWEST", iteration, surfaceID, ok)
		}
		if lifecycle := mapper.workspaceLifecycle("WORKSPACE-MULTI"); lifecycle != "needsInput" {
			t.Fatalf("iteration %d: lifecycle = %q, want newest needsInput", iteration, lifecycle)
		}
	}
}

func TestSocketResolver_LastSocketPath(t *testing.T) {
	wantPath := "mock://last-socket"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CMUX_SOCKET_PATH", "")
	stateDir := filepath.Join(home, ".local", "state", "cmux")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "last-socket-path"), []byte(wantPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSocketWithProbe(context.Background(), "", "", func(_ context.Context, path, _ string) error {
		if path == wantPath {
			return nil
		}
		return errors.New("not the fixture socket")
	})
	if err != nil {
		t.Fatalf("resolve socket: %v", err)
	}
	if resolved != wantPath {
		t.Fatalf("resolved = %q, want %q", resolved, wantPath)
	}
}

func TestControllerSocketResolverExplicitFailureDoesNotFallBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CMUX_SOCKET_PATH", "mock://fallback")
	var probed []string
	resolved, err := resolveControllerSocketWithProbe(context.Background(), " mock://explicit ", "", func(_ context.Context, path, _ string) error {
		probed = append(probed, path)
		if path == "mock://fallback" {
			return nil
		}
		return errors.New("fixture failure")
	})
	if err == nil {
		t.Fatalf("resolved explicit socket as %q, want failure", resolved)
	}
	if !reflect.DeepEqual(probed, []string{" mock://explicit "}) {
		t.Fatalf("probed paths = %v, want only the exact explicit controller binding", probed)
	}
}

func TestControllerSocketResolverReturnsExactExplicitBinding(t *testing.T) {
	explicit := " mock://explicit "
	resolved, err := resolveControllerSocketWithProbe(context.Background(), explicit, "secret", func(_ context.Context, path, password string) error {
		if path != explicit || password != "secret" {
			t.Fatalf("probe binding = %q/%q, want exact explicit path and password", path, password)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("resolve explicit controller socket: %v", err)
	}
	if resolved != explicit {
		t.Fatalf("resolved = %q, want exact binding %q", resolved, explicit)
	}
}

func TestControllerSocketResolverUnsetUsesDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CMUX_SOCKET_PATH", "mock://discovered")
	resolved, err := resolveControllerSocketWithProbe(context.Background(), "", "", func(_ context.Context, path, _ string) error {
		if path == "mock://discovered" {
			return nil
		}
		return errors.New("not the fixture socket")
	})
	if err != nil {
		t.Fatalf("resolve unset controller socket: %v", err)
	}
	if resolved != "mock://discovered" {
		t.Fatalf("resolved = %q, want discovered controller socket", resolved)
	}
}

func TestSocketResolverExplicitFailureStillFallsBackForAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CMUX_SOCKET_PATH", "mock://fallback")
	var probed []string
	resolved, err := resolveSocketWithProbe(context.Background(), "mock://explicit", "", func(_ context.Context, path, _ string) error {
		probed = append(probed, path)
		if path == "mock://fallback" {
			return nil
		}
		return errors.New("fixture failure")
	})
	if err != nil {
		t.Fatalf("resolve Agent.New socket: %v", err)
	}
	if resolved != "mock://fallback" {
		t.Fatalf("resolved = %q, want Agent.New fallback socket", resolved)
	}
	wantProbed := []string{"mock://explicit", "mock://fallback"}
	if !reflect.DeepEqual(probed, wantProbed) {
		t.Fatalf("probed paths = %v, want %v", probed, wantProbed)
	}
}

func TestMethodStrings_MatchCapabilitiesEnum(t *testing.T) {
	probeText := string(fixture(t, "PROBES.md"))
	methods := []string{
		methodEventsStream,
		methodFeedList,
		methodFeedPermissionReply,
		methodFeedQuestionReply,
		methodFeedExitPlanReply,
		methodListWorkspaces,
		methodNewWorkspace,
		methodReadScreen,
		methodSend,
		methodSendKey,
	}
	for _, method := range methods {
		if !strings.Contains(probeText, "`"+method+"`") {
			t.Errorf("method %q is not pinned in PROBES.md", method)
		}
	}
}

func TestEventStream_MockStreamsRawFixtureBytes(t *testing.T) {
	ackBytes := fixture(t, "events_stream_ack.json")
	hookBytes := fixture(t, "events_agent_hook.jsonl")
	emptyFeed := fixture(t, "feed_list_empty.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newClient("mock://event-stream", "")
	var serverWG sync.WaitGroup
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		serverWG.Add(1)
		go func() {
			defer serverWG.Done()
			defer serverConn.Close()
			line, readErr := bufio.NewReader(serverConn).ReadBytes('\n')
			if readErr != nil {
				return
			}
			var request rpcRequest
			if json.Unmarshal(line, &request) != nil {
				return
			}
			if request.Method == methodEventsStream {
				_, _ = serverConn.Write(append(append(ackBytes, '\n'), append(hookBytes, '\n')...))
				return
			}
			response := struct {
				ID     string          `json:"id"`
				OK     bool            `json:"ok"`
				Result json.RawMessage `json:"result"`
			}{ID: request.ID, OK: true, Result: emptyFeed}
			_ = json.NewEncoder(serverConn).Encode(response)
		}()
		return clientConn, nil
	}
	defer serverWG.Wait()
	mapper := fixtureMapper(t)
	feed := newFeedBridge(ctx, client, time.Minute, mapper)
	defer feed.close()
	bus := &eventBus{client: client, cursorPath: filepath.Join(t.TempDir(), "cursor.json"), feed: feed, ctx: ctx, cancel: cancel, sessions: make(map[string]*cmuxSession), lastActivity: make(map[string]time.Time)}
	session := newCmuxSession(ctx, client, bus, "WORKSPACE-FIXTURE", "SURFACE-FIXTURE", "/redacted/project", "once", time.Second)
	defer session.Close()
	bus.register(session)
	feed.mu.Lock()
	feed.sessions[session.workspaceID] = session
	feed.mu.Unlock()
	streamCtx, streamCancel := context.WithTimeout(ctx, time.Second)
	defer streamCancel()
	err := bus.connectOnce(streamCtx)
	if err == nil || !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("connectOnce error = %v, want stream close", err)
	}
	select {
	case hook := <-session.hookEvents:
		if hook != "Stop" {
			t.Fatalf("hook = %q, want Stop", hook)
		}
	case <-time.After(time.Second):
		t.Fatal("raw hook fixture was not dispatched")
	}
}

func TestListWorkspaces_ParsesRealResponseShape(t *testing.T) {
	listing := fixture(t, "workspace_list_real.json")
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodListWorkspaces {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		return listing, nil
	})
	workspaces, err := server.client().listWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}
	if len(workspaces) != 1 {
		t.Fatalf("len(workspaces) = %d, want 1", len(workspaces))
	}
	ws := workspaces[0]
	if ws.ID != "WORKSPACE-FIXTURE" {
		t.Fatalf("ID = %q", ws.ID)
	}
	if ws.Title != "cc-connect" {
		t.Fatalf("Title = %q, want the real live title field mapped for display only", ws.Title)
	}
	if ws.CWD != "/redacted/project" {
		t.Fatalf("CWD = %q, want the real \"current_directory\" field mapped", ws.CWD)
	}
}

func TestNewWorkspace_ParsesRealResponseShape(t *testing.T) {
	created := fixture(t, "workspace_create_result.json")
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodNewWorkspace {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		return created, nil
	})
	ws, err := server.client().newWorkspace(context.Background(), "cc-probe", "/redacted/project", "")
	if err != nil {
		t.Fatalf("newWorkspace: %v", err)
	}
	if ws.ID != "WORKSPACE-FIXTURE" {
		t.Fatalf("ID = %q, want the real flat workspace_id field", ws.ID)
	}
	if ws.SurfaceID != "SURFACE-FIXTURE" {
		t.Fatalf("SurfaceID = %q, want the real flat surface_id field", ws.SurfaceID)
	}
	if ws.Name != "cc-probe" || ws.CWD != "/redacted/project" {
		t.Fatalf("Name/CWD not preserved from request (workspace.create never echoes them): %+v", ws)
	}
}

func TestStartSession_AttachWithoutSurfaceFails(t *testing.T) {
	listing := fixture(t, "workspace_list_real.json")
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodListWorkspaces {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		return listing, nil
	})
	agent := &Agent{
		client:           server.client(),
		mapper:           newSessionMapper(t.TempDir()), // empty: no hook-sessions.json to resolve a surface
		workDir:          ".",
		createWorkspaces: false,
		pollInterval:     time.Second,
		feedReplyMode:    "once",
		sessions:         make(map[string]*cmuxSession),
	}
	_, err := agent.StartSession(context.Background(), "WORKSPACE-FIXTURE")
	if err == nil {
		t.Fatal("expected an error when no terminal surface can be resolved")
	}
	if !strings.Contains(err.Error(), "could not resolve a terminal surface") {
		t.Fatalf("error = %v, want surface-resolution error", err)
	}
}

func TestWatchTurn_OutstandingPermissionSuppressesStableCompletion(t *testing.T) {
	screen := json.RawMessage(`{"text":"after prompt"}`)
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method == methodReadScreen {
			return screen, nil
		}
		return nil, &rpcError{Code: "unexpected", Message: method}
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	session.pollInterval = 500 * time.Millisecond // watch interval clamps to 100ms
	session.turnMu.Lock()
	session.baseline = "before prompt"
	session.lastEmitted = "before prompt"
	session.turnMu.Unlock()
	feed.mu.Lock()
	feed.outstanding["REQ-WAITING"] = &pendingRef{
		item:     feedItem{RequestID: "REQ-WAITING"},
		session:  session,
		deadline: time.Now().Add(time.Minute),
	}
	feed.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.watchTurn(ctx)
	}()
	deadline := time.NewTimer(5500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-session.Events():
			if event.Type == core.EventResult {
				cancel()
				<-done
				t.Fatalf("turn completed while permission was outstanding: %#v", event)
			}
		case <-deadline.C:
			cancel()
			<-done
			return
		}
	}
}

func TestClientCall_CanceledContextUnblocksStalledRead(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requestRead := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(serverConn).ReadBytes('\n')
		close(requestRead)
	}()
	client := newClient("unused", "")
	client.dial = func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.call(ctx, methodPing, map[string]any{}, nil) }()
	<-requestRead
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("call error = %v, want cancellation/closed connection", err)
		}
	case <-time.After(500 * time.Millisecond):
		_ = serverConn.Close()
		<-done
		t.Fatal("call stayed blocked after its context was canceled")
	}
}

func TestClientFeedReplies_RequireDeliveredResult(t *testing.T) {
	replies := []struct {
		name   string
		method string
		call   func(*client) error
	}{
		{
			name:   "permission",
			method: methodFeedPermissionReply,
			call: func(client *client) error {
				return client.feedPermissionReply(context.Background(), "REQ-PERMISSION", "once")
			},
		},
		{
			name:   "question",
			method: methodFeedQuestionReply,
			call: func(client *client) error {
				return client.feedQuestionReply(context.Background(), "REQ-QUESTION", []string{"answer"})
			},
		},
	}
	results := []struct {
		name      string
		result    json.RawMessage
		wantStale bool
	}{
		{name: "delivered", result: json.RawMessage(`{"delivered":true}`)},
		{name: "not_delivered", result: json.RawMessage(`{"delivered":false}`), wantStale: true},
		{name: "missing", wantStale: true},
		{name: "null", result: json.RawMessage(`null`), wantStale: true},
		{name: "malformed", result: json.RawMessage(`{"delivered":"yes"}`), wantStale: true},
	}
	for _, reply := range replies {
		for _, result := range results {
			t.Run(reply.name+"/"+result.name, func(t *testing.T) {
				server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
					if method != reply.method {
						return nil, &rpcError{Code: "unexpected", Message: method}
					}
					return result.result, nil
				})
				err := reply.call(server.client())
				if result.wantStale {
					if !errors.Is(err, core.ErrAgentControlRequestStale) {
						t.Fatalf("reply error = %v, want stale request", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("reply error = %v, want delivery success", err)
				}
			})
		}
	}
}

func TestReplyPermission_TombstoneBlocksStaleResyncReemit(t *testing.T) {
	pending := fixture(t, "feed_list_pending.json")
	var listCalls int
	var mu sync.Mutex
	staleListStarted := make(chan struct{})
	releaseStaleList := make(chan struct{})
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		switch method {
		case methodFeedList:
			mu.Lock()
			listCalls++
			call := listCalls
			mu.Unlock()
			if call == 2 {
				close(staleListStarted)
				<-releaseStaleList
			}
			return pending, nil
		case methodFeedPermissionReply:
			return json.RawMessage(`{"delivered":true}`), nil
		default:
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	if err := feed.resync(context.Background()); err != nil {
		t.Fatalf("initial resync: %v", err)
	}
	<-session.Events()
	resyncDone := make(chan error, 1)
	go func() { resyncDone <- feed.resync(context.Background()) }()
	<-staleListStarted
	if err := feed.replyPermission(context.Background(), "REQ-PENDING", "once", nil); err != nil {
		t.Fatalf("reply permission: %v", err)
	}
	close(releaseStaleList)
	if err := <-resyncDone; err != nil {
		t.Fatalf("stale resync: %v", err)
	}
	select {
	case event := <-session.Events():
		t.Fatalf("stale feed.list re-emitted replied permission: %#v", event)
	case <-time.After(350 * time.Millisecond):
	}
}

func TestReplyPermission_MarkThenDeleteAroundWireCall(t *testing.T) {
	replyStarted := make(chan struct{})
	releaseReply := make(chan struct{})
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedPermissionReply {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		close(replyStarted)
		<-releaseReply
		return json.RawMessage(`{"delivered":true}`), nil
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	ref := &pendingRef{item: feedItem{RequestID: "REQ-MARK"}, session: session, deadline: time.Now().Add(time.Minute)}
	feed.mu.Lock()
	feed.outstanding["REQ-MARK"] = ref
	feed.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- feed.replyPermission(context.Background(), "REQ-MARK", "once", nil) }()
	<-replyStarted
	feed.mu.Lock()
	current := feed.outstanding["REQ-MARK"]
	marked := current == ref && current.replying
	feed.mu.Unlock()
	if !marked {
		t.Fatal("permission was not marked replying while wire call was in flight")
	}
	close(releaseReply)
	if err := <-done; err != nil {
		t.Fatalf("reply permission: %v", err)
	}
	feed.mu.Lock()
	_, exists := feed.outstanding["REQ-MARK"]
	until := feed.repliedUntil["REQ-MARK"]
	feed.mu.Unlock()
	if exists {
		t.Fatal("successful permission reply was not deleted")
	}
	if !until.After(time.Now()) {
		t.Fatal("successful permission reply did not leave a live tombstone")
	}
}

func TestFeedExpire_EmitsFallbackSentinelAndPurgesTombstones(t *testing.T) {
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		return nil, &rpcError{Code: "unexpected", Message: method}
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	now := time.Now()
	ref := &pendingRef{
		item:     feedItem{RequestID: "REQ-EXPIRED"},
		session:  session,
		deadline: now.Add(-time.Second),
	}
	feed.mu.Lock()
	feed.outstanding[ref.item.RequestID] = ref
	feed.repliedUntil["REQ-OLD-TOMBSTONE"] = now.Add(-time.Second)
	feed.mu.Unlock()
	notified := make(chan string, 1)
	cancelNotify := session.OnExternalResolution(ref.item.RequestID, func(note string) { notified <- note })
	defer cancelNotify()
	feed.expire(now)
	var note string
	select {
	case note = <-notified:
	case <-time.After(time.Second):
		t.Fatal("expiry did not notify external-resolution listener")
	}
	if note == "" || strings.Contains(strings.ToLower(note), "cmux") || strings.Contains(strings.ToLower(note), "terminal") {
		t.Fatalf("expiry note = %q, want opaque core fallback sentinel", note)
	}
	select {
	case event := <-session.Events():
		if event.Type != core.EventPermissionResolved || event.RequestID != ref.item.RequestID || event.Content != note {
			t.Fatalf("expiry event = %#v, note = %q", event, note)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry did not emit permission-resolved event")
	}
	feed.mu.Lock()
	_, outstanding := feed.outstanding[ref.item.RequestID]
	_, tombstone := feed.repliedUntil["REQ-OLD-TOMBSTONE"]
	feed.mu.Unlock()
	if outstanding || tombstone {
		t.Fatalf("expiry sweep left state: outstanding=%v tombstone=%v", outstanding, tombstone)
	}
}

func TestReplyPermission_AskUserQuestionRoutesSelections(t *testing.T) {
	paramsCh := make(chan map[string]any, 1)
	server := newMockCmuxServer(t, func(method string, params json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedQuestionReply {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		var decoded map[string]any
		if err := json.Unmarshal(params, &decoded); err != nil {
			return nil, &rpcError{Code: "decode", Message: err.Error()}
		}
		paramsCh <- decoded
		return json.RawMessage(`{"delivered":true}`), nil
	})
	feed, session := newTestFeedSession(t, server.client(), fixtureMapper(t))
	item := feedItem{
		RequestID: "REQ-QUESTION",
		ToolName:  "AskUserQuestion",
		ToolInput: `{"questions":[{"question":"Environment?","header":"Env","options":[{"label":"prod"},{"label":"staging"}]},{"question":"Region?","header":"Region","options":[{"label":"us"},{"label":"eu"}]}]}`,
	}
	feed.mu.Lock()
	feed.outstanding[item.RequestID] = &pendingRef{item: item, session: session, deadline: time.Now().Add(time.Minute)}
	feed.mu.Unlock()
	updated := map[string]any{"answers": map[string]any{"Region?": "eu", "Environment?": "staging"}}
	if err := feed.replyPermission(context.Background(), item.RequestID, "once", updated); err != nil {
		t.Fatalf("reply question: %v", err)
	}
	params := <-paramsCh
	if params["request_id"] != item.RequestID {
		t.Fatalf("request_id = %#v", params["request_id"])
	}
	if got, want := params["selections"], []any{"staging", "eu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selections = %#v, want %#v", got, want)
	}
}

func TestFeedTrigger_DebounceRearmsAfterFiring(t *testing.T) {
	var calls atomic.Int32
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodFeedList {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		calls.Add(1)
		return fixture(t, "feed_list_empty.json"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	feed := newFeedBridge(ctx, server.client(), time.Minute, fixtureMapper(t))
	defer func() {
		feed.close()
		cancel()
	}()
	triggerPair := func(want int32) {
		feed.trigger()
		time.Sleep(time.Millisecond)
		feed.trigger()
		deadline := time.Now().Add(time.Second)
		for calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := calls.Load(); got != want {
			t.Fatalf("feed.list calls = %d, want %d", got, want)
		}
	}
	triggerPair(1)
	triggerPair(2)
}

func TestEventBus_SharedPerSocketRefcount(t *testing.T) {
	socket := "test-refcount-" + t.Name()
	client := newClient(socket, "")
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test dial stopped")
	}
	mapper := newSessionMapper(t.TempDir())
	bus1, release1 := acquireEventBus(client, filepath.Join(t.TempDir(), "cursor-1.json"), time.Minute, mapper)
	bus2, release2 := acquireEventBus(client, filepath.Join(t.TempDir(), "cursor-2.json"), time.Minute, mapper)
	if bus1 != bus2 {
		t.Fatal("same socket acquired two different event buses")
	}
	sharedBuses.Lock()
	refs := sharedBuses.entries[socket].references
	sharedBuses.Unlock()
	if refs != 2 {
		t.Fatalf("references = %d, want 2", refs)
	}
	release1()
	sharedBuses.Lock()
	refs = sharedBuses.entries[socket].references
	sharedBuses.Unlock()
	if refs != 1 {
		t.Fatalf("references after first release = %d, want 1", refs)
	}
	release2()
	sharedBuses.Lock()
	_, exists := sharedBuses.entries[socket]
	sharedBuses.Unlock()
	if exists {
		t.Fatal("event bus entry survived final release")
	}
}

func TestMatchWorkspace_UsesCustomTitleButNotLiveTitle(t *testing.T) {
	listing := json.RawMessage(`{"workspaces":[
		{"id":"LIVE","title":"spinner-live-title","current_directory":"/one","custom_title":null,"has_custom_title":false},
		{"id":"CUSTOM","title":"spinner-other","current_directory":"/two","custom_title":"stable-name","has_custom_title":true}
	]}`)
	server := newMockCmuxServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *rpcError) {
		if method != methodListWorkspaces {
			return nil, &rpcError{Code: "unexpected", Message: method}
		}
		return listing, nil
	})
	workspaces, err := server.client().listWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if got := matchWorkspace(workspaces, "stable-name", ""); got == nil || got.ID != "CUSTOM" {
		t.Fatalf("custom-title match = %#v, want CUSTOM", got)
	}
	if got := matchWorkspace(workspaces, "spinner-live-title", ""); got != nil {
		t.Fatalf("live terminal title unexpectedly matched workspace: %#v", got)
	}
}

func TestListWorkspaces_MapsLatestSubmittedAt(t *testing.T) {
	listing := json.RawMessage(`{"workspaces":[{"id":"WS","title":"live","current_directory":"/project","latest_submitted_at":"2026-07-27T01:02:03Z"}]}`)
	server := newMockCmuxServer(t, func(string, json.RawMessage) (json.RawMessage, *rpcError) { return listing, nil })
	workspaces, err := server.client().listWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := parseTimestamp(workspaces[0].UpdatedAt); got.IsZero() {
		t.Fatal("latest_submitted_at was not mapped to workspace recency")
	}
}

func TestNew_AbsolutizesDefaultWorkDir(t *testing.T) {
	workDir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	resolved, err := absoluteWorkDir(".")
	if err != nil {
		t.Fatalf("absoluteWorkDir: %v", err)
	}
	resolvedInfo, resolvedErr := os.Stat(resolved)
	wantInfo, wantErr := os.Stat(workDir)
	if resolvedErr != nil || wantErr != nil || !os.SameFile(resolvedInfo, wantInfo) || !filepath.IsAbs(resolved) {
		t.Fatalf("workDir = %q, want absolute path to %q", resolved, workDir)
	}
}
