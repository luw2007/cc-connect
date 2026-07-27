package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type recordedAgentCall struct {
	method   string
	target   core.AgentControlTargetRef
	request  core.AgentControlRequestRef
	lines    int
	value    string
	decision string
	answers  []core.AgentControlQuestionAnswer
}

type fakeAgentController struct {
	targets  []core.AgentControlTarget
	requests []core.AgentControlRequest
	tail     string
	listErr  error
	callErr  error
	calls    []recordedAgentCall
}

func (f *fakeAgentController) ListAgents(context.Context) ([]core.AgentControlTarget, error) {
	f.calls = append(f.calls, recordedAgentCall{method: "list"})
	return f.targets, f.listErr
}

func (f *fakeAgentController) TailAgent(_ context.Context, target core.AgentControlTargetRef, lines int) (string, error) {
	f.calls = append(f.calls, recordedAgentCall{method: "tail", target: target, lines: lines})
	return f.tail, f.callErr
}

func (f *fakeAgentController) SendAgentPrompt(_ context.Context, target core.AgentControlTargetRef, message string) error {
	f.calls = append(f.calls, recordedAgentCall{method: "send", target: target, value: message})
	return f.callErr
}

func (f *fakeAgentController) SendAgentKey(_ context.Context, target core.AgentControlTargetRef, key string) error {
	f.calls = append(f.calls, recordedAgentCall{method: "key", target: target, value: key})
	return f.callErr
}

func (f *fakeAgentController) ListAgentRequests(_ context.Context, target core.AgentControlTargetRef) ([]core.AgentControlRequest, error) {
	f.calls = append(f.calls, recordedAgentCall{method: "requests", target: target})
	return f.requests, f.callErr
}

func (f *fakeAgentController) RespondAgentPermission(_ context.Context, target core.AgentControlTargetRef, request core.AgentControlRequestRef, decision string) error {
	f.calls = append(f.calls, recordedAgentCall{method: "approve", target: target, request: request, decision: decision})
	return f.callErr
}

func (f *fakeAgentController) AnswerAgentQuestions(_ context.Context, target core.AgentControlTargetRef, request core.AgentControlRequestRef, answers []core.AgentControlQuestionAnswer) error {
	f.calls = append(f.calls, recordedAgentCall{method: "answer", target: target, request: request, answers: answers})
	return f.callErr
}

type listOnlyAgentController struct {
	targets []core.AgentControlTarget
	calls   int
}

func (f *listOnlyAgentController) ListAgents(context.Context) ([]core.AgentControlTarget, error) {
	f.calls++
	return f.targets, nil
}

type fakeAgentControllerFactory struct {
	controller core.AgentController
	calls      int
	backend    string
	opts       map[string]any
}

func (f *fakeAgentControllerFactory) create(backend string, opts map[string]any) (core.AgentController, error) {
	f.calls++
	f.backend = backend
	f.opts = make(map[string]any, len(opts))
	for key, value := range opts {
		f.opts[key] = value
	}
	return f.controller, nil
}

func directAgentDeps(factory *fakeAgentControllerFactory) agentCommandDeps {
	return agentCommandDeps{
		createController: factory.create,
		loadConfig: func(string) (*config.Config, error) {
			return nil, errors.New("direct backend must not load config")
		},
		resolveConfigPath: func(string) string { return "unexpected" },
	}
}

func testAgentTarget(capabilities ...core.AgentControlCapability) core.AgentControlTarget {
	return core.AgentControlTarget{
		ID:           "target-1",
		Revision:     "target-revision-0123456789",
		Backend:      "cmux",
		Kind:         "claude",
		Directory:    "/repo",
		Status:       "running",
		Capabilities: capabilities,
	}
}

