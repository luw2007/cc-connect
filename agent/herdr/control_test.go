package herdr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type controllerRPCStep struct {
	method string
	result string
}

type controllerRPCCall struct {
	method string
	params map[string]any
}

type controllerRPCScript struct {
	t     *testing.T
	mu    sync.Mutex
	steps []controllerRPCStep
	calls []controllerRPCCall
	next  int
}

type herdrControlListOnly struct{}

func (herdrControlListOnly) ListAgents(context.Context) ([]core.AgentControlTarget, error) {
	return nil, nil
}

func newControllerRPCScript(t *testing.T, steps ...controllerRPCStep) (*Controller, *controllerRPCScript) {
	t.Helper()
	script := &controllerRPCScript{t: t, steps: steps}
	c := mockHerdrClient(t, script.respond)
	t.Cleanup(func() {
		script.mu.Lock()
		defer script.mu.Unlock()
		if len(script.calls) != len(script.steps) {
			t.Errorf("RPC calls = %v, want exactly %v", controllerCallMethods(script.calls), controllerStepMethods(script.steps))
		}
	})
	return &Controller{client: c}, script
}

func (s *controllerRPCScript) respond(method string, raw json.RawMessage) rpcResponse {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		s.t.Errorf("decode %s params: %v", method, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, controllerRPCCall{method: method, params: params})
	if s.next >= len(s.steps) {
		s.t.Errorf("unexpected RPC %q with params %#v", method, params)
		return rpcResponse{Error: &rpcError{Code: "unexpected_rpc", Message: method}}
	}
	step := s.steps[s.next]
	s.next++
	if method != step.method {
		s.t.Errorf("RPC %d method = %q, want %q", s.next, method, step.method)
	}
	result := step.result
	if result == "" {
		result = `{}`
	}
	return rpcResponse{Result: json.RawMessage(result)}
}

func (s *controllerRPCScript) call(t *testing.T, index int) controllerRPCCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.calls) {
		t.Fatalf("RPC call %d missing; calls = %v", index, controllerCallMethods(s.calls))
	}
	return s.calls[index]
}

func (s *controllerRPCScript) snapshotCalls() []controllerRPCCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]controllerRPCCall(nil), s.calls...)
}

func controllerCallMethods(calls []controllerRPCCall) []string {
	methods := make([]string, len(calls))
	for i, call := range calls {
		methods[i] = call.method
	}
	return methods
}

func controllerStepMethods(steps []controllerRPCStep) []string {
	methods := make([]string, len(steps))
	for i, step := range steps {
		methods[i] = step.method
	}
	return methods
}

func controllerAgent(name, status string) agentInfo {
	return agentInfo{
		Name:        name,
		TerminalID:  "terminal-" + name,
		PaneID:      "pane-" + name,
		WorkspaceID: "workspace-" + name,
		Agent:       "claude",
		AgentStatus: status,
		CWD:         "/repo/" + name,
	}
}

func agentListResult(agents ...agentInfo) string {
	encoded, _ := json.Marshal(map[string]any{"agents": agents})
	return string(encoded)
}

func visibleReadResult(screen string) string {
	encoded, _ := json.Marshal(map[string]any{"read": map[string]any{"text": screen}})
	return string(encoded)
}

func mustListSingleTarget(t *testing.T, controller core.AgentController) core.AgentControlTarget {
	t.Helper()
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("ListAgents() = %#v, want one target", targets)
	}
	return targets[0]
}

func mustListSingleRequest(t *testing.T, controller *Controller, target core.AgentControlTargetRef) core.AgentControlRequest {
	t.Helper()
	requests, err := controller.ListAgentRequests(context.Background(), target)
	if err != nil {
		t.Fatalf("ListAgentRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("ListAgentRequests() = %#v, want one request", requests)
	}
	return requests[0]
}

func assertSHA256Token(t *testing.T, field, token string) {
	t.Helper()
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("%s = %q, want a SHA-256 token", field, token)
	}
}

