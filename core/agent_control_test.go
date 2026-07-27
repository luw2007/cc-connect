package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// testControllerSeq makes registered test controller names unique for the
// lifetime of the test process, so repeated executions (e.g. `go test
// -count=N`) never collide with a name registered by an earlier run.
var testControllerSeq atomic.Uint64

func uniqueTestControllerName(base string) string {
	return fmt.Sprintf("%s-%d", base, testControllerSeq.Add(1))
}

type listOnlyAgentController struct{}

func (listOnlyAgentController) ListAgents(context.Context) ([]AgentControlTarget, error) {
	return []AgentControlTarget{{
		ID:       "target-1",
		Revision: "instance-1",
		Backend:  "test-controller",
	}}, nil
}

type tailingAgentController struct{ listOnlyAgentController }

func (tailingAgentController) TailAgent(context.Context, AgentControlTargetRef, int) (string, error) {
	return "tail", nil
}

func TestAgentControllerIsListOnly(t *testing.T) {
	var controller AgentController = listOnlyAgentController{}
	if _, ok := controller.(AgentControlTailer); ok {
		t.Fatal("list-only controller unexpectedly advertises terminal tail support")
	}
	if _, ok := any(tailingAgentController{}).(AgentControlTailer); !ok {
		t.Fatal("tailing controller does not satisfy optional tail capability")
	}
}

func TestAgentControlRefsAreOpaqueRequiredIdentity(t *testing.T) {
	target := AgentControlTarget{ID: "target-1", Revision: "instance-1", Backend: "test"}
	if got, want := target.Ref(), (AgentControlTargetRef{ID: "target-1", Revision: "instance-1"}); got != want {
		t.Fatalf("target.Ref() = %#v, want %#v", got, want)
	}
	if !target.Ref().Valid() {
		t.Fatal("complete target ref is invalid")
	}
	for _, ref := range []AgentControlTargetRef{{}, {ID: "target-1"}, {Revision: "instance-1"}, {ID: " ", Revision: "instance-1"}} {
		if ref.Valid() {
			t.Fatalf("incomplete target ref is valid: %#v", ref)
		}
	}

	request := AgentControlRequest{
		ID:               "request-1",
		Revision:         "pending-1",
		AllowedDecisions: []string{"allow"},
	}
	if got, want := request.Ref(), (AgentControlRequestRef{ID: "request-1", Revision: "pending-1"}); got != want {
		t.Fatalf("request.Ref() = %#v, want %#v", got, want)
	}
	if !request.Ref().Valid() {
		t.Fatal("complete request ref is invalid")
	}
	request.AllowedDecisions[0] = "forged"
	if got := request.Ref(); got != (AgentControlRequestRef{ID: "request-1", Revision: "pending-1"}) {
		t.Fatalf("request ref depends on mutable display metadata: %#v", got)
	}
	for _, ref := range []AgentControlRequestRef{{}, {ID: "request-1"}, {Revision: "pending-1"}, {ID: " ", Revision: "pending-1"}} {
		if ref.Valid() {
			t.Fatalf("incomplete request ref is valid: %#v", ref)
		}
	}
}

func TestAgentControlReferenceSentinels(t *testing.T) {
	if err := ValidateAgentControlTargetRef(AgentControlTargetRef{}); !errors.Is(err, ErrAgentControlTargetStale) {
		t.Fatalf("invalid target ref error = %v, want ErrAgentControlTargetStale", err)
	}
	if err := ValidateAgentControlRequestRef(AgentControlRequestRef{}); !errors.Is(err, ErrAgentControlRequestStale) {
		t.Fatalf("invalid request ref error = %v, want ErrAgentControlRequestStale", err)
	}
	if err := ValidateAgentControlTargetRef(AgentControlTargetRef{ID: "target", Revision: "v1"}); err != nil {
		t.Fatalf("valid target ref: %v", err)
	}
	if err := ValidateAgentControlRequestRef(AgentControlRequestRef{ID: "request", Revision: "v1"}); err != nil {
		t.Fatalf("valid request ref: %v", err)
	}
}

func TestAgentControlQuestionAnswerValid(t *testing.T) {
	valid := []AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"one"}},
		{Index: 1, Selections: []string{"one", "two"}},
		{Index: 2, Text: "free text"},
	}
	for _, answer := range valid {
		if !answer.Valid() {
			t.Fatalf("valid answer rejected: %#v", answer)
		}
	}
	invalid := []AgentControlQuestionAnswer{
		{Index: -1, Selections: []string{"one"}},
		{Index: 0},
		{Index: 0, Selections: []string{"one"}, Text: "also text"},
		{Index: 0, Text: "  "},
		{Index: 0, Selections: []string{"  "}},
	}
	for _, answer := range invalid {
		if answer.Valid() {
			t.Fatalf("invalid answer accepted: %#v", answer)
		}
	}
}

