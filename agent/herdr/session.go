package herdr

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

const (
	recentLines     = 2000
	minPollInterval = 100 * time.Millisecond
	waitTimeoutMs   = 30000
	statusQuietMin  = 2 * time.Second
)

type blockedRequest struct {
	requestID string
	tail      string
	question  string
	turn      *herdrTurn
}

type resolutionRegistration struct {
	notify func(string)
}

type herdrTurn struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	finishOnce      sync.Once
	failOnce        sync.Once
	outputMu        sync.Mutex
	finished        bool
	observedWorking atomic.Bool
	lastEmitted     string
	baseline        string
	statusCh        chan string
	fallbackCh      chan struct{}
}

type herdrSession struct {
	client  *client
	target  string
	workDir string
	pollInt time.Duration

	subscribeEnabled    bool
	blockedCardEnabled  bool
	streamOutputEnabled bool
	maxReadFailures     int
	statusQuietAfter    time.Duration
	stabilityThreshold  int

	events              chan core.Event
	ctx                 context.Context
	cancel              context.CancelFunc
	alive               atomic.Bool
	closeOnce           sync.Once
	sendMu              sync.Mutex
	statusCh            chan string
	subscribeFailures   chan struct{}
	subscriptionHealthy atomic.Bool
	subscriptionWG      sync.WaitGroup

	mu         sync.Mutex
	turn       *herdrTurn
	blocked    *blockedRequest
	blockedSeq int
	notifiers  map[string]*resolutionRegistration
}

func newHerdrSession(ctx context.Context, c *client, target, workDir string, pollInt time.Duration, subscribeEnabled, blockedCardEnabled, streamOutputEnabled bool, maxReadFailures int) *herdrSession {
	if pollInt < minPollInterval {
		pollInt = minPollInterval
	}
	if maxReadFailures < 1 {
		maxReadFailures = defaultMaxReadFailures
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &herdrSession{
		client:              c,
		target:              target,
		workDir:             workDir,
		pollInt:             pollInt,
		subscribeEnabled:    subscribeEnabled,
		blockedCardEnabled:  blockedCardEnabled,
		streamOutputEnabled: streamOutputEnabled,
		maxReadFailures:     maxReadFailures,
		statusQuietAfter:    max(statusQuietMin, 10*pollInt),
		stabilityThreshold:  max(10, 5000/int(pollInt.Milliseconds())),
		events:              make(chan core.Event, 128),
		ctx:                 sessionCtx,
		cancel:              cancel,
		statusCh:            make(chan string, 1),
		subscribeFailures:   make(chan struct{}, 1),
		notifiers:           make(map[string]*resolutionRegistration),
	}
	s.alive.Store(true)
	s.subscriptionHealthy.Store(subscribeEnabled)
	if subscribeEnabled {
		s.subscriptionWG.Add(2)
		go func() {
			defer s.subscriptionWG.Done()
			s.runSubscribeReader(s.ctx, s.statusCh, s.subscribeFailures)
		}()
		go func() {
			defer s.subscriptionWG.Done()
			s.dispatchSubscription()
		}()
	}
	return s
}

func (s *herdrSession) Send(prompt, messageID string, _ []core.ImageAttachment, files []core.FileAttachment) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if !s.alive.Load() {
		return fmt.Errorf("herdr: session closed")
	}
	s.mu.Lock()
	blocked := s.blocked != nil
	s.mu.Unlock()
	if blocked {
		return fmt.Errorf("herdr: target blocked")
	}
	if len(files) > 0 {
		if paths := core.SaveFilesToDisk(s.workDir, messageID, files); len(paths) > 0 {
			prompt += "\n# files: " + strings.Join(paths, ", ")
		}
	}

	// E3: cancellation alone is not a lifecycle barrier. Join every goroutine
	// from the previous turn before capturing or publishing state for this one.
	s.stopTurn()
	info, err := s.client.agentGet(s.ctx, s.target)
	if err != nil {
		return fmt.Errorf("herdr: inspect target before prompt: %w", err)
	}
	if info.AgentStatus == "blocked" {
		if s.blockedCardEnabled {
			s.handleBlocked(nil)
		}
		return fmt.Errorf("herdr: target blocked")
	}
	baseline, err := s.client.agentRead(s.ctx, s.target, "recent", recentLines)
	if err != nil {
		return fmt.Errorf("herdr: capture baseline: %w", err)
	}
	visible, err := s.client.agentRead(s.ctx, s.target, "visible", 0)
	if err != nil {
		return fmt.Errorf("herdr: capture visible baseline: %w", err)
	}
	turnCtx, turnCancel := context.WithCancel(s.ctx)
	t := &herdrTurn{
		ctx:         turnCtx,
		cancel:      turnCancel,
		lastEmitted: visible,
		baseline:    baseline,
		statusCh:    make(chan string, 1),
		fallbackCh:  make(chan struct{}, 1),
	}
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
	if err := s.client.agentPrompt(s.ctx, s.target, prompt); err != nil {
		t.cancel()
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		s.mu.Unlock()
		return fmt.Errorf("herdr: prompt: %w", err)
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		s.watchTurn(t)
	}()
	if s.streamOutputEnabled {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			s.streamOutputLoop(t)
		}()
	}
	return nil
}

