package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestNewValidation(t *testing.T) {
	// Missing init_command should fail before any socket access.
	if _, err := New(map[string]any{}); err == nil {
		t.Error("expected error when init_command is empty")
	}
}

func TestNewHerdrSessionWorkDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient("/nonexistent.sock") // not dialed by this test
	s := newHerdrSession(ctx, c, "cc-test1", "/tmp/workspace", 200*time.Millisecond, true, true, true, 5)
	defer s.Close()

	if s.workDir != "/tmp/workspace" {
		t.Errorf("workDir = %q, want /tmp/workspace", s.workDir)
	}
	if s.CurrentSessionID() != "cc-test1" {
		t.Errorf("CurrentSessionID() = %q, want cc-test1", s.CurrentSessionID())
	}
	if !s.Alive() {
		t.Error("expected new session to be alive")
	}
}

// mockHerdrClient uses one net.Pipe per RPC, preserving herdr's real
// one-request-per-connection framing without relying on sandbox-blocked Unix
// socket listeners.
type subscribeScript struct {
	frames     []string
	frameDelay time.Duration
	hold       bool
}

func mockHerdrClient(t *testing.T, respond func(method string, params json.RawMessage) rpcResponse, subscribeScripts ...subscribeScript) *client {
	t.Helper()
	var scriptMu sync.Mutex
	nextScript := 0
	c := newClient("mock")
	c.dial = func(ctx context.Context) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		clientConn, serverConn := net.Pipe()
		go func(c net.Conn) {
			defer c.Close()
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil && line == "" {
				return
			}
			var req struct {
				ID     string          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				return
			}
			resp := respond(req.Method, req.Params)
			resp.ID = req.ID
			b, _ := json.Marshal(resp)
			_, _ = c.Write(append(b, '\n'))
			if req.Method != "events.subscribe" || resp.Error != nil {
				return
			}
			scriptMu.Lock()
			idx := nextScript
			nextScript++
			scriptMu.Unlock()
			if idx >= len(subscribeScripts) {
				return
			}
			script := subscribeScripts[idx]
			for _, frame := range script.frames {
				if script.frameDelay > 0 {
					time.Sleep(script.frameDelay)
				}
				if _, err := c.Write(append([]byte(frame), '\n')); err != nil {
					return
				}
			}
			if script.hold {
				_, _ = bufio.NewReader(c).ReadByte()
			}
		}(serverConn)
		return clientConn, nil
	}
	return c
}

func waitEvent(t *testing.T, events <-chan core.Event, timeout time.Duration) core.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for event", timeout)
		return core.Event{}
	}
}

func TestClientCallSuccess(t *testing.T) {
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		if method != "agent.get" {
			t.Errorf("unexpected method %q", method)
		}
		return rpcResponse{Result: json.RawMessage(`{"agent":{"terminal_id":"t1","pane_id":"wA:p1","agent_status":"idle"}}`)}
	})

	info, err := c.agentGet(context.Background(), "cc-test")
	if err != nil {
		t.Fatalf("agentGet: %v", err)
	}
	if info.PaneID != "wA:p1" || info.AgentStatus != "idle" {
		t.Errorf("agentGet() = %+v, want pane_id=wA:p1 agent_status=idle", info)
	}
}

