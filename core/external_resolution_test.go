package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// externalNotifyAgentSession is a controllableAgentSession that also
// implements ExternalResolutionNotifier, letting tests simulate a backend
// resolving a permission request outside cc-connect (another client's UI,
// or the backend's own prompt/timeout).
type externalNotifyAgentSession struct {
	controllableAgentSession
	mu          sync.Mutex
	permCalls   int
	lastResult  PermissionResult
	notifyFn    func(note string)
	notifyReqID string
	cancelCalls int
}

func (s *externalNotifyAgentSession) RespondPermission(_ string, res PermissionResult) error {
	s.mu.Lock()
	s.permCalls++
	s.lastResult = res
	s.mu.Unlock()
	return nil
}

func (s *externalNotifyAgentSession) OnExternalResolution(requestID string, notify func(note string)) func() {
	s.mu.Lock()
	s.notifyFn = notify
	s.notifyReqID = requestID
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.cancelCalls++
		s.mu.Unlock()
	}
}

// fireExternal invokes the registered notify callback, failing the test if
// none was registered or it was registered for a different request.
func (s *externalNotifyAgentSession) fireExternal(t *testing.T, requestID, note string) {
	t.Helper()
	s.mu.Lock()
	fn := s.notifyFn
	gotID := s.notifyReqID
	s.mu.Unlock()
	if fn == nil {
		t.Fatal("OnExternalResolution was never registered")
	}
	if gotID != requestID {
		t.Fatalf("registered for request %q, want %q", gotID, requestID)
	}
	fn(note)
}

func (s *externalNotifyAgentSession) cancelCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelCalls
}

func (s *externalNotifyAgentSession) permCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.permCalls
}

// TestEngine_WaitForPermissionResolution_UnblocksOnExternalNotify is the
// C1 regression: the interactive EventPermissionRequest case's
// <-pending.Resolved wait must unblock when the backend reports the
// request was decided externally (ExternalResolutionNotifier), not just
// when the user replies allow/deny in chat. Without the engine.go wiring
// this test hangs until its own deadline and fails -- nothing else would
// ever close pending.Resolved for this stub.
func TestEngine_WaitForPermissionResolution_UnblocksOnExternalNotify(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := &externalNotifyAgentSession{controllableAgentSession: *newControllableSession("ext-notify-1")}
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)

	key := "test:chat:user1"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession: sess,
		platform:     p,
		replyCtx:     "ctx",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil // no in-flight Send(); Send() choreography is not under test here

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(context.Background(), state, session, e.sessions, key, "m1", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()

	sess.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-ext-1",
		ToolName:     "Bash",
		ToolInput:    "npm test",
		ToolInputRaw: map[string]any{"command": "npm test"},
	}

	// Wait until the prompt actually reaches the platform, proving
	// processInteractiveEvents has registered the notifier and is now
	// blocked on <-pending.Resolved (not racing ahead of registration).
	waitForSentText(t, p)
	p.clearSent()

	sess.fireExternal(t, "req-ext-1", "resolved via test authority")

	// Turn continues: send the final result so processInteractiveEvents
	// returns normally, proving the goroutine is back to reading events
	// (not stuck) after the external notify unblocked it.
	sess.events <- Event{Type: EventResult, Content: "ok", Done: true}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processInteractiveEvents did not unblock after external notify")
	}

	sent := p.getSent()
	found := false
	for _, msg := range sent {
		if strings.Contains(msg, "resolved via test authority") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the external resolution note to be sent, got: %v", sent)
	}

	state.mu.Lock()
	pendingCleared := state.pending == nil
	state.mu.Unlock()
	if !pendingCleared {
		t.Error("expected state.pending to be cleared after external resolution")
	}

	if got := sess.cancelCallCount(); got != 1 {
		t.Errorf("expected OnExternalResolution's cancel to be called exactly once on turn exit, got %d", got)
	}
}

