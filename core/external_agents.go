package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

const externalAgentTailLines = 80

// externalGroupBindingNamespace isolates chats bound to an externally owned
// agent from project, auto-group, and manual native-session bindings. A chat in
// this namespace never reaches the local agent.
const externalGroupBindingNamespace = "external:"

var (
	ErrExternalAgentBackendUnknown = errors.New("external agent controller is not configured")
	ErrExternalAgentTargetNotFound = errors.New("external agent target not found")
)

// SetAgentControllers installs controllers used by backend-specific external
// agent commands. Controllers are explicit project wiring; listing never
// creates external targets.
func (e *Engine) SetAgentControllers(controllers map[string]AgentController) {
	installed := make(map[string]AgentController, len(controllers))
	for backend, controller := range controllers {
		if strings.TrimSpace(backend) != "" && controller != nil {
			installed[backend] = controller
		}
	}
	e.agentControllersMu.Lock()
	e.agentControllers = installed
	e.agentControllersMu.Unlock()
}

func (e *Engine) externalControllers() map[string]AgentController {
	e.agentControllersMu.RLock()
	controllers := make(map[string]AgentController, len(e.agentControllers))
	for backend, controller := range e.agentControllers {
		controllers[backend] = controller
	}
	e.agentControllersMu.RUnlock()
	return controllers
}

// externalAgentCallTimeout bounds one backend round trip. Controllers shell out
// to per-target CLIs, so without a deadline a single wedged process would hang
// the chat goroutine waiting on it for the life of the process.
const externalAgentCallTimeout = 30 * time.Second

func (e *Engine) externalAgentContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, externalAgentCallTimeout)
}

// agentSessionBindings returns the binding store without creating one. The
// message path must never lazily construct a store; creation belongs to the
// explicit group-create path.
func (e *Engine) agentSessionBindings() *WorkspaceBindingManager {
	e.agentBindingsMu.RLock()
	bindings := e.workspaceBindings
	e.agentBindingsMu.RUnlock()
	return bindings
}

func (e *Engine) externalController(backend string) AgentController {
	e.agentControllersMu.RLock()
	controller := e.agentControllers[backend]
	e.agentControllersMu.RUnlock()
	return controller
}

// hasExternalController reports whether a command name is owned by a configured
// external controller. Command routing uses it so backend names stay in the
// registry instead of a hardcoded list in the engine.
func (e *Engine) hasExternalController(backend string) bool {
	return e.externalController(backend) != nil
}

func (e *Engine) cmdExternalAgents(p Platform, msg *Message) {
	e.replyWithCard(p, msg.ReplyCtx, e.renderExternalAgentsCard(msg.SessionKey))
}

// cmdExternalBackend implements `/<backend> [show|send|group] ...`. Listing is
// the bare form; every mutating form names an exact target so a refreshed list
// can never redirect a message to a different agent.
func (e *Engine) cmdExternalBackend(p Platform, msg *Message, backend string, args []string) {
	if len(args) == 0 {
		e.replyWithCard(p, msg.ReplyCtx, e.renderExternalBackendCard(backend, msg.SessionKey))
		return
	}

	action := strings.ToLower(args[0])
	rest := args[1:]
	// A bare selector is the common case ("/orca 2"), so treat it as show.
	if action != "show" && action != "send" && action != "group" && action != "list" && action != "page" {
		action, rest = "show", args
	}

	switch action {
	case "list", "page":
		e.replyWithCard(p, msg.ReplyCtx, e.renderExternalBackendCard(backend, msg.SessionKey, externalPageNumber(rest)))
		return
	case "send":
		if len(rest) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgExternalAgentUsage, backend, backend, backend, backend))
			return
		}
	case "show", "group":
		if len(rest) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgExternalAgentUsage, backend, backend, backend, backend))
			return
		}
	}

	controller, target, err := e.resolveExternalTarget(backend, rest[0])
	if err != nil {
		// A bad selector is a user typo, not a backend failure: point back at
		// the listing instead of surfacing an internal error string.
		if errors.Is(err, ErrExternalAgentTargetNotFound) {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgExternalAgentNotFound, rest[0], backend))
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.externalAgentErrorCard(err))
		return
	}

	switch action {
	case "show":
		e.replyWithCard(p, msg.ReplyCtx, e.renderExternalAgentDetail(controller, target))
	case "send":
		e.replyWithCard(p, msg.ReplyCtx, e.sendExternalAgentPrompt(controller, target, strings.Join(rest[1:], " ")))
	case "group":
		name := strings.TrimSpace(strings.Join(rest[1:], " "))
		e.replyWithCard(p, msg.ReplyCtx, e.createExternalAgentGroupCard(target, extractUserID(msg.SessionKey), name))
	}
}