func (s *herdrSession) stopTurn() {
	s.mu.Lock()
	t := s.turn
	s.turn = nil
	s.mu.Unlock()
	if t == nil {
		return
	}
	t.cancel()
	t.wg.Wait()
}

func (s *herdrSession) dispatchSubscription() {
	consecutiveFailures := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case status := <-s.statusCh:
			consecutiveFailures = 0
			s.subscriptionHealthy.Store(true)
			s.mu.Lock()
			t := s.turn
			s.mu.Unlock()
			if t != nil {
				if status == "working" || status == "blocked" {
					t.observedWorking.Store(true)
				}
				publishLatestStatus(t.ctx, t.statusCh, status)
				continue
			}
			s.handleStatusBetweenTurns(status)
		case <-s.subscribeFailures:
			consecutiveFailures++
			if consecutiveFailures < 3 {
				continue
			}
			s.subscriptionHealthy.Store(false)
			s.mu.Lock()
			t := s.turn
			s.mu.Unlock()
			if t != nil {
				notifyFailure(t.fallbackCh)
			}
		}
	}
}

func (s *herdrSession) handleStatusBetweenTurns(status string) {
	switch status {
	case "blocked":
		if s.blockedCardEnabled {
			s.handleBlocked(nil)
		}
	case "working", "idle", "done":
		s.clearBlocked(nil)
	}
}

func (s *herdrSession) watchTurn(t *herdrTurn) {
	if !s.subscribeEnabled || !s.subscriptionHealthy.Load() {
		s.watchViaWait(t)
		return
	}

	ticker := time.NewTicker(s.pollInt)
	defer ticker.Stop()
	sawWorking := t.observedWorking.Load()
	statusAware := false
	lastStatusAt := time.Time{}
	readFailures := 0
	var previous string
	stable := 0

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-t.fallbackCh:
			slog.Warn("herdr: subscription persistently failing; falling back to agent.wait", "target", s.target)
			s.watchViaWait(t)
			return
		case status := <-t.statusCh:
			lastStatusAt = time.Now()
			switch status {
			case "working":
				statusAware = true
				sawWorking = true
				s.clearBlocked(t)
			case "blocked":
				statusAware = true
				sawWorking = true
				if s.blockedCardEnabled {
					s.handleBlocked(t)
				}
			case "idle", "done":
				s.clearBlocked(t)
				if sawWorking {
					s.finish(t)
					return
				}
			}
		case <-ticker.C:
			if statusAware && time.Since(lastStatusAt) < s.statusQuietAfter {
				continue
			}
			statusAware = false
			current, err := s.client.agentRead(t.ctx, s.target, "visible", 0)
			if err != nil {
				if s.handleTurnReadFailure(t, err, &readFailures) {
					return
				}
				continue
			}
			readFailures = 0
			if current == previous {
				stable++
			} else {
				previous = current
				stable = 0
			}
			if stable >= s.stabilityThreshold {
				s.finish(t)
				return
			}
		}
	}
}

func (s *herdrSession) watchViaWait(t *herdrTurn) {
	sawWorking := t.observedWorking.Load()
	readFailures := 0
	var previous string
	stable := 0
	for t.ctx.Err() == nil {
		info, err := s.client.agentWait(t.ctx, s.target, []string{"idle", "blocked", "done"}, waitTimeoutMs)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			slog.Warn("herdr: agent.wait failed", "target", s.target, "err", err)
			if s.handleTurnReadFailure(t, err, &readFailures) {
				return
			}
			if !sleepContext(t.ctx, s.pollInt) {
				return
			}
			continue
		}
		readFailures = 0

		switch info.AgentStatus {
		case "working":
			sawWorking = true
			s.clearBlocked(t)
			if !sleepContext(t.ctx, s.pollInt) {
				return
			}
			continue
		case "blocked":
			sawWorking = true
			if s.blockedCardEnabled {
				s.handleBlocked(t)
			}
			if !sleepContext(t.ctx, s.pollInt) {
				return
			}
			continue
		case "idle", "done":
			s.clearBlocked(t)
			if sawWorking {
				s.finish(t)
				return
			}
			if !sleepContext(t.ctx, s.pollInt) {
				return
			}
		}

		current, err := s.client.agentRead(t.ctx, s.target, "visible", 0)
		if err != nil {
			if s.handleTurnReadFailure(t, err, &readFailures) {
				return
			}
			continue
		}
		readFailures = 0
		if current == previous {
			stable++
		} else {
			previous = current
			stable = 0
		}
		if stable >= s.stabilityThreshold {
			s.finish(t)
			return
		}
	}
}

