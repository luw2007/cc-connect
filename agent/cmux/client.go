// Package cmux drives cc-connect sessions through cmux GUI workspaces.
package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Method names are the raw v2 socket RPC verbs (dotted), NOT the cmux CLI's
// kebab-case subcommand aliases — the CLI translates subcommands like
// "list-workspaces"/"read-screen"/"send"/"send-key"/"ping" to these dotted
// verbs internally; a bare-socket client must send the dotted form directly
// (confirmed live via system.capabilities + raw-socket probes; see
// testdata/PROBES.md).
const (
	methodPing                = "system.ping"
	methodEventsStream        = "events.stream"
	methodFeedList            = "feed.list"
	methodFeedPermissionReply = "feed.permission.reply"
	methodFeedQuestionReply   = "feed.question.reply"
	methodFeedExitPlanReply   = "feed.exit_plan.reply"
	methodListWorkspaces      = "workspace.list"
	methodNewWorkspace        = "workspace.create"
	methodReadScreen          = "surface.read_text"
	methodSend                = "surface.send_text"
	methodSendKey             = "surface.send_key"
)

type rpcRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcResponse struct {
	ID     string          `json:"id"`
	OK     *bool           `json:"ok,omitempty"`
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

type dialFunc func(context.Context, string, string) (net.Conn, error)

var rpcCounter atomic.Uint64

type client struct {
	socketPath string
	password   string
	rpcTimeout time.Duration
	dial       dialFunc
}

func newClient(socketPath, password string) *client {
	var d net.Dialer
	return &client{socketPath: socketPath, password: password, rpcTimeout: 10 * time.Second, dial: d.DialContext}
}

func (c *client) connect(ctx context.Context) (net.Conn, *bufio.Reader, error) {
	conn, err := c.dial(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("cmux: connect %s: %w", c.socketPath, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	r := bufio.NewReader(conn)
	if c.password == "" {
		return conn, r, nil
	}
	// cmux documents the password option, but the raw line-oriented auth
	// handshake has not yet been captured in PROBES.md; keep it isolated
	// behind the non-empty password configuration until a live probe exists.
	if _, err := fmt.Fprintf(conn, "auth %s\n", c.password); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("cmux: write auth handshake: %w", err)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("cmux: read auth handshake: %w", err)
	}
	if strings.HasPrefix(strings.TrimSpace(line), "ERROR:") {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("cmux: auth handshake rejected: %s", strings.TrimSpace(line))
	}
	return conn, r, nil
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.rpcTimeout)
		defer cancel()
	}
	conn, r, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	closeOnCancel := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closeOnCancel:
		}
	}()
	defer close(closeOnCancel)

	id := strconv.FormatUint(rpcCounter.Add(1), 10)
	request, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("cmux: marshal %s request: %w", method, err)
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return fmt.Errorf("cmux: write %s request: %w", method, err)
	}
	line, err := r.ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return fmt.Errorf("cmux: read %s response: %w", method, err)
	}
	var response rpcResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("cmux: decode %s response: %w", method, err)
	}
	if response.Error != nil {
		return fmt.Errorf("cmux: %s: %w", method, response.Error)
	}
	if response.OK != nil && !*response.OK {
		return fmt.Errorf("cmux: %s: unsuccessful response", method)
	}
	if out == nil || len(response.Result) == 0 || string(response.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(response.Result, out); err != nil {
		return fmt.Errorf("cmux: decode %s result: %w", method, err)
	}
	return nil
}

func (c *client) ping(ctx context.Context) error {
	return c.call(ctx, methodPing, map[string]any{}, nil)
}

func resolveSocket(ctx context.Context, explicit, password string) (string, error) {
	return resolveSocketWithProbe(ctx, explicit, password, func(probeCtx context.Context, path, probePassword string) error {
		return newClient(path, probePassword).ping(probeCtx)
	})
}

func resolveControllerSocket(ctx context.Context, explicit, password string) (string, error) {
	return resolveControllerSocketWithProbe(ctx, explicit, password, func(probeCtx context.Context, path, probePassword string) error {
		return newClient(path, probePassword).ping(probeCtx)
	})
}