// resolveExternalTarget maps a user-facing selector to exactly one live target.
// A selector is a 1-based index into the current listing, an exact ID or title,
// or a unique ID prefix. Exact matches win over prefixes, and an ambiguous
// prefix is rejected rather than guessed.
//
// An index is resolved against the listing as it is NOW, not the listing the
// user was looking at: if targets appeared or disappeared in between, index N
// can name a different agent. Every reply therefore names the agent it acted
// on, and card buttons carry an exact (ID, Revision) pair instead of an index.
func (e *Engine) resolveExternalTarget(backend, selector string) (AgentController, AgentControlTarget, error) {
	controller := e.externalController(backend)
	if controller == nil {
		return nil, AgentControlTarget{}, fmt.Errorf("%w: %s", ErrExternalAgentBackendUnknown, backend)
	}
	ctx, cancel := e.externalAgentContext()
	defer cancel()
	targets, err := controller.ListAgents(ctx)
	if err != nil {
		return nil, AgentControlTarget{}, err
	}
	selector = strings.TrimSpace(selector)
	notFound := fmt.Errorf("%w: %s", ErrExternalAgentTargetNotFound, selector)
	if index, convErr := strconv.Atoi(selector); convErr == nil {
		if index < 1 || index > len(targets) {
			return nil, AgentControlTarget{}, notFound
		}
		return controller, targets[index-1], nil
	}

	// Exact matches first: a full ID that also prefixes a longer ID must not
	// be reported as ambiguous.
	for i := range targets {
		if targets[i].ID == selector || strings.EqualFold(targets[i].Title, selector) {
			return controller, targets[i], nil
		}
	}
	var matched *AgentControlTarget
	for i := range targets {
		if !strings.HasPrefix(targets[i].ID, selector) {
			continue
		}
		if matched != nil {
			return nil, AgentControlTarget{}, notFound
		}
		matched = &targets[i]
	}
	if matched == nil {
		return nil, AgentControlTarget{}, notFound
	}
	return controller, *matched, nil
}

