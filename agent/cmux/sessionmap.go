package cmux

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type hookSessionRef struct {
	SessionID string  `json:"sessionId"`
	UpdatedAt float64 `json:"updatedAt"`
}

type hookSession struct {
	SessionID      string  `json:"sessionId"`
	SurfaceID      string  `json:"surfaceId"`
	WorkspaceID    string  `json:"workspaceId"`
	AgentLifecycle string  `json:"agentLifecycle"`
	UpdatedAt      float64 `json:"updatedAt"`
}

type workspaceHookIdentity struct {
	Source    string
	SessionID string
	SurfaceID string
	Lifecycle string
}

type controlHookIdentity struct {
	Source       string
	WorkstreamID string
	SurfaceID    string
	Lifecycle    string
}

type controlHookSnapshot struct {
	byWorkspace map[string]controlHookIdentity
}

type hookSessionStore struct {
	ActiveBySurface   map[string]hookSessionRef `json:"activeSessionsBySurface"`
	ActiveByWorkspace map[string]hookSessionRef `json:"activeSessionsByWorkspace"`
	Sessions          map[string]hookSession    `json:"sessions"`
}

type fileStamp struct {
	modTime time.Time
	size    int64
}

// sessionMapper caches the last successful join over cmux's Claude and Codex
// hook-session stores. Failed reads never replace that mapping.
type sessionMapper struct {
	dir string

	mu                    sync.Mutex
	stamps                map[string]fileStamp
	workstreamToWorkspace map[string]string
	surfaceToWorkspace    map[string]string
	workspaceToSurface    map[string]string
	workspaceUpdated      map[string]time.Time
	workspaceLifecycles   map[string]string
	workspaceHooks        map[string]workspaceHookIdentity
	initialized           bool
	lastRefresh           time.Time
}

func newSessionMapper(dir string) *sessionMapper {
	return &sessionMapper{
		dir:                   dir,
		stamps:                make(map[string]fileStamp),
		workstreamToWorkspace: make(map[string]string),
		surfaceToWorkspace:    make(map[string]string),
		workspaceToSurface:    make(map[string]string),
		workspaceUpdated:      make(map[string]time.Time),
		workspaceLifecycles:   make(map[string]string),
		workspaceHooks:        make(map[string]workspaceHookIdentity),
	}
}

// controlSnapshot reads hook stores afresh without consulting or mutating the
// legacy bridge cache. Any current source failure removes all hook authority
// from this snapshot.
func (m *sessionMapper) controlSnapshot() controlHookSnapshot {
	empty := func() controlHookSnapshot {
		return controlHookSnapshot{byWorkspace: make(map[string]controlHookIdentity)}
	}
	stores := make(map[string]hookSessionStore)
	for _, source := range []string{"claude", "codex"} {
		path := filepath.Join(m.dir, source+"-hook-sessions.json")
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			slog.Warn("cmux: stat control hook session store failed; omitting hook authority", "path", path, "err", err)
			return empty()
		}
		store, _, err := readHookSessionStore(path)
		if err != nil {
			slog.Warn("cmux: read control hook session store failed; omitting hook authority", "path", path, "err", err)
			return empty()
		}
		stores[source] = store
	}

	candidates := make(map[string][]controlHookIdentity)
	workstreamCounts := make(map[string]int)
	surfaceCounts := make(map[string]int)
	for _, source := range []string{"claude", "codex"} {
		store, ok := stores[source]
		if !ok {
			continue
		}
		for workspaceID, ref := range store.ActiveByWorkspace {
			if workspaceID == "" || ref.SessionID == "" {
				continue
			}
			session, ok := strictHookSessionByID(store.Sessions, ref.SessionID)
			if !ok || session.WorkspaceID != workspaceID {
				continue
			}
			if activeRef, active := store.ActiveBySurface[session.SurfaceID]; active && activeRef.SessionID != ref.SessionID {
				continue
			}
			surfaceID := session.SurfaceID
			if surfaceID == "" {
				for candidateSurface, surfaceRef := range store.ActiveBySurface {
					if candidateSurface == "" || surfaceRef.SessionID != ref.SessionID {
						continue
					}
					if surfaceID != "" {
						surfaceID = ""
						break
					}
					surfaceID = candidateSurface
				}
				if surfaceID == "" {
					continue
				}
			}
			workstreamID := ref.SessionID
			if !strings.HasPrefix(workstreamID, source+"-") {
				workstreamID = source + "-" + workstreamID
			}
			identity := controlHookIdentity{
				Source:       source,
				WorkstreamID: workstreamID,
				SurfaceID:    surfaceID,
				Lifecycle:    session.AgentLifecycle,
			}
			candidates[workspaceID] = append(candidates[workspaceID], identity)
			workstreamCounts[workstreamID]++
			surfaceCounts[surfaceID]++
		}
	}

	snapshot := empty()
	for workspaceID, identities := range candidates {
		if len(identities) != 1 || workstreamCounts[identities[0].WorkstreamID] != 1 || surfaceCounts[identities[0].SurfaceID] != 1 {
			continue
		}
		snapshot.byWorkspace[workspaceID] = identities[0]
	}
	return snapshot
}