func TestParseAgentArgsRequiresFreshnessRefs(t *testing.T) {
	tests := [][]string{
		{"tail", "--backend", "cmux", "--id", "target-1"},
		{"send", "--backend", "cmux", "--id", "target-1", "--message", "hello"},
		{"key", "--backend", "cmux", "--id", "target-1", "--key", "ENTER"},
		{"requests", "--backend", "cmux", "--id", "target-1"},
		{"approve", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev", "--request", "req", "--mode", "once"},
		{"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev", "--request", "req", "--choice", "Yes"},
	}
	for _, args := range tests {
		if _, err := parseAgentArgs(args); err == nil {
			t.Errorf("parseAgentArgs(%q) accepted missing revision", args)
		}
	}
}

func TestParseAgentArgsTailLines(t *testing.T) {
	command, err := parseAgentArgs([]string{"tail", "--backend", "cmux", "--id", "target-1", "--revision", "rev"})
	if err != nil {
		t.Fatalf("parseAgentArgs: %v", err)
	}
	if command.lines != 100 {
		t.Fatalf("default lines = %d, want 100", command.lines)
	}
	for _, value := range []string{"0", "-1", "bad"} {
		_, err := parseAgentArgs([]string{"tail", "--backend", "cmux", "--id", "target-1", "--revision", "rev", "--lines", value})
		if err == nil {
			t.Errorf("accepted --lines %q", value)
		}
	}
}

func TestParseAgentArgsBuildsIndexedAnswers(t *testing.T) {
	command, err := parseAgentArgs([]string{
		"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev",
		"--request", "req", "--request-revision", "request-rev",
		"--answer", "0:Red", "--answer", "0:Blue", "--text", "1:explain: further",
	})
	if err != nil {
		t.Fatalf("parseAgentArgs: %v", err)
	}
	want := []core.AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"Red", "Blue"}},
		{Index: 1, Text: "explain: further"},
	}
	if !reflect.DeepEqual(command.answers, want) {
		t.Fatalf("answers = %#v, want %#v", command.answers, want)
	}

	shorthand, err := parseAgentArgs([]string{
		"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev",
		"--request", "req", "--request-revision", "request-rev", "--choice", "Yes",
	})
	if err != nil {
		t.Fatalf("parse shorthand: %v", err)
	}
	want = []core.AgentControlQuestionAnswer{{Index: 0, Selections: []string{"Yes"}}}
	if !reflect.DeepEqual(shorthand.answers, want) {
		t.Fatalf("shorthand answers = %#v, want %#v", shorthand.answers, want)
	}

	mixed, err := parseAgentArgs([]string{
		"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev",
		"--request", "req", "--request-revision", "request-rev",
		"--choice", "Yes", "--answer", "1:No", "--text", "2:details",
	})
	if err != nil {
		t.Fatalf("parse mixed shorthand: %v", err)
	}
	want = []core.AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"Yes"}},
		{Index: 1, Selections: []string{"No"}},
		{Index: 2, Text: "details"},
	}
	if !reflect.DeepEqual(mixed.answers, want) {
		t.Fatalf("mixed answers = %#v, want %#v", mixed.answers, want)
	}
}

func TestParseAgentArgsRejectsAmbiguousAnswers(t *testing.T) {
	base := []string{
		"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev",
		"--request", "req", "--request-revision", "request-rev",
	}
	tests := [][]string{
		nil,
		{"--choice", "Yes", "--answer", "0:No"},
		{"--choice", "Yes", "--text", "0:why"},
		{"--answer", "0:Yes", "--text", "0:why"},
		{"--text", "0:first", "--text", "0:second"},
		{"--answer", "bad"},
		{"--answer", "-1:Yes"},
		{"--text", "0:   "},
	}
	for _, extra := range tests {
		args := append(append([]string(nil), base...), extra...)
		if _, err := parseAgentArgs(args); err == nil {
			t.Errorf("accepted ambiguous answer flags %q", extra)
		}
	}
}

func TestParseAgentArgsPreservesAnswerValues(t *testing.T) {
	command, err := parseAgentArgs([]string{
		"answer", "--backend", "herdr", "--id", "worker", "--revision", "target-rev",
		"--request", "req", "--request-revision", "request-rev",
		"--answer", "0:  exact label  ", "--text", "1:  exact text  ",
	})
	if err != nil {
		t.Fatalf("parseAgentArgs: %v", err)
	}
	want := []core.AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"  exact label  "}},
		{Index: 1, Text: "  exact text  "},
	}
	if !reflect.DeepEqual(command.answers, want) {
		t.Fatalf("answers = %#v, want exact values %#v", command.answers, want)
	}
}