// externalPageNumber reads an optional page argument. An absent or unparseable
// value means page 1 rather than an error, so `/orca list` keeps working.
func externalPageNumber(args []string) int {
	if len(args) == 0 {
		return 1
	}
	page, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

// renderExternalAgentsCard is a lightweight backend launcher; target lists
// belong to the named backend menu so each tool keeps its own vocabulary.
func (e *Engine) renderExternalAgentsCard(_ string) *Card {
	controllers := e.externalControllers()
	cb := NewCard().Title(e.i18n.T(MsgExternalAgentsTitle), "blue")
	if len(controllers) == 0 {
		return cb.Markdown(e.i18n.T(MsgExternalAgentsNone)).Build()
	}
	buttons := make([]CardButton, 0, len(controllers))
	for _, backend := range sortedControllerBackends(controllers) {
		buttons = append(buttons, DefaultBtn(externalBackendTitle(backend), "nav:/"+backend))
	}
	cb.Markdown(e.i18n.T(MsgExternalAgentsPick)).Buttons(buttons...)
	return cb.Build()
}

// renderExternalBackendCard lists one backend's live targets. Real machines run
// dozens of worktrees and panes, so the listing is paged like /list; item
// numbers stay absolute across pages so a selector always means the same agent.
func (e *Engine) renderExternalBackendCard(backend, _ string, page ...int) *Card {
	controller := e.externalController(backend)
	cb := NewCard().Title(externalBackendTitle(backend), "blue")
	if controller == nil {
		return cb.Markdown(e.i18n.T(MsgExternalAgentsNone)).Build()
	}
	refresh := DefaultBtn(e.i18n.T(MsgExternalAgentRefreshBtn), "nav:/"+backend)
	allTools := DefaultBtn(e.i18n.T(MsgExternalAgentAllToolsBtn), "nav:/agents")
	listCtx, cancelList := e.externalAgentContext()
	defer cancelList()
	targets, err := controller.ListAgents(listCtx)
	if err != nil {
		return cb.Markdown(e.i18n.Tf(MsgExternalBackendUnavailable, err.Error())).Buttons(refresh, allTools).Build()
	}
	if len(targets) == 0 {
		return cb.Markdown(e.i18n.T(MsgExternalBackendEmpty)).Buttons(refresh, allTools).Build()
	}

	total := len(targets)
	totalPages := (total + listPageSize - 1) / listPageSize
	current := 1
	if len(page) > 0 && page[0] > 0 {
		current = page[0]
	}
	if current > totalPages {
		current = totalPages
	}
	start := (current - 1) * listPageSize
	end := start + listPageSize
	if end > total {
		end = total
	}

	for i := start; i < end; i++ {
		cb.ListItem(
			fmt.Sprintf("**%d.** %s", i+1, externalTargetSummary(targets[i])),
			e.i18n.T(MsgExternalAgentViewBtn),
			externalAction("show", backend, targets[i].Ref()),
		)
	}

	cb.Divider().Markdown(e.i18n.Tf(MsgExternalAgentUsage, backend, backend, backend, backend))
	buttons := make([]CardButton, 0, 4)
	if current > 1 {
		buttons = append(buttons, e.cardPrevButton(fmt.Sprintf("nav:/%s page %d", backend, current-1)))
	}
	if current < totalPages {
		buttons = append(buttons, e.cardNextButton(fmt.Sprintf("nav:/%s page %d", backend, current+1)))
	}
	cb.Buttons(append(buttons, refresh, allTools)...)
	if totalPages > 1 {
		cb.Note(fmt.Sprintf(e.i18n.T(MsgListPageHint), current, totalPages))
	}
	return cb.Build()
}

// externalTargetSummary is the one-line identity a user needs to tell two
// agents apart: window title, agent kind, working directory, and live status.
func externalTargetSummary(target AgentControlTarget) string {
	label := strings.TrimSpace(target.Title)
	if label == "" {
		label = target.Directory
	}
	if label == "" {
		label = target.ID
	}
	fields := []string{label}
	if target.Kind != "" {
		fields = append(fields, target.Kind)
	}
	if target.Status != "" {
		fields = append(fields, target.Status)
	}
	summary := strings.Join(fields, " · ")
	if target.Directory != "" && target.Directory != label {
		summary += "\n`" + target.Directory + "`"
	}
	return summary
}

func (e *Engine) handleExternalAgentAction(args, sessionKey string) *Card {
	parts := strings.SplitN(args, " ", 5)
	if len(parts) < 4 {
		return e.externalAgentErrorCard(fmt.Errorf("invalid agent action"))
	}
	action, backend := parts[0], parts[1]
	id, err := decodeExternalRef(parts[2])
	if err != nil {
		return e.externalAgentErrorCard(err)
	}
	revision, err := decodeExternalRef(parts[3])
	if err != nil {
		return e.externalAgentErrorCard(err)
	}
	controller := e.externalController(backend)
	if controller == nil {
		return e.externalAgentErrorCard(fmt.Errorf("%w: %s", ErrExternalAgentBackendUnknown, backend))
	}
	ref := AgentControlTargetRef{ID: id, Revision: revision}
	lookupCtx, cancelLookup := e.externalAgentContext()
	defer cancelLookup()
	target, err := externalExactTarget(lookupCtx, controller, ref)
	if err != nil {
		return e.externalAgentErrorCard(err)
	}
	// No card emits a "send" action: prompting is an explicit
	// /<backend> send command or a bound group, never a one-click button.
	switch action {
	case "show":
		return e.renderExternalAgentDetail(controller, target)
	case "group":
		return e.createExternalAgentGroupCard(target, extractUserID(sessionKey), "")
	default:
		return e.externalAgentErrorCard(fmt.Errorf("unsupported agent action %q", action))
	}
}

func (e *Engine) sendExternalAgentPrompt(controller AgentController, target AgentControlTarget, message string) *Card {
	message = strings.TrimSpace(message)
	if message == "" {
		return e.externalAgentErrorCard(ErrAgentControlInvalidAnswer)
	}
	prompter, ok := controller.(AgentControlPrompter)
	if !ok || !target.Supports(AgentControlCapabilityPrompt) {
		return e.externalAgentErrorCard(ErrAgentControlUnsupported)
	}
	sendCtx, cancelSend := e.externalAgentContext()
	defer cancelSend()
	if err := prompter.SendAgentPrompt(sendCtx, target.Ref(), message); err != nil {
		return e.externalAgentErrorCard(err)
	}
	return e.renderExternalAgentDetail(controller, target, e.i18n.T(MsgExternalAgentPromptSent))
}

func (e *Engine) renderExternalAgentDetail(controller AgentController, target AgentControlTarget, note ...string) *Card {
	cb := NewCard().Title(externalBackendTitle(target.Backend), "blue")
	cb.Markdown(externalTargetSummary(target))
	cb.Markdownf("`%s`", target.ID)
	if labels := externalCapabilityLabels(target); labels != "" {
		cb.Markdown(e.i18n.Tf(MsgExternalAgentCapabilities, labels))
	}
	if tailer, ok := controller.(AgentControlTailer); ok && target.Supports(AgentControlCapabilityTail) {
		tailCtx, cancelTail := e.externalAgentContext()
		text, err := tailer.TailAgent(tailCtx, target.Ref(), externalAgentTailLines)
		cancelTail()
		if err != nil {
			cb.Markdown(e.i18n.Tf(MsgExternalBackendUnavailable, err.Error()))
		} else if text != "" {
			cb.Divider().Markdown("```\n" + text + "\n```")
		}
	}
	if len(note) > 0 && note[0] != "" {
		cb.Markdown("✅ " + note[0])
	}
	// Free-form prompt forwarding stays an explicit /<backend> send command or a
	// bound group; a fixed continue button would be an unauditable mutation.
	buttons := []CardButton{DefaultBtn(e.i18n.T(MsgExternalAgentRefreshBtn), externalAction("show", target.Backend, target.Ref()))}
	if target.Supports(AgentControlCapabilityPrompt) {
		buttons = append(buttons, PrimaryBtn(e.i18n.T(MsgExternalAgentGroupBtn), externalAction("group", target.Backend, target.Ref())))
	}
	buttons = append(buttons, DefaultBtn(e.i18n.T(MsgExternalAgentBackBtn), "nav:/"+target.Backend))
	cb.Buttons(buttons...)
	return cb.Build()
}

func externalCapabilityLabels(target AgentControlTarget) string {
	if len(target.Capabilities) == 0 {
		return ""
	}
	labels := make([]string, 0, len(target.Capabilities))
	for _, capability := range target.Capabilities {
		labels = append(labels, string(capability))
	}
	return strings.Join(labels, ", ")
}

// ──────────────────────────────────────────────────────────────
// Dedicated group chats for an externally owned agent
// ──────────────────────────────────────────────────────────────

func (e *Engine) createExternalAgentGroupCard(target AgentControlTarget, ownerUserID, groupName string) *Card {
	groupCtx, cancelGroup := e.externalAgentContext()
	defer cancelGroup()
	result, err := e.CreateExternalAgentGroup(groupCtx, target, ownerUserID, groupName)
	if err != nil {
		return e.externalAgentErrorCard(err)
	}
	note := e.i18n.T(MsgExternalAgentGroupCreated)
	if !result.Created {
		note = e.i18n.T(MsgExternalAgentGroupExists)
	}
	cb := NewCard().Title(externalBackendTitle(target.Backend), "green")
	cb.Markdown(externalTargetSummary(target))
	cb.Markdown("✅ " + note)
	cb.Note(e.i18n.Tf(MsgExternalAgentGroupHint, target.Backend))
	cb.Buttons(DefaultBtn(e.i18n.T(MsgExternalAgentBackBtn), "nav:/"+target.Backend))
	return cb.Build()
}

// CreateExternalAgentGroup creates one group chat dedicated to an externally
// owned agent and binds the chat to it, so later messages in that chat are
// forwarded to the agent instead of the local one. Binding keys on the target's
// stable ID, never its revision: a revision changes whenever the backend's
// topology moves, but the chat must stay attached to the same agent.
func (e *Engine) CreateExternalAgentGroup(ctx context.Context, target AgentControlTarget, ownerUserID, groupName string) (AgentSessionGroupResult, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return AgentSessionGroupResult{}, ErrAgentSessionOwnerRequired
	}
	if strings.TrimSpace(target.Backend) == "" || strings.TrimSpace(target.ID) == "" {
		return AgentSessionGroupResult{}, ErrExternalAgentTargetNotFound
	}

	e.autoGroupSweepMu.Lock()
	defer e.autoGroupSweepMu.Unlock()

	namespace := externalGroupBindingNamespace + e.name
	bindingID := externalBindingID(target.Backend, target.ID)
	bindings := e.ensureAgentSessionBindings()
	if channelKey, _, found := bindings.LookupBySessionID(namespace, bindingID); found {
		return AgentSessionGroupResult{
			Created:          false,
			ChatID:           legacyWorkspaceChannelKey(channelKey),
			OwnerUserID:      ownerUserID,
			BindingNamespace: namespace,
		}, nil
	}

	creatorPlatform := e.firstGroupChatCreator()
	if creatorPlatform == nil {
		return AgentSessionGroupResult{}, ErrAgentSessionGroupUnsupported
	}
	if strings.TrimSpace(groupName) == "" {
		groupName = externalBackendTitle(target.Backend) + " · " + externalTargetLabel(target)
	}
	chatID, err := creatorPlatform.(GroupChatCreator).CreateGroupChat(ctx, groupName, "", ownerUserID)
	if err != nil {
		return AgentSessionGroupResult{}, fmt.Errorf("create group for external agent %q: %w", target.ID, err)
	}
	if strings.TrimSpace(chatID) == "" {
		return AgentSessionGroupResult{}, fmt.Errorf("create group for external agent %q: platform returned an empty chat ID", target.ID)
	}

	channelKey := workspaceChannelKey(creatorPlatform.Name(), chatID)
	bindings.BindSession(namespace, channelKey, groupName, target.Directory, bindingID)
	if err := verifyAgentSessionBinding(bindings, namespace, channelKey, bindingID); err != nil {
		return AgentSessionGroupResult{}, fmt.Errorf("persist group binding for external agent %q (orphaned chat %q): %w", target.ID, chatID, err)
	}

	slog.Info("external agent group created",
		"project", e.name, "backend", target.Backend, "target", target.ID, "chat", chatID)
	return AgentSessionGroupResult{
		Created:          true,
		ChatID:           chatID,
		OwnerUserID:      ownerUserID,
		BindingNamespace: namespace,
	}, nil
}

