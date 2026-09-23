package cmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type streamFrame struct {
	Seq         int64           `json:"seq"`
	Name        string          `json:"name"`
	Category    string          `json:"category"`
	WorkspaceID string          `json:"workspace_id"`
	SurfaceID   string          `json:"surface_id"`
	Source      string          `json:"source"`
	OccurredAt  string          `json:"occurred_at"`
	Payload     json.RawMessage `json:"payload"`
}

type streamResume struct {
	AfterSeq          int64  `json:"after_seq"`
	RequestedAfterSeq int64  `json:"requested_after_seq"`
	Gap               bool   `json:"gap"`
	GapReason         string `json:"gap_reason"`
	OldestSeq         int64  `json:"oldest_seq"`
	LatestSeq         int64  `json:"latest_seq"`
	NextSeq           int64  `json:"next_seq"`
}

type streamAck struct {
	Type                     string       `json:"type"`
	BootID                   string       `json:"boot_id"`
	SubscriptionID           string       `json:"subscription_id"`
	HeartbeatIntervalSeconds int          `json:"heartbeat_interval_seconds"`
	ReplayCount              int          `json:"replay_count"`
	Resume                   streamResume `json:"resume"`
}

type eventCursor struct {
	BootID string `json:"boot_id"`
	Seq    int64  `json:"seq"`
}

// eventBus is shared process-wide per resolved socket. Its run goroutine owns
// the streaming connection; ordinary RPCs always use client.call connections.
type eventBus struct {
	client     *client
	cursorPath string
	feed       *feedBridge

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.RWMutex
	cursor       eventCursor
	sessions     map[string]*cmuxSession
	lastActivity map[string]time.Time

	stopOnce sync.Once
	done     chan struct{}
}

type sharedBusEntry struct {
	bus        *eventBus
	references int
}

var sharedBuses = struct {
	sync.Mutex
	entries map[string]*sharedBusEntry
}{entries: make(map[string]*sharedBusEntry)}

func acquireEventBus(c *client, cursorPath string, hookTimeout time.Duration, mapper *sessionMapper) (*eventBus, func()) {
	sharedBuses.Lock()
	if entry := sharedBuses.entries[c.socketPath]; entry != nil {
		entry.references++
		if entry.bus.cursorPath != cursorPath {
			slog.Warn("cmux: shared event bus ignores differing cursor path", "socket_path", c.socketPath, "active_cursor_file", entry.bus.cursorPath, "requested_cursor_file", cursorPath)
		}
		if entry.bus.feed.hookTimeout != hookTimeout {
			slog.Warn("cmux: shared event bus ignores differing feed hook timeout", "socket_path", c.socketPath, "active_timeout", entry.bus.feed.hookTimeout, "requested_timeout", hookTimeout)
		}
		if entry.bus.feed.mapper != mapper {
			slog.Warn("cmux: shared event bus ignores differing session mapper", "socket_path", c.socketPath)
		}
		bus := entry.bus
		sharedBuses.Unlock()
		return bus, releaseEventBus(c.socketPath, bus)
	}
	bus := newEventBus(c, cursorPath, hookTimeout, mapper)
	sharedBuses.entries[c.socketPath] = &sharedBusEntry{bus: bus, references: 1}
	sharedBuses.Unlock()
	go bus.run()
	return bus, releaseEventBus(c.socketPath, bus)
}

func releaseEventBus(socketPath string, bus *eventBus) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			sharedBuses.Lock()
			entry := sharedBuses.entries[socketPath]
			if entry == nil || entry.bus != bus {
				sharedBuses.Unlock()
				return
			}
			entry.references--
			if entry.references > 0 {
				sharedBuses.Unlock()
				return
			}
			delete(sharedBuses.entries, socketPath)
			bus.stop()
			sharedBuses.Unlock()
		})
	}
}

func newEventBus(c *client, cursorPath string, hookTimeout time.Duration, mapper *sessionMapper) *eventBus {
	ctx, cancel := context.WithCancel(context.Background())
	bus := &eventBus{
		client:       c,
		cursorPath:   cursorPath,
		ctx:          ctx,
		cancel:       cancel,
		sessions:     make(map[string]*cmuxSession),
		lastActivity: make(map[string]time.Time),
		done:         make(chan struct{}),
	}
	bus.loadCursor()
	bus.feed = newFeedBridge(ctx, c, hookTimeout, mapper)
	return bus
}

