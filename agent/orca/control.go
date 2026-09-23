// Package orca controls existing Orca terminals without creating worktrees or agents.
package orca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/chenhg5/cc-connect/core"
)

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Controller discovers currently running Orca terminals. It never creates a
// worktree or terminal. Orca handles are runtime-scoped, so every operation
// re-discovers topology and rejects a changed target revision.
type Controller struct {
	runner commandRunner
}

type worktreeResponse struct {
	Result struct {
		Worktrees         []orcaWorktree   `json:"worktrees"`
		TopologyRevisions map[string]int64 `json:"topologyRevisions"`
	} `json:"result"`
}

type orcaWorktree struct {
	ID         string      `json:"worktreeId"`
	InstanceID string      `json:"worktreeInstanceId"`
	Path       string      `json:"path"`
	Status     string      `json:"status"`
	Agents     []orcaAgent `json:"agents"`
}

type orcaAgent struct {
	Type  string `json:"agentType"`
	State string `json:"state"`
}

type terminalResponse struct {
	Result struct {
		Terminals []orcaTerminal `json:"terminals"`
	} `json:"result"`
}

type orcaTerminal struct {
	Handle     string `json:"handle"`
	WorktreeID string `json:"worktreeId"`
	Worktree   string `json:"worktreePath"`
	Title      string `json:"title"`
	Connected  bool   `json:"connected"`
	Writable   bool   `json:"writable"`
	Preview    string `json:"preview"`
}

type terminalReadResponse struct {
	Result struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	} `json:"result"`
}

type targetState struct {
	target core.AgentControlTarget
	handle string
}

var (
	_ core.AgentController      = (*Controller)(nil)
	_ core.AgentControlTailer   = (*Controller)(nil)
	_ core.AgentControlPrompter = (*Controller)(nil)
)

func init() {
	core.RegisterAgentController("orca", NewController)
}

func NewController(_ map[string]any) (core.AgentController, error) {
	if _, err := exec.LookPath("orca"); err != nil {
		return nil, fmt.Errorf("orca: command not found: %w", err)
	}
	return &Controller{runner: execRunner{}}, nil
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
	output, err := c.runner.Run(ctx, "orca", "terminal", "read", "--terminal", state.handle, "--json")
	if err != nil {
		return "", fmt.Errorf("orca: read terminal %q: %w", state.handle, err)
	}
	var response terminalReadResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return "", fmt.Errorf("orca: decode terminal read: %w", err)
	}
	tail := response.Result.Terminal.Tail
	if lines > 0 && len(tail) > lines {
		tail = tail[len(tail)-lines:]
	}
	return strings.Join(tail, "\n"), nil
}

func (c *Controller) SendAgentPrompt(ctx context.Context, ref core.AgentControlTargetRef, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("orca: send prompt: %w", core.ErrAgentControlInvalidAnswer)
	}
	state, err := c.validateTarget(ctx, ref, core.AgentControlCapabilityPrompt)
	if err != nil {
		return err
	}
	if _, err := c.runner.Run(ctx, "orca", "terminal", "send", "--terminal", state.handle, "--text", message, "--enter", "--json"); err != nil {
		return fmt.Errorf("orca: send terminal %q: %w", state.handle, err)
	}
	return nil
}

