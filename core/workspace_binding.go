package core

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const sharedWorkspaceBindingsKey = "shared"

// FlexTime wraps time.Time with lenient JSON unmarshaling.
type FlexTime struct{ time.Time }

func (ft *FlexTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		ft.Time = time.Time{}
		return nil
	}
	if s == "" {
		ft.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			ft.Time = t
			return nil
		}
	}
	slog.Warn("workspace bindings: unparseable bound_at, treating as zero", "value", s)
	ft.Time = time.Time{}
	return nil
}

// WorkspaceBinding maps a channel to a workspace directory.
type WorkspaceBinding struct {
	ChannelName string   `json:"channel_name"`
	Workspace   string   `json:"workspace"`
	BoundAt     FlexTime `json:"bound_at"`
	// AgentSessionID, when set, is the external agent session ID this
	// binding was created for. Used by SetAutoGroupWorkspaces to dedup on
	// session identity rather than Workspace: two external workspaces can
	// share the same cwd (e.g. worktrees, parallel agents on one repo) and
	// must not collide into a single group.
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Activated marks that the auto-group sequence (CreateGroupChat, Bind,
	// SwitchToAgentSession, ReconstructReplyCtx, announce) fully completed
	// for this binding. A binding can exist with Activated=false when
	// CreateGroupChat succeeded but a later step failed -- the chat is
	// already recorded (never a duplicate group on retry) and the next
	// sweep retries only the remaining steps.
	Activated bool `json:"activated,omitempty"`
}

// WorkspaceBindingManager persists channel->workspace mappings.
// Top-level key is "project:<name>", second-level key is a workspace channel key.
type WorkspaceBindingManager struct {
	mu                sync.RWMutex
	bindings          map[string]map[string]*WorkspaceBinding
	storePath         string
	lastLoadedModTime time.Time
	lastLoadedSize    int64
}

func NewWorkspaceBindingManager(storePath string) *WorkspaceBindingManager {
	m := &WorkspaceBindingManager{
		bindings:  make(map[string]map[string]*WorkspaceBinding),
		storePath: storePath,
	}
	if storePath != "" {
		m.load()
	}
	return m
}

func legacyWorkspaceChannelKey(channelKey string) string {
	if i := strings.IndexByte(channelKey, ':'); i >= 0 {
		return channelKey[i+1:]
	}
	return channelKey
}

func workspaceChannelKeyCandidates(channelKey string) []string {
	if channelKey == "" {
		return nil
	}
	legacyKey := legacyWorkspaceChannelKey(channelKey)
	if legacyKey == channelKey {
		return []string{channelKey}
	}
	return []string{channelKey, legacyKey}
}

func (m *WorkspaceBindingManager) lookupLocked(projectKey, channelKey string) *WorkspaceBinding {
	proj := m.bindings[projectKey]
	if proj == nil {
		return nil
	}
	for _, candidate := range workspaceChannelKeyCandidates(channelKey) {
		if b := proj[candidate]; b != nil {
			return b
		}
	}
	return nil
}

func (m *WorkspaceBindingManager) Bind(projectKey, channelKey, channelName, workspace string) {
	m.BindSession(projectKey, channelKey, channelName, workspace, "")
}

// BindSession is Bind plus an AgentSessionID recorded on the binding, for
// callers that need to dedup by external session identity (SetAutoGroupWorkspaces)
// rather than by workspace path.
func (m *WorkspaceBindingManager) BindSession(projectKey, channelKey, channelName, workspace, agentSessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if m.bindings[projectKey] == nil {
		m.bindings[projectKey] = make(map[string]*WorkspaceBinding)
	}
	m.bindings[projectKey][channelKey] = &WorkspaceBinding{
		ChannelName:    channelName,
		Workspace:      workspace,
		BoundAt:        FlexTime{time.Now()},
		AgentSessionID: agentSessionID,
	}
	m.saveLocked()
}

// MigrateChannelKey copies an existing default binding from oldChannelKey to
// newChannelKey. It never overwrites an existing destination and deliberately
// preserves the default binding for other channels and older versions.
func (m *WorkspaceBindingManager) MigrateChannelKey(projectKey, oldChannelKey, newChannelKey string) bool {
	if oldChannelKey == "" || newChannelKey == "" || oldChannelKey == newChannelKey {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()

	proj := m.bindings[projectKey]
	if proj == nil || m.lookupLocked(projectKey, newChannelKey) != nil {
		return false
	}

	var binding *WorkspaceBinding
	for _, candidate := range workspaceChannelKeyCandidates(oldChannelKey) {
		if b := proj[candidate]; b != nil {
			binding = b
			break
		}
	}
	if binding == nil {
		return false
	}

	inherited := *binding
	proj[newChannelKey] = &inherited
	m.saveLocked()
	return true
}

// LookupBySessionID returns the channel key and binding already associated
// with agentSessionID within projectKey (C4: dedup on external session
// identity, not workspace path -- two workspaces on the same repo path are
// distinct sessions and must not collide). The returned binding is a COPY
// (B2): the manager's internal *WorkspaceBinding can be mutated by
// MarkActivated or replaced wholesale by refreshLocked (an external file
// change) while a caller holds a pointer read outside this lock, so callers
// must never receive the live one.
func (m *WorkspaceBindingManager) LookupBySessionID(projectKey, agentSessionID string) (channelKey string, binding *WorkspaceBinding, found bool) {
	if agentSessionID == "" {
		return "", nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	for ck, b := range m.bindings[projectKey] {
		if b.AgentSessionID == agentSessionID {
			b2 := *b
			return ck, &b2, true
		}
	}
	return "", nil, false
}

// MarkActivated flags an existing binding as fully activated (switched +
// announced), so a repeating sweep does not resend the announcement on
// every tick after a successful bind. No-op if the binding is missing.
func (m *WorkspaceBindingManager) MarkActivated(projectKey, channelKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if proj := m.bindings[projectKey]; proj != nil {
		if b := proj[channelKey]; b != nil {
			b.Activated = true
			m.saveLocked()
		}
	}
}

func (m *WorkspaceBindingManager) Unbind(projectKey, channelKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if proj := m.bindings[projectKey]; proj != nil {
		for _, candidate := range workspaceChannelKeyCandidates(channelKey) {
			delete(proj, candidate)
		}
		if len(proj) == 0 {
			delete(m.bindings, projectKey)
		}
	}
	m.saveLocked()
}

func (m *WorkspaceBindingManager) Lookup(projectKey, channelKey string) *WorkspaceBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	return m.lookupLocked(projectKey, channelKey)
}

// LookupEffective returns the effective binding for a channel, checking the
// current project first and then the shared routing layer.
func (m *WorkspaceBindingManager) LookupEffective(projectKey, channelKey string) (*WorkspaceBinding, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if b := m.lookupLocked(projectKey, channelKey); b != nil {
		return b, projectKey
	}
	if b := m.lookupLocked(sharedWorkspaceBindingsKey, channelKey); b != nil {
		return b, sharedWorkspaceBindingsKey
	}
	return nil, ""
}

func (m *WorkspaceBindingManager) ListByProject(projectKey string) map[string]*WorkspaceBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	result := make(map[string]*WorkspaceBinding)
	if proj := m.bindings[projectKey]; proj != nil {
		for k, v := range proj {
			result[k] = v
		}
	}
	return result
}

// WorkspaceBindingMatch represents a binding found via reverse lookup.
type WorkspaceBindingMatch struct {
	ProjectKey string
	ChannelKey string
	Binding    *WorkspaceBinding
}

// LookupByWorkspace returns all channel bindings that point to the given workspace path.
func (m *WorkspaceBindingManager) LookupByWorkspace(workspace string) []WorkspaceBindingMatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()

	normalized := normalizeWorkspacePath(workspace)
	var matches []WorkspaceBindingMatch
	for projectKey, proj := range m.bindings {
		for channelKey, binding := range proj {
			if binding.Workspace == normalized {
				matches = append(matches, WorkspaceBindingMatch{
					ProjectKey: projectKey,
					ChannelKey: channelKey,
					Binding:    binding,
				})
			}
		}
	}
	return matches
}