func (b *eventBus) register(s *cmuxSession) {
	b.mu.Lock()
	b.sessions[s.workspaceID] = s
	b.mu.Unlock()
}

func (b *eventBus) unregister(s *cmuxSession) {
	b.mu.Lock()
	if b.sessions[s.workspaceID] == s {
		delete(b.sessions, s.workspaceID)
		delete(b.lastActivity, s.workspaceID)
	}
	b.mu.Unlock()
}

func (b *eventBus) stop() {
	b.stopOnce.Do(func() {
		b.cancel()
		<-b.done
	})
}

func (b *eventBus) run() {
	defer close(b.done)
	persistTicker := time.NewTicker(time.Second)
	defer persistTicker.Stop()
	defer b.saveCursor()
	defer b.feed.close()

	backoff := 200 * time.Millisecond
	for {
		streamStarted := time.Now()
		streamDone := make(chan error, 1)
		go func() { streamDone <- b.connectOnce(b.ctx) }()
		streamActive := true
		for streamActive {
			select {
			case <-b.ctx.Done():
				<-streamDone
				return
			case <-persistTicker.C:
				b.saveCursor()
			case err := <-streamDone:
				streamActive = false
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("cmux: events stream disconnected", "socket_path", b.client.socketPath, "err", err, "retry_in", backoff)
				}
			}
		}
		if time.Since(streamStarted) >= 30*time.Second {
			backoff = 200 * time.Millisecond
		}

		wait := time.NewTimer(backoff)
		select {
		case <-b.ctx.Done():
			wait.Stop()
			return
		case <-persistTicker.C:
			b.saveCursor()
			if !wait.Stop() {
				<-wait.C
			}
		case <-wait.C:
		}
		backoff *= 2
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}

func (b *eventBus) connectOnce(ctx context.Context) error {
	conn, reader, err := b.client.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	closeOnCancel := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closeOnCancel:
		}
	}()
	defer close(closeOnCancel)

	b.mu.RLock()
	afterSeq := b.cursor.Seq
	b.mu.RUnlock()
	id := fmt.Sprintf("stream-%d", rpcCounter.Add(1))
	request, err := json.Marshal(rpcRequest{ID: id, Method: methodEventsStream, Params: map[string]any{
		"after_seq":  afterSeq,
		"categories": []string{"feed", "agent"},
	}})
	if err != nil {
		return fmt.Errorf("cmux: marshal events.stream request: %w", err)
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return fmt.Errorf("cmux: write events.stream request: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	ackLine, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("cmux: read events.stream ack: %w", err)
	}
	ack, err := decodeStreamAck(ackLine)
	if err != nil {
		return err
	}
	gap := b.applyAck(ack)
	if gap {
		b.saveCursor()
		if err := b.feed.resync(ctx); err != nil {
			slog.Warn("cmux: feed resync after stream gap failed", "gap_reason", ack.Resume.GapReason, "err", err)
		}
	}
	if ack.HeartbeatIntervalSeconds > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Duration(ack.HeartbeatIntervalSeconds) * time.Second))
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("cmux: events.stream closed: %w", err)
			}
			return fmt.Errorf("cmux: read events.stream frame: %w", err)
		}
		if ack.HeartbeatIntervalSeconds > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Duration(ack.HeartbeatIntervalSeconds) * time.Second))
		}
		var frame streamFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			slog.Warn("cmux: malformed events.stream frame", "err", err)
			continue
		}
		if frame.Seq == 0 {
			continue
		}
		b.mu.Lock()
		if frame.Seq <= b.cursor.Seq {
			b.mu.Unlock()
			continue
		}
		b.cursor.Seq = frame.Seq
		b.mu.Unlock()
		b.dispatch(frame)
	}
}