func TestHerdrControllerRegistrationDoesNotRequireInitCommand(t *testing.T) {
	socketPath := t.TempDir() + "/herdr.sock"
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatalf("create socket placeholder: %v", err)
	}

	controller, err := core.CreateAgentController("herdr", map[string]any{"socket_path": socketPath})
	if err != nil {
		t.Fatalf("CreateAgentController without init_command: %v", err)
	}
	if _, ok := controller.(*Controller); !ok {
		t.Fatalf("controller = %T, want *herdr.Controller", controller)
	}
	if _, ok := controller.(core.AgentControlTailer); !ok {
		t.Fatal("herdr controller does not implement tail capability")
	}
	if _, ok := controller.(core.AgentControlPrompter); ok {
		t.Fatal("herdr controller must not implement prompt capability")
	}
	if _, ok := controller.(core.AgentControlKeySender); ok {
		t.Fatal("herdr controller must not implement key capability")
	}
	if _, ok := controller.(core.AgentControlRequestLister); !ok {
		t.Fatal("herdr controller does not implement request-list capability")
	}
	if _, ok := controller.(core.AgentControlQuestionResponder); ok {
		t.Fatal("herdr controller must not implement question response capability")
	}
	if _, ok := controller.(core.AgentControlPermissionResponder); ok {
		t.Fatal("herdr controller must not implement permission response")
	}
}

func TestHerdrControllerListUsesOnlyAgentListAndAdvertisesActualCapabilities(t *testing.T) {
	blocked := controllerAgent("alpha", "blocked")
	idle := controllerAgent("beta", "idle")
	idle.Agent = ""
	missingReadHandle := controllerAgent("missing-read-handle", "blocked")
	missingReadHandle.TerminalID = ""
	missingReadHandle.PaneID = ""
	unnamed := controllerAgent("", "blocked")

	controller, _ := newControllerRPCScript(t, controllerRPCStep{
		method: "agent.list",
		result: agentListResult(blocked, idle, missingReadHandle, unnamed),
	})

	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("ListAgents() = %#v, want only named targets with canonical instance identity", targets)
	}
	wantBlocked := core.AgentControlTarget{
		ID:        "alpha",
		Backend:   "herdr",
		Kind:      "claude",
		Directory: "/repo/alpha",
		Status:    "blocked",
		Capabilities: []core.AgentControlCapability{
			core.AgentControlCapabilityTail,
			core.AgentControlCapabilityRequests,
		},
	}
	wantIdle := core.AgentControlTarget{
		ID:        "beta",
		Backend:   "herdr",
		Kind:      "unknown",
		Directory: "/repo/beta",
		Status:    "idle",
		Capabilities: []core.AgentControlCapability{
			core.AgentControlCapabilityTail,
		},
	}
	blockedRevision := targets[0].Revision
	idleRevision := targets[1].Revision
	targets[0].Revision = ""
	targets[1].Revision = ""
	if !reflect.DeepEqual(targets[0], wantBlocked) || !reflect.DeepEqual(targets[1], wantIdle) {
		t.Fatalf("ListAgents() = %#v, want %#v and %#v", targets, wantBlocked, wantIdle)
	}
	assertSHA256Token(t, "blocked target revision", blockedRevision)
	assertSHA256Token(t, "idle target revision", idleRevision)
	if wantBlocked.Supports(core.AgentControlCapabilityPermission) {
		t.Fatal("herdr blocked target advertised permission capability")
	}
}

func TestHerdrControlTargetUsesSupportedControllerCapabilities(t *testing.T) {
	target, ok := herdrControlTarget(herdrControlListOnly{}, controllerAgent("alpha", "blocked"))
	if !ok {
		t.Fatal("herdrControlTarget rejected valid agent")
	}
	if len(target.Capabilities) != 0 {
		t.Fatalf("capabilities = %#v, want none for list-only controller", target.Capabilities)
	}
}