// TestEngine_EventPermissionResolved_ClearsMatchingPending covers the
// defensive/observability case added to the interactive switch: an
// EventPermissionResolved event whose RequestID matches the live
// state.pending must clear it, resolve the pendingPermission, and surface
// a note -- the same idempotent shape the notify callback uses, so a
// notify+event double-fire for the same request is safe.
func TestEngine_EventPermissionResolved_ClearsMatchingPending(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("event-resolved-1")
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)

	key := "test:chat:user1"
	session := e.sessions.GetOrCreateActive(key)
	pending := &pendingPermission{
		RequestID: "req-evt-1",
		ToolName:  "Bash",
		Resolved:  make(chan struct{}),
	}
	state := &interactiveState{
		agentSession: sess,
		platform:     p,
		replyCtx:     "ctx",
		pending:      pending,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(context.Background(), state, session, e.sessions, key, "m1", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()

	sess.events <- Event{
		Type:      EventPermissionResolved,
		RequestID: "req-evt-1",
		Content:   "resolved via feed diff",
	}

	msg := waitForSentText(t, p)
	if !strings.Contains(msg, "resolved via feed diff") {
		t.Fatalf("expected the resolved note to be sent, got %q", msg)
	}

	state.mu.Lock()
	stillPending := state.pending
	state.mu.Unlock()
	if stillPending != nil {
		t.Error("expected state.pending to be cleared")
	}

	select {
	case <-pending.Resolved:
	default:
		t.Error("expected the pendingPermission's Resolved channel to be closed")
	}

	sess.events <- Event{Type: EventResult, Content: "ok", Done: true}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processInteractiveEvents did not complete")
	}
}

// TestEngine_EventPermissionResolved_IgnoresNonMatchingRequest confirms the
// same case is a safe no-op (no panic, no note sent, no crash) when no
// pending permission matches -- the ordinary case once the wiring is
// correct, since a live wait never reads this channel.
func TestEngine_EventPermissionResolved_IgnoresNonMatchingRequest(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("event-resolved-2")
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)

	key := "test:chat:user1"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession: sess,
		platform:     p,
		replyCtx:     "ctx",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(context.Background(), state, session, e.sessions, key, "m1", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()

	sess.events <- Event{Type: EventPermissionResolved, RequestID: "req-unknown", Content: "stale"}
	sess.events <- Event{Type: EventResult, Content: "ok", Done: true}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processInteractiveEvents did not complete")
	}

	// The normal end-of-turn response ("ok") is still expected to be sent;
	// what must NOT happen is an extra note from the ignored
	// EventPermissionResolved (which would show up as a second message, or
	// as stray content mixed into this one).
	sent := p.getSent()
	if len(sent) != 1 || sent[0] != "ok" {
		t.Fatalf("expected exactly the normal turn response [\"ok\"] with no extra note injected by the non-matching EventPermissionResolved, got %v", sent)
	}
}

// TestEngine_BackgroundPermission_NoAutoDenyForExternalNotifier is the §3.3
// background-policy regression: the unsolicited reader's EventPermissionRequest
// handling must NOT auto-deny for a session implementing
// ExternalResolutionNotifier (an auto-deny would write a real DENY into an
// external approval authority); instead it must install pending state and
// register the notifier, leaving resolution to a later chat reply/card tap
// or the external authority itself.
func TestEngine_BackgroundPermission_NoAutoDenyForExternalNotifier(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()

	sess := &externalNotifyAgentSession{controllableAgentSession: *newControllableSession("unsol-ext-notify")}

	sessions := e.sessions
	session := sessions.GetOrCreateActive("test:extnotify:u1")

	state := &interactiveState{
		agentSession:     sess,
		platform:         p,
		replyCtx:         "ctx",
		eventsNeedResync: false,
		approveAll:       false,
	}

	e.startUnsolicitedReader(state, session, sessions, "test:extnotify:u1", "")

	sess.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-bg-1",
		ToolName:     "Bash",
		ToolInput:    "npm test",
		ToolInputRaw: map[string]any{"command": "npm test"},
	}

	// The permission card/prompt must still be sent to the bound chat...
	waitForSentText(t, p)

	// ...but RespondPermission must NOT be called (no auto-deny). Give the
	// (incorrect) old auto-deny goroutine a window it would need.
	time.Sleep(150 * time.Millisecond)
	if got := sess.permCallCount(); got != 0 {
		t.Fatalf("expected RespondPermission NOT to be called (no auto-deny) for an ExternalResolutionNotifier session, got %d call(s)", got)
	}

	// state.pending must be installed so a later chat reply / card tap
	// resolves it via the normal handlePendingPermission path.
	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil || pending.RequestID != "req-bg-1" {
		t.Fatalf("expected state.pending installed for req-bg-1, got %+v", pending)
	}

	sess.mu.Lock()
	gotReqID := sess.notifyReqID
	sess.mu.Unlock()
	if gotReqID != "req-bg-1" {
		t.Fatalf("expected OnExternalResolution registered for req-bg-1, got %q", gotReqID)
	}

	sess.fireExternal(t, "req-bg-1", "resolved elsewhere")

	deadline := time.After(2 * time.Second)
	for {
		state.mu.Lock()
		cleared := state.pending == nil
		state.mu.Unlock()
		if cleared {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for state.pending to clear after external notify")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := sess.permCallCount(); got != 0 {
		t.Fatalf("expected RespondPermission still never called after external resolution, got %d call(s)", got)
	}

	e.stopUnsolicitedReader(state)
}

