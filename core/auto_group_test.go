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

// stubAutoGroupPlatform implements GroupChatCreator + ReplyContextReconstructor
// on top of stubPlatformEngine, with controllable failure injection for
// CreateGroupChat and ReconstructReplyCtx so tests can exercise the
// partial-failure retry path (C4).
type stubAutoGroupPlatform struct {
	stubPlatformEngine

	mu             sync.Mutex
	createCalls    int
	createErr      error
	nextChatID     string
	reconstructErr error
	// createDelay, when set, is slept BEFORE recording the call (outside
	// any lock this type or WorkspaceBindingManager holds). It widens the
	// LookupBySessionID(not found) -> BindSession window so a concurrency
	// test can reliably force two sweeps to overlap instead of racing to
	// interleave by luck. Zero by default -- no behavior change for every
	// other test using this stub. Set once at construction, before any
	// goroutine using it starts, so reading it unlocked here is safe.
	createDelay time.Duration
}

func (p *stubAutoGroupPlatform) CreateGroupChat(_ context.Context, _, _, _ string) (string, error) {
	if p.createDelay > 0 {
		time.Sleep(p.createDelay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls++
	if p.createErr != nil {
		return "", p.createErr
	}
	// Real backends mint a unique chat ID per call; a fixed ID across two
	// creates would make two DISTINCT sessions collide onto one binding
	// map key, masking the exact bug this dedup design defends against.
	return fmt.Sprintf("%s-%d", p.nextChatID, p.createCalls), nil
}

func (p *stubAutoGroupPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reconstructErr != nil {
		return nil, p.reconstructErr
	}
	return "rctx-" + sessionKey, nil
}

func (p *stubAutoGroupPlatform) createCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createCalls
}

func (p *stubAutoGroupPlatform) setReconstructErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconstructErr = err
}

// waitForSessionBound polls until sessionID has a binding recorded under
// projectKey, or fails the test after a timeout. Needed because
// SetAutoGroupWorkspaces's background loop goroutine now runs an eager
// first sweep (m6) concurrently with a test's own explicit
// e.autoGroupSweep() call immediately after -- both are safe to run
// concurrently (B2's TryLock), but which one actually does the work is
// nondeterministic, so the FIRST observation after enabling must poll
// rather than assert immediately.
func waitForSessionBound(t *testing.T, mgr *WorkspaceBindingManager, projectKey, sessionID string) (string, *WorkspaceBinding) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ck, b, found := mgr.LookupBySessionID(projectKey, sessionID); found {
			return ck, b
		}
		select {
		case <-deadline:
			t.Fatalf("no binding for session %q under %q before timeout", sessionID, projectKey)
		case <-ticker.C:
		}
	}
}

