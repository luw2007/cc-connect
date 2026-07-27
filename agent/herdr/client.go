package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
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

func (e *rpcError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

var requestCounter atomic.Int64

// client is a stateless handle to a herdr Unix socket; each call opens its
// own connection (see wire protocol notes above).
type client struct {
	socketPath string
	dial       func(context.Context) (net.Conn, error)
	rpcTimeout time.Duration
}

const defaultRPCTimeout = 10 * time.Second

func newClient(socketPath string) *client {
	c := &client{socketPath: socketPath, rpcTimeout: defaultRPCTimeout}
	c.dial = func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}
	return c
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, c.rpcTimeout)
	conn, err := c.dial(dialCtx)
	dialCancel()
	if err != nil {
		return fmt.Errorf("herdr: connect %s: %w", c.socketPath, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(c.rpcTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-readDone:
		}
	}()

	id := fmt.Sprintf("%d", requestCounter.Add(1))
	line, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("herdr: marshal %s request: %w", method, err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("herdr: write %s request: %w", method, err)
	}

	respLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && respLine == "" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("herdr: read %s response: %w", method, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return fmt.Errorf("herdr: malformed %s response: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("herdr: %s: %w", method, resp.Error)
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
	TerminalID    string `json:"terminal_id"`
	TerminalTitle string `json:"terminal_title,omitempty"`
	Name          string `json:"name,omitempty"`
	Agent         string `json:"agent,omitempty"`
	AgentStatus   string `json:"agent_status"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	PaneID        string `json:"pane_id"`
	CWD           string `json:"cwd,omitempty"`
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

// tabCreate creates the shell pane that protocol-17 agent.start requires.
func (c *client) tabCreate(ctx context.Context, cwd, label string) (tabID, paneID string, err error) {
	params := map[string]any{"focus": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if label != "" {
		params["label"] = label
	}
	var out struct {
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := c.call(ctx, "tab.create", params, &out); err != nil {
		return "", "", err
	}
	if out.Tab.TabID == "" || out.RootPane.PaneID == "" {
		return "", "", fmt.Errorf("herdr: tab.create response omitted tab_id or root pane_id")
	}
	return out.Tab.TabID, out.RootPane.PaneID, nil
}

func (c *client) tabClose(ctx context.Context, tabID string) error {
	return c.call(ctx, "tab.close", map[string]any{"tab_id": tabID}, nil)
}

// agentStart uses protocol 17's required {name, kind, pane_id} shape. The
// pane must already exist at an interactive shell prompt.
func (c *client) agentStart(ctx context.Context, name, kind, paneID string, args []string) (agentInfo, error) {
	params := map[string]any{"name": name, "kind": kind, "pane_id": paneID}
	if len(args) > 0 {
		params["args"] = args
	}
	var out struct {
		Agent agentInfo `json:"agent"`
	}
	if err := c.call(ctx, "agent.start", params, &out); err != nil {
		return agentInfo{}, err
	}
	return out.Agent, nil
}

// agentPrompt submits text to the target through herdr's protocol-17 prompt
// verb. agent.prompt performs submission itself; callers must not append an
// Enter byte.
func (c *client) agentPrompt(ctx context.Context, target, text string) error {
	return c.call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
}

// agentSendKeys sends named key sequences to an agent target. Protocol 17's
// AgentSendKeysParams is target-addressed, so no pane-id lookup is needed.
func (c *client) agentSendKeys(ctx context.Context, target string, keys []string) error {
	return c.call(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, nil)
}

// agentWait performs one server-side long poll. Dial and write operations
// are bounded independently, while the read has no client deadline: herdr may
// legitimately hold it for timeout_ms or longer. Context cancellation closes
// the socket so a blocked read returns promptly.
func (c *client) agentWait(ctx context.Context, target string, until []string, timeoutMs int) (agentInfo, error) {
	params := map[string]any{"target": target, "until": until, "timeout_ms": timeoutMs}
	var out struct {
		Agent agentInfo `json:"agent"`
	}
	if err := c.callLongPoll(ctx, "agent.wait", params, &out); err != nil {
		return agentInfo{}, err
	}
	return out.Agent, nil
}

func (c *client) callLongPoll(ctx context.Context, method string, params, out any) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, c.rpcTimeout)
	conn, err := c.dial(dialCtx)
	dialCancel()
	if err != nil {
		return fmt.Errorf("herdr: connect %s: %w", c.socketPath, err)
	}
	defer conn.Close()

	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-readDone:
		}
	}()

	id := fmt.Sprintf("%d", requestCounter.Add(1))
	line, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("herdr: marshal %s request: %w", method, err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(c.rpcTimeout))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("herdr: write %s request: %w", method, err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	respLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && respLine == "" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("herdr: read %s response: %w", method, err)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return fmt.Errorf("herdr: malformed %s response: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("herdr: %s: %w", method, resp.Error)
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("herdr: unmarshal %s result: %w", method, err)
	}
	return nil
}

func isNonTransientRPCError(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	switch rpcErr.Code {
	case "timeout", "unavailable", "server_busy":
		return false
	default:
		return true
	}
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