func TestHerdrControllerListRejectsDuplicateSameNameTargets(t *testing.T) {
	original := controllerAgent("alpha", "idle")
	replacement := original
	replacement.PaneID = "replacement-pane"
	cases := []struct {
		name   string
		agents []agentInfo
	}{
		{name: "original first", agents: []agentInfo{original, replacement}},
		{name: "replacement first", agents: []agentInfo{replacement, original}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller, _ := newControllerRPCScript(t, controllerRPCStep{
				method: "agent.list",
				result: agentListResult(tc.agents...),
			})
			targets, err := controller.ListAgents(context.Background())
			if err != nil {
				t.Fatalf("ListAgents: %v", err)
			}
			if len(targets) != 0 {
				t.Fatalf("ListAgents() = %#v, want ambiguous same-name targets rejected", targets)
			}
		})
	}
}

func TestHerdrControllerTargetRevisionCoversCanonicalBackendFields(t *testing.T) {
	baseline := controllerAgent("alpha", "blocked")
	controller, _ := newControllerRPCScript(t, controllerRPCStep{method: "agent.list", result: agentListResult(baseline)})
	baselineRevision := mustListSingleTarget(t, controller).Revision

	mutations := map[string]func(*agentInfo){
		"name":         func(info *agentInfo) { info.Name = "renamed" },
		"terminal_id":  func(info *agentInfo) { info.TerminalID = "other-terminal" },
		"pane_id":      func(info *agentInfo) { info.PaneID = "other-pane" },
		"workspace_id": func(info *agentInfo) { info.WorkspaceID = "other-workspace" },
		"agent":        func(info *agentInfo) { info.Agent = "codex" },
		"agent_status": func(info *agentInfo) { info.AgentStatus = "working" },
		"cwd":          func(info *agentInfo) { info.CWD = "/other/repo" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := baseline
			mutate(&changed)
			controller, _ := newControllerRPCScript(t, controllerRPCStep{method: "agent.list", result: agentListResult(changed)})
			if got := mustListSingleTarget(t, controller).Revision; got == baselineRevision {
				t.Fatalf("revision did not change when %s changed", name)
			}
		})
	}
}

func TestHerdrControllerTailRevalidatesExactTargetBeforeRead(t *testing.T) {
	agent := controllerAgent("alpha", "idle")
	controller, script := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult("last two lines")},
	)
	target := mustListSingleTarget(t, controller)

	got, err := controller.TailAgent(context.Background(), target.Ref(), 2)
	if err != nil {
		t.Fatalf("TailAgent: %v", err)
	}
	if got != "last two lines" {
		t.Fatalf("TailAgent() = %q, want last two lines", got)
	}
	call := script.call(t, 2)
	if call.params["target"] != agent.TerminalID || call.params["source"] != "recent" || call.params["lines"] != float64(2) {
		t.Fatalf("agent.read params = %#v, want immutable terminal target %q, recent source, and two lines", call.params, agent.TerminalID)
	}
}

func TestHerdrControllerUsesUniquePaneWhenTerminalIDIsMissing(t *testing.T) {
	agent := controllerAgent("alpha", "idle")
	agent.TerminalID = ""
	controller, script := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult("pane output")},
	)
	target := mustListSingleTarget(t, controller)
	if _, err := controller.TailAgent(context.Background(), target.Ref(), 1); err != nil {
		t.Fatalf("TailAgent: %v", err)
	}
	if got := script.call(t, 2).params["target"]; got != agent.PaneID {
		t.Fatalf("agent.read target = %#v, want immutable pane ID %q", got, agent.PaneID)
	}
}