// TestAutoGroup_DedupsAcrossTicks_AndRecoversPartialFailure exercises both
// halves of critique finding C4: (1) sweeping the SAME external session
// across multiple ticks must never call CreateGroupChat a second time
// (dedup by session ID, not workspace path); (2) when CreateGroupChat
// succeeds but a later step (ReconstructReplyCtx) fails, the chat is
// already recorded so the next sweep retries only the remaining
// activation steps -- never creating a duplicate group -- and the
// announcement is sent exactly once, only after the retry actually
// succeeds.
func TestAutoGroup_DedupsAcrossTicks_AndRecoversPartialFailure(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{
				{ID: "sess-1", ProjectPath: "/tmp/auto-group-proj", Summary: "auto-group-proj"},
			}, nil
		},
	}
	plat := &stubAutoGroupPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "test"},
		nextChatID:         "chat-1",
		reconstructErr:     errors.New("not ready yet"),
	}
	e := NewEngine("autogrouptest", agent, []Platform{plat}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetAutoGroupWorkspaces(true, 5*time.Second)
	defer e.Stop() // stop the sweep goroutine so it does not leak / touch a torn-down t.TempDir()

	projectKey := "autogroup:autogrouptest"

	// Sweep 1: CreateGroupChat succeeds; ReconstructReplyCtx fails ->
	// partial failure. The binding must exist (so retry never re-creates
	// the chat) but must NOT be activated, and no announcement is sent.
	// SetAutoGroupWorkspaces above already kicked off an eager sweep in the
	// background loop goroutine (m6), racing this explicit call on
	// autoGroupSweepMu (B2) -- poll for the binding rather than asserting
	// immediately; whichever of the two actually did the work, the outcome
	// is identical (dedup-by-session-ID makes redundant sweeps no-ops).
	e.autoGroupSweep()

	_, binding := waitForSessionBound(t, e.workspaceBindings, projectKey, "sess-1")
	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("sweep 1: expected 1 CreateGroupChat call, got %d", got)
	}
	if binding.Activated {
		t.Fatal("sweep 1: binding must not be Activated when ReconstructReplyCtx failed")
	}
	if sent := plat.getSent(); len(sent) != 0 {
		t.Fatalf("sweep 1: expected no announcement after partial failure, got %v", sent)
	}

	// Sweep 2, still failing: must NOT call CreateGroupChat again for the
	// same session -- this is the "never a duplicate group" guarantee.
	e.autoGroupSweep()
	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("sweep 2: expected CreateGroupChat still called only once (no duplicate group), got %d", got)
	}

	// Let the retry succeed.
	plat.setReconstructErr(nil)
	e.autoGroupSweep()

	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("sweep 3: expected CreateGroupChat still called only once after recovery, got %d", got)
	}
	_, binding, found := e.workspaceBindings.LookupBySessionID(projectKey, "sess-1")
	if !found || !binding.Activated {
		t.Fatal("sweep 3: expected binding to be Activated once the retry succeeds")
	}
	sent := plat.getSent()
	if len(sent) != 1 {
		t.Fatalf("sweep 3: expected exactly one announcement, got %v", sent)
	}

	// Sweep again now that it's activated: must not resend the announcement.
	e.autoGroupSweep()
	if got := len(plat.getSent()); got != 1 {
		t.Fatalf("sweep 4: expected announcement sent only once total, got %d", got)
	}
}

// TestAutoGroup_TwoWorkspacesSameRepoGetTwoGroups guards the core C4 defect:
// dedup keyed on workspace path (instead of session ID) would silently
// collapse two distinct external sessions that share a cwd (worktrees,
// parallel agents on one repo) into a single group.
func TestAutoGroup_TwoWorkspacesSameRepoGetTwoGroups(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{
				{ID: "sess-a", ProjectPath: "/tmp/shared-repo", Summary: "shared-repo-a"},
				{ID: "sess-b", ProjectPath: "/tmp/shared-repo", Summary: "shared-repo-b"},
			}, nil
		},
	}
	plat := &stubAutoGroupPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "test"},
		nextChatID:         "chat-shared",
	}
	e := NewEngine("autogrouptest2", agent, []Platform{plat}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetAutoGroupWorkspaces(true, 5*time.Second)
	defer e.Stop() // stop the sweep goroutine so it does not leak / touch a torn-down t.TempDir()

	e.autoGroupSweep()

	// SetAutoGroupWorkspaces above already kicked off an eager sweep (m6);
	// poll for both bindings rather than asserting immediately -- see
	// waitForSessionBound's doc comment.
	projectKey := "autogroup:autogrouptest2"
	waitForSessionBound(t, e.workspaceBindings, projectKey, "sess-a")
	waitForSessionBound(t, e.workspaceBindings, projectKey, "sess-b")

	if got := plat.createCallCount(); got != 2 {
		t.Fatalf("expected 2 CreateGroupChat calls (one per session sharing a path), got %d", got)
	}
}

// TestAutoGroup_NoGroupChatCreatorPlatform_NoOp confirms the sweep is
// inert when no platform in the project implements GroupChatCreator.
func TestAutoGroup_NoGroupChatCreatorPlatform_NoOp(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{{ID: "sess-1", ProjectPath: "/tmp/x"}}, nil
		},
	}
	plain := &stubPlatformEngine{n: "plain"}
	e := NewEngine("autogrouptest3", agent, []Platform{plain}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetAutoGroupWorkspaces(true, 5*time.Second)
	defer e.Stop() // stop the sweep goroutine so it does not leak / touch a torn-down t.TempDir()

	e.autoGroupSweep() // must not panic; must not create anything

	if _, _, found := e.workspaceBindings.LookupBySessionID("autogroup:autogrouptest3", "sess-1"); found {
		t.Fatal("expected no binding when no platform implements GroupChatCreator")
	}
}