func TestClientCallError(t *testing.T) {
	c := mockHerdrClient(t, func(_ string, _ json.RawMessage) rpcResponse {
		return rpcResponse{Error: &rpcError{Code: "agent_not_found", Message: "boom"}}
	})

	_, err := c.agentGet(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestClientCallOneRequestPerConnection guards the documented herdr
// behavior this package relies on: the server answers exactly one request
// per connection. If a future herdr version changes this, client.go's
// "open a fresh connection per call" design would need revisiting.
func TestClientCallOneRequestPerConnection(t *testing.T) {
	var connCount int
	c := mockHerdrClient(t, func(_ string, _ json.RawMessage) rpcResponse {
		connCount++
		return rpcResponse{Result: json.RawMessage(`{}`)}
	})

	for i := 0; i < 3; i++ {
		if err := c.call(context.Background(), "ping", nil, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if connCount != 3 {
		t.Errorf("connCount = %d, want 3 (one connection per call)", connCount)
	}
}

func TestSend_UsesAgentPrompt(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "agent.get":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		case "agent.read":
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"baseline"}}`)}
		case "agent.prompt":
			return rpcResponse{Result: json.RawMessage(`{}`)}
		case "agent.wait":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: "unexpected " + method}}
		}
	})

	s := newHerdrSession(context.Background(), c, "cc-turn", "/tmp", 100*time.Millisecond, false, false, false, 5)
	defer s.Close()
	if err := s.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(methods, "agent.prompt") {
		t.Fatalf("methods = %v, want agent.prompt", methods)
	}
	if slices.Contains(methods, "agent.send") {
		t.Fatalf("methods = %v, agent.send is not protocol 17", methods)
	}
}

func TestBlockedEmitsPermissionRequest_EdgeTriggered(t *testing.T) {
	tail := "Choose:\n1. Allow once\n2. Deny"
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		if method != "agent.read" {
			return rpcResponse{Error: &rpcError{Message: "unexpected " + method}}
		}
		return rpcResponse{Result: json.RawMessage(`{"read":{"text":"Choose:\n1. Allow once\n2. Deny"}}`)}
	})
	s := newHerdrSession(context.Background(), c, "cc-blocked", "/tmp", 100*time.Millisecond, false, true, true, 5)
	defer s.Close()

	turnCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel}
	s.handleBlocked(turn)
	s.handleBlocked(turn)
	ev := waitEvent(t, s.Events(), time.Second)
	if ev.Type != core.EventPermissionRequest || ev.ToolName != "AskUserQuestion" {
		t.Fatalf("event = %+v, want AskUserQuestion permission request", ev)
	}
	if ev.ToolInput != tail || len(ev.Questions) != 1 || ev.Questions[0].Question != tail {
		t.Fatalf("blocked tail not preserved in event: %+v", ev)
	}
	select {
	case extra := <-s.Events():
		t.Fatalf("repeated blocked status emitted a second event: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRespondPermission_MapsOptionToSendKeys(t *testing.T) {
	var gotMethod string
	var gotParams map[string]any
	var mu sync.Mutex
	c := mockHerdrClient(t, func(method string, params json.RawMessage) rpcResponse {
		switch method {
		case "agent.get":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		case "agent.read":
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"1. Yes\n2. No"}}`)}
		case "agent.send_keys":
			mu.Lock()
			gotMethod = method
			_ = json.Unmarshal(params, &gotParams)
			mu.Unlock()
			return rpcResponse{Result: json.RawMessage(`{}`)}
		default:
			return rpcResponse{Error: &rpcError{Message: "unexpected " + method}}
		}
	})
	s := newHerdrSession(context.Background(), c, "cc-blocked", "/tmp", 100*time.Millisecond, false, true, true, 5)
	defer s.Close()
	s.handleBlocked(nil)
	ev := waitEvent(t, s.Events(), time.Second)
	question := ev.Questions[0].Question
	result := core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{
		"answers": map[string]any{question: "Key2"},
	}}
	if err := s.RespondPermission(ev.RequestID, result); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "agent.send_keys" {
		t.Fatalf("method = %q, want agent.send_keys", gotMethod)
	}
	if gotParams["target"] != "cc-blocked" {
		t.Fatalf("target = %#v, want cc-blocked", gotParams["target"])
	}
	keys, _ := gotParams["keys"].([]any)
	if len(keys) != 1 || keys[0] != "2" {
		t.Fatalf("keys = %#v, want [2]", gotParams["keys"])
	}
}

func TestOptionLabelsNeverParseAsInt(t *testing.T) {
	questions := []core.UserQuestion{
		buildBlockedQuestion("plain blocked screen"),
		buildBlockedQuestion("Choose:\n1. Alpha\n2. Beta\n3. Gamma"),
	}
	for _, q := range questions {
		for _, opt := range q.Options {
			if _, err := strconv.Atoi(opt.Label); err == nil {
				t.Fatalf("option label %q parses as an integer", opt.Label)
			}
		}
	}
}