func TestHerdrControllerUsesUniquePaneWhenTerminalIDIsAmbiguous(t *testing.T) {
	agent := controllerAgent("alpha", "idle")
	collision := controllerAgent("beta", "idle")
	collision.TerminalID = agent.TerminalID
	controller, script := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent, collision)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent, collision)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult("pane output")},
	)
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != agent.Name {
		t.Fatalf("ListAgents() = %#v, want alpha followed by beta", targets)
	}
	if _, err := controller.TailAgent(context.Background(), targets[0].Ref(), 1); err != nil {
		t.Fatalf("TailAgent: %v", err)
	}
	if got := script.call(t, 2).params["target"]; got != agent.PaneID {
		t.Fatalf("agent.read target = %#v, want unique pane ID %q after terminal ambiguity", got, agent.PaneID)
	}
}

func TestHerdrControllerRejectsDuplicateStableReadIdentity(t *testing.T) {
	original := controllerAgent("alpha", "idle")
	collision := controllerAgent("beta", "idle")
	collision.TerminalID = original.TerminalID
	collision.PaneID = original.PaneID

	t.Run("list", func(t *testing.T) {
		controller, _ := newControllerRPCScript(t, controllerRPCStep{method: "agent.list", result: agentListResult(original, collision)})
		targets, err := controller.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("ListAgents: %v", err)
		}
		if len(targets) != 0 {
			t.Fatalf("ListAgents() = %#v, want targets with duplicate terminal and pane identities rejected", targets)
		}
	})

	t.Run("recheck does not read", func(t *testing.T) {
		controller, script := newControllerRPCScript(t,
			controllerRPCStep{method: "agent.list", result: agentListResult(original)},
			controllerRPCStep{method: "agent.list", result: agentListResult(original, collision)},
		)
		target := mustListSingleTarget(t, controller)
		if _, err := controller.TailAgent(context.Background(), target.Ref(), 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
			t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
		}
		if calls := script.snapshotCalls(); len(calls) != 2 {
			t.Fatalf("RPC calls = %v, want validation lists only and no agent.read", controllerCallMethods(calls))
		}
	})
}

func TestHerdrControllerRejectsCrossFieldAmbiguousReadIdentity(t *testing.T) {
	original := controllerAgent("alpha", "idle")
	terminalCollision := controllerAgent("beta", "idle")
	terminalCollision.PaneID = original.TerminalID
	paneCollision := controllerAgent("gamma", "idle")
	paneCollision.TerminalID = original.PaneID
	current := []agentInfo{original, terminalCollision, paneCollision}

	t.Run("list", func(t *testing.T) {
		controller, _ := newControllerRPCScript(t, controllerRPCStep{method: "agent.list", result: agentListResult(current...)})
		targets, err := controller.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("ListAgents: %v", err)
		}
		for _, target := range targets {
			if target.ID == original.Name {
				t.Fatalf("ListAgents() = %#v, want alpha omitted because both read handles are ambiguous", targets)
			}
		}
	})

	t.Run("recheck does not read", func(t *testing.T) {
		controller, script := newControllerRPCScript(t,
			controllerRPCStep{method: "agent.list", result: agentListResult(original)},
			controllerRPCStep{method: "agent.list", result: agentListResult(current...)},
		)
		target := mustListSingleTarget(t, controller)
		if _, err := controller.TailAgent(context.Background(), target.Ref(), 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
			t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
		}
		if calls := script.snapshotCalls(); len(calls) != 2 {
			t.Fatalf("RPC calls = %v, want validation lists only and no agent.read", controllerCallMethods(calls))
		}
	})
}

