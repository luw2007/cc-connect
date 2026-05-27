package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type MemoryExtractor struct {
	config      NotesConfig
	storage     *MemoryStorage
	httpClient  *http.Client
	pending     sync.Map
	cleanupOnce sync.Once
}

func NewMemoryExtractor(config NotesConfig, storage *MemoryStorage) *MemoryExtractor {
	return &MemoryExtractor{
		config:     config,
		storage:    storage,
		httpClient: HTTPClient,
	}
}

func (m *MemoryExtractor) TriggerExtraction(ctx context.Context, sessionKey string, p Platform, replyCtx any, history []HistoryEntry, userID, projectPath string) {
	if !m.config.Enabled {
		return
	}

	if len(history) > 20 {
		history = history[len(history)-20:]
	}

	prompt := m.buildExtractionPrompt(history, projectPath)
	candidates, err := m.callClaudeAPI(ctx, prompt)
	if err != nil {
		slog.Warn("notes: extraction failed", "error", err, "session", sessionKey)
		return
	}
	if len(candidates) == 0 {
		return
	}

	max := m.config.MaxMemoriesPerSession
	if max <= 0 {
		max = 10
	}
	if len(candidates) > max {
		candidates = candidates[:max]
	}

	confirmationID := generateConfirmationID()
	now := time.Now()
	timeoutMins := m.config.ConfirmationTimeoutMins
	if timeoutMins <= 0 {
		timeoutMins = 30
	}

	confirmation := &MemoryConfirmation{
		ConfirmationID:    confirmationID,
		SessionKey:        sessionKey,
		UserID:            userID,
		Platform:          p.Name(),
		CWD:               projectPath,
		CandidateMemories: candidates,
		CreatedAt:         now,
		ExpiresAt:         now.Add(time.Duration(timeoutMins) * time.Minute),
	}

	m.pending.Store(confirmationID, confirmation)

	card := m.buildConfirmationCard(confirmation)
	if cs, ok := p.(CardSender); ok {
		if err := cs.SendCard(ctx, replyCtx, card); err != nil {
			slog.Warn("notes: failed to send confirmation card", "error", err)
		}
	}
}

func (m *MemoryExtractor) HandleConfirmAction(confirmationID, action string, index int) (string, error) {
	val, ok := m.pending.Load(confirmationID)
	if !ok {
		return "", fmt.Errorf("confirmation not found or expired")
	}
	confirmation := val.(*MemoryConfirmation)

	switch action {
	case "approve_all":
		saved := 0
		for _, mem := range confirmation.CandidateMemories {
			mem.CreatedBy = confirmation.UserID
			mem.SessionID = confirmation.SessionKey
			if err := m.storage.Save(confirmation.UserID, mem.MemoryType, mem); err == nil {
				saved++
			}
		}
		m.pending.Delete(confirmationID)
		return fmt.Sprintf("Saved %d memories.", saved), nil

	case "reject_all":
		m.pending.Delete(confirmationID)
		return "All candidate memories dismissed.", nil

	case "approve_single":
		if index < 0 || index >= len(confirmation.CandidateMemories) {
			return "", fmt.Errorf("invalid memory index: %d", index)
		}
		mem := confirmation.CandidateMemories[index]
		mem.CreatedBy = confirmation.UserID
		mem.SessionID = confirmation.SessionKey
		if err := m.storage.Save(confirmation.UserID, mem.MemoryType, mem); err != nil {
			return "", fmt.Errorf("notes: save memory: %w", err)
		}
		confirmation.CandidateMemories = append(confirmation.CandidateMemories[:index], confirmation.CandidateMemories[index+1:]...)
		if len(confirmation.CandidateMemories) == 0 {
			m.pending.Delete(confirmationID)
		}
		return fmt.Sprintf("Memory #%d saved.", index+1), nil

	case "reject_single":
		if index < 0 || index >= len(confirmation.CandidateMemories) {
			return "", fmt.Errorf("invalid memory index: %d", index)
		}
		confirmation.CandidateMemories = append(confirmation.CandidateMemories[:index], confirmation.CandidateMemories[index+1:]...)
		if len(confirmation.CandidateMemories) == 0 {
			m.pending.Delete(confirmationID)
		}
		return fmt.Sprintf("Memory #%d skipped.", index+1), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func (m *MemoryExtractor) StartCleanup(ctx context.Context) {
	m.cleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					now := time.Now()
					m.pending.Range(func(key, value any) bool {
						conf := value.(*MemoryConfirmation)
						if now.After(conf.ExpiresAt) {
							m.pending.Delete(key)
						}
						return true
					})
				}
			}
		}()
	})
}