func TestParseAgentArgsRejectsConflictingSendInputs(t *testing.T) {
	_, err := parseAgentArgs([]string{
		"send", "--backend", "cmux", "--id", "target-1", "--revision", "rev",
		"--message", "hello", "--stdin",
	})
	if err == nil {
		t.Fatal("accepted --message with --stdin")
	}
}

func TestParseAgentArgsAcceptsEitherSendInput(t *testing.T) {
	tests := [][]string{
		{"send", "--backend", "cmux", "--id", "target-1", "--revision", "rev", "--message", "hello"},
		{"send", "--backend", "cmux", "--id", "target-1", "--revision", "rev", "--stdin"},
	}
	for _, args := range tests {
		if _, err := parseAgentArgs(args); err != nil {
			t.Errorf("parseAgentArgs(%q): %v", args, err)
		}
	}
}

func TestParseAgentArgsRejectsFlagsIrrelevantToEachAction(t *testing.T) {
	flagValues := map[string]string{
		"--id":               "target-1",
		"--revision":         "target-rev",
		"--lines":            "10",
		"--message":          "hello",
		"--key":              "ENTER",
		"--request":          "request-1",
		"--request-revision": "request-rev",
		"--mode":             "once",
		"--choice":           "Yes",
		"--answer":           "0:Yes",
		"--text":             "0:details",
	}
	tests := []struct {
		action     string
		base       []string
		irrelevant []string
	}{
		{"list", []string{"list", "--backend", "cmux"}, []string{"--id", "--revision", "--lines", "--message", "--stdin", "--key", "--request", "--request-revision", "--mode", "--choice", "--answer", "--text"}},
		{"tail", []string{"tail", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev"}, []string{"--message", "--stdin", "--key", "--request", "--request-revision", "--mode", "--choice", "--answer", "--text"}},
		{"send", []string{"send", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev", "--message", "hello"}, []string{"--lines", "--key", "--request", "--request-revision", "--mode", "--choice", "--answer", "--text"}},
		{"key", []string{"key", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev", "--key", "ENTER"}, []string{"--lines", "--message", "--stdin", "--request", "--request-revision", "--mode", "--choice", "--answer", "--text"}},
		{"requests", []string{"requests", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev"}, []string{"--lines", "--message", "--stdin", "--key", "--request", "--request-revision", "--mode", "--choice", "--answer", "--text"}},
		{"approve", []string{"approve", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev", "--request", "request-1", "--request-revision", "request-rev", "--mode", "once"}, []string{"--lines", "--message", "--stdin", "--key", "--choice", "--answer", "--text"}},
		{"answer", []string{"answer", "--backend", "cmux", "--id", "target-1", "--revision", "target-rev", "--request", "request-1", "--request-revision", "request-rev", "--choice", "Yes"}, []string{"--lines", "--message", "--stdin", "--key", "--mode"}},
	}
	for _, test := range tests {
		for _, flag := range test.irrelevant {
			t.Run(test.action+"/"+flag, func(t *testing.T) {
				args := append([]string(nil), test.base...)
				args = append(args, flag)
				if flag != "--stdin" {
					args = append(args, flagValues[flag])
				}
				if _, err := parseAgentArgs(args); err == nil {
					t.Fatalf("parseAgentArgs(%q) accepted irrelevant %s", args, flag)
				}
			})
		}
	}
}

func TestExecuteAgentRequestsHumanTableOutput(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityRequests)
	requests := []core.AgentControlRequest{
		{ID: "permission-1", Revision: "permission-revision-0123456789", Kind: core.AgentControlRequestPermission, ToolName: "Bash", AllowedDecisions: []string{"once", "always"}},
		{ID: "question-1", Revision: "question-revision-0123456789", Kind: core.AgentControlRequestQuestion, Questions: []core.UserQuestion{{Question: "Continue?"}}},
	}
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, requests: requests}
	factory := &fakeAgentControllerFactory{controller: controller}
	var stdout bytes.Buffer
	err := executeAgent(context.Background(), []string{
		"requests", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision,
	}, strings.NewReader(""), &stdout, directAgentDeps(factory))
	if err != nil {
		t.Fatalf("requests: %v", err)
	}
	output := stdout.String()
	for _, value := range []string{
		"ID", "REVISION", "KIND", "TOOL", "DECISIONS", "QUESTIONS",
		"permission-1", "permission-r…", "permission", "Bash", "once,always",
		"question-1", "question-rev…", "question", "1",
		"Use --json to copy the full --request and --request-revision values for a response.",
	} {
		if !strings.Contains(output, value) {
			t.Errorf("human requests output missing %q:\n%s", value, output)
		}
	}
	for _, revision := range []string{requests[0].Revision, requests[1].Revision} {
		if strings.Contains(output, revision) {
			t.Errorf("human requests output exposed full revision %q:\n%s", revision, output)
		}
	}
}

func TestExecuteAgentRequestsJSONOutput(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityRequests)
	requests := []core.AgentControlRequest{
		{ID: "permission-1", Revision: "permission-revision", Kind: core.AgentControlRequestPermission, ToolName: "Bash", Input: map[string]any{"command": "go test ./..."}, AllowedDecisions: []string{"once"}},
		{ID: "question-1", Revision: "question-revision", Kind: core.AgentControlRequestQuestion, Questions: []core.UserQuestion{{Question: "Continue?"}}},
	}
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, requests: requests}
	factory := &fakeAgentControllerFactory{controller: controller}
	var stdout bytes.Buffer
	err := executeAgent(context.Background(), []string{
		"requests", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision, "--json",
	}, strings.NewReader(""), &stdout, directAgentDeps(factory))
	if err != nil {
		t.Fatalf("requests --json: %v", err)
	}
	var got []core.AgentControlRequest
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("requests JSON=%s error=%v", stdout.String(), err)
	}
	if !reflect.DeepEqual(got, requests) {
		t.Fatalf("requests JSON value=%#v, want %#v", got, requests)
	}
}

func TestParseAgentArgsRejectsBlankSelectionSources(t *testing.T) {
	for _, flag := range []string{"--backend", "--project", "--config", "--socket"} {
		if _, err := parseAgentArgs([]string{"list", flag, "   "}); err == nil {
			t.Errorf("accepted blank %s", flag)
		}
	}
}

func TestExecuteAgentDirectBackendPassesOnlySocketOption(t *testing.T) {
	factory := &fakeAgentControllerFactory{controller: &fakeAgentController{}}
	err := executeAgent(context.Background(), []string{"list", "--backend", "cmux", "--socket", "/tmp/cmux.sock"}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
	if err != nil {
		t.Fatalf("executeAgent: %v", err)
	}
	if factory.backend != "cmux" || !reflect.DeepEqual(factory.opts, map[string]any{"socket_path": "/tmp/cmux.sock"}) {
		t.Fatalf("factory got backend=%q opts=%#v", factory.backend, factory.opts)
	}
}

func TestExecuteAgentProjectSelectionCopiesOptions(t *testing.T) {
	cfg := &config.Config{Projects: []config.ProjectConfig{
		{Name: "one", Agent: config.AgentConfig{Type: "cmux", Options: map[string]any{"socket_path": "/configured.sock", "password": "secret"}}},
		{Name: "two", Agent: config.AgentConfig{Type: "herdr"}},
	}}
	factory := &fakeAgentControllerFactory{controller: &fakeAgentController{}}
	deps := agentCommandDeps{
		createController: factory.create,
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		resolveConfigPath: func(path string) string {
			return "/resolved/" + path
		},
	}
	var stdout bytes.Buffer
	err := executeAgent(context.Background(), []string{"list", "--config", "config.toml", "--project", "one", "--backend", "cmux", "--socket", "/override.sock"}, strings.NewReader(""), &stdout, deps)
	if err != nil {
		t.Fatalf("executeAgent: %v", err)
	}
	want := map[string]any{"socket_path": "/override.sock", "password": "secret"}
	if !reflect.DeepEqual(factory.opts, want) {
		t.Fatalf("factory opts = %#v, want %#v", factory.opts, want)
	}
	if cfg.Projects[0].Agent.Options["socket_path"] != "/configured.sock" {
		t.Fatal("project options were mutated")
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatalf("output leaked options: %s", stdout.String())
	}
}

func TestExecuteAgentDefaultConfigRequiresSingleton(t *testing.T) {
	single := &config.Config{Projects: []config.ProjectConfig{{Name: "only", Agent: config.AgentConfig{Type: "herdr"}}}}
	factory := &fakeAgentControllerFactory{controller: &fakeAgentController{}}
	deps := agentCommandDeps{
		createController: factory.create,
		loadConfig:       func(string) (*config.Config, error) { return single, nil },
		resolveConfigPath: func(path string) string {
			if path != "" {
				t.Fatalf("default resolver input = %q", path)
			}
			return "/default/config.toml"
		},
	}
	if err := executeAgent(context.Background(), []string{"list"}, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatalf("singleton default config: %v", err)
	}
	if factory.backend != "herdr" {
		t.Fatalf("backend = %q, want herdr", factory.backend)
	}

	multi := &config.Config{Projects: []config.ProjectConfig{
		{Name: "one", Agent: config.AgentConfig{Type: "cmux"}},
		{Name: "two", Agent: config.AgentConfig{Type: "herdr"}},
	}}
	factory.calls = 0
	deps.loadConfig = func(string) (*config.Config, error) { return multi, nil }
	if err := executeAgent(context.Background(), []string{"list"}, strings.NewReader(""), io.Discard, deps); err == nil {
		t.Fatal("accepted ambiguous default config")
	}
	if factory.calls != 0 {
		t.Fatal("created controller before rejecting ambiguous config")
	}
}

func TestExecuteAgentRejectsProjectBackendMismatch(t *testing.T) {
	cfg := &config.Config{Projects: []config.ProjectConfig{{Name: "one", Agent: config.AgentConfig{Type: "cmux"}}}}
	factory := &fakeAgentControllerFactory{controller: &fakeAgentController{}}
	deps := agentCommandDeps{
		createController: factory.create,
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		resolveConfigPath: func(string) string {
			return "/config.toml"
		},
	}
	err := executeAgent(context.Background(), []string{"list", "--project", "one", "--backend", "herdr"}, strings.NewReader(""), io.Discard, deps)
	if err == nil || factory.calls != 0 {
		t.Fatalf("mismatch error=%v factory calls=%d", err, factory.calls)
	}
}

func TestExecuteAgentRejectsUnknownAndBackendOnlyConfigSelection(t *testing.T) {
	cfg := &config.Config{Projects: []config.ProjectConfig{
		{Name: "one", Agent: config.AgentConfig{Type: "cmux"}},
		{Name: "two", Agent: config.AgentConfig{Type: "herdr"}},
	}}
	for _, args := range [][]string{
		{"list", "--project", "missing"},
		{"list", "--config", "config.toml", "--backend", "cmux"},
	} {
		factory := &fakeAgentControllerFactory{controller: &fakeAgentController{}}
		deps := agentCommandDeps{
			createController: factory.create,
			loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
			resolveConfigPath: func(string) string {
				return "/config.toml"
			},
		}
		if err := executeAgent(context.Background(), args, strings.NewReader(""), io.Discard, deps); err == nil {
			t.Errorf("executeAgent(%q) accepted ambiguous or unknown project", args)
		}
		if factory.calls != 0 {
			t.Errorf("executeAgent(%q) created controller before rejecting selection", args)
		}
	}
}

func TestExecuteAgentPassesExactTargetRef(t *testing.T) {
	tests := []struct {
		name       string
		capability core.AgentControlCapability
		args       []string
		stdin      string
		method     string
		value      string
	}{
		{"tail", core.AgentControlCapabilityTail, []string{"tail", "--lines", "17"}, "", "tail", ""},
		{"send", core.AgentControlCapabilityPrompt, []string{"send", "--message", "work"}, "", "send", "work"},
		{"stdin", core.AgentControlCapabilityPrompt, []string{"send", "--stdin"}, "from stdin\n", "send", "from stdin"},
		{"key", core.AgentControlCapabilityKey, []string{"key", "--key", "CTRL_C"}, "", "key", "CTRL_C"},
		{"requests", core.AgentControlCapabilityRequests, []string{"requests"}, "", "requests", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := testAgentTarget(test.capability)
			controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
			factory := &fakeAgentControllerFactory{controller: controller}
			args := append(test.args, "--backend", "cmux", "--id", target.ID, "--revision", target.Revision)
			if err := executeAgent(context.Background(), args, strings.NewReader(test.stdin), io.Discard, directAgentDeps(factory)); err != nil {
				t.Fatalf("executeAgent: %v", err)
			}
			if len(controller.calls) != 2 || controller.calls[1].method != test.method || controller.calls[1].target != target.Ref() || controller.calls[1].value != test.value {
				t.Fatalf("calls = %#v", controller.calls)
			}
			if test.method == "tail" && controller.calls[1].lines != 17 {
				t.Fatalf("tail lines = %d", controller.calls[1].lines)
			}
		})
	}
}

func TestExecuteAgentRejectsHerdrMutationsBeforeInspection(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "send", args: []string{"send", "--message", "work"}},
		{name: "key", args: []string{"key", "--key", "ENTER"}},
		{name: "answer", args: []string{"answer", "--request", "question-1", "--request-revision", "request-rev", "--choice", "Yes"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := testAgentTarget(core.AgentControlCapabilityRequests)
			target.Backend = "herdr"
			controller := &fakeAgentController{
				targets:  []core.AgentControlTarget{target},
				requests: []core.AgentControlRequest{{ID: "question-1", Revision: "request-rev", Kind: core.AgentControlRequestQuestion}},
			}
			factory := &fakeAgentControllerFactory{controller: controller}
			args := append(test.args, "--backend", "herdr", "--id", target.ID, "--revision", target.Revision)

			err := executeAgent(context.Background(), args, strings.NewReader(""), io.Discard, directAgentDeps(factory))

			if !errors.Is(err, core.ErrAgentControlUnsupported) {
				t.Fatalf("error = %v, want ErrAgentControlUnsupported", err)
			}
			if len(controller.calls) != 1 || controller.calls[0].method != "list" {
				t.Fatalf("calls = %#v, Herdr mutation must stop after discovery", controller.calls)
			}
		})
	}
}

func TestExecuteAgentRejectsEmptyPromptStdinBeforeMutation(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityPrompt)
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
	factory := &fakeAgentControllerFactory{controller: controller}
	err := executeAgent(context.Background(), []string{
		"send", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision, "--stdin",
	}, strings.NewReader(" \n\t"), io.Discard, directAgentDeps(factory))
	if err == nil || !strings.Contains(err.Error(), "empty message") {
		t.Fatalf("error = %v, want empty stdin rejection", err)
	}
	if len(controller.calls) != 1 || controller.calls[0].method != "list" {
		t.Fatalf("calls = %#v, empty stdin must not be sent", controller.calls)
	}
}

func TestExecuteAgentTrimsTerminalNewlinesFromPromptStdin(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityPrompt)
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
	factory := &fakeAgentControllerFactory{controller: controller}
	if err := executeAgent(context.Background(), []string{
		"send", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision, "--stdin",
	}, strings.NewReader("from stdin\r\n"), io.Discard, directAgentDeps(factory)); err != nil {
		t.Fatalf("executeAgent: %v", err)
	}
	if len(controller.calls) != 2 || controller.calls[1].method != "send" || controller.calls[1].value != "from stdin" {
		t.Fatalf("calls = %#v, want terminal newline removed", controller.calls)
	}
}

func TestExecuteAgentStopsOnStaleTargetOrUnsupportedCapability(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		target := testAgentTarget(core.AgentControlCapabilityTail)
		controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
		factory := &fakeAgentControllerFactory{controller: controller}
		err := executeAgent(context.Background(), []string{"tail", "--backend", "cmux", "--id", target.ID, "--revision", "old"}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
		if !errors.Is(err, core.ErrAgentControlTargetStale) || len(controller.calls) != 1 {
			t.Fatalf("error=%v calls=%#v", err, controller.calls)
		}
	})

	t.Run("not advertised", func(t *testing.T) {
		target := testAgentTarget()
		controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
		factory := &fakeAgentControllerFactory{controller: controller}
		err := executeAgent(context.Background(), []string{"tail", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
		if !errors.Is(err, core.ErrAgentControlUnsupported) || len(controller.calls) != 1 {
			t.Fatalf("error=%v calls=%#v", err, controller.calls)
		}
	})

	t.Run("interface absent", func(t *testing.T) {
		target := testAgentTarget(core.AgentControlCapabilityTail)
		controller := &listOnlyAgentController{targets: []core.AgentControlTarget{target}}
		factory := &fakeAgentControllerFactory{controller: controller}
		err := executeAgent(context.Background(), []string{"tail", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
		if !errors.Is(err, core.ErrAgentControlUnsupported) || controller.calls != 1 {
			t.Fatalf("error=%v list calls=%d", err, controller.calls)
		}
	})
}

func TestExecuteAgentResponderCapabilityMismatchStopsBeforeRequestInspection(t *testing.T) {
	tests := []struct {
		name       string
		capability core.AgentControlCapability
		args       []string
	}{
		{
			name:       "permission",
			capability: core.AgentControlCapabilityPermission,
			args:       []string{"approve", "--request", "req", "--request-revision", "req-rev", "--mode", "once"},
		},
		{
			name:       "question",
			capability: core.AgentControlCapabilityQuestion,
			args:       []string{"answer", "--request", "req", "--request-revision", "req-rev", "--choice", "Yes"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := testAgentTarget(core.AgentControlCapabilityRequests)
			controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
			factory := &fakeAgentControllerFactory{controller: controller}
			args := append(test.args, "--backend", "cmux", "--id", target.ID, "--revision", target.Revision)
			err := executeAgent(context.Background(), args, strings.NewReader(""), io.Discard, directAgentDeps(factory))
			if !errors.Is(err, core.ErrAgentControlUnsupported) {
				t.Fatalf("error = %v, want ErrAgentControlUnsupported for %s", err, test.capability)
			}
			if len(controller.calls) != 1 || controller.calls[0].method != "list" {
				t.Fatalf("calls = %#v, mismatch must stop before request inspection", controller.calls)
			}
		})
	}
}

func TestExecuteAgentRespondersReceiveOnlyExactRefs(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityRequests, core.AgentControlCapabilityPermission, core.AgentControlCapabilityQuestion)
	permission := core.AgentControlRequest{ID: "permission-1", Revision: "permission-rev", Kind: core.AgentControlRequestPermission, AllowedDecisions: []string{"once"}, Input: map[string]any{"untrusted": true}}
	question := core.AgentControlRequest{ID: "question-1", Revision: "question-rev", Kind: core.AgentControlRequestQuestion, Questions: []core.UserQuestion{{Question: "Untrusted projection"}}}

	t.Run("approve", func(t *testing.T) {
		controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, requests: []core.AgentControlRequest{permission, question}}
		factory := &fakeAgentControllerFactory{controller: controller}
		err := executeAgent(context.Background(), []string{
			"approve", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision,
			"--request", permission.ID, "--request-revision", permission.Revision, "--mode", "once",
		}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
		if err != nil {
			t.Fatalf("executeAgent: %v", err)
		}
		got := controller.calls[len(controller.calls)-1]
		if got.method != "approve" || got.target != target.Ref() || got.request != permission.Ref() || got.decision != "once" {
			t.Fatalf("approve call = %#v", got)
		}
	})

	t.Run("answer", func(t *testing.T) {
		controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, requests: []core.AgentControlRequest{permission, question}}
		factory := &fakeAgentControllerFactory{controller: controller}
		err := executeAgent(context.Background(), []string{
			"answer", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision,
			"--request", question.ID, "--request-revision", question.Revision,
			"--answer", "0:Yes", "--text", "1:details",
		}, strings.NewReader(""), io.Discard, directAgentDeps(factory))
		if err != nil {
			t.Fatalf("executeAgent: %v", err)
		}
		got := controller.calls[len(controller.calls)-1]
		wantAnswers := []core.AgentControlQuestionAnswer{{Index: 0, Selections: []string{"Yes"}}, {Index: 1, Text: "details"}}
		if got.method != "answer" || got.target != target.Ref() || got.request != question.Ref() || !reflect.DeepEqual(got.answers, wantAnswers) {
			t.Fatalf("answer call = %#v", got)
		}
	})
}

func TestExecuteAgentStopsOnStaleRequestBeforeResponder(t *testing.T) {
	tests := []struct {
		name         string
		capability   core.AgentControlCapability
		kind         core.AgentControlRequestKind
		responseArgs []string
	}{
		{
			name:         "permission",
			capability:   core.AgentControlCapabilityPermission,
			kind:         core.AgentControlRequestPermission,
			responseArgs: []string{"approve", "--mode", "once"},
		},
		{
			name:         "question",
			capability:   core.AgentControlCapabilityQuestion,
			kind:         core.AgentControlRequestQuestion,
			responseArgs: []string{"answer", "--choice", "Yes"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := testAgentTarget(core.AgentControlCapabilityRequests, test.capability)
			request := core.AgentControlRequest{ID: "request-1", Revision: "new-revision", Kind: test.kind}
			controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, requests: []core.AgentControlRequest{request}}
			factory := &fakeAgentControllerFactory{controller: controller}
			args := append(test.responseArgs,
				"--backend", "cmux", "--id", target.ID, "--revision", target.Revision,
				"--request", request.ID, "--request-revision", "old-revision",
			)
			err := executeAgent(context.Background(), args, strings.NewReader(""), io.Discard, directAgentDeps(factory))
			if !errors.Is(err, core.ErrAgentControlRequestStale) || len(controller.calls) != 2 || controller.calls[1].method != "requests" {
				t.Fatalf("error=%v calls=%#v", err, controller.calls)
			}
		})
	}
}

func TestExecuteAgentJSONUsesBareCoreValues(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityTail, core.AgentControlCapabilityKey)
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}, tail: "exact tail"}
	factory := &fakeAgentControllerFactory{controller: controller}
	var stdout bytes.Buffer
	if err := executeAgent(context.Background(), []string{"list", "--backend", "cmux", "--json"}, strings.NewReader(""), &stdout, directAgentDeps(factory)); err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []core.AgentControlTarget
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !reflect.DeepEqual(got, []core.AgentControlTarget{target}) {
		t.Fatalf("list JSON=%s error=%v value=%#v", stdout.String(), err, got)
	}

	controller.calls = nil
	stdout.Reset()
	if err := executeAgent(context.Background(), []string{"tail", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision, "--json"}, strings.NewReader(""), &stdout, directAgentDeps(factory)); err != nil {
		t.Fatalf("tail: %v", err)
	}
	var tail string
	if err := json.Unmarshal(stdout.Bytes(), &tail); err != nil || tail != "exact tail" {
		t.Fatalf("tail JSON=%s error=%v value=%q", stdout.String(), err, tail)
	}

	controller.calls = nil
	stdout.Reset()
	if err := executeAgent(context.Background(), []string{"key", "--backend", "cmux", "--id", target.ID, "--revision", target.Revision, "--key", "ENTER", "--json"}, strings.NewReader(""), &stdout, directAgentDeps(factory)); err != nil {
		t.Fatalf("key: %v", err)
	}
	if stdout.String() != "null\n" {
		t.Fatalf("void JSON = %q, want null", stdout.String())
	}
}

