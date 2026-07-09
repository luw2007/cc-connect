package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
)

// Wire protocol confirmed against a live herdr socket (v0.36+/0.7+): one JSON
// object per line, newline-delimited. herdr closes the connection after
// answering exactly one request (mirrors how each `herdr <cmd>` CLI
// invocation works — connect, one request, one response, disconnect), so a
// fresh connection is opened per call rather than multiplexed over one
// persistent socket. No auth handshake — the socket file's 0600 permission
// is the trust boundary.

type rpcRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

var requestCounter atomic.Int64

// client is a stateless handle to a herdr Unix socket; each call opens its
// own connection (see wire protocol notes above).
type client struct {
	socketPath string
}

func newClient(socketPath string) *client {
	return &client{socketPath: socketPath}
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("herdr: connect %s: %w", c.socketPath, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	id := fmt.Sprintf("%d", requestCounter.Add(1))
	line, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("herdr: marshal %s request: %w", method, err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		return fmt.Errorf("herdr: write %s request: %w", method, err)
	}

	respLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && respLine == "" {
		return fmt.Errorf("herdr: read %s response: %w", method, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return fmt.Errorf("herdr: malformed %s response: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("herdr: %s: %s", method, resp.Error.Message)
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("herdr: unmarshal %s result: %w", method, err)
	}
	return nil
}

// agentInfo mirrors herdr's AgentInfo (src/api/schema/agents.rs); only the
// fields this package actually reads are declared.
type agentInfo struct {
	TerminalID  string `json:"terminal_id"`
	Name        string `json:"name,omitempty"`
	Agent       string `json:"agent,omitempty"`
	AgentStatus string `json:"agent_status"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	PaneID      string `json:"pane_id"`
	CWD         string `json:"cwd,omitempty"`
}

func (c *client) agentList(ctx context.Context) ([]agentInfo, error) {
	var out struct {
		Agents []agentInfo `json:"agents"`
	}
	if err := c.call(ctx, "agent.list", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// findByName returns the agentInfo whose Name matches, or (agentInfo{}, false).
func (c *client) findByName(ctx context.Context, name string) (agentInfo, bool, error) {
	agents, err := c.agentList(ctx)
	if err != nil {
		return agentInfo{}, false, err
	}
	for _, a := range agents {
		if a.Name == name {
			return a, true, nil
		}
	}
	return agentInfo{}, false, nil
}

// agentStart mirrors herdr's AgentStartParams. argv[0] is the command;
// the rest are its arguments — the same shape as exec.Command.
func (c *client) agentStart(ctx context.Context, name, cwd string, argv []string, env map[string]string) (agentInfo, error) {
	params := map[string]any{
		"name":  name,
		"argv":  argv,
		"focus": false,
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if len(env) > 0 {
		params["env"] = env
	}
	var out struct {
		Agent agentInfo `json:"agent"`
	}
	if err := c.call(ctx, "agent.start", params, &out); err != nil {
		return agentInfo{}, err
	}
	return out.Agent, nil
}

// agentSend writes text verbatim to the target's PTY (no key-name parsing —
// mirrors herdr's handle_agent_send: runtime.try_send_bytes(text) as-is).
// Callers wanting to submit a line must append "\r" themselves — herdr
// encodes Enter as carriage return (13), not line feed (10); sending "\n"
// types a literal newline into the input box without submitting it.
func (c *client) agentSend(ctx context.Context, target, text string) error {
	return c.call(ctx, "agent.send", map[string]any{"target": target, "text": text}, nil)
}

// paneSendKeys sends named key sequences (e.g. "C-c", "Escape", "Up",
// "Enter") to a pane, letting herdr translate them to the right terminal
// escape codes. There is no target-flexible "agent.send_keys" — only
// "pane.send_keys", which requires a resolved pane_id (see agentGet).
func (c *client) paneSendKeys(ctx context.Context, paneID string, keys []string) error {
	return c.call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

// agentRead mirrors AgentReadParams; source is "recent" (scrollback,
// including content scrolled off screen) or "visible" (current frame only).
func (c *client) agentRead(ctx context.Context, target, source string, lines int) (string, error) {
	params := map[string]any{
		"target":     target,
		"source":     source,
		"format":     "text", // plain text — this package doesn't render ANSI colors
		"strip_ansi": true,
	}
	if lines > 0 {
		params["lines"] = lines
	}
	var out struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := c.call(ctx, "agent.read", params, &out); err != nil {
		return "", err
	}
	return out.Read.Text, nil
}

// agentGet fetches a single agent's current info (including agent_status)
// by target (name, terminal id, or pane id). Response is tagged
// {"type":"agent_info","agent":{...}} — see herdr's ResponseResult enum
// (src/api/schema/response.rs, #[serde(tag = "type")]).
func (c *client) agentGet(ctx context.Context, target string) (agentInfo, error) {
	var out struct {
		Agent agentInfo `json:"agent"`
	}
	if err := c.call(ctx, "agent.get", map[string]any{"target": target}, &out); err != nil {
		return agentInfo{}, err
	}
	return out.Agent, nil
}
