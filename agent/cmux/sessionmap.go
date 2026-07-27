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

type hookSessionStore struct {
	ActiveBySurface   map[string]hookSessionRef `json:"activeSessionsBySurface"`
	ActiveByWorkspace map[string]hookSessionRef `json:"activeSessionsByWorkspace"`
	Sessions          map[string]hookSession    `json:"sessions"`
}

type fileStamp struct {
	modTime time.Time
	size    int64
}

// sessionMapper is an mtime-cached join over cmux's Claude and Codex hook
// session stores. Failed reads never replace a previously successful cache.
type sessionMapper struct {
	dir string

	mu                    sync.Mutex
	stamps                map[string]fileStamp
	workstreamToWorkspace map[string]string
	surfaceToWorkspace    map[string]string
	workspaceToSurface    map[string]string
	workspaceUpdated      map[string]time.Time
	workspaceLifecycles   map[string]string
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
	}
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

func (m *sessionMapper) hasHookStore() bool {
	for _, source := range []string{"claude", "codex"} {
		if _, err := os.Stat(filepath.Join(m.dir, source+"-hook-sessions.json")); err == nil {
			return true
		}
	}
	return false
}

func (m *sessionMapper) refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.initialized && now.Sub(m.lastRefresh) < 25*time.Millisecond {
		return
	}
	m.lastRefresh = now
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
	if !changed {
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
	surfaceToWorkspace := make(map[string]string)
	workspaceToSurface := make(map[string]string)
	workspaceUpdated := make(map[string]time.Time)
	workspaceLifecycles := make(map[string]string)
	// A workspace can have historical sessions in both hook stores. Keep the
	// recency guard outside the per-store loop so map iteration order and
	// claude/codex source order cannot choose an older terminal surface.
	bestSurface := make(map[string]time.Time)
	for source, store := range stores {
		for workspaceID, ref := range store.ActiveByWorkspace {
			if ref.SessionID == "" {
				continue
			}
			workstreamID := ref.SessionID
			if !strings.HasPrefix(workstreamID, source+"-") {
				workstreamID = source + "-" + workstreamID
			}
			workstreamToWorkspace[workstreamID] = workspaceID
			workspaceUpdated[workspaceID] = unixFloatTime(ref.UpdatedAt)
		}
		for _, session := range store.Sessions {
			if session.WorkspaceID == "" {
				continue
			}
			if session.SurfaceID != "" {
				surfaceToWorkspace[session.SurfaceID] = session.WorkspaceID
				updated := unixFloatTime(session.UpdatedAt)
				if _, chosen := workspaceToSurface[session.WorkspaceID]; !chosen || updated.After(bestSurface[session.WorkspaceID]) {
					bestSurface[session.WorkspaceID] = updated
					workspaceToSurface[session.WorkspaceID] = session.SurfaceID
					if session.AgentLifecycle != "" {
						workspaceLifecycles[session.WorkspaceID] = session.AgentLifecycle
					}
				}
			}
			if updated := unixFloatTime(session.UpdatedAt); updated.After(workspaceUpdated[session.WorkspaceID]) {
				workspaceUpdated[session.WorkspaceID] = updated
			}
		}
		for surfaceID, ref := range store.ActiveBySurface {
			for workspaceID, workspaceRef := range store.ActiveByWorkspace {
				if ref.SessionID == workspaceRef.SessionID {
					surfaceToWorkspace[surfaceID] = workspaceID
					workspaceToSurface[workspaceID] = surfaceID
					break
				}
			}
		}
	}
	m.stamps = stamps
	m.workstreamToWorkspace = workstreamToWorkspace
	m.surfaceToWorkspace = surfaceToWorkspace
	m.workspaceToSurface = workspaceToSurface
	m.workspaceUpdated = workspaceUpdated
	m.workspaceLifecycles = workspaceLifecycles
	m.initialized = true
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
