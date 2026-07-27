package cmux

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// permissionNoteFallbackTimeout aliases the pinned core sentinel so call
// sites stay short; see core.PermissionNoteFallbackTimeout for the contract.
const permissionNoteFallbackTimeout = core.PermissionNoteFallbackTimeout

type pendingRef struct {
	item      feedItem
	session   *cmuxSession
	deadline  time.Time
	missCount int
	replying  bool
}

type feedBridge struct {
	client      *client
	mapper      *sessionMapper
	hookTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup

	mu           sync.Mutex
	outstanding  map[string]*pendingRef
	repliedUntil map[string]time.Time
	sessions     map[string]*cmuxSession
	lastActivity map[string]time.Time
	// triggerCh is owned by feedBridge and intentionally never closed; ctx
	// cancellation stops both consumers, so late producers cannot panic.
	triggerCh chan struct{}
}

func newFeedBridge(parent context.Context, c *client, hookTimeout time.Duration, mapper *sessionMapper) *feedBridge {
	ctx, cancel := context.WithCancel(parent)
	fb := &feedBridge{
		client:       c,
		mapper:       mapper,
		hookTimeout:  hookTimeout,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		outstanding:  make(map[string]*pendingRef),
		repliedUntil: make(map[string]time.Time),
		sessions:     make(map[string]*cmuxSession),
		lastActivity: make(map[string]time.Time),
		triggerCh:    make(chan struct{}, 1),
	}
	fb.wg.Add(2)
	go func() {
		defer fb.wg.Done()
		fb.run()
	}()
	go func() {
		defer fb.wg.Done()
		fb.runSweeper()
	}()
	go func() {
		fb.wg.Wait()
		close(fb.done)
	}()
	return fb
}

func (fb *feedBridge) run() {
	var debounce *time.Timer
	var debounceC <-chan time.Time
	for {
		select {
		case <-fb.ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case <-fb.triggerCh:
			if debounce == nil {
				debounce = time.NewTimer(300 * time.Millisecond)
			} else {
				// Timer.Stop returns true when it cancels an still-pending
				// timer; Reset must still run in that case (only the drain
				// is conditional) or a second trigger inside the debounce
				// window permanently kills the timer. Real cmux fires
				// feed.item.received then feed.item.completed ~1ms apart
				// (PROBES.md), so this is the common case, not an edge case.
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(300 * time.Millisecond)
			}
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			if err := fb.resync(fb.ctx); err != nil {
				slog.Warn("cmux: feed.list refresh failed", "err", err)
			}
		}
	}
}

func (fb *feedBridge) runSweeper() {
	sweepInterval := fb.hookTimeout / 4
	if sweepInterval < 100*time.Millisecond {
		sweepInterval = 100 * time.Millisecond
	}
	if sweepInterval > time.Second {
		sweepInterval = time.Second
	}
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-fb.ctx.Done():
			return
		case now := <-ticker.C:
			fb.expire(now)
		}
	}
}

func (fb *feedBridge) close() {
	fb.cancel()
	<-fb.done
}

func (fb *feedBridge) trigger() {
	select {
	case fb.triggerCh <- struct{}{}:
	default:
	}
}

func (fb *feedBridge) register(s *cmuxSession) {
	fb.mu.Lock()
	fb.sessions[s.workspaceID] = s
	fb.mu.Unlock()
	fb.trigger()
}

func (fb *feedBridge) unregister(s *cmuxSession) {
	fb.mu.Lock()
	if fb.sessions[s.workspaceID] == s {
		delete(fb.sessions, s.workspaceID)
	}
	for requestID, ref := range fb.outstanding {
		if ref.session == s {
			delete(fb.outstanding, requestID)
		}
	}
	fb.mu.Unlock()
}

type feedAction struct {
	request  *pendingRef
	event    core.Event
	note     string
	resolved bool
}

