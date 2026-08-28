package cmux

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/agent/internal/termdiff"
	"github.com/chenhg5/cc-connect/core"
)

var cmuxPermissionDecisions = []string{"once", "always", "all", "bypass", "deny"}

func timeAgo(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

type controllerSocketResolver func(context.Context, string, string) (string, error)

type Controller struct {
	client *client
	mapper *sessionMapper
}

type controlTargetState struct {
	target       core.AgentControlTarget
	surfaceID    string
	workstreamID string
}

type controlRequestState struct {
	request core.AgentControlRequest
	item    feedItem
}

var (
	_ core.AgentController                 = (*Controller)(nil)
	_ core.AgentControlTailer              = (*Controller)(nil)
	_ core.AgentControlKeySender           = (*Controller)(nil)
	_ core.AgentControlRequestLister       = (*Controller)(nil)
	_ core.AgentControlPermissionResponder = (*Controller)(nil)
	_ core.AgentControlQuestionResponder   = (*Controller)(nil)
)

func init() {
	core.RegisterAgentController("cmux", NewController)
}

func NewController(opts map[string]any) (core.AgentController, error) {
	return newControllerWithResolver(opts, resolveControllerSocket)
}

func newControllerWithResolver(opts map[string]any, resolver controllerSocketResolver) (*Controller, error) {
	password, _ := opts["password"].(string)
	explicitSocket, _ := opts["socket_path"].(string)
	probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socketPath, err := resolver(probeCtx, explicitSocket, password)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cmux: resolve home directory: %w", err)
	}
	rpcTimeoutMs := intOpt(opts["rpc_timeout_ms"], 10000)
	if rpcTimeoutMs < 1000 {
		rpcTimeoutMs = 1000
	}
	c := newClient(socketPath, password)
	c.rpcTimeout = time.Duration(rpcTimeoutMs) * time.Millisecond
	return &Controller{client: c, mapper: newSessionMapper(filepath.Join(home, ".cmuxterm"))}, nil
}

func (c *Controller) ListAgents(ctx context.Context) ([]core.AgentControlTarget, error) {
	states, err := c.listTargetStates(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]core.AgentControlTarget, 0, len(states))
	for _, state := range states {
		targets = append(targets, state.target)
	}
	return targets, nil
}

func (c *Controller) TailAgent(ctx context.Context, ref core.AgentControlTargetRef, lines int) (string, error) {
	state, err := c.validateTarget(ctx, ref, core.AgentControlCapabilityTail)
	if err != nil {
		return "", err
	}
	text, err := c.client.readScreen(ctx, state.target.ID, state.surfaceID)
	if err != nil {
		return "", fmt.Errorf("cmux: tail target %q: %w", state.target.ID, err)
	}
	return controlTail(stripControlCharacters(termdiff.Normalize(text)), lines), nil
}

func (c *Controller) SendAgentKey(ctx context.Context, ref core.AgentControlTargetRef, key string) error {
	state, err := c.validateTarget(ctx, ref, core.AgentControlCapabilityKey)
	if err != nil {
		return err
	}
	if err := c.client.sendKey(ctx, state.target.ID, state.surfaceID, key); err != nil {
		return fmt.Errorf("cmux: send key to %q: %w", state.target.ID, err)
	}
	return nil
}

func (c *Controller) ListAgentRequests(ctx context.Context, ref core.AgentControlTargetRef) ([]core.AgentControlRequest, error) {
	state, err := c.validateTarget(ctx, ref, core.AgentControlCapabilityRequests)
	if err != nil {
		return nil, err
	}
	items, err := c.client.feedList(ctx)
	if err != nil {
		return nil, fmt.Errorf("cmux: list requests for %q: %w", state.target.ID, err)
	}
	states := c.requestStates(state, items)
	requests := make([]core.AgentControlRequest, 0, len(states))
	for _, requestState := range states {
		requests = append(requests, requestState.request)
	}
	return requests, nil
}

func (c *Controller) RespondAgentPermission(ctx context.Context, targetRef core.AgentControlTargetRef, requestRef core.AgentControlRequestRef, decision string) error {
	if err := core.ValidateAgentControlTargetRef(targetRef); err != nil {
		return fmt.Errorf("cmux: permission target: %w", err)
	}
	if err := core.ValidateAgentControlRequestRef(requestRef); err != nil {
		return fmt.Errorf("cmux: permission request: %w", err)
	}
	state, err := c.validateTarget(ctx, targetRef, core.AgentControlCapabilityPermission)
	if err != nil {
		return err
	}
	requestState, err := c.reloadRequest(ctx, state, requestRef, core.AgentControlRequestPermission)
	if err != nil {
		return err
	}
	if !controlDecisionAllowed(requestState.request.AllowedDecisions, decision) {
		return fmt.Errorf("cmux: permission decision %q: %w", decision, core.ErrAgentControlInvalidAnswer)
	}
	if err := c.client.feedPermissionReply(ctx, requestState.item.RequestID, decision); err != nil {
		return fmt.Errorf("cmux: reply to permission %q: %w", requestState.item.RequestID, err)
	}
	return nil
}

