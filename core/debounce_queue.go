package core

import (
	"sync"
	"time"
)

// DebounceMessage is the unit accumulated by the DebounceQueue.
type DebounceMessage struct {
	SessionKey string
	Platform   string
	Content    string
	UserID     string
	UserName   string
	ReplyCtx   any
	ReceivedAt time.Time
}

// FlushFunc is called when a debounce window expires and the scope is unblocked.
type FlushFunc func(scope string, batch []DebounceMessage)

// DebounceQueue accumulates messages per scope within a quiet window, then
// flushes them as a single batch. Designed as a pre-processing layer that
// merges rapid-fire user messages BEFORE they reach the engine's per-session
// pendingMessages queue. While a scope is blocked (agent running), messages
// keep accumulating but no flush fires until unblock re-arms the timer.
type DebounceQueue struct {
	mu      sync.Mutex
	entries map[string]*debounceEntry
	blocked map[string]bool
	delay   time.Duration
	onFlush FlushFunc
}

type debounceEntry struct {
	messages []DebounceMessage
	timer    *time.Timer
}

// NewDebounceQueue creates a queue with the given quiet-window duration.
func NewDebounceQueue(delay time.Duration, onFlush FlushFunc) *DebounceQueue {
	return &DebounceQueue{
		entries: make(map[string]*debounceEntry),
		blocked: make(map[string]bool),
		delay:   delay,
		onFlush: onFlush,
	}
}

// Push adds a message to the scope's queue. Resets the debounce timer
// unless the scope is blocked. Returns the current queue depth.
func (q *DebounceQueue) Push(scope string, msg DebounceMessage) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, ok := q.entries[scope]
	if ok {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entry.messages = append(entry.messages, msg)
		if !q.blocked[scope] {
			entry.timer = q.armTimer(scope)
		} else {
			entry.timer = nil
		}
	} else {
		entry = &debounceEntry{messages: []DebounceMessage{msg}}
		if !q.blocked[scope] {
			entry.timer = q.armTimer(scope)
		}
		q.entries[scope] = entry
	}
	return len(entry.messages)
}

// Block pauses the debounce timer for a scope. Messages continue to accumulate.
func (q *DebounceQueue) Block(scope string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.blocked[scope] {
		return
	}
	q.blocked[scope] = true
	if entry, ok := q.entries[scope]; ok && entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
}

// Unblock resumes the debounce timer. If messages are queued, arms a fresh window.
func (q *DebounceQueue) Unblock(scope string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.blocked[scope] {
		return
	}
	delete(q.blocked, scope)
	entry, ok := q.entries[scope]
	if !ok || len(entry.messages) == 0 {
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timer = q.armTimer(scope)
}

// Cancel removes all pending messages for a scope. Returns the cancelled batch.
func (q *DebounceQueue) Cancel(scope string) []DebounceMessage {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, ok := q.entries[scope]
	if !ok {
		return nil
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	delete(q.entries, scope)
	delete(q.blocked, scope)
	return entry.messages
}

// CancelAll drains all scopes. Used on shutdown.
func (q *DebounceQueue) CancelAll() {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, entry := range q.entries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}
	q.entries = make(map[string]*debounceEntry)
	q.blocked = make(map[string]bool)
}

// Pending returns the number of queued messages for a scope.
func (q *DebounceQueue) Pending(scope string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, ok := q.entries[scope]
	if !ok {
		return 0
	}
	return len(entry.messages)
}

func (q *DebounceQueue) armTimer(scope string) *time.Timer {
	return time.AfterFunc(q.delay, func() {
		q.flush(scope)
	})
}

func (q *DebounceQueue) flush(scope string) {
	q.mu.Lock()
	if q.blocked[scope] {
		q.mu.Unlock()
		return
	}
	entry, ok := q.entries[scope]
	if !ok || len(entry.messages) == 0 {
		q.mu.Unlock()
		return
	}
	batch := entry.messages
	delete(q.entries, scope)
	q.mu.Unlock()

	q.onFlush(scope, batch)
}
