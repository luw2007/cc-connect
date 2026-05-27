package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessRegistry_RegisterAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "procs.json")
	r := NewProcessRegistry(path)

	entry, err := r.Register("myproject", "claudecode", "/tmp/data", "v1.0.0")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if entry.ID == "" || len(entry.ID) != 8 {
		t.Fatalf("entry.ID = %q, want 8-char hex", entry.ID)
	}
	if entry.PID != os.Getpid() {
		t.Fatalf("PID = %d, want %d", entry.PID, os.Getpid())
	}

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List() = %d entries, want 1", len(list))
	}
	if list[0].ID != entry.ID {
		t.Fatalf("listed ID = %q, want %q", list[0].ID, entry.ID)
	}
}

func TestProcessRegistry_Unregister(t *testing.T) {
	path := filepath.Join(t.TempDir(), "procs.json")
	r := NewProcessRegistry(path)

	entry, _ := r.Register("proj", "codex", "/tmp", "v2.0.0")
	if err := r.Unregister(entry.ID); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	list := r.List()
	if len(list) != 0 {
		t.Fatalf("List() after unregister = %d, want 0", len(list))
	}
}

func TestProcessRegistry_PrunesDeadPIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "procs.json")
	r := NewProcessRegistry(path)

	entry, _ := r.Register("proj", "cc", "/tmp", "v1")

	// Manually inject a dead PID entry
	r.mu.Lock()
	live := r.readAndPrune()
	dead := ProcessEntry{
		ID:        "deadbeef",
		PID:       999999999,
		Project:   "proj",
		Agent:     "cc",
		DataDir:   "/tmp",
		StartedAt: "2024-01-01T00:00:00Z",
		Version:   "v0",
	}
	live = append(live, dead)
	_ = r.writeAtomic(live)
	r.mu.Unlock()

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List() = %d, want 1 (dead should be pruned)", len(list))
	}
	if list[0].ID != entry.ID {
		t.Fatalf("surviving entry = %q, want %q", list[0].ID, entry.ID)
	}
}

func TestProcessRegistry_SameProjectOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "procs.json")
	r := NewProcessRegistry(path)

	r.Register("proj-a", "cc", "/tmp/a", "v1")
	r.Register("proj-b", "cc", "/tmp/b", "v1")

	others := r.SameProjectOthers("proj-a", os.Getpid())
	if len(others) != 0 {
		t.Fatalf("SameProjectOthers should exclude own PID, got %d", len(others))
	}
}

func TestProcessRegistry_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "nested", "dir")
	path := filepath.Join(subdir, "procs.json")
	r := NewProcessRegistry(path)

	_, err := r.Register("p", "a", "/d", "v1")
	if err != nil {
		t.Fatalf("Register with nested dir: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry file not created: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(subdir, "*.tmp-*"))
	if len(matches) > 0 {
		t.Fatalf("leftover tmp files: %v", matches)
	}
}

func TestPidAlive_CurrentProcess(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("current process should be alive")
	}
}

func TestPidAlive_DeadPID(t *testing.T) {
	if pidAlive(999999999) {
		t.Fatal("PID 999999999 should not be alive")
	}
}

func TestPidAlive_InvalidPID(t *testing.T) {
	if pidAlive(0) {
		t.Fatal("PID 0 should not be alive")
	}
	if pidAlive(-1) {
		t.Fatal("PID -1 should not be alive")
	}
}