// TestEngine_BackgroundPermission_ApproveAllStillAppliesForExternalNotifier
// guards the "/yolo approveAll behavior preserved" requirement: a session
// implementing ExternalResolutionNotifier must still auto-allow when
// approveAll is set, exactly like every other session.
func TestEngine_BackgroundPermission_ApproveAllStillAppliesForExternalNotifier(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()

	sess := &externalNotifyAgentSession{controllableAgentSession: *newControllableSession("unsol-ext-yolo")}

	sessions := e.sessions
	session := sessions.GetOrCreateActive("test:extyolo:u1")

	state := &interactiveState{
		agentSession:     sess,
		platform:         p,
		replyCtx:         "ctx",
		eventsNeedResync: false,
		approveAll:       true,
	}

	e.startUnsolicitedReader(state, session, sessions, "test:extyolo:u1", "")

	sess.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-yolo-1",
		ToolName:     "Bash",
		ToolInputRaw: map[string]any{"command": "npm test"},
	}

	deadline := time.After(2 * time.Second)
	for {
		if sess.permCallCount() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for approveAll to auto-allow")
		case <-time.After(10 * time.Millisecond):
		}
	}

	sess.mu.Lock()
	result := sess.lastResult
	sess.mu.Unlock()
	if result.Behavior != "allow" {
		t.Fatalf("expected approveAll to auto-allow, got behavior=%q", result.Behavior)
	}

	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending != nil {
		t.Error("expected no pending state installed on the approveAll (auto-allow) path")
	}

	e.stopUnsolicitedReader(state)
}

// multiExternalNotifyAgentSession is like externalNotifyAgentSession but
// tracks one OnExternalResolution registration PER request ID (a map, not a
// single slot), for tests that need two permission requests in flight at
// once -- exactly what real adapters do (design doc: cmux's feedBridge
// tracks many concurrent requestIDs).
type multiExternalNotifyAgentSession struct {
	controllableAgentSession
	mu         sync.Mutex
	notifiers  map[string]func(note string)
	cancels    map[string]int
	respondIDs []string
}

func (s *multiExternalNotifyAgentSession) RespondPermission(reqID string, _ PermissionResult) error {
	s.mu.Lock()
	s.respondIDs = append(s.respondIDs, reqID)
	s.mu.Unlock()
	return nil
}

func (s *multiExternalNotifyAgentSession) OnExternalResolution(requestID string, notify func(note string)) func() {
	s.mu.Lock()
	if s.notifiers == nil {
		s.notifiers = make(map[string]func(note string))
		s.cancels = make(map[string]int)
	}
	s.notifiers[requestID] = notify
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.cancels[requestID]++
		s.mu.Unlock()
	}
}

func (s *multiExternalNotifyAgentSession) registered(requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.notifiers[requestID]
	return ok
}

func (s *multiExternalNotifyAgentSession) fireExternal(t *testing.T, requestID, note string) {
	t.Helper()
	s.mu.Lock()
	fn := s.notifiers[requestID]
	s.mu.Unlock()
	if fn == nil {
		t.Fatalf("OnExternalResolution was never registered for %q", requestID)
	}
	fn(note)
}

