package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ProcessEntry represents a running cc-connect instance in the process registry.
type ProcessEntry struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	Project   string `json:"project"`
	Agent     string `json:"agent"`
	DataDir   string `json:"data_dir"`
	StartedAt string `json:"started_at"`
	Version   string `json:"version"`
}

type processRegistryFile struct {
	Entries []ProcessEntry `json:"entries"`
}

// ProcessRegistry manages a multi-instance process registry with PID liveness
// detection and atomic file writes. Enables conflict detection when multiple
// cc-connect instances target the same project.
type ProcessRegistry struct {
	path string
	mu   sync.Mutex
}

// NewProcessRegistry creates a registry backed by the given file path.
func NewProcessRegistry(path string) *ProcessRegistry {
	return &ProcessRegistry{path: path}
}

// Register adds the current process to the registry. Returns the entry
// representing this process. Caller must call Unregister on shutdown.
func (r *ProcessRegistry) Register(project, agent, dataDir, version string) (*ProcessEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	live := r.readAndPrune()
	entry := ProcessEntry{
		ID:        generateProcessID(),
		PID:       os.Getpid(),
		Project:   project,
		Agent:     agent,
		DataDir:   dataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Version:   version,
	}
	live = append(live, entry)
	if err := r.writeAtomic(live); err != nil {
		return nil, fmt.Errorf("process registry: register: %w", err)
	}
	return &entry, nil
}

// Unregister removes an entry by ID. No-op if not found.
func (r *ProcessRegistry) Unregister(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	live := r.readAndPrune()
	filtered := make([]ProcessEntry, 0, len(live))
	for _, e := range live {
		if e.ID != id {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == len(live) {
		return nil
	}
	return r.writeAtomic(filtered)
}

// List returns all living entries, pruning dead PIDs.
func (r *ProcessRegistry) List() []ProcessEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readAndPrune()
}

// SameProjectOthers returns living entries with the same project name,
// excluding the given PID (typically os.Getpid()).
func (r *ProcessRegistry) SameProjectOthers(project string, excludePID int) []ProcessEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result []ProcessEntry
	for _, e := range r.readAndPrune() {
		if e.Project == project && e.PID != excludePID {
			result = append(result, e)
		}
	}
	return result
}

func (r *ProcessRegistry) readAndPrune() []ProcessEntry {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil
	}
	var rf processRegistryFile
	if json.Unmarshal(data, &rf) != nil {
		return nil
	}
	live := make([]ProcessEntry, 0, len(rf.Entries))
	for _, e := range rf.Entries {
		if pidAlive(e.PID) {
			live = append(live, e)
		}
	}
	return live
}

func (r *ProcessRegistry) writeAtomic(entries []ProcessEntry) error {
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", r.path, os.Getpid())
	data, err := json.MarshalIndent(processRegistryFile{Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func generateProcessID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