func decodeStreamAck(line []byte) (streamAck, error) {
	var ack streamAck
	if err := json.Unmarshal(line, &ack); err == nil && ack.Type == "ack" {
		return ack, nil
	}
	var response rpcResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return ack, fmt.Errorf("cmux: decode events.stream ack: %w", err)
	}
	if response.Error != nil {
		return ack, fmt.Errorf("cmux: events.stream: %w", response.Error)
	}
	if err := json.Unmarshal(response.Result, &ack); err != nil {
		return ack, fmt.Errorf("cmux: decode events.stream ack result: %w", err)
	}
	if ack.Type != "ack" {
		return ack, fmt.Errorf("cmux: events.stream: expected ack, got %q", ack.Type)
	}
	return ack, nil
}

func (b *eventBus) applyAck(ack streamAck) bool {
	b.mu.Lock()
	knownBootID := b.cursor.BootID
	gap := ack.Resume.Gap || (knownBootID != "" && ack.BootID != knownBootID)
	b.cursor.BootID = ack.BootID
	if gap {
		b.cursor.Seq = ack.Resume.AfterSeq
	}
	b.mu.Unlock()
	if gap {
		slog.Warn("cmux: events stream resume gap", "socket_path", b.client.socketPath, "gap_reason", ack.Resume.GapReason, "after_seq", ack.Resume.AfterSeq, "boot_id", ack.BootID)
	}
	return gap
}

func (b *eventBus) dispatch(frame streamFrame) {
	if frame.Category == "feed" {
		b.feed.trigger()
		return
	}
	if frame.Category != "agent" || !strings.HasPrefix(frame.Name, "agent.hook.") {
		return
	}
	workspaceID := frame.WorkspaceID
	tried := []string{"workspace_id=" + frame.WorkspaceID, "surface_id=" + frame.SurfaceID}
	if workspaceID == "" && frame.SurfaceID != "" {
		if mapped, ok := b.feed.mapper.workspaceIDForSurface(frame.SurfaceID); ok {
			workspaceID = mapped
		}
	}
	b.mu.Lock()
	session := b.sessions[workspaceID]
	if session != nil {
		if occurredAt, err := time.Parse(time.RFC3339Nano, frame.OccurredAt); err == nil {
			b.lastActivity[workspaceID] = occurredAt
		}
	}
	b.mu.Unlock()
	if session == nil {
		slog.Warn("cmux: unmatched agent hook event", "event", frame.Name, "tried_keys", tried)
		return
	}
	session.onHookFrame(strings.TrimPrefix(frame.Name, "agent.hook."))
}

func (b *eventBus) activity(workspaceID string) time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastActivity[workspaceID]
}

func (b *eventBus) loadCursor() {
	data, err := os.ReadFile(b.cursorPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cmux: read events cursor failed", "path", b.cursorPath, "err", err)
		}
		return
	}
	var cursor eventCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		slog.Warn("cmux: decode events cursor failed", "path", b.cursorPath, "err", err)
		return
	}
	b.cursor = cursor
}

func (b *eventBus) saveCursor() {
	b.mu.RLock()
	cursor := b.cursor
	b.mu.RUnlock()
	data, err := json.Marshal(cursor)
	if err != nil {
		return
	}
	dir := filepath.Dir(b.cursorPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("cmux: create events cursor directory failed", "path", dir, "err", err)
		return
	}
	temp, err := os.CreateTemp(dir, ".cmux-events-cursor-*")
	if err != nil {
		slog.Warn("cmux: create temporary events cursor failed", "path", b.cursorPath, "err", err)
		return
	}
	tempName := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		slog.Warn("cmux: chmod temporary events cursor failed", "path", tempName, "err", err)
		return
	}
	if _, err := temp.Write(data); err != nil {
		slog.Warn("cmux: write temporary events cursor failed", "path", tempName, "err", err)
		return
	}
	if err := temp.Close(); err != nil {
		slog.Warn("cmux: close temporary events cursor failed", "path", tempName, "err", err)
		return
	}
	if err := os.Rename(tempName, b.cursorPath); err != nil {
		slog.Warn("cmux: replace events cursor failed", "path", b.cursorPath, "err", err)
		return
	}
	removeTemp = false
}
