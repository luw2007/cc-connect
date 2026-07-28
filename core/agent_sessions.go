package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultAgentSessionListLimit = 100
const defaultAgentSessionHistoryLimit = 50

var (
	ErrAgentSessionHistoryUnsupported = errors.New("agent session history is unsupported")
	ErrAgentSessionGroupUnsupported   = errors.New("agent session group creation is unsupported")
	ErrAgentSessionOwnerRequired      = errors.New("agent session group owner is required")
	ErrAgentSessionNotFound           = errors.New("agent session not found")
)

// AgentSessionHistoryReader is an optional agent capability for reading the
// agent CLI's native, persisted conversation history.
type AgentSessionHistoryReader interface {
	GetSessionHistory(ctx context.Context, sessionID string, limit int) ([]HistoryEntry, error)
}

type AgentSessionQuery struct {
	Since time.Time
	Until time.Time
	Limit int
	Scope string
}

type AgentSessionView struct {
	ID           string     `json:"id"`
	Summary      string     `json:"summary,omitempty"`
	MessageCount int        `json:"message_count"`
	ModifiedAt   *time.Time `json:"modified_at,omitempty"`
	TimeSource   string     `json:"time_source"`
	ProjectPath  string     `json:"project_path,omitempty"`
	AgentType    string     `json:"agent_type"`
	Bound        bool       `json:"bound"`
	BoundChatID  string     `json:"bound_chat_id,omitempty"`
	CCSessionID  string     `json:"cc_session_id,omitempty"`
}

type AgentSessionResult struct {
	Sessions             []AgentSessionView `json:"sessions"`
	Count                int                `json:"count"`
	Scope                string             `json:"scope"`
	AllSessionsSupported bool               `json:"all_sessions_supported"`
	FilteredUnknownTime  int                `json:"filtered_unknown_time"`
}

type AgentSessionGroupResult struct {
	Created          bool   `json:"created"`
	ChatID           string `json:"chat_id"`
	OwnerUserID      string `json:"owner_user_id"`
	BindingNamespace string `json:"binding_namespace"`
}

// ListAgentSessions returns the agent CLI's native session inventory. Time
// filtering is fail-closed: records without a trustworthy timestamp are only
// returned when no time bound was requested.
func (e *Engine) ListAgentSessions(ctx context.Context, q AgentSessionQuery) (AgentSessionResult, error) {
	scope := normalizedAgentSessionScope(q.Scope)
	infos, allSupported, err := e.listAgentSessionSnapshot(ctx, scope == "all")
	if err != nil {
		return AgentSessionResult{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultAgentSessionListLimit
	}
	hasTimeFilter := !q.Since.IsZero() || !q.Until.IsZero()
	views := make([]AgentSessionView, 0, len(infos))
	filteredUnknown := 0
	for _, info := range infos {
		view := AgentSessionView{
			ID:           info.ID,
			Summary:      info.Summary,
			MessageCount: info.MessageCount,
			ProjectPath:  info.ProjectPath,
			AgentType:    e.agent.Name(),
			TimeSource:   "unknown",
		}
		if info.ModifiedAt.IsZero() {
			if hasTimeFilter {
				filteredUnknown++
				continue
			}
		} else {
			modifiedAt := info.ModifiedAt
			view.ModifiedAt = &modifiedAt
			view.TimeSource = "record"
			if (!q.Since.IsZero() && modifiedAt.Before(q.Since)) ||
				(!q.Until.IsZero() && modifiedAt.After(q.Until)) {
				continue
			}
		}
		views = append(views, view)
	}

	sort.SliceStable(views, func(i, j int) bool {
		left, right := views[i].ModifiedAt, views[j].ModifiedAt
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		return left.After(*right)
	})

	e.populateAgentSessionBindings(views)
	if len(views) > limit {
		views = views[:limit]
	}

	return AgentSessionResult{
		Sessions:             views,
		Count:                len(views),
		Scope:                scope,
		AllSessionsSupported: allSupported,
		FilteredUnknownTime:  filteredUnknown,
	}, nil
}

// AgentSessionHistory reads history from the agent CLI's native session store.
func (e *Engine) AgentSessionHistory(ctx context.Context, sessionID string, limit int) ([]HistoryEntry, error) {
	reader, ok := e.agent.(AgentSessionHistoryReader)
	if !ok {
		return nil, ErrAgentSessionHistoryUnsupported
	}
	if limit <= 0 {
		limit = defaultAgentSessionHistoryLimit
	}
	entries, err := reader.GetSessionHistory(ctx, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("read agent session history %q: %w", sessionID, err)
	}
	return entries, nil
}

// CreateAgentSessionGroup creates one group chat for an existing native agent
// session and records it under a namespace isolated from project and auto-group
// bindings. The mutex closes the lookup/create/bind race for concurrent calls.
func (e *Engine) CreateAgentSessionGroup(ctx context.Context, sessionID, ownerUserID, groupName string) (AgentSessionGroupResult, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return AgentSessionGroupResult{}, ErrAgentSessionOwnerRequired
	}

	e.autoGroupSweepMu.Lock()
	defer e.autoGroupSweepMu.Unlock()

	infos, _, err := e.listAgentSessionSnapshot(ctx, true)
	if err != nil {
		return AgentSessionGroupResult{}, err
	}
	var selected *AgentSessionInfo
	for i := range infos {
		if infos[i].ID == sessionID {
			selected = &infos[i]
			break
		}
	}
	if selected == nil {
		return AgentSessionGroupResult{}, ErrAgentSessionNotFound
	}

	namespace := "manual:" + e.name
	bindings := e.ensureAgentSessionBindings()
	if channelKey, _, found := bindings.LookupBySessionID(namespace, sessionID); found {
		return AgentSessionGroupResult{
			Created:          false,
			ChatID:           legacyWorkspaceChannelKey(channelKey),
			OwnerUserID:      ownerUserID,
			BindingNamespace: namespace,
		}, nil
	}

	creatorPlatform := e.firstGroupChatCreator()
	if creatorPlatform == nil {
		return AgentSessionGroupResult{}, ErrAgentSessionGroupUnsupported
	}
	creator := creatorPlatform.(GroupChatCreator)
	chatID, err := creator.CreateGroupChat(ctx, groupName, "", ownerUserID)
	if err != nil {
		return AgentSessionGroupResult{}, fmt.Errorf("create group for agent session %q: %w", sessionID, err)
	}
	if strings.TrimSpace(chatID) == "" {
		return AgentSessionGroupResult{}, fmt.Errorf("create group for agent session %q: platform returned an empty chat ID", sessionID)
	}

	channelKey := workspaceChannelKey(creatorPlatform.Name(), chatID)
	bindings.BindSession(namespace, channelKey, groupName, selected.ProjectPath, sessionID)
	if err := verifyAgentSessionBinding(bindings, namespace, channelKey, sessionID); err != nil {
		return AgentSessionGroupResult{}, fmt.Errorf("persist group binding for agent session %q (orphaned chat %q): %w", sessionID, chatID, err)
	}

	return AgentSessionGroupResult{
		Created:          true,
		ChatID:           chatID,
		OwnerUserID:      ownerUserID,
		BindingNamespace: namespace,
	}, nil
}

func normalizedAgentSessionScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "all") {
		return "all"
	}
	return "project"
}

func (e *Engine) listAgentSessionSnapshot(ctx context.Context, requestAll bool) ([]AgentSessionInfo, bool, error) {
	if e.agent == nil {
		return nil, false, errors.New("list agent sessions: agent is not configured")
	}
	allLister, allSupported := e.agent.(AllSessionsLister)
	var (
		infos []AgentSessionInfo
		err   error
	)
	if requestAll && allSupported {
		infos, err = allLister.ListAllSessions(ctx)
	} else {
		infos, err = e.agent.ListSessions(ctx)
	}
	if err != nil {
		return nil, allSupported, fmt.Errorf("list agent sessions: %w", err)
	}
	return infos, allSupported, nil
}

func (e *Engine) populateAgentSessionBindings(views []AgentSessionView) {
	ccSessionIDs := make(map[string]string)
	if e.sessions != nil {
		for _, session := range e.sessions.AllSessions() {
			agentSessionID := session.GetAgentSessionID()
			if agentSessionID == "" {
				continue
			}
			current, exists := ccSessionIDs[agentSessionID]
			if !exists || session.ID < current {
				ccSessionIDs[agentSessionID] = session.ID
			}
		}
	}

	e.autoGroupSweepMu.Lock()
	defer e.autoGroupSweepMu.Unlock()
	bindings := e.workspaceBindings
	namespace := "manual:" + e.name
	for i := range views {
		if ccSessionID := ccSessionIDs[views[i].ID]; ccSessionID != "" {
			views[i].Bound = true
			views[i].CCSessionID = ccSessionID
		}
		if bindings == nil {
			continue
		}
		if channelKey, _, found := bindings.LookupBySessionID(namespace, views[i].ID); found {
			views[i].BoundChatID = legacyWorkspaceChannelKey(channelKey)
		}
	}
}

func (e *Engine) ensureAgentSessionBindings() *WorkspaceBindingManager {
	if e.workspaceBindings == nil {
		storePath := ""
		if e.dataDir != "" {
			storePath = filepath.Join(e.dataDir, "workspace_bindings.json")
		}
		e.workspaceBindings = NewWorkspaceBindingManager(storePath)
	}
	return e.workspaceBindings
}

// verifyAgentSessionBinding compensates for WorkspaceBindingManager's legacy
// void-returning BindSession API. It verifies the in-memory index and, when a
// store is configured, the durable JSON record so callers never receive a
// false success after an AtomicWriteFile failure.
func verifyAgentSessionBinding(bindings *WorkspaceBindingManager, namespace, channelKey, sessionID string) error {
	boundChannelKey, _, found := bindings.LookupBySessionID(namespace, sessionID)
	if !found || boundChannelKey != channelKey {
		return errors.New("binding is absent from the workspace binding index")
	}
	if bindings.storePath == "" {
		return nil
	}
	data, err := os.ReadFile(bindings.storePath)
	if err != nil {
		return err
	}
	var persisted map[string]map[string]*WorkspaceBinding
	if err := json.Unmarshal(data, &persisted); err != nil {
		return err
	}
	binding := persisted[namespace][channelKey]
	if binding == nil || binding.AgentSessionID != sessionID {
		return errors.New("binding is absent from the persisted workspace binding store")
	}
	return nil
}