func TestStreamOutput_SeedsBaseline_NoFullScreenDump(t *testing.T) {
	var mu sync.Mutex
	visibleReads := 0
	c := mockHerdrClient(t, func(method string, params json.RawMessage) rpcResponse {
		switch method {
		case "agent.get":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		case "agent.read":
			var request struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(params, &request)
			if request.Source == "recent" {
				return rpcResponse{Result: json.RawMessage(`{"read":{"text":"prior turn from recent capture"}}`)}
			}
			mu.Lock()
			defer mu.Unlock()
			visibleReads++
			if visibleReads == 1 {
				return rpcResponse{Result: json.RawMessage(`{"read":{"text":"TUI header\nold visible frame\n>"}}`)}
			}
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"TUI header\nold visible frame\nnew output only\n>"}}`)}
		case "agent.prompt":
			return rpcResponse{Result: json.RawMessage(`{}`)}
		case "agent.wait":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		default:
			return rpcResponse{Error: &rpcError{Message: "unexpected " + method}}
		}
	})
	s := newHerdrSession(context.Background(), c, "cc-stream", "/tmp", 100*time.Millisecond, false, false, true, 5)
	defer s.Close()
	if err := s.Send("new prompt", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(t, s.Events(), time.Second)
	if ev.Type != core.EventText || ev.Content != "new output only" {
		t.Fatalf("first stream event = %+v, want only post-prompt visible delta", ev)
	}
	if strings.Contains(ev.Content, "prior turn") || strings.Contains(ev.Content, "old visible") {
		t.Fatalf("first stream delta dumped pre-turn content: %+v", ev)
	}
}

func TestSubscribeReconnect_ResnapshotsBeforeFrames(t *testing.T) {
	var getMu sync.Mutex
	getCount := 0
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		switch method {
		case "agent.get":
			getMu.Lock()
			getCount++
			status := "idle"
			if getCount > 1 {
				status = "blocked"
			}
			getMu.Unlock()
			return rpcResponse{Result: json.RawMessage(`{"agent":{"pane_id":"p1","agent_status":"` + status + `"}}`)}
		case "events.subscribe":
			return rpcResponse{Result: json.RawMessage(`{"type":"subscription_started"}`)}
		default:
			return rpcResponse{Error: &rpcError{Message: "unexpected " + method}}
		}
	},
		subscribeScript{frames: []string{`{"event":"pane_agent_status_changed","data":{"pane_id":"p1","agent_status":"working"}}`}, frameDelay: 30 * time.Millisecond},
		subscribeScript{frames: []string{`{"event":"pane_agent_status_changed","data":{"pane_id":"p1","agent_status":"done"}}`}, frameDelay: 30 * time.Millisecond, hold: true},
	)

	s := newHerdrSession(context.Background(), c, "cc-sub", "/tmp", 100*time.Millisecond, false, true, true, 5)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statuses := make(chan string, 1)
	failures := make(chan struct{}, 4)
	go s.runSubscribeReader(ctx, statuses, failures)

	want := []string{"idle", "working", "blocked", "done"}
	for i, expected := range want {
		select {
		case got := <-statuses:
			if got != expected {
				t.Fatalf("status[%d] = %q, want %q", i, got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for status[%d]=%q", i, expected)
		}
	}
}

func TestExternalResolution_NotifyAndEvent(t *testing.T) {
	s := newHerdrSession(context.Background(), newClient("/nonexistent"), "cc-blocked", "/tmp", 100*time.Millisecond, false, true, true, 5)
	defer s.Close()
	s.mu.Lock()
	s.blocked = &blockedRequest{requestID: "herdr-blocked-cc-blocked-1", tail: "screen"}
	s.mu.Unlock()
	notified := make(chan string, 1)
	cancel := s.OnExternalResolution("herdr-blocked-cc-blocked-1", func(note string) { notified <- note })
	defer cancel()
	s.clearBlocked(nil)
	select {
	case note := <-notified:
		if note != "" {
			t.Fatalf("notify note = %q", note)
		}
	case <-time.After(time.Second):
		t.Fatal("external resolution notifier was not called")
	}
	if ev := waitEvent(t, s.Events(), time.Second); ev.Type != core.EventPermissionResolved {
		t.Fatalf("event type = %q, want permission_resolved", ev.Type)
	}
}

func TestStopTurn_FullEventsBufferDoesNotDeadlock(t *testing.T) {
	s := newHerdrSession(context.Background(), newClient("/not-used"), "cc-deadlock", "/tmp", 100*time.Millisecond, false, false, false, 5)
	for i := 0; i < cap(s.events); i++ {
		s.events <- core.Event{Type: core.EventText, Content: "fill"}
	}
	turnCtx, cancel := context.WithCancel(s.ctx)
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel}
	s.mu.Lock()
	s.turn = turn
	s.mu.Unlock()
	started := make(chan struct{})
	turn.wg.Add(1)
	go func() {
		defer turn.wg.Done()
		close(started)
		s.emitDelta(turn, "blocked")
	}()
	<-started

	stopped := make(chan struct{})
	go func() {
		s.stopTurn()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stopTurn deadlocked with a full events buffer")
	}
	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked after stopping a parked sender")
	}
}

func TestWatchViaWait_AgentNotFoundEmitsErrorAndStops(t *testing.T) {
	var mu sync.Mutex
	waitCalls := 0
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		switch method {
		case "agent.get":
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"idle"}}`)}
		case "agent.read":
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"baseline"}}`)}
		case "agent.prompt":
			return rpcResponse{Result: json.RawMessage(`{"type":"agent_prompted","agent":{}}`)}
		case "agent.wait":
			mu.Lock()
			waitCalls++
			mu.Unlock()
			return rpcResponse{Error: &rpcError{Code: "agent_not_found", Message: "missing"}}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
	})
	s := newHerdrSession(context.Background(), c, "missing", "/tmp", 100*time.Millisecond, false, false, false, 5)
	defer s.Close()
	if err := s.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(t, s.Events(), time.Second)
	if ev.Type != core.EventError || ev.Error == nil {
		t.Fatalf("event = %+v, want EventError", ev)
	}
	var rpcErr *rpcError
	if !errors.As(ev.Error, &rpcErr) || rpcErr.Code != "agent_not_found" {
		t.Fatalf("error = %v, want preserved agent_not_found code", ev.Error)
	}
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if waitCalls != 1 {
		t.Fatalf("agent.wait calls = %d, want 1 (no infinite retry)", waitCalls)
	}
}

func TestClientCall_CancelClosesBlockedRead(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c := newClient("pipe")
	c.rpcTimeout = time.Second
	c.dial = func(context.Context) (net.Conn, error) { return clientConn, nil }
	requestRead := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
		close(requestRead)
		_, _ = bufio.NewReader(serverConn).ReadByte()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.call(ctx, "ping", struct{}{}, nil) }()
	<-requestRead
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("call ignored context cancellation")
	}
}

func TestSubscribe_BlockedBetweenTurnsEmitsCard(t *testing.T) {
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		switch method {
		case "agent.get":
			return rpcResponse{Result: json.RawMessage(`{"type":"agent_info","agent":{"pane_id":"p1","agent_status":"blocked"}}`)}
		case "events.subscribe":
			return rpcResponse{Result: json.RawMessage(`{"type":"subscription_started"}`)}
		case "agent.read":
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"1. Continue\n2. Stop"}}`)}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
	}, subscribeScript{hold: true})
	s := newHerdrSession(context.Background(), c, "cc-background", "/tmp", 100*time.Millisecond, true, true, false, 5)
	defer s.Close()
	ev := waitEvent(t, s.Events(), 2*time.Second)
	if ev.Type != core.EventPermissionRequest || ev.RequestID == "" {
		t.Fatalf("event = %+v, want between-turn permission request", ev)
	}
}