func strictHookSessionByID(sessions map[string]hookSession, sessionID string) (hookSession, bool) {
	var match hookSession
	matches := 0
	for key, session := range sessions {
		candidateID := session.SessionID
		if candidateID == "" {
			candidateID = key
		}
		if candidateID != sessionID {
			continue
		}
		match = session
		matches++
	}
	return match, matches == 1
}

func (m *sessionMapper) workspaceIDForWorkstream(workstreamID string) (string, bool) {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	workspaceID, ok := m.workstreamToWorkspace[workstreamID]
	return workspaceID, ok
}

func (m *sessionMapper) workspaceIDForSurface(surfaceID string) (string, bool) {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	workspaceID, ok := m.surfaceToWorkspace[surfaceID]
	return workspaceID, ok
}

func (m *sessionMapper) surfaceIDForWorkspace(workspaceID string) (string, bool) {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	surfaceID, ok := m.workspaceToSurface[workspaceID]
	return surfaceID, ok
}

func (m *sessionMapper) workspaceUpdatedAt(workspaceID string) time.Time {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workspaceUpdated[workspaceID]
}

func (m *sessionMapper) workspaceLifecycle(workspaceID string) string {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workspaceLifecycles[workspaceID]
}

func (m *sessionMapper) workspaceHook(workspaceID string) workspaceHookIdentity {
	m.refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workspaceHooks[workspaceID]
}

func (m *sessionMapper) workspaceKind(workspaceID string) string {
	return m.workspaceHook(workspaceID).Source
}

func (m *sessionMapper) hasHookStore() bool {
	for _, source := range []string{"claude", "codex"} {
		if _, err := os.Stat(filepath.Join(m.dir, source+"-hook-sessions.json")); err == nil {
			return true
		}
	}
	return false
}

func (m *sessionMapper) refreshNow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked(true)
}

func (m *sessionMapper) refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.initialized && now.Sub(m.lastRefresh) < 25*time.Millisecond {
		return
	}
	m.lastRefresh = now
	m.refreshLocked(false)
}

