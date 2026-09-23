package cmux

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/agent/internal/termdiff"
	"github.com/chenhg5/cc-connect/core"
)

type externalRegistration struct {
	notify    func(string)
	once      sync.Once
	cancelled atomic.Bool
}

type cmuxSession struct {
	client        *client
	bus           *eventBus
	feed          *feedBridge
	workspaceID   string
	surfaceID     string
	workDir       string
	pollInterval  time.Duration
	feedReplyMode string

	ctx    context.Context
	cancel context.CancelFunc
	alive  atomic.Bool

	// dispatcher is the sole writer and closer of events. All producers write
	// emitCh; teardown unregisters producers before cancelling the dispatcher.
	events chan core.Event
	emitCh chan core.Event

	// hookEvents is owned by cmuxSession and intentionally never closed;
	// producers stop via session/bus unregistration and context cancellation.
	hookEvents chan string

	turnMu      sync.Mutex
	turnCancel  context.CancelFunc
	turnWG      sync.WaitGroup
	baseline    string
	lastEmitted string

	notifyMu      sync.Mutex
	notifiers     map[string]*externalRegistration
	resolvedNotes map[string]string

	closeOnce sync.Once
}

func newCmuxSession(parent context.Context, c *client, bus *eventBus, workspaceID, surfaceID, workDir, feedReplyMode string, pollInterval time.Duration) *cmuxSession {
	ctx, cancel := context.WithCancel(parent)
	s := &cmuxSession{
		client:        c,
		bus:           bus,
		feed:          bus.feed,
		workspaceID:   workspaceID,
		surfaceID:     surfaceID,
		workDir:       workDir,
		pollInterval:  pollInterval,
		feedReplyMode: feedReplyMode,
		ctx:           ctx,
		cancel:        cancel,
		events:        make(chan core.Event, 128),
		emitCh:        make(chan core.Event, 128),
		hookEvents:    make(chan string, 16),
		notifiers:     make(map[string]*externalRegistration),
		resolvedNotes: make(map[string]string),
	}
	s.alive.Store(true)
	go s.dispatchEvents()
	return s
}

func (s *cmuxSession) dispatchEvents() {
	defer close(s.events)
	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.emitCh:
			select {
			case <-s.ctx.Done():
				return
			case s.events <- event:
			}
		}
	}
}

func (s *cmuxSession) emit(event core.Event) {
	select {
	case <-s.ctx.Done():
	case s.emitCh <- event:
	}
}

func (s *cmuxSession) onHookFrame(name string) {
	select {
	case <-s.ctx.Done():
	case s.hookEvents <- name:
	default:
		slog.Warn("cmux: dropping hook event for slow session", "workspace_id", s.workspaceID, "hook", name)
	}
}

func (s *cmuxSession) Send(prompt, messageID string, _ []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.alive.Load() {
		return fmt.Errorf("cmux: session is closed")
	}
	if len(files) > 0 {
		paths := core.SaveFilesToDisk(s.workDir, messageID, files)
		if len(paths) > 0 {
			prompt += "\n# files: " + strings.Join(paths, ", ")
		}
	}

	s.cancelTurnAndWait()
	baseline, err := s.client.readScreen(s.ctx, s.workspaceID, s.surfaceID)
	if err != nil {
		return fmt.Errorf("cmux: capture send baseline: %w", err)
	}
drain:
	for {
		select {
		case <-s.hookEvents:
		default:
			break drain
		}
	}
	turnCtx, turnCancel := context.WithCancel(s.ctx)
	s.turnMu.Lock()
	s.baseline = baseline
	s.lastEmitted = baseline
	s.turnCancel = turnCancel
	s.turnMu.Unlock()
	if err := s.client.send(s.ctx, s.workspaceID, s.surfaceID, prompt); err != nil {
		turnCancel()
		return fmt.Errorf("cmux: send prompt: %w", err)
	}
	if err := s.client.sendKey(s.ctx, s.workspaceID, s.surfaceID, "Enter"); err != nil {
		turnCancel()
		return fmt.Errorf("cmux: submit prompt: %w", err)
	}
	s.turnWG.Add(1)
	go func() {
		defer s.turnWG.Done()
		s.watchTurn(turnCtx)
	}()
	return nil
}

func (s *cmuxSession) cancelTurnAndWait() {
	s.turnMu.Lock()
	cancel := s.turnCancel
	s.turnCancel = nil
	s.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.turnWG.Wait()
}