func TestSubscriptionFailures_ResetAfterStatusSuccess(t *testing.T) {
	s := newHerdrSession(context.Background(), newClient("/not-used"), "cc-reset", "/tmp", 100*time.Millisecond, false, false, false, 5)
	s.subscribeEnabled = true
	s.subscriptionHealthy.Store(true)
	turnCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel, statusCh: make(chan string, 1), fallbackCh: make(chan struct{}, 1)}
	s.mu.Lock()
	s.turn = turn
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.dispatchSubscription()
		close(done)
	}()
	s.subscribeFailures <- struct{}{}
	s.subscribeFailures <- struct{}{}
	s.statusCh <- "working"
	select {
	case <-turn.statusCh:
	case <-time.After(time.Second):
		t.Fatal("status was not dispatched")
	}
	s.subscribeFailures <- struct{}{}
	select {
	case <-turn.fallbackCh:
		t.Fatal("one failure after a successful status incorrectly triggered fallback")
	case <-time.After(100 * time.Millisecond):
	}
	s.cancel()
	<-done
	s.mu.Lock()
	s.turn = nil
	s.mu.Unlock()
	_ = s.Close()
}

func TestWatchTurn_StatusSilenceRearmsStabilityFallback(t *testing.T) {
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		if method != "agent.read" {
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
		return rpcResponse{Result: json.RawMessage(`{"read":{"text":"stable screen"}}`)}
	})
	s := newHerdrSession(context.Background(), c, "cc-quiet", "/tmp", 100*time.Millisecond, false, false, false, 5)
	defer s.Close()
	s.subscribeEnabled = true
	s.subscriptionHealthy.Store(true)
	s.statusQuietAfter = 100 * time.Millisecond
	s.stabilityThreshold = 2
	turnCtx, cancel := context.WithCancel(s.ctx)
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel, baseline: "stable screen", lastEmitted: "stable screen", statusCh: make(chan string, 1), fallbackCh: make(chan struct{}, 1)}
	turn.statusCh <- "working"
	go s.watchTurn(turn)
	ev := waitEvent(t, s.Events(), 2*time.Second)
	if ev.Type != core.EventResult || !ev.Done {
		t.Fatalf("event = %+v, want stability fallback result", ev)
	}
}