func (s *multiExternalNotifyAgentSession) cancelCallCount(requestID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancels[requestID]
}

func (s *multiExternalNotifyAgentSession) respondedTo() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.respondIDs))
	copy(out, s.respondIDs)
	return out
}

// TestEngine_BackgroundPermission_OverlappingRequestsDoNotCrossResolve is
// the B1 regression: a second background EventPermissionRequest arriving
// while the first is still pending must NOT overwrite state.pending (a
// single slot) -- the permission card's buttons carry no request ID, so a
// second installed pending would let a tap on card #1 silently resolve
// request #2 instead. Card #1's buttons must keep resolving request #1, and
// request #2 -- never shown a card -- must still be resolvable through the
// external authority instead of leaking its notifier registration forever.
func TestEngine_BackgroundPermission_OverlappingRequestsDoNotCrossResolve(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()

	sess := &multiExternalNotifyAgentSession{controllableAgentSession: *newControllableSession("unsol-overlap")}

	sessions := e.sessions
	session := sessions.GetOrCreateActive("test:overlap:u1")

	state := &interactiveState{
		agentSession: sess,
		platform:     p,
		replyCtx:     "ctx",
	}
	// handlePendingPermission (exercised below for the card #1 tap) looks
	// state up via e.interactiveStates, unlike the notifier-only checks the
	// sibling background-permission tests use -- register it explicitly.
	e.interactiveMu.Lock()
	e.interactiveStates["test:overlap:u1"] = state
	e.interactiveMu.Unlock()

	e.startUnsolicitedReader(state, session, sessions, "test:overlap:u1", "")

	sess.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-1",
		ToolName:     "Bash",
		ToolInput:    "rm -rf /a",
		ToolInputRaw: map[string]any{"command": "rm -rf /a"},
	}

	// Card #1 must reach the platform before request #2 arrives, so there
	// is no ambiguity about which card is "first".
	waitForSentText(t, p)
	p.clearSent()

	sess.events <- Event{
		Type:         EventPermissionRequest,
		RequestID:    "req-2",
		ToolName:     "Write",
		ToolInput:    "harmless.txt",
		ToolInputRaw: map[string]any{"command": "harmless.txt"},
	}

	// req-2 must still get a notifier registration (so the external
	// authority can decide it) even though it gets no card of its own. By
	// the time this is observable, the reader's synchronous handling of the
	// req-2 event -- including any (buggy) card send -- has already run.
	deadline := time.After(2 * time.Second)
	for !sess.registered("req-2") {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for req-2's OnExternalResolution registration")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sent := p.getSent(); len(sent) != 0 {
		t.Fatalf("expected no second card for the displaced request, got %v", sent)
	}

	// state.pending must still be request #1 -- not overwritten by #2.
	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("expected state.pending to still be req-1, got %+v", pending)
	}

	// Firing req-2's external resolution while req-1 is still live must not
	// touch state.pending (still req-1) and must not send a note (no card
	// baseline to reference for a request the user was never shown).
	sess.fireExternal(t, "req-2", "resolved via test authority")

	deadline = time.After(2 * time.Second)
	for sess.cancelCallCount("req-2") == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for req-2's notifier to be cancelled after external resolution")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sent := p.getSent(); len(sent) != 0 {
		t.Fatalf("expected no note sent for the displaced request's resolution, got %v", sent)
	}
	state.mu.Lock()
	pending = state.pending
	state.mu.Unlock()
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("expected state.pending to still be req-1 after req-2 resolved, got %+v", pending)
	}

	// Card #1's buttons resolve via handlePendingPermission, exactly like a
	// real Allow tap.
	msg := &Message{SessionKey: "test:overlap:u1", IsPermissionResponse: true}
	if !e.handlePendingPermission(p, msg, "allow", "test:overlap:u1") {
		t.Fatal("expected handlePendingPermission to handle the allow tap")
	}
	if got := sess.respondedTo(); len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("expected exactly one RespondPermission call, for req-1, got %v", got)
	}

	state.mu.Lock()
	pending = state.pending
	state.mu.Unlock()
	if pending != nil {
		t.Fatalf("expected state.pending to be cleared after resolving req-1, got %+v", pending)
	}

	e.stopUnsolicitedReader(state)
}