func resolveControllerSocketWithProbe(ctx context.Context, explicit, password string, probe socketProbe) (string, error) {
	if explicit == "" {
		return resolveSocketWithProbe(ctx, "", password, probe)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	err := probe(probeCtx, explicit, password)
	cancel()
	if err != nil {
		return "", fmt.Errorf("cmux: explicit controller socket %q is not responsive: %w", explicit, err)
	}
	return explicit, nil
}

type socketProbe func(context.Context, string, string) error

func resolveSocketWithProbe(ctx context.Context, explicit, password string, probe socketProbe) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cmux: resolve home directory: %w", err)
	}
	candidates := make([]string, 0, 5)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		for _, existing := range candidates {
			if existing == path {
				return
			}
		}
		candidates = append(candidates, path)
	}
	add(explicit)
	add(os.Getenv("CMUX_SOCKET_PATH"))
	lastPathFile := filepath.Join(home, ".local", "state", "cmux", "last-socket-path")
	if data, readErr := os.ReadFile(lastPathFile); readErr == nil {
		add(string(data))
	}
	if currentUser, userErr := user.Current(); userErr == nil && currentUser.Uid != "" {
		add(filepath.Join(home, ".local", "state", "cmux", "cmux-"+currentUser.Uid+".sock"))
	}
	add(filepath.Join(home, ".config", "cmux", "cmux.sock"))

	var probeErrors []error
	for _, path := range candidates {
		probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		err := probe(probeCtx, path, password)
		cancel()
		if err == nil {
			return path, nil
		}
		probeErrors = append(probeErrors, fmt.Errorf("%s: %w", path, err))
	}
	return "", fmt.Errorf("cmux: no responsive socket found: %w", errors.Join(probeErrors...))
}

type feedDecision struct {
	Kind string `json:"kind"`
	Mode string `json:"mode"`
}

type feedItem struct {
	ID                  string          `json:"id"`
	Kind                string          `json:"kind"`
	Source              string          `json:"source"`
	Status              string          `json:"status"`
	RequestID           string          `json:"request_id"`
	ToolName            string          `json:"tool_name"`
	ToolInput           string          `json:"tool_input"`
	CWD                 string          `json:"cwd"`
	Title               string          `json:"title"`
	WorkstreamID        string          `json:"workstream_id"`
	SessionID           string          `json:"session_id"`
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
	ResolvedAt          string          `json:"resolved_at"`
	Decision            feedDecision    `json:"decision"`
	Questions           json.RawMessage `json:"questions"`
	QuestionOptions     json.RawMessage `json:"question_options"`
	QuestionMultiSelect bool            `json:"question_multi_select"`
}

