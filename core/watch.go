package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ── SSE Watch Endpoint (Management API) ─────────────────────────

// WatchSessionInfo is a public snapshot of one active interactive session.
type WatchSessionInfo struct {
	Project           string            `json:"project"`
	SessionKey        string            `json:"session_key"`
	SessionID         string            `json:"session_id,omitempty"`
	SessionName       string            `json:"session_name,omitempty"`
	AgentType         string            `json:"agent_type,omitempty"`
	Workspace         string            `json:"workspace,omitempty"`
	Busy              bool              `json:"busy"`
	PendingPermission *WatchPendingPerm `json:"pending_permission,omitempty"`
	QueuedMessages    int               `json:"queued_messages"`
	CreatedAt         *time.Time        `json:"created_at,omitempty"`
	UpdatedAt         *time.Time        `json:"updated_at,omitempty"`
}

// WatchPendingPerm is the public view of a pending permission request.
type WatchPendingPerm struct {
	ToolName     string `json:"tool_name"`
	InputPreview string `json:"input_preview,omitempty"`
}

// WatchSnapshot collects a point-in-time snapshot of all active sessions.
func (e *Engine) WatchSnapshot() []WatchSessionInfo {
	e.interactiveMu.Lock()
	states := make(map[string]*interactiveState, len(e.interactiveStates))
	for k, v := range e.interactiveStates {
		states[k] = v
	}
	e.interactiveMu.Unlock()

	var infos []WatchSessionInfo
	for key, state := range states {
		if state.platform == nil {
			continue
		}

		info := WatchSessionInfo{
			SessionKey: key,
			Workspace:  state.workspaceDir,
		}

		if state.agent != nil {
			info.AgentType = state.agent.Name()
		}

		state.mu.Lock()
		info.QueuedMessages = len(state.pendingMessages)
		if state.pending != nil {
			info.PendingPermission = &WatchPendingPerm{
				ToolName:     state.pending.ToolName,
				InputPreview: state.pending.InputPreview,
			}
		}
		state.mu.Unlock()

		sm := e.GetSessions()
		if sm != nil {
			if sid := sm.ActiveSessionID(key); sid != "" {
				if sess := sm.FindByID(sid); sess != nil {
					info.SessionID = sess.ID
					info.SessionName = sess.Name
					info.Busy = sess.Busy()
					if !sess.CreatedAt.IsZero() {
						t := sess.CreatedAt
						info.CreatedAt = &t
					}
					if !sess.UpdatedAt.IsZero() {
						t := sess.UpdatedAt
						info.UpdatedAt = &t
					}
					if sess.AgentType != "" {
						info.AgentType = sess.AgentType
					}
				}
			}
		}

		infos = append(infos, info)
	}
	return infos
}

// WatchEvent is the SSE payload pushed to watch subscribers.
type WatchEvent struct {
	Total    int                `json:"total"`
	Sessions []WatchSessionInfo `json:"sessions"`
}

func (m *ManagementServer) handleWatch(w http.ResponseWriter, r *http.Request) {
	m.setCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !m.authenticate(r) {
		mgmtError(w, http.StatusUnauthorized, "unauthorized: missing or invalid token")
		return
	}
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		mgmtError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastHash string

	if hash := m.sendWatchEvent(w, flusher, ""); hash != "" {
		lastHash = hash
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if hash := m.sendWatchEvent(w, flusher, lastHash); hash != "" {
				lastHash = hash
			}
		}
	}
}

func (m *ManagementServer) sendWatchEvent(w http.ResponseWriter, flusher http.Flusher, lastHash string) string {
	m.mu.RLock()
	var allSessions []WatchSessionInfo
	for name, e := range m.engines {
		snapshot := e.WatchSnapshot()
		for i := range snapshot {
			snapshot[i].Project = name
		}
		allSessions = append(allSessions, snapshot...)
	}
	m.mu.RUnlock()

	evt := WatchEvent{
		Total:    len(allSessions),
		Sessions: allSessions,
	}

	data, err := json.Marshal(evt)
	if err != nil {
		slog.Error("watch: marshal failed", "error", err)
		return ""
	}

	hash := sha256Short(data)
	if hash == lastHash {
		return ""
	}

	fmt.Fprintf(w, "event: sessions\ndata: %s\n\n", data)
	flusher.Flush()
	return hash
}