func (s *herdrSession) handleTurnReadFailure(t *herdrTurn, err error, failures *int) bool {
	(*failures)++
	if !isNonTransientRPCError(err) && *failures < s.maxReadFailures {
		return false
	}
	s.failTurn(t, err)
	return true
}

func (s *herdrSession) failTurn(t *herdrTurn, err error) {
	t.failOnce.Do(func() {
		s.sendTurn(t.ctx, core.Event{Type: core.EventError, Error: err})
		s.finish(t)
	})
}

func (s *herdrSession) streamOutputLoop(t *herdrTurn) {
	ticker := time.NewTicker(s.pollInt)
	defer ticker.Stop()
	readFailures := 0
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			current, err := s.client.agentRead(t.ctx, s.target, "visible", 0)
			if err != nil {
				if s.handleTurnReadFailure(t, err, &readFailures) {
					return
				}
				continue
			}
			readFailures = 0
			s.emitDelta(t, current)
		}
	}
}

func (s *herdrSession) emitDelta(t *herdrTurn, current string) {
	t.outputMu.Lock()
	defer t.outputMu.Unlock()
	if t.finished {
		return
	}
	s.emitDeltaLocked(t, current)
}

func (s *herdrSession) emitDeltaLocked(t *herdrTurn, current string) {
	current = termdiff.NormalizeCapture(current, false)
	baseline := termdiff.NormalizeCapture(t.lastEmitted, false)
	delta := termdiff.ExtractNew(baseline, current)
	if delta == "" {
		return
	}
	t.lastEmitted = current
	s.sendTurn(t.ctx, core.Event{Type: core.EventText, Content: delta})
}

func (s *herdrSession) finish(t *herdrTurn) {
	t.finishOnce.Do(func() {
		defer t.cancel()
		if t.ctx.Err() != nil {
			return
		}
		t.outputMu.Lock()
		defer t.outputMu.Unlock()
		if t.finished {
			return
		}
		// D6: synchronously recover output printed after the final ticker tick.
		if s.streamOutputEnabled {
			if visible, err := s.client.agentRead(t.ctx, s.target, "visible", 0); err == nil {
				s.emitDeltaLocked(t, visible)
			}
		}
		s.clearBlocked(t)
		recent, err := s.client.agentRead(t.ctx, s.target, "recent", recentLines)
		t.finished = true
		if err != nil {
			slog.Warn("herdr: final agent.read failed", "target", s.target, "err", err)
			s.sendTurn(t.ctx, core.Event{Type: core.EventResult, Done: true})
			return
		}
		content := termdiff.ExtractNew(
			termdiff.NormalizeCapture(t.baseline, false),
			termdiff.NormalizeCapture(recent, false),
		)
		if content != "" {
			content = "```\n" + content + "\n```"
		}
		s.sendTurn(t.ctx, core.Event{Type: core.EventResult, Content: content, Done: true})
	})
}

func (s *herdrSession) handleBlocked(t *herdrTurn) {
	ctx := s.ctx
	if t != nil {
		ctx = t.ctx
	}
	screen, err := s.client.agentRead(ctx, s.target, "visible", 0)
	if err != nil {
		slog.Warn("herdr: read blocked screen failed", "target", s.target, "err", err)
		return
	}
	tail := clipBlockedTail(screen)
	question := buildBlockedQuestion(tail)

	s.mu.Lock()
	old := s.blocked
	if old != nil && old.turn == t {
		s.mu.Unlock()
		return
	}
	s.blockedSeq++
	requestID := fmt.Sprintf("herdr-blocked-%s-%d", s.target, s.blockedSeq)
	s.blocked = &blockedRequest{requestID: requestID, tail: tail, question: question.Question, turn: t}
	var oldReg *resolutionRegistration
	if old != nil {
		oldReg = s.notifiers[old.requestID]
		delete(s.notifiers, old.requestID)
	}
	s.mu.Unlock()

	if old != nil {
		if oldReg != nil && oldReg.notify != nil {
			oldReg.notify("")
		}
		s.sendForTurn(t, core.Event{Type: core.EventPermissionResolved, RequestID: old.requestID})
	}
	s.sendForTurn(t, core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    requestID,
		ToolName:     "AskUserQuestion",
		ToolInput:    tail,
		ToolInputRaw: map[string]any{"screen_tail": tail},
		Questions:    []core.UserQuestion{question},
	})
}