func (m *MemoryExtractor) callClaudeAPI(ctx context.Context, prompt string) ([]MemoryEntry, error) {
	baseURL := m.config.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	model := m.config.Model
	if model == "" {
		model = "c_new"
	}

	var url string
	var bodyBytes []byte
	var isOpenAI bool

	if strings.Contains(baseURL, "anthropic.com") {
		url = strings.TrimRight(baseURL, "/") + "/v1/messages"
		reqBody := map[string]any{
			"model":      model,
			"max_tokens": 4096,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
		}
		bodyBytes, _ = json.Marshal(reqBody)
	} else {
		isOpenAI = true
		url = strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
		reqBody := map[string]any{
			"model":      model,
			"max_tokens": 4096,
			"messages": []map[string]any{
				{"role": "user", "content": prompt},
			},
		}
		bodyBytes, _ = json.Marshal(reqBody)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("notes: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if isOpenAI {
		req.Header.Set("Authorization", "Bearer "+m.config.APIKey)
	} else {
		req.Header.Set("x-api-key", m.config.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notes: llm api call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("notes: llm api status %d: %s", resp.StatusCode, string(body))
	}

	var text string
	if isOpenAI {
		var apiResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("notes: decode api response: %w", err)
		}
		if len(apiResp.Choices) == 0 {
			return nil, nil
		}
		text = apiResp.Choices[0].Message.Content
	} else {
		var apiResp struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("notes: decode api response: %w", err)
		}
		if len(apiResp.Content) == 0 {
			return nil, nil
		}
		text = apiResp.Content[0].Text
	}
	text = strings.TrimSpace(text)
	if start := strings.Index(text, "{"); start > 0 {
		text = text[start:]
	}
	if end := strings.LastIndex(text, "}"); end >= 0 && end < len(text)-1 {
		text = text[:end+1]
	}

	var candidateList MemoryCandidateList
	if err := json.Unmarshal([]byte(text), &candidateList); err != nil {
		return nil, fmt.Errorf("notes: parse extraction result: %w", err)
	}

	now := time.Now()
	var entries []MemoryEntry
	for _, c := range candidateList.Memories {
		memType := MemoryTypeProject
		if c.Category == "user_preference" {
			memType = MemoryTypeUser
		}
		entries = append(entries, MemoryEntry{
			ID:          generateConfirmationID(),
			MemoryType:  memType,
			Category:    MemoryCategory(c.Category),
			Title:       c.Title,
			Content:     c.Content,
			Tags:        c.Tags,
			Importance:  MemoryImportance(c.Importance),
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}
	return entries, nil
}

func (m *MemoryExtractor) buildExtractionPrompt(history []HistoryEntry, projectPath string) string {
	customPrompt := m.config.ExtractionPrompt
	if customPrompt != "" {
		summary := summarizeHistory(history)
		return fmt.Sprintf(customPrompt, projectPath, summary)
	}

	summary := summarizeHistory(history)
	return fmt.Sprintf(`Analyze the following conversation and extract information worth saving as long-term memories.

Working directory: %s
Conversation summary:
%s

Extract the following types of memories:
1. **project_convention**: coding standards, architecture decisions, design patterns
2. **important_conclusion**: technical decisions, problem solutions
3. **user_preference**: programming habits, preferred tools, code style preferences
4. **context_info**: project structure, key module descriptions

Return a JSON object with a "memories" array. Each memory should have:
- "category": one of "project_convention", "important_conclusion", "user_preference", "context_info"
- "title": short title
- "content": detailed content
- "tags": array of relevant tags
- "importance": one of "high", "medium", "low"

Only extract truly valuable information. If nothing is worth saving, return {"memories": []}.`, projectPath, summary)
}

func (m *MemoryExtractor) buildConfirmationCard(confirmation *MemoryConfirmation) *Card {
	card := NewCard().
		Title(fmt.Sprintf("Memory Extraction (%d items)", len(confirmation.CandidateMemories)), "blue").
		Markdownf("Extracted **%d** candidate memories from session. Please review and confirm.", len(confirmation.CandidateMemories)).
		Divider()

	for i, mem := range confirmation.CandidateMemories {
		categoryLabel := strings.ReplaceAll(string(mem.Category), "_", " ")

		content := mem.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}

		card.Markdownf("**%d. [%s] %s**\n%s", i+1, categoryLabel, mem.Title, content)

		if len(mem.Tags) > 0 {
			card.Markdown("Tags: " + strings.Join(mem.Tags, ", "))
		}

		card.Buttons(
			DefaultBtn("Approve", fmt.Sprintf("act:notes_approve %s %d", confirmation.ConfirmationID, i)),
			DefaultBtn("Skip", fmt.Sprintf("act:notes_reject %s %d", confirmation.ConfirmationID, i)),
		)

		if i < len(confirmation.CandidateMemories)-1 {
			card.Divider()
		}
	}

	card.Divider().Buttons(
		PrimaryBtn("Approve All", fmt.Sprintf("act:notes_approve_all %s", confirmation.ConfirmationID)),
		DangerBtn("Reject All", fmt.Sprintf("act:notes_reject_all %s", confirmation.ConfirmationID)),
	)

	return card.Build()
}

func summarizeHistory(history []HistoryEntry) string {
	var parts []string
	for _, entry := range history {
		content := entry.Content
		if len([]rune(content)) > 100 {
			content = string([]rune(content)[:100]) + "..."
		}
		parts = append(parts, fmt.Sprintf("%s: %s", entry.Role, content))
	}
	return strings.Join(parts, "\n")
}

func generateConfirmationID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "mem_" + hex.EncodeToString(b)
}