func sha256Short(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

// ── /watch Command (Card-based, per-platform) ───────────────────

// watchState tracks an active /watch card refresh loop for a session.
type watchState struct {
	ticker     *time.Ticker
	stopCh     chan struct{}
	platform   Platform
	replyCtx   any
	sessionKey string
}

func (e *Engine) cmdWatch(p Platform, msg *Message, args []string) {
	if len(args) > 0 && strings.EqualFold(args[0], "stop") {
		e.stopWatch(msg.SessionKey)
		e.reply(p, msg.ReplyCtx, "Watch stopped.")
		return
	}

	e.stopWatch(msg.SessionKey)

	card := e.renderWatchCard(msg.SessionKey)
	e.replyWithCard(p, msg.ReplyCtx, card)

	e.startWatchLoop(p, msg.ReplyCtx, msg.SessionKey)
}

func (e *Engine) startWatchLoop(p Platform, replyCtx any, sessionKey string) {
	ws := &watchState{
		ticker:     time.NewTicker(30 * time.Second),
		stopCh:     make(chan struct{}),
		platform:   p,
		replyCtx:   replyCtx,
		sessionKey: sessionKey,
	}

	e.watchMu.Lock()
	e.watchStates[sessionKey] = ws
	e.watchMu.Unlock()

	go e.watchLoop(ws)
}

func (e *Engine) watchLoop(ws *watchState) {
	defer ws.ticker.Stop()
	for {
		select {
		case <-ws.stopCh:
			return
		case <-e.ctx.Done():
			return
		case <-ws.ticker.C:
			card := e.renderWatchCard(ws.sessionKey)
			if refresher, ok := ws.platform.(CardRefresher); ok {
				if err := refresher.RefreshCard(e.ctx, ws.sessionKey, card); err != nil {
					slog.Debug("watch: refresh card skipped", "session", ws.sessionKey, "error", err)
				}
			}
		}
	}
}

func (e *Engine) stopWatch(sessionKey string) {
	e.watchMu.Lock()
	ws, ok := e.watchStates[sessionKey]
	if ok {
		delete(e.watchStates, sessionKey)
	}
	e.watchMu.Unlock()
	if ok {
		close(ws.stopCh)
	}
}

func (e *Engine) renderWatchCard(sessionKey string) *Card {
	agent, _ := e.sessionContextForKey(sessionKey)

	var agentSessions []AgentSessionInfo
	if lister, ok := agent.(AllSessionsLister); ok {
		agentSessions, _ = lister.ListAllSessions(e.ctx)
	} else if agent != nil {
		agentSessions, _ = agent.ListSessions(e.ctx)
	}

	// Classify sessions
	var awaiting, working, completed int
	for _, s := range agentSessions {
		switch classifySession(s) {
		case sessionStatusAwaiting:
			awaiting++
		case sessionStatusWorking:
			working++
		default:
			completed++
		}
	}

	// Group by ProjectPath
	groups := groupByProject(agentSessions)

	// Build card body
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**%d** awaiting input · **%d** working · **%d** completed\n\n",
		awaiting, working, completed))

	home, _ := os.UserHomeDir()
	for _, g := range groups {
		dir := g.path
		if home != "" && strings.HasPrefix(dir, home) {
			dir = "~" + dir[len(home):]
		}
		sb.WriteString(fmt.Sprintf("**%s**\n", dir))
		for _, s := range g.sessions {
			icon := sessionIcon(classifySession(s))
			summary := strings.ReplaceAll(s.Summary, "\n", " ")
			summary = strings.Join(strings.Fields(summary), " ")
			if len([]rune(summary)) > 60 {
				summary = string([]rune(summary)[:60]) + "…"
			}
			age := shortAge(time.Since(s.ModifiedAt))
			sb.WriteString(fmt.Sprintf(" %s %-28s %s  %s\n", icon, truncate(s.ID, 28), summary, age))
		}
		sb.WriteString("\n")
	}

	title := fmt.Sprintf("Session Monitor — %s", time.Now().Format("15:04:05"))
	cb := NewCard().Title(title, "blue").Markdown(sb.String())
	cb.Buttons(
		DefaultBtn("🔄 Refresh", "nav:/watch"),
		DangerBtn("⏹ Stop", "act:/watch stop"),
	)
	return cb.Build()
}

// ── Helpers ─────────────────────────────────────────────────────

type sessionStatus int

const (
	sessionStatusCompleted sessionStatus = iota
	sessionStatusWorking
	sessionStatusAwaiting
)

func classifySession(s AgentSessionInfo) sessionStatus {
	age := time.Since(s.ModifiedAt)
	if age < 60*time.Second {
		return sessionStatusWorking
	}
	if age < 5*time.Minute {
		return sessionStatusAwaiting
	}
	return sessionStatusCompleted
}

func sessionIcon(st sessionStatus) string {
	switch st {
	case sessionStatusAwaiting:
		return "✻"
	case sessionStatusWorking:
		return "✳"
	default:
		return "∙"
	}
}

type projectGroup struct {
	path     string
	sessions []AgentSessionInfo
}

func groupByProject(sessions []AgentSessionInfo) []projectGroup {
	m := make(map[string][]AgentSessionInfo)
	for _, s := range sessions {
		p := s.ProjectPath
		if p == "" {
			p = "(unknown)"
		}
		m[p] = append(m[p], s)
	}

	var groups []projectGroup
	for p, ss := range m {
		sort.Slice(ss, func(i, j int) bool {
			return ss[i].ModifiedAt.After(ss[j].ModifiedAt)
		})
		groups = append(groups, projectGroup{path: p, sessions: ss})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].path < groups[j].path
	})
	return groups
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