func TestStartSession_CreateUsesProtocol17Shape(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var startParams map[string]any
	c := mockHerdrClient(t, func(method string, params json.RawMessage) rpcResponse {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "agent.list":
			return rpcResponse{Result: json.RawMessage(`{"type":"agent_list","agents":[]}`)}
		case "tab.create":
			return rpcResponse{Result: json.RawMessage(`{"type":"tab_created","tab":{"tab_id":"tab-1"},"root_pane":{"pane_id":"pane-1"}}`)}
		case "agent.start":
			_ = json.Unmarshal(params, &startParams)
			return rpcResponse{Result: json.RawMessage(`{"type":"agent_started","agent":{"name":"cc-new","pane_id":"pane-1","agent_status":"idle"},"argv":["claude"]}`)}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
	})
	a := &Agent{workDir: "/tmp", initCmd: "claude --model sonnet", namePrefix: "cc-", pollMs: 100, rpcTimeoutMs: 1000, maxReadFailures: 5, clientFactory: func() *client { return c }}
	session, err := a.StartSession(context.Background(), "cc-new")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer session.Close()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(methods, []string{"agent.list", "tab.create", "agent.start"}) {
		t.Fatalf("methods = %v", methods)
	}
	if startParams["name"] != "cc-new" || startParams["kind"] != "claude" || startParams["pane_id"] != "pane-1" {
		t.Fatalf("agent.start params = %#v", startParams)
	}
	if _, present := startParams["argv"]; present {
		t.Fatalf("protocol-17 agent.start must not contain argv: %#v", startParams)
	}
	args, _ := startParams["args"].([]any)
	if !slices.Equal(args, []any{"--model", "sonnet"}) {
		t.Fatalf("agent.start args = %#v", startParams["args"])
	}
}

func TestListSessions_SkipsUnnamedAndUsesSummaryFallback(t *testing.T) {
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		if method != "agent.list" {
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
		return rpcResponse{Result: json.RawMessage(`{"agents":[{"name":null,"pane_id":"p0"},{"name":"named","terminal_title":null,"cwd":null,"pane_id":"p1"}]}`)}
	})
	a := &Agent{attachExisting: true, rpcTimeoutMs: 1000, clientFactory: func() *client { return c }}
	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "named" || sessions[0].Summary != "named" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestMethodStrings_MatchProtocol17Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "protocol-17-schema-excerpt.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		Protocol   int      `json:"protocol"`
		MethodEnum []string `json:"method_enum"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if fixture.Protocol != 17 {
		t.Fatalf("protocol = %d, want 17", fixture.Protocol)
	}
	methods := []string{"agent.list", "agent.get", "agent.read", "agent.wait", "agent.prompt", "agent.send_keys", "agent.start", "tab.create", "tab.close", "events.subscribe"}
	for _, method := range methods {
		if !slices.Contains(fixture.MethodEnum, method) {
			t.Errorf("client method %q absent from protocol-17 fixture", method)
		}
	}
}

func TestBlockedQuestion_HasNoHardcodedDisplayCopy(t *testing.T) {
	questions := []core.UserQuestion{
		buildBlockedQuestion("plain screen"),
		buildBlockedQuestion("Choose:\n1. Continue\n2. Stop"),
	}
	for _, question := range questions {
		if question.Header != "" {
			t.Errorf("Header = %q, want empty for engine translation", question.Header)
		}
		for _, option := range question.Options {
			if option.Description != "" {
				t.Errorf("option %q Description = %q, want empty", option.Label, option.Description)
			}
		}
	}
}

func TestSend_RefusesBlockedTargetBeforePrompt(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		if method == "agent.get" {
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"blocked"}}`)}
		}
		return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
	})
	s := newHerdrSession(context.Background(), c, "cc-blocked", "/tmp", 100*time.Millisecond, false, false, false, 5)
	defer s.Close()
	if err := s.Send("must not be typed into menu", nil, nil); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("Send error = %v, want blocked", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(methods, "agent.prompt") {
		t.Fatalf("methods = %v, blocked target must not receive agent.prompt", methods)
	}
}

