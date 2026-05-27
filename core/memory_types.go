package core

import "time"

type MemoryType string

const (
	MemoryTypeUser    MemoryType = "user"
	MemoryTypeProject MemoryType = "project"
)

type MemoryCategory string

const (
	MemoryCategoryProjectConvention  MemoryCategory = "project_convention"
	MemoryCategoryImportantConclusion MemoryCategory = "important_conclusion"
	MemoryCategoryUserPreference     MemoryCategory = "user_preference"
	MemoryCategoryContextInfo        MemoryCategory = "context_info"
)

type MemoryImportance string

const (
	MemoryImportanceHigh   MemoryImportance = "high"
	MemoryImportanceMedium MemoryImportance = "medium"
	MemoryImportanceLow    MemoryImportance = "low"
)

type MemoryEntry struct {
	ID             string           `json:"id"`
	MemoryType     MemoryType       `json:"memory_type"`
	Category       MemoryCategory   `json:"category"`
	Title          string           `json:"title"`
	Content        string           `json:"content"`
	Tags           []string         `json:"tags"`
	Importance     MemoryImportance `json:"importance"`
	SessionID      string           `json:"session_id,omitempty"`
	ProjectPath    string           `json:"project_path,omitempty"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	AccessCount    int              `json:"access_count"`
	LastAccessedAt *time.Time       `json:"last_accessed_at,omitempty"`
}

type MemoryConfirmation struct {
	ConfirmationID    string        `json:"confirmation_id"`
	SessionKey        string        `json:"session_key"`
	UserID            string        `json:"user_id"`
	Platform          string        `json:"platform"`
	CWD               string        `json:"cwd"`
	CandidateMemories []MemoryEntry `json:"candidate_memories"`
	CreatedAt         time.Time     `json:"created_at"`
	ExpiresAt         time.Time     `json:"expires_at"`
}

type NotesConfig struct {
	Enabled               bool   `toml:"enabled"`
	Model                 string `toml:"model"`
	APIKey                string `toml:"api_key"`
	BaseURL               string `toml:"base_url"`
	ExtractionPrompt      string `toml:"extraction_prompt"`
	ConfirmationTimeoutMins int  `toml:"confirmation_timeout_mins"`
	MaxMemoriesPerSession int    `toml:"max_memories_per_session"`
}

type MemoryCandidate struct {
	Category   string   `json:"category"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags"`
	Importance string   `json:"importance"`
}

type MemoryCandidateList struct {
	Memories []MemoryCandidate `json:"memories"`
}

func DefaultNotesConfig() NotesConfig {
	return NotesConfig{
		Enabled:               false,
		Model:                 "claude-sonnet-4-6",
		ConfirmationTimeoutMins: 30,
		MaxMemoriesPerSession: 10,
	}
}