func (c *Controller) AnswerAgentQuestions(ctx context.Context, targetRef core.AgentControlTargetRef, requestRef core.AgentControlRequestRef, answers []core.AgentControlQuestionAnswer) error {
	if err := core.ValidateAgentControlTargetRef(targetRef); err != nil {
		return fmt.Errorf("cmux: question target: %w", err)
	}
	if err := core.ValidateAgentControlRequestRef(requestRef); err != nil {
		return fmt.Errorf("cmux: question request: %w", err)
	}
	state, err := c.validateTarget(ctx, targetRef, core.AgentControlCapabilityQuestion)
	if err != nil {
		return err
	}
	requestState, err := c.reloadRequest(ctx, state, requestRef, core.AgentControlRequestQuestion)
	if err != nil {
		return err
	}
	if err := core.ValidateAgentControlQuestionAnswers(requestState.request.Questions, answers); err != nil {
		return fmt.Errorf("cmux: question answers: %w", err)
	}
	for _, answer := range answers {
		if answer.Text != "" {
			return fmt.Errorf("cmux: question answers: free-text answers have no proven wire encoding: %w", core.ErrAgentControlInvalidAnswer)
		}
		if len(answer.Selections) > 1 {
			return fmt.Errorf("cmux: question answers: multiple selections have no proven wire encoding: %w", core.ErrAgentControlInvalidAnswer)
		}
	}
	selections := make([]string, len(requestState.request.Questions))
	for _, answer := range answers {
		selections[answer.Index] = answer.Selections[0]
	}
	if err := c.client.feedQuestionReply(ctx, requestState.item.RequestID, selections); err != nil {
		return fmt.Errorf("cmux: answer question %q: %w", requestState.item.RequestID, err)
	}
	return nil
}

func (c *Controller) listTargetStates(ctx context.Context) ([]controlTargetState, error) {
	workspaces, err := c.client.listWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("cmux: list control targets: %w", err)
	}
	hooks := c.mapper.controlSnapshot()
	workspaceCounts := make(map[string]int, len(workspaces))
	surfaceCounts := make(map[string]int, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.ID != "" {
			workspaceCounts[workspace.ID]++
		}
		if workspace.SurfaceID != "" {
			surfaceCounts[workspace.SurfaceID]++
		}
	}
	states := make([]controlTargetState, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.ID == "" || workspaceCounts[workspace.ID] != 1 {
			continue
		}
		surfaceID := ""
		if workspace.SurfaceID != "" && surfaceCounts[workspace.SurfaceID] == 1 {
			surfaceID = workspace.SurfaceID
		}
		hook, hasHook := hooks.byWorkspace[workspace.ID]
		if hasHook && workspace.SurfaceID != "" && workspace.SurfaceID != hook.SurfaceID {
			hook = controlHookIdentity{}
			hasHook = false
		}
		kind := "unknown"
		if hasHook {
			kind = hook.Source
		}
		candidates := make([]core.AgentControlCapability, 0, 5)
		if surfaceID != "" {
			candidates = append(candidates,
				core.AgentControlCapabilityTail,
				core.AgentControlCapabilityKey,
			)
		}
		if hasHook {
			candidates = append(candidates,
				core.AgentControlCapabilityRequests,
				core.AgentControlCapabilityPermission,
				core.AgentControlCapabilityQuestion,
			)
		}
		target := core.AgentControlTarget{
			ID:        workspace.ID,
			Revision:  controlHash("cmux-target-v2", workspace.ID, surfaceID, workspace.CWD, hook.Source, hook.WorkstreamID, hook.SurfaceID, hook.Lifecycle),
			Backend:   "cmux",
			Kind:      kind,
			Title:     workspace.stableName(),
			Directory: workspace.CWD,
			Status:    hook.Lifecycle,
			Description: strings.TrimSpace(workspace.Command + " · " + timeAgo(workspace.UpdatedAt)),
		}
		target.Capabilities = core.AgentControlSupportedCapabilities(c, candidates...)
		states = append(states, controlTargetState{target: target, surfaceID: surfaceID, workstreamID: hook.WorkstreamID})
	}
	// Deterministic order keeps the chat listing's numbering stable between
	// two refreshes that see the same workspaces.
	sort.Slice(states, func(i, j int) bool { return states[i].target.ID < states[j].target.ID })
	return states, nil
}