// isExternalAgentGroup reports whether this chat is bound to an externally
// owned agent. Bound chats are handled before workspace resolution, so this
// must stay a cheap, side-effect-free lookup.
func (e *Engine) isExternalAgentGroup(msg *Message) bool {
	_, _, ok := e.externalGroupBinding(msg)
	return ok
}

func (e *Engine) externalGroupBinding(msg *Message) (backend, targetID string, ok bool) {
	if msg == nil {
		return "", "", false
	}
	bindings := e.agentSessionBindings()
	if bindings == nil {
		return "", "", false
	}
	channelKey := effectiveWorkspaceChannelKey(msg)
	if channelKey == "" {
		return "", "", false
	}
	binding := bindings.Lookup(externalGroupBindingNamespace+e.name, channelKey)
	if binding == nil {
		return "", "", false
	}
	// Lookup hands back manager-owned storage that the manager may replace on
	// its next refresh, so read the field out before returning.
	return parseExternalBindingID(binding.AgentSessionID)
}

// forwardExternalAgentGroupMessage sends a bound group's message to its agent.
// It never falls through: a message typed in a bound group must not reach the
// local agent, so every failure answers in the group instead.
func (e *Engine) forwardExternalAgentGroupMessage(p Platform, msg *Message, content string) {
	backend, targetID, ok := e.externalGroupBinding(msg)
	if !ok {
		return
	}
	if !e.isAdmin(msg.UserID) {
		slog.Info("audit: command_blocked",
			"user_id", msg.UserID, "platform", msg.Platform,
			"project", e.name, "command", "agents", "reason", "unauthorized")
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/"+backend))
		return
	}

	content = strings.TrimSpace(content)
	if content == "" {
		// Attachments have no meaning for a terminal-shaped backend; say so
		// rather than silently dropping the message.
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgExternalAgentTextOnly, backend))
		return
	}

	controller := e.externalController(backend)
	if controller == nil {
		e.replyWithCard(p, msg.ReplyCtx, e.externalAgentErrorCard(fmt.Errorf("%w: %s", ErrExternalAgentBackendUnknown, backend)))
		return
	}
	ctx, cancel := e.externalAgentContext()
	defer cancel()
	target, err := externalTargetByID(ctx, controller, targetID)
	if err != nil {
		e.replyWithCard(p, msg.ReplyCtx, e.externalAgentErrorCard(err))
		return
	}
	slog.Info("audit: external_agent_prompt",
		"user_id", msg.UserID, "platform", msg.Platform,
		"project", e.name, "backend", backend, "target", targetID)
	e.replyWithCard(p, msg.ReplyCtx, e.sendExternalAgentPrompt(controller, target, content))
}