func (m *sessionMapper) refreshLocked(force bool) {
	sources := []string{"claude", "codex"}
	changed := !m.initialized
	for _, source := range sources {
		path := filepath.Join(m.dir, source+"-hook-sessions.json")
		info, err := os.Stat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("cmux: stat hook session store failed", "path", path, "err", err)
			}
			if _, previouslyPresent := m.stamps[source]; previouslyPresent {
				changed = true
			}
			continue
		}
		stamp := fileStamp{modTime: info.ModTime(), size: info.Size()}
		if previous, ok := m.stamps[source]; !ok || previous != stamp {
			changed = true
		}
	}
	if !force && !changed {
		return
	}

	stores := make(map[string]hookSessionStore)
	stamps := make(map[string]fileStamp)
	for _, source := range sources {
		path := filepath.Join(m.dir, source+"-hook-sessions.json")
		store, stamp, err := readHookSessionStore(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			slog.Warn("cmux: read hook session store failed; keeping prior mapping", "path", path, "err", err)
			return
		}
		stores[source] = store
		stamps[source] = stamp
	}

	workstreamToWorkspace := make(map[string]string)
	ambiguousWorkstreams := make(map[string]struct{})
	surfaceToWorkspace := make(map[string]string)
	workspaceToSurface := make(map[string]string)
	workspaceUpdated := make(map[string]time.Time)
	workspaceLifecycles := make(map[string]string)
	workspaceHooks := make(map[string]workspaceHookIdentity)
	bestHookAt := make(map[string]time.Time)
	bestHookKey := make(map[string]string)
	for _, source := range sources {
		store, ok := stores[source]
		if !ok {
			continue
		}
		for workspaceID, ref := range store.ActiveByWorkspace {
			if workspaceID == "" || ref.SessionID == "" {
				continue
			}
			session, found := strictHookSessionByID(store.Sessions, ref.SessionID)
			if !found || session.WorkspaceID != workspaceID {
				continue
			}
			workstreamID := ref.SessionID
			if !strings.HasPrefix(workstreamID, source+"-") {
				workstreamID = source + "-" + workstreamID
			}
			if _, ambiguous := ambiguousWorkstreams[workstreamID]; !ambiguous {
				if mappedWorkspaceID, exists := workstreamToWorkspace[workstreamID]; !exists {
					workstreamToWorkspace[workstreamID] = workspaceID
				} else if mappedWorkspaceID != workspaceID {
					delete(workstreamToWorkspace, workstreamID)
					ambiguousWorkstreams[workstreamID] = struct{}{}
				}
			}
			if updated := unixFloatTime(ref.UpdatedAt); updated.After(workspaceUpdated[workspaceID]) {
				workspaceUpdated[workspaceID] = updated
			}
		}
		for sessionKey, session := range store.Sessions {
			if session.WorkspaceID == "" {
				continue
			}
			sessionID := session.SessionID
			if sessionID == "" {
				sessionID = sessionKey
			}
			if session.SurfaceID != "" {
				surfaceToWorkspace[session.SurfaceID] = session.WorkspaceID
			}
			updated := unixFloatTime(session.UpdatedAt)
			key := source + "\x00" + sessionID + "\x00" + session.SurfaceID
			if betterHookCandidate(updated, key, bestHookAt[session.WorkspaceID], bestHookKey[session.WorkspaceID]) {
				bestHookAt[session.WorkspaceID] = updated
				bestHookKey[session.WorkspaceID] = key
				identity := workspaceHookIdentity{Source: source, SessionID: sessionID, SurfaceID: session.SurfaceID, Lifecycle: session.AgentLifecycle}
				workspaceHooks[session.WorkspaceID] = identity
				if identity.SurfaceID == "" {
					delete(workspaceToSurface, session.WorkspaceID)
				} else {
					workspaceToSurface[session.WorkspaceID] = identity.SurfaceID
				}
				workspaceLifecycles[session.WorkspaceID] = identity.Lifecycle
			}
			if updated.After(workspaceUpdated[session.WorkspaceID]) {
				workspaceUpdated[session.WorkspaceID] = updated
			}
		}
	}

	// Active hook-session indexes are authoritative over historical sessions.
	bestActiveAt := make(map[string]time.Time)
	bestActiveKey := make(map[string]string)
	for _, source := range sources {
		store, ok := stores[source]
		if !ok {
			continue
		}
		for workspaceID, ref := range store.ActiveByWorkspace {
			if workspaceID == "" || ref.SessionID == "" {
				continue
			}
			session, found := strictHookSessionByID(store.Sessions, ref.SessionID)
			if !found || session.WorkspaceID != workspaceID {
				continue
			}
			surfaceID := session.SurfaceID
			if surfaceID == "" {
				continue
			}
			updated := unixFloatTime(ref.UpdatedAt)
			if sessionUpdated := unixFloatTime(session.UpdatedAt); sessionUpdated.After(updated) {
				updated = sessionUpdated
			}
			key := source + "\x00" + ref.SessionID + "\x00" + surfaceID
			if !betterHookCandidate(updated, key, bestActiveAt[workspaceID], bestActiveKey[workspaceID]) {
				continue
			}
			bestActiveAt[workspaceID] = updated
			bestActiveKey[workspaceID] = key
			identity := workspaceHookIdentity{Source: source, SessionID: ref.SessionID, SurfaceID: surfaceID, Lifecycle: session.AgentLifecycle}
			workspaceHooks[workspaceID] = identity
			workspaceLifecycles[workspaceID] = identity.Lifecycle
			delete(workspaceToSurface, workspaceID)
			workspaceToSurface[workspaceID] = surfaceID
			surfaceToWorkspace[surfaceID] = workspaceID
		}
	}
	m.stamps = stamps
	m.workstreamToWorkspace = workstreamToWorkspace
	m.surfaceToWorkspace = surfaceToWorkspace
	m.workspaceToSurface = workspaceToSurface
	m.workspaceUpdated = workspaceUpdated
	m.workspaceLifecycles = workspaceLifecycles
	m.workspaceHooks = workspaceHooks
	m.initialized = true
}

func betterHookCandidate(updated time.Time, key string, bestUpdated time.Time, bestKey string) bool {
	return bestKey == "" || updated.After(bestUpdated) || (updated.Equal(bestUpdated) && key < bestKey)
}

func hookSessionByID(sessions map[string]hookSession, sessionID string) (hookSession, bool) {
	if session, ok := sessions[sessionID]; ok {
		return session, true
	}
	for _, session := range sessions {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return hookSession{}, false
}

func readHookSessionStore(path string) (hookSessionStore, fileStamp, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var store hookSessionStore
		info, err := os.Stat(path)
		if err != nil {
			return store, fileStamp{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return store, fileStamp{}, err
		}
		if err := json.Unmarshal(data, &store); err == nil {
			return store, fileStamp{modTime: info.ModTime(), size: info.Size()}, nil
		} else if attempt == 1 {
			return store, fileStamp{}, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return hookSessionStore{}, fileStamp{}, nil
}

func unixFloatTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return time.Unix(whole, nanos)
}