func (c *Controller) listTargetStates(ctx context.Context) ([]targetState, error) {
	output, err := c.runner.Run(ctx, "orca", "worktree", "ps", "--json")
	if err != nil {
		return nil, fmt.Errorf("orca: list worktrees: %w", err)
	}
	var worktrees worktreeResponse
	if err := json.Unmarshal(output, &worktrees); err != nil {
		return nil, fmt.Errorf("orca: decode worktrees: %w", err)
	}
	byID := make(map[string]orcaWorktree, len(worktrees.Result.Worktrees))
	for _, worktree := range worktrees.Result.Worktrees {
		if worktree.ID != "" {
			byID[worktree.ID] = worktree
		}
	}

	worktreeIDs := make([]string, 0, len(byID))
	for worktreeID := range byID {
		worktreeIDs = append(worktreeIDs, worktreeID)
	}
	sort.Strings(worktreeIDs)
	terminalsByWorktree, err := c.listTerminalsForWorktrees(ctx, worktreeIDs)
	if err != nil {
		return nil, err
	}

	states := make([]targetState, 0, len(worktreeIDs))
	for i, worktreeID := range worktreeIDs {
		worktree := byID[worktreeID]
		for _, terminal := range terminalsByWorktree[i] {
			if terminal.Handle == "" || terminal.WorktreeID != worktreeID || !terminal.Connected {
				continue
			}
			kind := "terminal"
			status := worktree.Status
			if len(worktree.Agents) == 1 && worktree.Agents[0].Type != "" {
				kind = worktree.Agents[0].Type
				if worktree.Agents[0].State != "" {
					status = worktree.Agents[0].State
				}
			}
			id := worktreeID + "/" + terminal.Handle
			target := core.AgentControlTarget{
				ID:        id,
				Revision:  orcaRevision(worktree, worktrees.Result.TopologyRevisions[worktreeID], terminal),
				Backend:   "orca",
				Kind:      kind,
				Title:     terminal.Title,
				Directory: terminal.Worktree,
				Status:    status,
			}
			target.Capabilities = core.AgentControlSupportedCapabilities(c, core.AgentControlCapabilityTail, core.AgentControlCapabilityPrompt)
			states = append(states, targetState{target: target, handle: terminal.Handle})
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].target.ID < states[j].target.ID })
	return states, nil
}

// maxParallelTerminalListings bounds concurrent `orca terminal list` calls.
// Orca exposes no bulk terminal query, so discovery costs one subprocess per
// worktree; walking dozens of worktrees serially makes a chat command take
// tens of seconds. Fan out, but never spawn unbounded processes.
const maxParallelTerminalListings = 8

// listTerminalsForWorktrees returns each worktree's terminals positionally.
// Discovery stays fail-closed: a single failed worktree aborts the listing,
// because a silently short list would renumber targets and could point an
// index-based selection at a different agent.
func (c *Controller) listTerminalsForWorktrees(ctx context.Context, worktreeIDs []string) ([][]orcaTerminal, error) {
	terminals := make([][]orcaTerminal, len(worktreeIDs))
	errs := make([]error, len(worktreeIDs))
	slots := make(chan struct{}, maxParallelTerminalListings)
	var wg sync.WaitGroup
	for i, worktreeID := range worktreeIDs {
		wg.Add(1)
		go func(index int, worktreeID string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			terminals[index], errs[index] = c.listTerminals(ctx, worktreeID)
		}(i, worktreeID)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return terminals, nil
}

func (c *Controller) listTerminals(ctx context.Context, worktreeID string) ([]orcaTerminal, error) {
	output, err := c.runner.Run(ctx, "orca", "terminal", "list", "--worktree", "id:"+worktreeID, "--json")
	if err != nil {
		return nil, fmt.Errorf("orca: list terminals for %q: %w", worktreeID, err)
	}
	var response terminalResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("orca: decode terminals for %q: %w", worktreeID, err)
	}
	return response.Result.Terminals, nil
}

func (c *Controller) validateTarget(ctx context.Context, ref core.AgentControlTargetRef, capability core.AgentControlCapability) (targetState, error) {
	if err := core.ValidateAgentControlTargetRef(ref); err != nil {
		return targetState{}, fmt.Errorf("orca: validate target: %w", err)
	}
	states, err := c.listTargetStates(ctx)
	if err != nil {
		return targetState{}, err
	}
	for _, state := range states {
		if state.target.ID != ref.ID {
			continue
		}
		if state.target.Revision != ref.Revision {
			return targetState{}, fmt.Errorf("orca: target %q changed: %w", ref.ID, core.ErrAgentControlTargetStale)
		}
		if !state.target.Supports(capability) {
			return targetState{}, fmt.Errorf("orca: target %q does not support %s: %w", ref.ID, capability, core.ErrAgentControlUnsupported)
		}
		return state, nil
	}
	return targetState{}, fmt.Errorf("orca: target %q missing: %w", ref.ID, core.ErrAgentControlTargetStale)
}

func orcaRevision(worktree orcaWorktree, topologyRevision int64, terminal orcaTerminal) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{worktree.ID, worktree.InstanceID, fmt.Sprint(topologyRevision), terminal.Handle, terminal.Worktree, fmt.Sprint(terminal.Connected), fmt.Sprint(terminal.Writable)}, "\x00")))
	return hex.EncodeToString(sum[:])
}
