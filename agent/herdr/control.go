package herdr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Controller directly inspects existing named herdr agents without creating
// tabs, panes, or agent processes.
type Controller struct {
	client *client
}

type herdrControlTargetState struct {
	target     core.AgentControlTarget
	readHandle string
}

func NewController(opts map[string]any) (core.AgentController, error) {
	socketPath, _ := opts["socket_path"].(string)
	if socketPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("herdr: resolve home dir for default socket_path: %w", err)
		}
		socketPath = filepath.Join(home, ".config", "herdr", "herdr.sock")
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("herdr: socket not found at %s (is `herdr` running?): %w", socketPath, err)
	}

	c := newClient(socketPath)
	rpcTimeoutMs := intOpt(opts["rpc_timeout_ms"], int(defaultRPCTimeout/time.Millisecond))
	if rpcTimeoutMs < 1000 {
		rpcTimeoutMs = 1000
	}
	c.rpcTimeout = time.Duration(rpcTimeoutMs) * time.Millisecond
	return &Controller{client: c}, nil
}

func (c *Controller) ListAgents(ctx context.Context) ([]core.AgentControlTarget, error) {
	states, err := c.listTargetStates(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]core.AgentControlTarget, len(states))
	for i, state := range states {
		targets[i] = state.target
	}
	return targets, nil
}

func (c *Controller) listTargetStates(ctx context.Context) ([]herdrControlTargetState, error) {
	agents, err := c.client.agentList(ctx)
	if err != nil {
		return nil, fmt.Errorf("herdr: list agents: %w", err)
	}

	nameCounts := make(map[string]int, len(agents))
	identityOwners := make(map[string]int, len(agents)*3)
	for i, agent := range agents {
		nameCounts[agent.Name]++
		for _, identity := range []string{agent.Name, agent.TerminalID, agent.PaneID} {
			if strings.TrimSpace(identity) == "" {
				continue
			}
			owner, exists := identityOwners[identity]
			if !exists {
				identityOwners[identity] = i
			} else if owner != i {
				identityOwners[identity] = -1
			}
		}
	}

	states := make([]herdrControlTargetState, 0, len(agents))
	for i, agent := range agents {
		if strings.TrimSpace(agent.Name) == "" || nameCounts[agent.Name] != 1 {
			continue
		}
		readHandle := ""
		if strings.TrimSpace(agent.TerminalID) != "" && identityOwners[agent.TerminalID] == i {
			readHandle = agent.TerminalID
		} else if strings.TrimSpace(agent.PaneID) != "" && identityOwners[agent.PaneID] == i {
			readHandle = agent.PaneID
		}
		if readHandle == "" {
			continue
		}
		target, ok := herdrControlTarget(c, agent)
		if ok {
			states = append(states, herdrControlTargetState{target: target, readHandle: readHandle})
		}
	}
	return states, nil
}

func (c *Controller) TailAgent(ctx context.Context, ref core.AgentControlTargetRef, lines int) (string, error) {
	state, err := c.validateTarget(ctx, ref)
	if err != nil {
		return "", err
	}
	text, err := c.client.agentRead(ctx, state.readHandle, "recent", lines)
	if err != nil {
		return "", fmt.Errorf("herdr: read agent %q: %w", ref.ID, err)
	}
	return text, nil
}

func (c *Controller) ListAgentRequests(ctx context.Context, ref core.AgentControlTargetRef) ([]core.AgentControlRequest, error) {
	state, err := c.validateTarget(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state.target.Status != "blocked" {
		return nil, fmt.Errorf("%w: herdr agent %q is not blocked", core.ErrAgentControlUnsupported, ref.ID)
	}

	request, err := c.readCanonicalBlockedQuestion(ctx, state)
	if err != nil {
		return nil, err
	}
	return []core.AgentControlRequest{request}, nil
}

func (c *Controller) validateTarget(ctx context.Context, ref core.AgentControlTargetRef) (herdrControlTargetState, error) {
	if err := core.ValidateAgentControlTargetRef(ref); err != nil {
		return herdrControlTargetState{}, err
	}

	states, err := c.listTargetStates(ctx)
	if err != nil {
		return herdrControlTargetState{}, err
	}
	for _, state := range states {
		if state.target.ID == ref.ID && state.target.Revision == ref.Revision {
			return state, nil
		}
	}
	return herdrControlTargetState{}, fmt.Errorf("%w: herdr agent %q is missing or changed", core.ErrAgentControlTargetStale, ref.ID)
}

func (c *Controller) readCanonicalBlockedQuestion(ctx context.Context, state herdrControlTargetState) (core.AgentControlRequest, error) {
	screen, err := c.client.agentRead(ctx, state.readHandle, "visible", 0)
	if err != nil {
		return core.AgentControlRequest{}, fmt.Errorf("herdr: read blocked agent %q: %w", state.target.ID, err)
	}
	tail := clipBlockedTail(screen)
	question := core.UserQuestion{Question: tail, Options: parseNumberedMenu(tail)}
	id, revision := blockedRequestTokens(state.target.Revision, screen)
	return core.AgentControlRequest{
		ID:       id,
		Revision: revision,
		Kind:     core.AgentControlRequestQuestion,
		ToolName: "AskUserQuestion",
		Input:    map[string]any{"screen": tail},
		Questions: []core.UserQuestion{
			question,
		},
	}, nil
}

func herdrControlTarget(controller core.AgentController, agent agentInfo) (core.AgentControlTarget, bool) {
	if strings.TrimSpace(agent.Name) == "" {
		return core.AgentControlTarget{}, false
	}
	kind := strings.TrimSpace(agent.Agent)
	if kind == "" {
		kind = "unknown"
	}
	candidates := []core.AgentControlCapability{core.AgentControlCapabilityTail}
	if agent.AgentStatus == "blocked" {
		candidates = append(candidates, core.AgentControlCapabilityRequests)
	}
	capabilities := core.AgentControlSupportedCapabilities(controller, candidates...)
	return core.AgentControlTarget{
		ID:           agent.Name,
		Revision:     targetRevision(agent),
		Backend:      "herdr",
		Kind:         kind,
		Directory:    agent.CWD,
		Status:       agent.AgentStatus,
		Capabilities: capabilities,
	}, true
}

func targetRevision(agent agentInfo) string {
	return hashCanonicalFields(
		agent.Name,
		agent.TerminalID,
		agent.PaneID,
		agent.WorkspaceID,
		agent.Agent,
		agent.AgentStatus,
		agent.CWD,
	)
}

func blockedRequestTokens(targetRevision, visibleScreen string) (string, string) {
	token := hashCanonicalFields(targetRevision, visibleScreen)
	return token, token
}

func hashCanonicalFields(fields ...string) string {
	payload, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

var (
	_ core.AgentController           = (*Controller)(nil)
	_ core.AgentControlTailer        = (*Controller)(nil)
	_ core.AgentControlRequestLister = (*Controller)(nil)
)
