package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type MemoryStorage struct {
	dataDir string
	mu      sync.RWMutex
}

func NewMemoryStorage(dataDir string) *MemoryStorage {
	return &MemoryStorage{dataDir: dataDir}
}

func (s *MemoryStorage) Save(userID string, memType MemoryType, entry MemoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.ID == "" {
		entry.ID = fmt.Sprintf("mem_%d", time.Now().UnixNano())
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	entry.UpdatedAt = time.Now()
	entry.MemoryType = memType

	path := s.memoriesFilePath(string(memType), userID)
	entries, err := s.readMemoriesFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("memory storage: read: %w", err)
	}

	entries = append(entries, entry)
	return s.writeMemoriesFile(path, entries)
}

func (s *MemoryStorage) List(userID string, memType MemoryType, projectPath string, limit int) ([]MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.memoriesFilePath(string(memType), userID)
	entries, err := s.readMemoriesFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory storage: list: %w", err)
	}

	if projectPath != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.ProjectPath == projectPath {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	return entries, nil
}

func (s *MemoryStorage) Get(userID, memoryID string) (*MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, memType := range []MemoryType{MemoryTypeUser, MemoryTypeProject} {
		path := s.memoriesFilePath(string(memType), userID)
		entries, err := s.readMemoriesFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("memory storage: get: %w", err)
		}
		for i := range entries {
			if entries[i].ID == memoryID {
				return &entries[i], nil
			}
		}
	}

	return nil, fmt.Errorf("memory storage: entry %q not found", memoryID)
}

func (s *MemoryStorage) Delete(userID, memoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, memType := range []MemoryType{MemoryTypeUser, MemoryTypeProject} {
		path := s.memoriesFilePath(string(memType), userID)
		entries, err := s.readMemoriesFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("memory storage: delete read: %w", err)
		}
		for i, e := range entries {
			if e.ID == memoryID {
				entries = append(entries[:i], entries[i+1:]...)
				return s.writeMemoriesFile(path, entries)
			}
		}
	}

	return fmt.Errorf("memory storage: entry %q not found", memoryID)
}

func (s *MemoryStorage) Clear(userID string, memType MemoryType, projectPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.memoriesFilePath(string(memType), userID)
	entries, err := s.readMemoriesFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("memory storage: clear read: %w", err)
	}

	if projectPath == "" {
		entries = nil
	} else {
		filtered := entries[:0]
		for _, e := range entries {
			if e.ProjectPath != projectPath {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	return s.writeMemoriesFile(path, entries)
}

func (s *MemoryStorage) Search(userID, query string) ([]MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(query)
	var results []MemoryEntry

	for _, memType := range []MemoryType{MemoryTypeUser, MemoryTypeProject} {
		path := s.memoriesFilePath(string(memType), userID)
		entries, err := s.readMemoriesFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("memory storage: search: %w", err)
		}
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Title), q) ||
				strings.Contains(strings.ToLower(e.Content), q) ||
				tagsContain(e.Tags, q) {
				results = append(results, e)
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].CreatedAt.After(results[j].CreatedAt)
	})

	return results, nil
}

func tagsContain(tags []string, q string) bool {
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return false
}

func (s *MemoryStorage) memoriesFilePath(memType, userID string) string {
	return filepath.Join(s.dataDir, memType, userID, "memories.json")
}

func (s *MemoryStorage) readMemoriesFile(path string) ([]MemoryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []MemoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("memory storage: unmarshal %s: %w", path, err)
	}
	return entries, nil
}

func (s *MemoryStorage) writeMemoriesFile(path string, entries []MemoryEntry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("memory storage: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("memory storage: marshal: %w", err)
	}
	return AtomicWriteFile(path, data, 0644)
}