func (s *cmuxSession) watchTurn(ctx context.Context) {
	interval := s.pollInterval / 5
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	intervalMs := max(1, int(interval/time.Millisecond))
	stableThreshold := max(10, 5000/intervalMs)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	stable := 0
	for {
		select {
		case <-ctx.Done():
			return
		case hook := <-s.hookEvents:
			if hook == "Stop" {
				s.finish(ctx)
				return
			}
			stable = 0
		case <-ticker.C:
			current, err := s.client.readScreen(ctx, s.workspaceID, s.surfaceID)
			if err != nil {
				slog.Warn("cmux: read screen while watching turn failed", "workspace_id", s.workspaceID, "err", err)
				continue
			}
			changed := s.emitDelta(current)
			if changed || s.feed.hasOutstanding(s) {
				stable = 0
			} else {
				stable++
			}
			if stable >= stableThreshold && current != s.baselineSnapshot() {
				s.finish(ctx)
				return
			}
		}
	}
}

func (s *cmuxSession) baselineSnapshot() string {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.baseline
}

func (s *cmuxSession) emitDelta(current string) bool {
	s.turnMu.Lock()
	previous := s.lastEmitted
	if current == previous {
		s.turnMu.Unlock()
		return false
	}
	s.lastEmitted = current
	s.turnMu.Unlock()
	delta := termdiff.ExtractNew(termdiff.Normalize(previous), termdiff.Normalize(current))
	if delta != "" {
		s.emit(core.Event{Type: core.EventText, Content: delta})
	}
	return true
}

func (s *cmuxSession) finish(ctx context.Context) {
	if current, err := s.client.readScreen(ctx, s.workspaceID, s.surfaceID); err == nil {
		s.emitDelta(current)
	} else {
		slog.Warn("cmux: final screen read failed", "workspace_id", s.workspaceID, "err", err)
	}
	s.emit(core.Event{Type: core.EventResult, Done: true})
}

func (s *cmuxSession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !s.alive.Load() {
		return fmt.Errorf("cmux: session is closed")
	}
	mode := "deny"
	if result.Behavior == "allow" {
		mode = s.feedReplyMode
	}
	if err := s.feed.replyPermission(s.ctx, requestID, mode, result.UpdatedInput); err != nil {
		return fmt.Errorf("cmux: respond permission: %w", err)
	}
	return nil
}

func (s *cmuxSession) Events() <-chan core.Event { return s.events }

func (s *cmuxSession) CurrentSessionID() string { return s.workspaceID }

func (s *cmuxSession) Alive() bool { return s.alive.Load() }

func (s *cmuxSession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.cancelTurnAndWait()
		s.bus.unregister(s)
		s.feed.unregister(s)
		s.cancel()
	})
	return nil
}

func (s *cmuxSession) InjectKey(key string) error {
	if !s.alive.Load() {
		return fmt.Errorf("cmux: session is closed")
	}
	if err := s.client.sendKey(s.ctx, s.workspaceID, s.surfaceID, key); err != nil {
		return fmt.Errorf("cmux: inject key: %w", err)
	}
	return nil
}

func (s *cmuxSession) CaptureBuffer() (string, error) {
	if !s.alive.Load() {
		return "", fmt.Errorf("cmux: session is closed")
	}
	buffer, err := s.client.readScreen(s.ctx, s.workspaceID, s.surfaceID)
	if err != nil {
		return "", fmt.Errorf("cmux: capture buffer: %w", err)
	}
	return buffer, nil
}

// OnExternalResolution implements core.ExternalResolutionNotifier. Resolution
// is remembered if it races ahead of registration; notify still runs at most once.
var _ core.ExternalResolutionNotifier = (*cmuxSession)(nil)

func (s *cmuxSession) OnExternalResolution(requestID string, notify func(note string)) func() {
	if notify == nil {
		notify = func(string) {}
	}
	registration := &externalRegistration{notify: notify}
	s.notifyMu.Lock()
	note, alreadyResolved := s.resolvedNotes[requestID]
	if alreadyResolved {
		delete(s.resolvedNotes, requestID)
	} else {
		s.notifiers[requestID] = registration
	}
	s.notifyMu.Unlock()
	if alreadyResolved {
		registration.once.Do(func() { notify(note) })
	}
	var cancelOnce sync.Once
	return func() {
		cancelOnce.Do(func() {
			registration.cancelled.Store(true)
			s.notifyMu.Lock()
			if s.notifiers[requestID] == registration {
				delete(s.notifiers, requestID)
			}
			s.notifyMu.Unlock()
		})
	}
}

func (s *cmuxSession) resolveExternal(requestID, note string) {
	s.notifyMu.Lock()
	registration := s.notifiers[requestID]
	if registration != nil {
		delete(s.notifiers, requestID)
	} else {
		s.resolvedNotes[requestID] = note
	}
	s.notifyMu.Unlock()
	if registration != nil && !registration.cancelled.Load() {
		registration.once.Do(func() { registration.notify(note) })
	}
}