func (fb *feedBridge) resync(ctx context.Context) error {
	items, err := fb.client.feedList(ctx)
	if err != nil {
		return fmt.Errorf("cmux: feed.list: %w", err)
	}
	now := time.Now()
	byRequestID := make(map[string]feedItem)
	for _, item := range items {
		if item.RequestID != "" {
			byRequestID[item.RequestID] = item
		}
	}

	var actions []feedAction
	fb.mu.Lock()
	for _, item := range items {
		if item.Kind != "permissionRequest" || item.Status != "pending" || item.RequestID == "" {
			continue
		}
		if _, exists := fb.outstanding[item.RequestID]; exists {
			continue
		}
		if until, replied := fb.repliedUntil[item.RequestID]; replied && now.Before(until) {
			continue
		}
		session, tried := fb.resolveSessionLocked(item)
		if session == nil {
			slog.Warn("cmux: unmatched pending feed item", "request_id", item.RequestID, "item_id", item.ID, "tried_keys", tried)
			continue
		}
		ref := &pendingRef{item: item, session: session, deadline: now.Add(fb.hookTimeout)}
		fb.outstanding[item.RequestID] = ref
		actions = append(actions, feedAction{request: ref, event: permissionEvent(item)})
	}

	for requestID, ref := range fb.outstanding {
		item, present := byRequestID[requestID]
		if present && item.Status == "pending" {
			ref.item = item
			ref.missCount = 0
			continue
		}
		if ref.replying {
			continue
		}
		if present {
			delete(fb.outstanding, requestID)
			note := resolvedNote(item)
			actions = append(actions, feedAction{request: ref, event: core.Event{Type: core.EventPermissionResolved, RequestID: requestID, Content: note}, note: note, resolved: true})
			continue
		}
		ref.missCount++
		if ref.missCount >= 2 {
			delete(fb.outstanding, requestID)
			note := ""
			actions = append(actions, feedAction{request: ref, event: core.Event{Type: core.EventPermissionResolved, RequestID: requestID, Content: note}, note: note, resolved: true})
		}
	}

	for _, item := range items {
		if item.WorkstreamID == "" {
			continue
		}
		workspaceID, ok := fb.mapper.workspaceIDForWorkstream(item.WorkstreamID)
		if !ok {
			continue
		}
		if updated := parseTimestamp(item.UpdatedAt); updated.After(fb.lastActivity[workspaceID]) {
			fb.lastActivity[workspaceID] = updated
		}
	}
	fb.mu.Unlock()

	for _, action := range actions {
		if action.resolved {
			action.request.session.resolveExternal(action.event.RequestID, action.note)
		}
		action.request.session.emit(action.event)
	}
	return nil
}

func (fb *feedBridge) resolveSessionLocked(item feedItem) (*cmuxSession, []string) {
	tried := []string{"workstream_id=" + item.WorkstreamID}
	if item.WorkstreamID != "" {
		if workspaceID, ok := fb.mapper.workspaceIDForWorkstream(item.WorkstreamID); ok {
			tried = append(tried, "workspace_id="+workspaceID)
			if session := fb.sessions[workspaceID]; session != nil {
				return session, tried
			}
		}
	}
	if item.CWD != "" {
		tried = append(tried, "cwd="+item.CWD)
		var matches []*cmuxSession
		for _, session := range fb.sessions {
			if session.workDir == item.CWD {
				matches = append(matches, session)
			}
		}
		if len(matches) == 1 {
			return matches[0], tried
		}
		if len(matches) > 1 {
			tried = append(tried, "cwd_ambiguous=true")
		}
	}
	return nil, tried
}

func permissionEvent(item feedItem) core.Event {
	input := make(map[string]any)
	if item.ToolInput != "" {
		if err := json.Unmarshal([]byte(item.ToolInput), &input); err != nil {
			slog.Warn("cmux: decode feed tool_input failed", "request_id", item.RequestID, "err", err)
		}
	}
	event := core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    item.RequestID,
		ToolName:     item.ToolName,
		ToolInputRaw: input,
	}
	if item.ToolName == "AskUserQuestion" {
		event.Questions = parseQuestions(input)
	}
	return event
}