func TestHumanListAndHelpExposeSafeRevisionFlow(t *testing.T) {
	target := testAgentTarget(core.AgentControlCapabilityTail)
	controller := &fakeAgentController{targets: []core.AgentControlTarget{target}}
	factory := &fakeAgentControllerFactory{controller: controller}
	var stdout bytes.Buffer
	if err := executeAgent(context.Background(), []string{"list", "--backend", "cmux"}, strings.NewReader(""), &stdout, directAgentDeps(factory)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout.String(), "--json") || !strings.Contains(stdout.String(), "--revision") || strings.Contains(stdout.String(), target.Revision) {
		t.Fatalf("unsafe human list:\n%s", stdout.String())
	}

	stdout.Reset()
	printAgentUsage(&stdout)
	help := stdout.String()
	for _, token := range []string{"list", "tail", "key", "requests", "approve", "answer", "--revision", "--request-revision", "--answer"} {
		if !strings.Contains(help, token) {
			t.Errorf("help missing %q:\n%s", token, help)
		}
	}
	if strings.Contains(help, "send") || strings.Contains(help, "--text") {
		t.Fatalf("help advertises unsupported cmux direct mutation:\n%s", help)
	}
	if strings.Index(help, "agent list") > strings.Index(help, "agent approve") {
		t.Fatalf("help does not show discovery before mutation:\n%s", help)
	}
}

func TestAgentHelpDoesNotSuggestHerdrMutation(t *testing.T) {
	var stdout bytes.Buffer
	printAgentUsage(&stdout)
	help := stdout.String()
	if strings.Contains(help, "agent answer --backend herdr") {
		t.Fatalf("help suggests unsupported Herdr mutation:\n%s", help)
	}
}