func (s *herdrSession) clearBlocked(t *herdrTurn) {
	s.mu.Lock()
	req := s.blocked
	if req == nil {
		s.mu.Unlock()
		return
	}
	s.blocked = nil
	reg := s.notifiers[req.requestID]
	delete(s.notifiers, req.requestID)
	s.mu.Unlock()
	if reg != nil && reg.notify != nil {
		reg.notify("")
	}
	s.sendForTurn(t, core.Event{Type: core.EventPermissionResolved, RequestID: req.requestID})
}

// OnExternalResolution implements core.ExternalResolutionNotifier: notify is
// invoked at most once if requestID is resolved outside cc-connect (the user
// typed directly in the herdr pane); the returned cancel deregisters and is
// idempotent.
var _ core.ExternalResolutionNotifier = (*herdrSession)(nil)

func (s *herdrSession) OnExternalResolution(requestID string, notify func(note string)) (cancel func()) {
	reg := &resolutionRegistration{notify: notify}
	s.mu.Lock()
	s.notifiers[requestID] = reg
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.notifiers[requestID] == reg {
				delete(s.notifiers, requestID)
			}
			s.mu.Unlock()
		})
	}
}

func (s *herdrSession) RespondPermission(requestID string, result core.PermissionResult) error {
	s.mu.Lock()
	req := s.blocked
	if req == nil || req.requestID != requestID {
		s.mu.Unlock()
		return fmt.Errorf("herdr: no matching blocked request %q", requestID)
	}
	s.blocked = nil
	delete(s.notifiers, requestID)
	s.mu.Unlock()

	if result.Behavior != "allow" {
		return s.client.agentSendKeys(s.ctx, s.target, []string{"Escape"})
	}
	answer := extractPermissionAnswer(req.question, result.UpdatedInput)
	if answer == "" {
		return fmt.Errorf("herdr: blocked response %q contains no answer", requestID)
	}
	if key, ok := blockedKeyForLabel(answer); ok {
		return s.client.agentSendKeys(s.ctx, s.target, []string{key})
	}
	return s.client.agentPrompt(s.ctx, s.target, answer)
}

func extractPermissionAnswer(question string, input map[string]any) string {
	if answers, ok := input["answers"].(map[string]any); ok {
		if answer, ok := answers[question].(string); ok {
			return answer
		}
		for _, value := range answers {
			if answer, ok := value.(string); ok {
				return answer
			}
		}
	}
	if answers, ok := input["answers"].(map[string]string); ok {
		if answer := answers[question]; answer != "" {
			return answer
		}
		for _, answer := range answers {
			return answer
		}
	}
	if answer, ok := input["answer"].(string); ok {
		return answer
	}
	return ""
}

func (s *herdrSession) sendForTurn(t *herdrTurn, event core.Event) {
	if t == nil {
		s.sendSession(event)
		return
	}
	s.sendTurn(t.ctx, event)
}

func (s *herdrSession) sendTurn(ctx context.Context, event core.Event) {
	defer func() { _ = recover() }()
	select {
	case s.events <- event:
	case <-ctx.Done():
	case <-s.ctx.Done():
	}
}

func (s *herdrSession) sendSession(event core.Event) {
	defer func() { _ = recover() }()
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *herdrSession) Events() <-chan core.Event { return s.events }
func (s *herdrSession) CurrentSessionID() string  { return s.target }
func (s *herdrSession) Alive() bool               { return s.alive.Load() }

func (s *herdrSession) Close() error {
	s.closeOnce.Do(func() {
		s.sendMu.Lock()
		defer s.sendMu.Unlock()
		s.alive.Store(false)
		s.cancel()
		s.stopTurn()
		s.subscriptionWG.Wait()
		close(s.events)
	})
	return nil
}

func (s *herdrSession) InjectKey(key string) error {
	if !s.alive.Load() {
		return fmt.Errorf("herdr: session not alive")
	}
	return s.client.agentSendKeys(s.ctx, s.target, []string{key})
}

func (s *herdrSession) CaptureBuffer() (string, error) {
	if !s.alive.Load() {
		return "", fmt.Errorf("herdr: session not alive")
	}
	return s.client.agentRead(s.ctx, s.target, "recent", recentLines)
}