func TestHerdrControllerRejectsInvalidOrReusedTargetBeforeRead(t *testing.T) {
	t.Run("invalid ref", func(t *testing.T) {
		controller, _ := newControllerRPCScript(t)
		if _, err := controller.TailAgent(context.Background(), core.AgentControlTargetRef{ID: "alpha"}, 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
			t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
		}
	})

	t.Run("reused name", func(t *testing.T) {
		original := controllerAgent("alpha", "idle")
		replacement := original
		replacement.PaneID = "replacement-pane"
		controller, _ := newControllerRPCScript(t,
			controllerRPCStep{method: "agent.list", result: agentListResult(original)},
			controllerRPCStep{method: "agent.list", result: agentListResult(replacement)},
		)
		target := mustListSingleTarget(t, controller)
		if _, err := controller.TailAgent(context.Background(), target.Ref(), 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
			t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
		}
	})

	t.Run("fallback matches", func(t *testing.T) {
		other := controllerAgent("alpha-copy", "idle")
		other.CWD = "alpha"
		caseVariant := controllerAgent("ALPHA", "idle")
		controller, _ := newControllerRPCScript(t, controllerRPCStep{
			method: "agent.list",
			result: agentListResult(other, caseVariant),
		})
		ref := core.AgentControlTargetRef{ID: "alpha", Revision: "old-revision"}
		if _, err := controller.TailAgent(context.Background(), ref, 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
			t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
		}
	})

	t.Run("duplicate same-name targets", func(t *testing.T) {
		original := controllerAgent("alpha", "idle")
		replacement := original
		replacement.PaneID = "replacement-pane"
		cases := []struct {
			name   string
			agents []agentInfo
		}{
			{name: "original first", agents: []agentInfo{original, replacement}},
			{name: "replacement first", agents: []agentInfo{replacement, original}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				controller, _ := newControllerRPCScript(t,
					controllerRPCStep{method: "agent.list", result: agentListResult(original)},
					controllerRPCStep{method: "agent.list", result: agentListResult(tc.agents...)},
				)
				target := mustListSingleTarget(t, controller)
				if _, err := controller.TailAgent(context.Background(), target.Ref(), 1); !errors.Is(err, core.ErrAgentControlTargetStale) {
					t.Fatalf("TailAgent error = %v, want ErrAgentControlTargetStale", err)
				}
			})
		}
	})
}

func TestHerdrControllerBlockedWithoutNumberedMenuIsInspectableButNotResponseCapable(t *testing.T) {
	agent := controllerAgent("alpha", "blocked")
	screen := "Permission requested\nPress Enter to continue"
	controller, _ := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult(screen)},
	)
	target := mustListSingleTarget(t, controller)

	if !target.Supports(core.AgentControlCapabilityRequests) {
		t.Fatal("blocked target does not advertise request inspection")
	}
	for _, capability := range []core.AgentControlCapability{
		core.AgentControlCapabilityPrompt,
		core.AgentControlCapabilityKey,
		core.AgentControlCapabilityQuestion,
	} {
		if target.Supports(capability) {
			t.Fatalf("blocked target unexpectedly advertises %q", capability)
		}
	}
	if _, ok := any(controller).(core.AgentControlQuestionResponder); ok {
		t.Fatal("herdr controller exposes question mutation for an inspect-only request")
	}
	request := mustListSingleRequest(t, controller, target.Ref())
	if len(request.Questions) != 1 || len(request.Questions[0].Options) != 0 {
		t.Fatalf("request questions = %#v, want no fabricated choices", request.Questions)
	}
	if request.Input["screen"] != clipBlockedTail(screen) {
		t.Fatalf("request input = %#v, want clipped visible-screen display", request.Input)
	}
}

func TestHerdrControllerListRequestsRequiresCurrentBlockedTarget(t *testing.T) {
	working := controllerAgent("alpha", "working")
	controller, _ := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(working)},
		controllerRPCStep{method: "agent.list", result: agentListResult(working)},
	)
	target := mustListSingleTarget(t, controller)

	if _, err := controller.ListAgentRequests(context.Background(), target.Ref()); !errors.Is(err, core.ErrAgentControlUnsupported) {
		t.Fatalf("ListAgentRequests error = %v, want ErrAgentControlUnsupported", err)
	}
}