func (m *WorkspaceBindingManager) saveLocked() {
	if m.storePath == "" {
		return
	}
	data, err := json.MarshalIndent(m.bindings, "", "  ")
	if err != nil {
		slog.Error("workspace bindings: marshal error", "err", err)
		return
	}
	if err := AtomicWriteFile(m.storePath, data, 0o644); err != nil {
		slog.Error("workspace bindings: save error", "err", err)
		return
	}
	if info, err := os.Stat(m.storePath); err == nil {
		m.lastLoadedModTime = info.ModTime()
		m.lastLoadedSize = info.Size()
	}
}

func (m *WorkspaceBindingManager) load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
}

func (m *WorkspaceBindingManager) refreshLocked() {
	if m.storePath == "" {
		return
	}
	info, err := os.Stat(m.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			m.bindings = make(map[string]map[string]*WorkspaceBinding)
			m.lastLoadedModTime = time.Time{}
			m.lastLoadedSize = 0
			return
		}
		slog.Error("workspace bindings: stat error", "err", err)
		return
	}
	if !m.lastLoadedModTime.IsZero() && info.ModTime().Equal(m.lastLoadedModTime) && info.Size() == m.lastLoadedSize {
		return
	}

	data, err := os.ReadFile(m.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("workspace bindings: load error", "err", err)
		}
		return
	}
	loaded := make(map[string]map[string]*WorkspaceBinding)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &loaded); err != nil {
			slog.Error("workspace bindings: unmarshal error", "err", err)
			return
		}
	}
	m.bindings = loaded
	m.lastLoadedModTime = info.ModTime()
	m.lastLoadedSize = info.Size()
}