func (c *client) feedList(ctx context.Context) ([]feedItem, error) {
	var result struct {
		Items []feedItem `json:"items"`
	}
	if err := c.call(ctx, methodFeedList, map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

type feedReplyResult struct {
	Delivered bool `json:"delivered"`
}

func (c *client) feedReply(ctx context.Context, method string, params any) error {
	var rawResult json.RawMessage
	if err := c.call(ctx, method, params, &rawResult); err != nil {
		return err
	}
	var result feedReplyResult
	if len(rawResult) == 0 || string(rawResult) == "null" {
		return fmt.Errorf("cmux: %s result did not confirm delivery: %w", method, core.ErrAgentControlRequestStale)
	}
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return fmt.Errorf("cmux: decode %s delivery result: %v: %w", method, err, core.ErrAgentControlRequestStale)
	}
	if !result.Delivered {
		return fmt.Errorf("cmux: %s reply was not delivered: %w", method, core.ErrAgentControlRequestStale)
	}
	return nil
}

func (c *client) feedPermissionReply(ctx context.Context, requestID, mode string) error {
	return c.feedReply(ctx, methodFeedPermissionReply, map[string]any{"request_id": requestID, "mode": mode})
}

func (c *client) feedQuestionReply(ctx context.Context, requestID string, selections []string) error {
	return c.feedReply(ctx, methodFeedQuestionReply, map[string]any{"request_id": requestID, "selections": selections})
}

// workspaceInfo mirrors workspace.list's per-item shape. workspace.create's
// response uses a different flat shape entirely (see newWorkspace) and does
// not populate this struct via unmarshal.
type workspaceInfo struct {
	ID                       string  `json:"id"`
	Name                     string  `json:"-"`
	Title                    string  `json:"title"`
	CustomTitle              *string `json:"custom_title"`
	HasCustomTitle            bool    `json:"has_custom_title"`
	CWD                      string  `json:"current_directory"`
	Command                  string  `json:"command,omitempty"`
	SurfaceID                string  `json:"surface_id,omitempty"`
	UpdatedAt                string  `json:"latest_submitted_at,omitempty"`
	LatestConversationMessage string  `json:"latest_conversation_message,omitempty"`
	LatestSubmittedMessage   string  `json:"latest_submitted_message,omitempty"`
	Description              string  `json:"description,omitempty"`
}

func (w workspaceInfo) stableName() string {
	if w.Name != "" {
		return w.Name
	}
	if w.HasCustomTitle && w.CustomTitle != nil {
		return *w.CustomTitle
	}
	return ""
}

func (w workspaceInfo) description() string {
	var parts []string
	if w.Command != "" {
		parts = append(parts, w.Command)
	}
	if t := timeAgo(w.UpdatedAt); t != "" {
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

func (w workspaceInfo) latestMessage() string {
	if w.LatestConversationMessage != "" {
		return w.LatestConversationMessage
	}
	return w.LatestSubmittedMessage
}

func (w workspaceInfo) displayName() string {
	if name := w.stableName(); name != "" {
		return name
	}
	if base := filepath.Base(filepath.Clean(w.CWD)); base != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	return w.ID
}

func (c *client) listWorkspaces(ctx context.Context) ([]workspaceInfo, error) {
	var raw json.RawMessage
	if err := c.call(ctx, methodListWorkspaces, map[string]any{}, &raw); err != nil {
		return nil, err
	}
	var direct []workspaceInfo
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("cmux: decode list-workspaces result: %w", err)
	}
	return wrapped.Workspaces, nil
}

func (c *client) newWorkspace(ctx context.Context, name, cwd, command string) (workspaceInfo, error) {
	params := map[string]any{"name": name}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if command != "" {
		params["command"] = command
	}
	// workspace.create's response is flat ({workspace_id, surface_id,
	// window_id, ...} plus *_ref fields) and never echoes name/cwd back
	// (verified live, PROBES.md) — build workspaceInfo from the request
	// values we already know plus the returned identifiers.
	var result struct {
		WorkspaceID string `json:"workspace_id"`
		SurfaceID   string `json:"surface_id"`
	}
	if err := c.call(ctx, methodNewWorkspace, params, &result); err != nil {
		return workspaceInfo{}, err
	}
	if result.WorkspaceID == "" {
		return workspaceInfo{}, fmt.Errorf("cmux: create workspace: response omitted workspace_id")
	}
	return workspaceInfo{ID: result.WorkspaceID, Name: name, CWD: cwd, SurfaceID: result.SurfaceID}, nil
}

func (c *client) readScreen(ctx context.Context, workspaceID, surfaceID string) (string, error) {
	params := map[string]any{"workspace_id": workspaceID}
	if surfaceID != "" {
		params["surface_id"] = surfaceID
	}
	var raw json.RawMessage
	if err := c.call(ctx, methodReadScreen, params, &raw); err != nil {
		return "", err
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var wrapped struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return "", fmt.Errorf("cmux: decode read-screen result: %w", err)
	}
	return wrapped.Text, nil
}

func (c *client) send(ctx context.Context, workspaceID, surfaceID, text string) error {
	params := map[string]any{"workspace_id": workspaceID, "text": text}
	if surfaceID != "" {
		params["surface_id"] = surfaceID
	}
	return c.call(ctx, methodSend, params, nil)
}

func (c *client) sendKey(ctx context.Context, workspaceID, surfaceID, key string) error {
	params := map[string]any{"workspace_id": workspaceID, "key": key}
	if surfaceID != "" {
		params["surface_id"] = surfaceID
	}
	return c.call(ctx, methodSendKey, params, nil)
}