func TestHerdrControllerListRequestsHashesCanonicalTargetAndFullScreen(t *testing.T) {
	agent := controllerAgent("alpha", "blocked")
	screen := "header\nChoose:\n1. Allow\n2. Deny"
	controller, script := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult(screen)},
	)
	target := mustListSingleTarget(t, controller)
	request := mustListSingleRequest(t, controller, target.Ref())
	tail := clipBlockedTail(screen)

	assertSHA256Token(t, "request ID", request.ID)
	assertSHA256Token(t, "request revision", request.Revision)
	wantID, wantRevision := blockedRequestTokens(target.Revision, screen)
	if request.ID != wantID || request.Revision != wantRevision {
		t.Fatalf("request ref = %#v, want identity derived from canonical target and complete visible screen", request.Ref())
	}
	if request.Kind != core.AgentControlRequestQuestion || request.ToolName != "AskUserQuestion" {
		t.Fatalf("request = %#v, want inspectable question-shaped AskUserQuestion", request)
	}
	if len(request.AllowedDecisions) != 0 {
		t.Fatalf("AllowedDecisions = %#v, herdr must not expose permission decisions", request.AllowedDecisions)
	}
	wantQuestion := core.UserQuestion{Question: tail, Options: parseNumberedMenu(tail)}
	if len(request.Questions) != 1 || !reflect.DeepEqual(request.Questions[0], wantQuestion) {
		t.Fatalf("Questions = %#v, want direct canonical numbered-menu projection %#v", request.Questions, wantQuestion)
	}
	if request.Input["screen"] != tail {
		t.Fatalf("request input = %#v, want clipped visible-screen display", request.Input)
	}
	read := script.call(t, 2)
	if read.params["target"] != agent.TerminalID || read.params["source"] != "visible" {
		t.Fatalf("agent.read params = %#v, want immutable terminal target %q and visible source", read.params, agent.TerminalID)
	}
	if _, hasLines := read.params["lines"]; hasLines {
		t.Fatalf("visible agent.read unexpectedly limited lines: %#v", read.params)
	}
}

func TestHerdrControllerRequestRefChangesWhenOnlyContentOutsideDisplayTailChanges(t *testing.T) {
	agent := controllerAgent("alpha", "blocked")
	displayTail := strings.Repeat("stable visible line\n", blockedTailLines-2) + "1. Allow\n2. Deny"
	before := "old hidden line\n" + displayTail
	after := "new hidden line\n" + displayTail
	if clipBlockedTail(before) != clipBlockedTail(after) {
		t.Fatal("test setup changed the clipped display tail")
	}
	controller, script := newControllerRPCScript(t,
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult(before)},
		controllerRPCStep{method: "agent.list", result: agentListResult(agent)},
		controllerRPCStep{method: "agent.read", result: visibleReadResult(after)},
	)
	target := mustListSingleTarget(t, controller)
	oldRequest := mustListSingleRequest(t, controller, target.Ref())
	currentRequest := mustListSingleRequest(t, controller, target.Ref())

	if oldRequest.Input["screen"] != currentRequest.Input["screen"] || !reflect.DeepEqual(oldRequest.Questions, currentRequest.Questions) {
		t.Fatalf("display projections differ: old=%#v current=%#v", oldRequest, currentRequest)
	}
	if oldRequest.Ref() == currentRequest.Ref() {
		t.Fatalf("old request ref %#v remained current after complete screen changed", oldRequest.Ref())
	}
	oldID, oldRevision := blockedRequestTokens(target.Revision, before)
	currentID, currentRevision := blockedRequestTokens(target.Revision, after)
	if oldRequest.Ref() != (core.AgentControlRequestRef{ID: oldID, Revision: oldRevision}) || currentRequest.Ref() != (core.AgentControlRequestRef{ID: currentID, Revision: currentRevision}) {
		t.Fatalf("request refs do not bind complete screens: old=%#v current=%#v", oldRequest.Ref(), currentRequest.Ref())
	}
	if _, ok := any(controller).(core.AgentControlQuestionResponder); ok {
		t.Fatal("herdr controller exposes a write path for the stale request ref")
	}
	for _, call := range script.snapshotCalls() {
		if call.method == "agent.prompt" || call.method == "agent.send_keys" {
			t.Fatalf("unexpected mutation RPC %q", call.method)
		}
	}
}