// TestAutoGroup_Disabled_SweepIsNoOp confirms SetAutoGroupWorkspaces(false, ...)
// never starts the loop and a direct autoGroupSweep call is a no-op.
func TestAutoGroup_Disabled_SweepIsNoOp(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{{ID: "sess-1", ProjectPath: "/tmp/x"}}, nil
		},
	}
	plat := &stubAutoGroupPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, nextChatID: "chat-1"}
	e := NewEngine("autogrouptest4", agent, []Platform{plat}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetAutoGroupWorkspaces(false, 5*time.Second)
	defer e.Stop() // stop the sweep goroutine so it does not leak / touch a torn-down t.TempDir()

	if e.isAutoGroupEnabled() {
		t.Fatal("expected auto-group to remain disabled")
	}
	e.autoGroupSweep()
	if got := plat.createCallCount(); got != 0 {
		t.Fatalf("expected no CreateGroupChat calls while disabled, got %d", got)
	}
}

// TestAutoGroup_ConcurrentSweeps_NeverCreateDuplicateGroups is the B2
// regression: the periodic ticker sweep and the /watch "bind unbound
// groups" on-demand sweep (executeCardAction's "/watch" bind-groups case:
// `go e.autoGroupSweep()`) can run concurrently. Pre-fix, with no
// serialization, both can pass LookupBySessionID (session not yet bound)
// before either calls BindSession, so both call CreateGroupChat -- the
// reviewer reproduced exactly this, 2 group chats for one session, 5/5
// runs deterministic under -race. autoGroupSweepMu's TryLock (B2) must
// make exactly one of two concurrent sweeps do the work.
func TestAutoGroup_ConcurrentSweeps_NeverCreateDuplicateGroups(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{
				{ID: "sess-race", ProjectPath: "/tmp/auto-group-race", Summary: "auto-group-race"},
			}, nil
		},
	}
	plat := &stubAutoGroupPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "test"},
		nextChatID:         "chat-race",
		// Widen the LookupBySessionID(not found) -> BindSession window so
		// two concurrent sweeps reliably overlap pre-fix instead of racing
		// to interleave by luck; harmless post-fix since TryLock means only
		// one sweep ever reaches CreateGroupChat regardless of timing.
		createDelay: 50 * time.Millisecond,
	}
	e := NewEngine("autogroupracetest", agent, []Platform{plat}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	// Drive auto-group state directly (not via SetAutoGroupWorkspaces) so
	// this test exercises exactly the two real concurrent trigger sources
	// under test -- the ticker sweep and the /watch bind-groups on-demand
	// sweep -- without the unrelated m6 eager-sweep-on-enable noise.
	e.autoGroupMu.Lock()
	e.autoGroupEnabled = true
	e.autoGroupMu.Unlock()
	e.workspaceBindings = NewWorkspaceBindingManager(filepath.Join(t.TempDir(), "workspace_bindings.json"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.autoGroupSweep() }() // simulates the ticker
	go func() { defer wg.Done(); e.autoGroupSweep() }() // simulates the /watch bind-groups button
	wg.Wait()

	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 CreateGroupChat call from two concurrent sweeps of one session (want 1, reviewer reproduced 2/2 pre-fix), got %d", got)
	}
	if _, _, found := e.workspaceBindings.LookupBySessionID("autogroup:autogroupracetest", "sess-race"); !found {
		t.Fatal("expected exactly one binding to exist for sess-race")
	}
}