func TestClientCall_DefaultDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c := newClient("pipe")
	c.rpcTimeout = 50 * time.Millisecond
	c.dial = func(context.Context) (net.Conn, error) { return clientConn, nil }
	go func() {
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
		_, _ = bufio.NewReader(serverConn).ReadByte()
	}()
	started := time.Now()
	err := c.call(context.Background(), "ping", struct{}{}, nil)
	if err == nil {
		t.Fatal("call returned nil without a response")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("default RPC deadline took %s", elapsed)
	}
}

func TestWatchViaWait_BlockedBranchSleeps(t *testing.T) {
	var mu sync.Mutex
	waitCalls := 0
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		switch method {
		case "agent.wait":
			mu.Lock()
			waitCalls++
			mu.Unlock()
			return rpcResponse{Result: json.RawMessage(`{"agent":{"agent_status":"blocked"}}`)}
		case "agent.read":
			return rpcResponse{Result: json.RawMessage(`{"read":{"text":"1. Continue\n2. Stop"}}`)}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
	})
	s := newHerdrSession(context.Background(), c, "cc-sleep", "/tmp", 100*time.Millisecond, false, true, false, 5)
	defer s.Close()
	turnCtx, cancel := context.WithCancel(s.ctx)
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel, statusCh: make(chan string, 1), fallbackCh: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		s.watchViaWait(turn)
		close(done)
	}()
	time.Sleep(350 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if waitCalls < 2 || waitCalls > 5 {
		t.Fatalf("agent.wait calls in 350ms = %d, want bounded polling", waitCalls)
	}
}

func TestFinish_OrdersTrailingTextBeforeResult(t *testing.T) {
	c := mockHerdrClient(t, func(method string, params json.RawMessage) rpcResponse {
		if method != "agent.read" {
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
		return rpcResponse{Result: json.RawMessage(`{"read":{"text":"base\nnew"}}`)}
	})
	s := newHerdrSession(context.Background(), c, "cc-order", "/tmp", 100*time.Millisecond, false, false, true, 5)
	defer s.Close()
	turnCtx, cancel := context.WithCancel(s.ctx)
	turn := &herdrTurn{ctx: turnCtx, cancel: cancel, baseline: "base", lastEmitted: "base"}
	s.finish(turn)
	first := waitEvent(t, s.Events(), time.Second)
	second := waitEvent(t, s.Events(), time.Second)
	if first.Type != core.EventText || first.Content != "new" || second.Type != core.EventResult || !second.Done {
		t.Fatalf("events = [%+v, %+v], want text before result", first, second)
	}
}

func TestStartSession_FailedStartClosesCreatedTab(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "agent.list":
			return rpcResponse{Result: json.RawMessage(`{"agents":[]}`)}
		case "tab.create":
			return rpcResponse{Result: json.RawMessage(`{"tab":{"tab_id":"tab-clean"},"root_pane":{"pane_id":"pane-clean"}}`)}
		case "agent.start":
			return rpcResponse{Error: &rpcError{Code: "start_failed", Message: "boom"}}
		case "tab.close":
			return rpcResponse{Result: json.RawMessage(`{"type":"ok"}`)}
		default:
			return rpcResponse{Error: &rpcError{Code: "invalid_request", Message: method}}
		}
	})
	a := &Agent{workDir: "/tmp", initCmd: "claude", rpcTimeoutMs: 1000, clientFactory: func() *client { return c }}
	if _, err := a.StartSession(context.Background(), "cc-clean"); err == nil {
		t.Fatal("StartSession succeeded despite agent.start failure")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(methods, []string{"agent.list", "tab.create", "agent.start", "tab.close"}) {
		t.Fatalf("methods = %v, want cleanup tab.close", methods)
	}
}

func TestStartSession_UnrecognizedKindIsAttachOnly(t *testing.T) {
	var methods []string
	c := mockHerdrClient(t, func(method string, _ json.RawMessage) rpcResponse {
		methods = append(methods, method)
		return rpcResponse{Result: json.RawMessage(`{"agents":[]}`)}
	})
	a := &Agent{workDir: "/tmp", initCmd: "bash -l", rpcTimeoutMs: 1000, clientFactory: func() *client { return c }}
	_, err := a.StartSession(context.Background(), "cc-attach")
	if err == nil || !strings.Contains(err.Error(), "attach an existing") {
		t.Fatalf("StartSession error = %v, want attach-only guidance", err)
	}
	if !slices.Equal(methods, []string{"agent.list"}) {
		t.Fatalf("methods = %v, unrecognized kind must not create a tab", methods)
	}
}