func (c *Controller) validateTarget(ctx context.Context, ref core.AgentControlTargetRef, capability core.AgentControlCapability) (controlTargetState, error) {
	if err := core.ValidateAgentControlTargetRef(ref); err != nil {
		return controlTargetState{}, fmt.Errorf("cmux: validate target: %w", err)
	}
	states, err := c.listTargetStates(ctx)
	if err != nil {
		return controlTargetState{}, err
	}
	for _, state := range states {
		if state.target.ID != ref.ID {
			continue
		}
		if state.target.Revision != ref.Revision {
			return controlTargetState{}, fmt.Errorf("cmux: target %q revision changed: %w", ref.ID, core.ErrAgentControlTargetStale)
		}
		if !state.target.Supports(capability) {
			return controlTargetState{}, fmt.Errorf("cmux: target %q does not support %s: %w", ref.ID, capability, core.ErrAgentControlUnsupported)
		}
		return state, nil
	}
	return controlTargetState{}, fmt.Errorf("cmux: target %q missing: %w", ref.ID, core.ErrAgentControlTargetStale)
}

func (c *Controller) requestStates(target controlTargetState, items []feedItem) []controlRequestState {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		if item.Status == "pending" && item.RequestID != "" {
			counts[item.RequestID]++
		}
	}
	states := make([]controlRequestState, 0, len(items))
	for _, item := range items {
		if counts[item.RequestID] != 1 {
			continue
		}
		requestState, ok := c.requestState(target, item)
		if ok {
			states = append(states, requestState)
		}
	}
	return states
}

func (c *Controller) requestState(target controlTargetState, item feedItem) (controlRequestState, bool) {
	if item.Kind != "permissionRequest" || item.Status != "pending" || item.RequestID == "" || item.WorkstreamID == "" {
		return controlRequestState{}, false
	}
	if target.workstreamID == "" || item.WorkstreamID != target.workstreamID || item.ToolName == "ExitPlanMode" {
		return controlRequestState{}, false
	}
	input := make(map[string]any)
	if item.ToolInput != "" {
		if err := json.Unmarshal([]byte(item.ToolInput), &input); err != nil || input == nil {
			return controlRequestState{}, false
		}
	}
	kind := core.AgentControlRequestPermission
	var questions []core.UserQuestion
	var allowedDecisions []string
	if item.ToolName == "AskUserQuestion" {
		kind = core.AgentControlRequestQuestion
		var ok bool
		questions, ok = parseBridgeQuestions(input)
		if !ok {
			return controlRequestState{}, false
		}
	} else {
		allowedDecisions = append([]string(nil), cmuxPermissionDecisions...)
	}
	multiSelect := "false"
	if item.QuestionMultiSelect {
		multiSelect = "true"
	}
	request := core.AgentControlRequest{
		ID: item.RequestID,
		Revision: controlHash(
			"cmux-request-v1",
			target.target.Revision,
			item.RequestID,
			item.ID,
			item.Source,
			item.WorkstreamID,
			item.SessionID,
			item.CWD,
			item.Kind,
			item.Status,
			item.ToolName,
			item.ToolInput,
			string(item.Questions),
			string(item.QuestionOptions),
			multiSelect,
		),
		Kind:             kind,
		ToolName:         item.ToolName,
		Input:            input,
		Questions:        questions,
		AllowedDecisions: allowedDecisions,
	}
	return controlRequestState{request: request, item: item}, true
}

func (c *Controller) reloadRequest(ctx context.Context, target controlTargetState, ref core.AgentControlRequestRef, kind core.AgentControlRequestKind) (controlRequestState, error) {
	items, err := c.client.feedList(ctx)
	if err != nil {
		return controlRequestState{}, fmt.Errorf("cmux: reload request %q: %w", ref.ID, err)
	}
	states := c.requestStates(target, items)
	for _, state := range states {
		if state.request.ID != ref.ID {
			continue
		}
		if state.request.Revision != ref.Revision || state.request.Kind != kind {
			return controlRequestState{}, fmt.Errorf("cmux: request %q changed: %w", ref.ID, core.ErrAgentControlRequestStale)
		}
		return state, nil
	}
	return controlRequestState{}, fmt.Errorf("cmux: request %q missing: %w", ref.ID, core.ErrAgentControlRequestStale)
}

func controlDecisionAllowed(allowed []string, decision string) bool {
	for _, candidate := range allowed {
		if decision == candidate {
			return true
		}
	}
	return false
}

func stripControlCharacters(text string) string {
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' {
			return character
		}
		if character < ' ' || character >= '\u007f' && character <= '\u009f' {
			return -1
		}
		return character
	}, text)
}

func controlTail(text string, lines int) string {
	if lines <= 0 || text == "" {
		return text
	}
	trailingNewline := strings.HasSuffix(text, "\n")
	parts := strings.Split(text, "\n")
	if trailingNewline {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	text = strings.Join(parts, "\n")
	if trailingNewline {
		text += "\n"
	}
	return text
}

func controlHash(parts ...string) string {
	hasher := sha256.New()
	for _, part := range parts {
		writeControlHashPart(hasher, part)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeControlHashPart(hasher hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write([]byte(value))
}