// TestAutoGroup_MultiWorkspaceEnabled_ForeignProjectPathNeverUnbound is the
// B3 regression: when both auto-group and multi-workspace are enabled on
// one project (main.go wires auto_group_workspaces outside the
// mode=="multi-workspace" check, so this combination is reachable), an
// external session's ProjectPath -- routinely absent or not present on this
// host, the normal case for a genuinely external workspace -- must never
// cause the auto-group binding to be dropped and re-created on the next
// sweep. Pre-fix, auto-group wrote into the SAME "project:"+e.name key
// multi-workspace routing reads and Unbinds on a missing directory
// (lookupEffectiveWorkspaceBinding), so the first routing touch after the
// sweep would unbind the auto-group binding and the next sweep would
// create a second group chat for the same session -- unbounded group spam
// on every auto_group_interval_ms tick thereafter.
func TestAutoGroup_MultiWorkspaceEnabled_ForeignProjectPathNeverUnbound(t *testing.T) {
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{
				{ID: "sess-foreign", ProjectPath: "/nonexistent/external/ws", Summary: "external-ws"},
			}, nil
		},
	}
	plat := &stubAutoGroupPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "test"},
		nextChatID:         "chat-foreign",
	}
	e := NewEngine("autogroupmwtest", agent, []Platform{plat}, "", LangEnglish)
	dataDir := t.TempDir()
	e.SetDataDir(dataDir)
	// Both features enabled on the same project -- exactly the combination
	// main.go allows and B3 guards against.
	e.SetMultiWorkspace(t.TempDir(), filepath.Join(dataDir, "workspace_bindings.json"))
	e.SetAutoGroupWorkspaces(true, time.Hour) // long interval; sweeps below are explicit
	defer e.Stop()

	e.autoGroupSweep()
	channelKey, _ := waitForSessionBound(t, e.workspaceBindings, "autogroup:autogroupmwtest", "sess-foreign")
	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("expected 1 CreateGroupChat call after the first sweep, got %d", got)
	}

	// Simulate the message-routing touch the reviewer's probe used: a
	// lookup for this exact channel through the SAME path multi-workspace
	// routing uses. Pre-B3 (shared "project:"+e.name namespace) this would
	// find the binding, os.Stat the missing directory, and Unbind it.
	if _, _, usable := e.lookupEffectiveWorkspaceBinding(channelKey); usable {
		t.Fatal("expected the routing lookup to miss (separate namespace), not report usable")
	}
	if _, _, found := e.workspaceBindings.LookupBySessionID("autogroup:autogroupmwtest", "sess-foreign"); !found {
		t.Fatal("expected the auto-group binding to survive the routing lookup (B3: separate namespace)")
	}

	// A second sweep must not see a missing binding and re-create a group.
	e.autoGroupSweep()
	if got := plat.createCallCount(); got != 1 {
		t.Fatalf("expected still exactly 1 CreateGroupChat call after a second sweep (no re-create churn), got %d", got)
	}
}

// TestEngine_AutoGroupSweep_IdempotentAcrossRestart is the M1 test named by
// critique C4 fix item 4: a fresh Engine pointed at the SAME data dir as one
// that already created+activated a binding for a session must not create a
// second group chat for that session on its first sweep -- the binding
// persists to disk (workspace_bindings.json) and is reloaded on restart, so
// LookupBySessionID must still find it under the same session ID (guards
// against, e.g., a dropped `json:"agent_session_id"` tag silently breaking
// restart dedup).
func TestEngine_AutoGroupSweep_IdempotentAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	agent := &controllableAgent{
		listFn: func() ([]AgentSessionInfo, error) {
			return []AgentSessionInfo{
				{ID: "sess-restart", ProjectPath: "/tmp/auto-group-restart", Summary: "restart-proj"},
			}, nil
		},
	}
	plat1 := &stubAutoGroupPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, nextChatID: "chat-restart"}
	e1 := NewEngine("autogrouprestart", agent, []Platform{plat1}, "", LangEnglish)
	e1.SetDataDir(dataDir)
	e1.SetAutoGroupWorkspaces(true, time.Hour)
	e1.autoGroupSweep()
	waitForSessionBound(t, e1.workspaceBindings, "autogroup:autogrouprestart", "sess-restart")
	if got := plat1.createCallCount(); got != 1 {
		t.Fatalf("first engine: expected 1 CreateGroupChat call, got %d", got)
	}
	e1.Stop()

	// Fresh Engine, same name (-> same project key) and same data dir ->
	// same workspace_bindings.json file, simulating a process restart.
	plat2 := &stubAutoGroupPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, nextChatID: "chat-restart-2"}
	e2 := NewEngine("autogrouprestart", agent, []Platform{plat2}, "", LangEnglish)
	e2.SetDataDir(dataDir)
	e2.SetAutoGroupWorkspaces(true, time.Hour)
	defer e2.Stop()
	e2.autoGroupSweep()

	if got := plat2.createCallCount(); got != 0 {
		t.Fatalf("second engine (restart): expected 0 NEW CreateGroupChat calls (binding must load from disk), got %d", got)
	}
	if _, binding, found := e2.workspaceBindings.LookupBySessionID("autogroup:autogrouprestart", "sess-restart"); !found || !binding.Activated {
		t.Fatalf("expected the restarted engine to see the persisted, activated binding, found=%v binding=%+v", found, binding)
	}
}