func externalTargetLabel(target AgentControlTarget) string {
	if label := strings.TrimSpace(target.Title); label != "" {
		return label
	}
	if target.Directory != "" {
		return target.Directory
	}
	return target.ID
}

func externalBindingID(backend, targetID string) string {
	return backend + "/" + targetID
}

func parseExternalBindingID(value string) (backend, targetID string, ok bool) {
	backend, targetID, found := strings.Cut(value, "/")
	if !found || backend == "" || targetID == "" {
		return "", "", false
	}
	return backend, targetID, true
}

// externalTargetByID re-discovers a target from its stable ID and returns the
// current revision, so a bound chat keeps working across backend topology
// changes that only alter the opaque revision.
func externalTargetByID(ctx context.Context, controller AgentController, id string) (AgentControlTarget, error) {
	targets, err := controller.ListAgents(ctx)
	if err != nil {
		return AgentControlTarget{}, err
	}
	for _, target := range targets {
		if target.ID == id {
			return target, nil
		}
	}
	return AgentControlTarget{}, fmt.Errorf("%w: %s", ErrExternalAgentTargetNotFound, id)
}

func externalExactTarget(ctx context.Context, controller AgentController, ref AgentControlTargetRef) (AgentControlTarget, error) {
	if err := ValidateAgentControlTargetRef(ref); err != nil {
		return AgentControlTarget{}, err
	}
	targets, err := controller.ListAgents(ctx)
	if err != nil {
		return AgentControlTarget{}, err
	}
	for _, target := range targets {
		if target.ID == ref.ID && target.Revision == ref.Revision {
			return target, nil
		}
	}
	return AgentControlTarget{}, ErrAgentControlTargetStale
}