func parseQuestions(input map[string]any) []core.UserQuestion {
	rawQuestions, ok := input["questions"].([]any)
	if !ok {
		return nil
	}
	questions := make([]core.UserQuestion, 0, len(rawQuestions))
	for _, raw := range rawQuestions {
		questionMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		question := core.UserQuestion{
			Question:    stringValue(questionMap["question"]),
			Header:      stringValue(questionMap["header"]),
			MultiSelect: boolValue(questionMap["multiSelect"]),
		}
		if rawOptions, ok := questionMap["options"].([]any); ok {
			for _, rawOption := range rawOptions {
				optionMap, ok := rawOption.(map[string]any)
				if !ok {
					continue
				}
				question.Options = append(question.Options, core.UserQuestionOption{
					Label:       stringValue(optionMap["label"]),
					Description: stringValue(optionMap["description"]),
				})
			}
		}
		questions = append(questions, question)
	}
	return questions
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	boolean, _ := value.(bool)
	return boolean
}

func resolvedNote(item feedItem) string {
	return ""
}

func (fb *feedBridge) expire(now time.Time) {
	var expired []*pendingRef
	fb.mu.Lock()
	for requestID, ref := range fb.outstanding {
		if ref.replying || now.Before(ref.deadline) {
			continue
		}
		delete(fb.outstanding, requestID)
		expired = append(expired, ref)
	}
	for requestID, until := range fb.repliedUntil {
		if !now.Before(until) {
			delete(fb.repliedUntil, requestID)
		}
	}
	fb.mu.Unlock()
	for _, ref := range expired {
		note := permissionNoteFallbackTimeout
		ref.session.resolveExternal(ref.item.RequestID, note)
		ref.session.emit(core.Event{Type: core.EventPermissionResolved, RequestID: ref.item.RequestID, Content: note})
	}
}

func (fb *feedBridge) replyPermission(ctx context.Context, requestID, mode string, updatedInput map[string]any) error {
	fb.mu.Lock()
	ref := fb.outstanding[requestID]
	if ref == nil {
		fb.mu.Unlock()
		return fmt.Errorf("cmux: no outstanding permission %q", requestID)
	}
	if time.Now().After(ref.deadline) {
		delete(fb.outstanding, requestID)
		fb.mu.Unlock()
		note := permissionNoteFallbackTimeout
		ref.session.resolveExternal(requestID, note)
		ref.session.emit(core.Event{Type: core.EventPermissionResolved, RequestID: requestID, Content: note})
		return nil
	}
	if ref.replying {
		fb.mu.Unlock()
		return fmt.Errorf("cmux: permission %q reply already in flight", requestID)
	}
	ref.replying = true
	item := ref.item
	fb.mu.Unlock()

	var err error
	if item.ToolName == "AskUserQuestion" {
		selections := questionSelections(updatedInput, permissionEvent(item).Questions)
		err = fb.client.feedQuestionReply(ctx, requestID, selections)
	} else {
		err = fb.client.feedPermissionReply(ctx, requestID, mode)
	}
	fb.mu.Lock()
	current := fb.outstanding[requestID]
	if current == ref {
		if err == nil {
			delete(fb.outstanding, requestID)
			fb.repliedUntil[requestID] = time.Now().Add(30 * time.Second)
		} else {
			ref.replying = false
		}
	}
	fb.mu.Unlock()
	if err != nil {
		return fmt.Errorf("cmux: reply permission: %w", err)
	}
	return nil
}

func (fb *feedBridge) hasOutstanding(session *cmuxSession) bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, ref := range fb.outstanding {
		if ref.session == session {
			return true
		}
	}
	return false
}

func questionSelections(updatedInput map[string]any, questions []core.UserQuestion) []string {
	answers, ok := updatedInput["answers"]
	if !ok {
		return []string{}
	}
	values := make(map[string]string)
	switch typed := answers.(type) {
	case map[string]any:
		for key, value := range typed {
			values[key] = stringValue(value)
		}
	}
	selections := make([]string, 0, len(values))
	for _, question := range questions {
		if answer, ok := values[question.Question]; ok {
			selections = append(selections, answer)
			delete(values, question.Question)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		selections = append(selections, values[key])
	}
	return selections
}

func (fb *feedBridge) activity(workspaceID string) time.Time {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.lastActivity[workspaceID]
}

func parseTimestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func validReplyMode(mode string) bool {
	switch strings.ToLower(mode) {
	case "once", "always", "all", "bypass", "deny":
		return true
	default:
		return false
	}
}