func TestAgentControlTargetCapabilitiesAreExplicit(t *testing.T) {
	target := AgentControlTarget{Capabilities: []AgentControlCapability{AgentControlCapabilityTail, AgentControlCapabilityRequests}}
	if !target.Supports(AgentControlCapabilityTail) || !target.Supports(AgentControlCapabilityRequests) {
		t.Fatalf("target capabilities not found: %#v", target)
	}
	if target.Supports(AgentControlCapabilityPermission) || target.Supports(AgentControlCapabilityQuestion) {
		t.Fatalf("target advertises unsupported responder capability: %#v", target)
	}
}

type fullAgentController struct{ listOnlyAgentController }

func (fullAgentController) TailAgent(context.Context, AgentControlTargetRef, int) (string, error) {
	return "", nil
}

func (fullAgentController) SendAgentPrompt(context.Context, AgentControlTargetRef, string) error {
	return nil
}

func (fullAgentController) SendAgentKey(context.Context, AgentControlTargetRef, string) error {
	return nil
}

func (fullAgentController) ListAgentRequests(context.Context, AgentControlTargetRef) ([]AgentControlRequest, error) {
	return nil, nil
}

func (fullAgentController) RespondAgentPermission(context.Context, AgentControlTargetRef, AgentControlRequestRef, string) error {
	return nil
}

func (fullAgentController) AnswerAgentQuestions(context.Context, AgentControlTargetRef, AgentControlRequestRef, []AgentControlQuestionAnswer) error {
	return nil
}

func TestAgentControlSupportedCapabilitiesMatchInterfaces(t *testing.T) {
	listOnly := listOnlyAgentController{}
	if got := AgentControlSupportedCapabilities(listOnly, AgentControlCapabilityTail, AgentControlCapabilityRequests); len(got) != 0 {
		t.Fatalf("list-only capabilities = %#v, want none", got)
	}
	all := fullAgentController{}
	want := []AgentControlCapability{
		AgentControlCapabilityTail,
		AgentControlCapabilityPrompt,
		AgentControlCapabilityKey,
		AgentControlCapabilityRequests,
		AgentControlCapabilityPermission,
		AgentControlCapabilityQuestion,
	}
	got := AgentControlSupportedCapabilities(all,
		AgentControlCapabilityTail,
		AgentControlCapabilityPrompt,
		AgentControlCapabilityKey,
		AgentControlCapabilityRequests,
		AgentControlCapabilityPermission,
		AgentControlCapabilityQuestion,
		AgentControlCapabilityQuestion,
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supported capabilities = %#v, want %#v", got, want)
	}
}

func TestValidateAgentControlQuestionAnswers(t *testing.T) {
	questions := []UserQuestion{
		{Question: "database", Options: []UserQuestionOption{{Label: "sqlite"}, {Label: "postgres"}}},
		{Question: "regions", MultiSelect: true, Options: []UserQuestionOption{{Label: "us"}, {Label: "eu"}}},
		{Question: "reason"},
	}
	valid := []AgentControlQuestionAnswer{
		{Index: 0, Selections: []string{"postgres"}},
		{Index: 1, Selections: []string{"us", "eu"}},
		{Index: 2, Text: "because tests"},
	}
	if err := ValidateAgentControlQuestionAnswers(questions, valid); err != nil {
		t.Fatalf("ValidateAgentControlQuestionAnswers(valid): %v", err)
	}
	invalid := [][]AgentControlQuestionAnswer{
		valid[:2],
		{{Index: 0, Selections: []string{"sqlite"}}, {Index: 0, Selections: []string{"us"}}, {Index: 2, Text: "x"}},
		{{Index: 0, Selections: []string{"missing"}}, {Index: 1, Selections: []string{"us"}}, {Index: 2, Text: "x"}},
		{{Index: 0, Selections: []string{"sqlite", "postgres"}}, {Index: 1, Selections: []string{"us"}}, {Index: 2, Text: "x"}},
		{{Index: 0, Selections: []string{"sqlite"}}, {Index: 1, Selections: []string{"us", "us"}}, {Index: 2, Text: "x"}},
		{{Index: 0, Selections: []string{"sqlite"}}, {Index: 1, Text: "not an option"}, {Index: 2, Text: "x"}},
		{{Index: 0, Selections: []string{"sqlite"}}, {Index: 3, Text: "out of range"}, {Index: 2, Text: "x"}},
	}
	for _, answers := range invalid {
		if err := ValidateAgentControlQuestionAnswers(questions, answers); !errors.Is(err, ErrAgentControlInvalidAnswer) {
			t.Fatalf("ValidateAgentControlQuestionAnswers(%#v) = %v, want ErrAgentControlInvalidAnswer", answers, err)
		}
	}
}