func externalAction(action, backend string, ref AgentControlTargetRef) string {
	return "act:/agents " + strings.Join([]string{action, backend, encodeExternalRef(ref.ID), encodeExternalRef(ref.Revision)}, " ")
}

func encodeExternalRef(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeExternalRef(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || strings.TrimSpace(string(decoded)) == "" {
		return "", fmt.Errorf("invalid external target reference")
	}
	return string(decoded), nil
}

func (e *Engine) externalAgentErrorCard(err error) *Card {
	return NewCard().
		Title(e.i18n.T(MsgExternalAgentsTitle), "red").
		Markdown(e.i18n.Tf(MsgExternalAgentActionFailed, err.Error())).
		Build()
}

func sortedControllerBackends(controllers map[string]AgentController) []string {
	backends := make([]string, 0, len(controllers))
	for backend := range controllers {
		backends = append(backends, backend)
	}
	sort.Strings(backends)
	return backends
}

func externalBackendTitle(backend string) string {
	switch backend {
	case "cmux":
		return "Cmux Agents"
	case "herdr":
		return "Herdr Agents"
	case "orca":
		return "Orca Agents"
	default:
		if backend == "" {
			return "Agents"
		}
		return strings.ToUpper(backend[:1]) + backend[1:] + " Agents"
	}
}
