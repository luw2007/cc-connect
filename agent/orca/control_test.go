package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type fakeRunner struct {
	mu     sync.Mutex
	calls  []runnerCall
	result map[string][]byte
	err    error
}

type runnerCall struct {
	name string
	args []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, runnerCall{name: name, args: args})
	err, result := r.err, r.result
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	key := args[0]
	if len(args) > 1 && args[0] == "terminal" {
		key = args[1]
	}
	return result[key], nil
}

func (r *fakeRunner) recordedCalls() []runnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runnerCall(nil), r.calls...)
}

func TestControllerListAgentsReturnsStableTerminalTargets(t *testing.T) {
	runner := &fakeRunner{result: map[string][]byte{
		"worktree": []byte(`{"result":{"worktrees":[{"worktreeId":"repo::/work/a","worktreeInstanceId":"instance-a","path":"/work/a","status":"working","agents":[{"agentType":"codex","state":"working"}]}],"topologyRevisions":{"repo::/work/a":7}}}`),
		"list":     []byte(`{"result":{"terminals":[{"handle":"term-a","worktreeId":"repo::/work/a","worktreePath":"/work/a","title":"worker","connected":true,"writable":true,"preview":"working"}]}}`),
	}}
	controller := &Controller{runner: runner}
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %#v", targets)
	}
	target := targets[0]
	if target.ID != "repo::/work/a/term-a" || target.Backend != "orca" || target.Kind != "codex" || target.Directory != "/work/a" || target.Status != "working" {
		t.Fatalf("target = %#v", target)
	}
	if !target.Supports(core.AgentControlCapabilityTail) || !target.Supports(core.AgentControlCapabilityPrompt) {
		t.Fatalf("capabilities = %#v", target.Capabilities)
	}
	if target.Revision == "" {
		t.Fatal("revision must be opaque and non-empty")
	}
	want := []runnerCall{{name: "orca", args: []string{"worktree", "ps", "--json"}}, {name: "orca", args: []string{"terminal", "list", "--worktree", "id:repo::/work/a", "--json"}}}
	if !reflect.DeepEqual(runner.recordedCalls(), want) {
		t.Fatalf("calls = %#v, want %#v", runner.recordedCalls(), want)
	}
}

// Terminal discovery fans out one subprocess per worktree. The fan-out must
// query every worktree exactly once and keep the listing complete and ordered;
// a dropped or duplicated worktree renumbers targets, and an index-based
// selection would then address a different agent.
func TestControllerListAgentsFansOutOverEveryWorktree(t *testing.T) {
	const worktreeCount = 25
	worktrees := make([]string, 0, worktreeCount)
	revisions := make([]string, 0, worktreeCount)
	terminals := make([]string, 0, worktreeCount)
	for i := 0; i < worktreeCount; i++ {
		id := fmt.Sprintf("repo::/work/%02d", i)
		worktrees = append(worktrees, fmt.Sprintf(`{"worktreeId":%q,"worktreeInstanceId":"inst-%02d","path":"/work/%02d","status":"working"}`, id, i, i))
		revisions = append(revisions, fmt.Sprintf("%q:%d", id, i))
		terminals = append(terminals, fmt.Sprintf(`{"handle":"term-%02d","worktreeId":%q,"worktreePath":"/work/%02d","connected":true,"writable":true}`, i, id, i))
	}
	runner := &perWorktreeRunner{
		worktreePayload: []byte(fmt.Sprintf(`{"result":{"worktrees":[%s],"topologyRevisions":{%s}}}`,
			strings.Join(worktrees, ","), strings.Join(revisions, ","))),
		terminalPayloads: terminals,
	}

	targets, err := (&Controller{runner: runner}).ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(targets) != worktreeCount {
		t.Fatalf("targets = %d, want %d", len(targets), worktreeCount)
	}
	for i := 1; i < len(targets); i++ {
		if targets[i-1].ID >= targets[i].ID {
			t.Fatalf("targets are not ordered: %q then %q", targets[i-1].ID, targets[i].ID)
		}
	}
	for worktreeID, count := range runner.terminalCalls() {
		if count != 1 {
			t.Fatalf("worktree %q queried %d times, want exactly 1", worktreeID, count)
		}
	}
	if got := len(runner.terminalCalls()); got != worktreeCount {
		t.Fatalf("queried %d worktrees, want %d", got, worktreeCount)
	}
}

// perWorktreeRunner answers `terminal list` with the payload matching the
// requested worktree, so a fan-out that mixes up results is detectable.
type perWorktreeRunner struct {
	mu               sync.Mutex
	worktreePayload  []byte
	terminalPayloads []string
	seen             map[string]int
}

func (r *perWorktreeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if args[0] == "worktree" {
		return r.worktreePayload, nil
	}
	worktreeID := strings.TrimPrefix(args[3], "id:")
	r.mu.Lock()
	if r.seen == nil {
		r.seen = make(map[string]int)
	}
	r.seen[worktreeID]++
	r.mu.Unlock()
	for _, payload := range r.terminalPayloads {
		if strings.Contains(payload, `"worktreeId":"`+worktreeID+`"`) {
			return []byte(`{"result":{"terminals":[` + payload + `]}}`), nil
		}
	}
	return nil, fmt.Errorf("unknown worktree %q", worktreeID)
}

func (r *perWorktreeRunner) terminalCalls() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.seen))
	for id, count := range r.seen {
		out[id] = count
	}
	return out
}

func TestControllerTailRejectsStaleTarget(t *testing.T) {
	runner := &fakeRunner{result: map[string][]byte{
		"worktree": []byte(`{"result":{"worktrees":[{"worktreeId":"repo::/work/a","worktreeInstanceId":"instance-a","path":"/work/a","status":"working"}],"topologyRevisions":{"repo::/work/a":7}}}`),
		"list":     []byte(`{"result":{"terminals":[{"handle":"term-a","worktreeId":"repo::/work/a","worktreePath":"/work/a","connected":true,"writable":true}]}}`),
	}}
	_, err := (&Controller{runner: runner}).TailAgent(context.Background(), core.AgentControlTargetRef{ID: "repo::/work/a/term-a", Revision: "stale"}, 10)
	if !errors.Is(err, core.ErrAgentControlTargetStale) {
		t.Fatalf("err = %v, want stale target", err)
	}
}

func TestControllerTailReadsExactTerminal(t *testing.T) {
	runner := &fakeRunner{result: map[string][]byte{
		"worktree": []byte(`{"result":{"worktrees":[{"worktreeId":"repo::/work/a","worktreeInstanceId":"instance-a","path":"/work/a","status":"working"}],"topologyRevisions":{"repo::/work/a":7}}}`),
		"list":     []byte(`{"result":{"terminals":[{"handle":"term-a","worktreeId":"repo::/work/a","worktreePath":"/work/a","connected":true,"writable":true}]}}`),
		"read":     []byte(`{"result":{"terminal":{"tail":["one","two"]}}}`),
	}}
	controller := &Controller{runner: runner}
	targets, err := controller.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text, err := controller.TailAgent(context.Background(), targets[0].Ref(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if text != "two" {
		t.Fatalf("tail = %q", text)
	}
	got := runner.calls[len(runner.calls)-1]
	want := runnerCall{name: "orca", args: []string{"terminal", "read", "--terminal", "term-a", "--json"}}
	if !reflect.DeepEqual(got, want) {
		data, _ := json.Marshal(got)
		t.Fatalf("read call = %s", data)
	}
}