func TestRegisterAndCreateAgentController(t *testing.T) {
	name := uniqueTestControllerName("test-controller-options")
	wantOpts := map[string]any{"socket_path": "/tmp/controller.sock"}
	var gotOpts map[string]any
	RegisterAgentController(name, func(opts map[string]any) (AgentController, error) {
		gotOpts = opts
		return listOnlyAgentController{}, nil
	})

	controller, err := CreateAgentController(name, wantOpts)
	if err != nil {
		t.Fatalf("CreateAgentController: %v", err)
	}
	if _, ok := controller.(listOnlyAgentController); !ok {
		t.Fatalf("controller = %T, want listOnlyAgentController", controller)
	}
	if !reflect.DeepEqual(gotOpts, wantOpts) {
		t.Fatalf("factory options = %#v, want %#v", gotOpts, wantOpts)
	}
}

func TestCreateAgentControllerPropagatesFactoryError(t *testing.T) {
	want := errors.New("controller unavailable")
	name := uniqueTestControllerName("test-controller-error")
	RegisterAgentController(name, func(map[string]any) (AgentController, error) {
		return nil, want
	})
	if _, err := CreateAgentController(name, nil); !errors.Is(err, want) {
		t.Fatalf("CreateAgentController error = %v, want %v", err, want)
	}
}

func TestListRegisteredAgentControllersIsSorted(t *testing.T) {
	RegisterAgentController(uniqueTestControllerName("test-controller-zeta"), func(map[string]any) (AgentController, error) {
		return listOnlyAgentController{}, nil
	})
	RegisterAgentController(uniqueTestControllerName("test-controller-alpha"), func(map[string]any) (AgentController, error) {
		return listOnlyAgentController{}, nil
	})
	names := ListRegisteredAgentControllers()
	for index := 1; index < len(names); index++ {
		if names[index-1] > names[index] {
			t.Fatalf("controller names are not sorted: %v", names)
		}
	}
}

func TestRegisterAgentControllerRejectsInvalidFactories(t *testing.T) {
	t.Run("empty name", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("empty controller registration did not panic")
			}
		}()
		RegisterAgentController("", func(map[string]any) (AgentController, error) {
			return listOnlyAgentController{}, nil
		})
	})
	t.Run("whitespace name", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("whitespace controller registration did not panic")
			}
		}()
		RegisterAgentController(" \t ", func(map[string]any) (AgentController, error) {
			return listOnlyAgentController{}, nil
		})
	})
	t.Run("nil", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("nil controller registration did not panic")
			}
		}()
		RegisterAgentController(uniqueTestControllerName("test-controller-nil"), nil)
	})
	t.Run("duplicate", func(t *testing.T) {
		name := uniqueTestControllerName("test-controller-duplicate")
		RegisterAgentController(name, func(map[string]any) (AgentController, error) {
			return listOnlyAgentController{}, nil
		})
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate controller registration did not panic")
			}
		}()
		RegisterAgentController(name, func(map[string]any) (AgentController, error) {
			return listOnlyAgentController{}, nil
		})
	})
}

func TestAgentControllerRegistryConcurrentAccess(t *testing.T) {
	stableName := uniqueTestControllerName("test-controller-concurrent-stable")
	RegisterAgentController(stableName, func(map[string]any) (AgentController, error) {
		return listOnlyAgentController{}, nil
	})

	concurrentNamePrefix := uniqueTestControllerName("test-controller-concurrent")
	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var wg sync.WaitGroup
	for index := range workers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			if _, err := CreateAgentController(stableName, nil); err != nil {
				errs <- fmt.Errorf("create stable controller: %w", err)
			}
			if len(ListRegisteredAgentControllers()) == 0 {
				errs <- errors.New("list registered controllers returned no names")
			}
		}(index)
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("%s-%d", concurrentNamePrefix, index)
			RegisterAgentController(name, func(map[string]any) (AgentController, error) {
				return listOnlyAgentController{}, nil
			})
		}(index)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
func TestCreateAgentControllerUnknown(t *testing.T) {
	if _, err := CreateAgentController("nonexistent-controller", nil); !errors.Is(err, ErrAgentControllerNotRegistered) {
		t.Fatalf("CreateAgentController error = %v, want ErrAgentControllerNotRegistered", err)
	}
}
