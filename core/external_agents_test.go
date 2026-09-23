package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type externalTestController struct {
	targets []AgentControlTarget
	tail    string
	sent    string
	sentRef AgentControlTargetRef
}

func (c *externalTestController) ListAgents(context.Context) ([]AgentControlTarget, error) {
	return c.targets, nil
}

func (c *externalTestController) TailAgent(_ context.Context, ref AgentControlTargetRef, _ int) (string, error) {
	if ref != c.targets[0].Ref() {
		return "", ErrAgentControlTargetStale
	}
	return c.tail, nil
}

func (c *externalTestController) SendAgentPrompt(_ context.Context, ref AgentControlTargetRef, message string) error {
	for _, target := range c.targets {
		if target.Ref() == ref {
			c.sent = message
			c.sentRef = ref
			return nil
		}
	}
	return ErrAgentControlTargetStale
}

func externalTestTarget() AgentControlTarget {
	return AgentControlTarget{
		ID: "term-1", Revision: "revision-1", Backend: "orca",
		Kind: "codex", Title: "sol worker", Directory: "/repo", Status: "working",
		Capabilities: []AgentControlCapability{AgentControlCapabilityTail, AgentControlCapabilityPrompt},
	}
}

func TestExternalAgentCardShowsDetailAndExplicitSend(t *testing.T) {
	controller := &externalTestController{targets: []AgentControlTarget{externalTestTarget()}, tail: "latest output"}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": controller})
	if overview := e.renderExternalAgentsCard("key"); !strings.Contains(overview.RenderText(), "Orca Agents") {
		t.Fatalf("overview = %s", overview.RenderText())
	}
	detail := e.handleExternalAgentAction("show orca dGVybS0x cmV2aXNpb24tMQ", "key")
	if detail == nil || !strings.Contains(detail.RenderText(), "latest output") {
		t.Fatalf("detail = %#v", detail)
	}
	if strings.Contains(detail.RenderText(), "Continue") {
		t.Fatalf("detail exposes fixed prompt button: %s", detail.RenderText())
	}
	// Prompting is never a one-click card action; it is an explicit command.
	if card := e.handleExternalAgentAction("send orca dGVybS0x cmV2aXNpb24tMQ continue", "key"); card == nil ||
		!strings.Contains(card.RenderText(), "unsupported agent action") {
		t.Fatalf("send must not be reachable as a card action: %#v", card)
	}
	if controller.sent != "" {
		t.Fatalf("a card action sent %q", controller.sent)
	}

	platform := &stubPlatformEngine{n: "feishu"}
	e.SetAdminFrom("user-1")
	e.handleCommand(platform, &Message{SessionKey: "feishu:chat-1:user-1", Platform: "feishu", UserID: "user-1", ReplyCtx: "ctx"}, "/orca send 1 continue")
	if controller.sent != "continue" {
		t.Fatalf("sent = %q", controller.sent)
	}
	if got := strings.Join(platform.getSent(), "\n"); !strings.Contains(got, e.i18n.T(MsgExternalAgentPromptSent)) {
		t.Fatalf("send reply = %s", got)
	}
}

// Card buttons must carry the act:/agents prefix handleCardNav parses; without
// it every button in the backend list is a dead no-op on the platform side.
func TestExternalAgentButtonsRouteThroughCardNav(t *testing.T) {
	controller := &externalTestController{targets: []AgentControlTarget{externalTestTarget()}, tail: "latest output"}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": controller})

	value := externalAction("show", "orca", externalTestTarget().Ref())
	if !strings.HasPrefix(value, "act:/agents ") {
		t.Fatalf("button value = %q, want an act:/agents action", value)
	}
	card := e.handleCardNav(value, "feishu:chat-1:user-1")
	if card == nil || !strings.Contains(card.RenderText(), "latest output") {
		t.Fatalf("card nav result = %#v", card)
	}
	if listCard := e.handleCardNav("nav:/orca", "feishu:chat-1:user-1"); listCard == nil ||
		!strings.Contains(listCard.RenderText(), "sol worker") {
		t.Fatalf("backend nav card = %#v", listCard)
	}
}

func TestExternalBackendMenuFiltersTargets(t *testing.T) {
	orca := &externalTestController{targets: []AgentControlTarget{{ID: "orca-1", Revision: "rev-orca", Backend: "orca", Kind: "codex"}}}
	cmux := &externalTestController{targets: []AgentControlTarget{{ID: "cmux-1", Revision: "rev-cmux", Backend: "cmux", Kind: "claude"}}}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": orca, "cmux": cmux})
	text := e.renderExternalBackendCard("orca", "key").RenderText()
	if !strings.Contains(text, "Orca Agents") || !strings.Contains(text, "orca-1") || strings.Contains(text, "cmux-1") {
		t.Fatalf("card = %s", text)
	}
}

// The listing must expose the identity a user needs to tell two agents in the
// same repo apart: window title, agent kind, and live status.
func TestExternalBackendMenuShowsWindowIdentity(t *testing.T) {
	controller := &externalTestController{targets: []AgentControlTarget{
		{ID: "wt/a", Revision: "rev-a", Backend: "orca", Kind: "codex", Title: "sol worker", Directory: "/repo", Status: "working"},
		{ID: "wt/b", Revision: "rev-b", Backend: "orca", Kind: "claude", Title: "luna scout", Directory: "/repo", Status: "idle"},
	}}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": controller})
	text := e.renderExternalBackendCard("orca", "key").RenderText()
	for _, want := range []string{"1.", "2.", "sol worker", "luna scout", "codex", "claude", "working", "idle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q:\n%s", want, text)
		}
	}
}

func TestResolveExternalTargetBySelector(t *testing.T) {
	controller := &externalTestController{targets: []AgentControlTarget{
		{ID: "wt/a", Revision: "rev-a", Backend: "orca", Title: "sol worker"},
		{ID: "wt/b", Revision: "rev-b", Backend: "orca", Title: "luna scout"},
	}}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": controller})

	for _, tc := range []struct {
		selector string
		wantID   string
	}{
		{"1", "wt/a"},
		{"2", "wt/b"},
		{"wt/b", "wt/b"},
		{"luna scout", "wt/b"},
	} {
		_, target, err := e.resolveExternalTarget("orca", tc.selector)
		if err != nil {
			t.Fatalf("resolveExternalTarget(%q) error = %v", tc.selector, err)
		}
		if target.ID != tc.wantID {
			t.Fatalf("resolveExternalTarget(%q) = %q, want %q", tc.selector, target.ID, tc.wantID)
		}
	}

	// An out-of-range index or an ambiguous prefix must fail instead of
	// silently picking a neighbour and messaging the wrong agent.
	for _, selector := range []string{"0", "3", "wt/", "nobody"} {
		if _, _, err := e.resolveExternalTarget("orca", selector); !errors.Is(err, ErrExternalAgentTargetNotFound) {
			t.Fatalf("resolveExternalTarget(%q) error = %v, want ErrExternalAgentTargetNotFound", selector, err)
		}
	}
	if _, _, err := e.resolveExternalTarget("cmux", "1"); !errors.Is(err, ErrExternalAgentBackendUnknown) {
		t.Fatalf("unconfigured backend error = %v, want ErrExternalAgentBackendUnknown", err)
	}
}

// A backend command exists only while its controller is configured, so an
// unconfigured tool falls through to the agent instead of shadowing it.
func TestExternalBackendCommandFollowsControllerRegistration(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)
	if e.hasExternalController("orca") {
		t.Fatal("orca command exists without a configured controller")
	}
	e.SetAgentControllers(map[string]AgentController{"orca": &externalTestController{}})
	if !e.hasExternalController("orca") {
		t.Fatal("orca command missing after the controller was configured")
	}
	if e.hasExternalController("cmux") {
		t.Fatal("cmux command exists without a configured controller")
	}

	var found bool
	for _, cmd := range e.GetAllCommands() {
		if cmd.Command == "orca" {
			found = true
		}
		if cmd.Command == "cmux" {
			t.Fatalf("bot menu advertises an unconfigured backend: %#v", cmd)
		}
	}
	if !found {
		t.Fatalf("bot menu missing the configured orca command: %#v", e.GetAllCommands())
	}
}

func TestCreateExternalAgentGroupIsIdempotentAndRoutesMessages(t *testing.T) {
	dir := t.TempDir()
	target := externalTestTarget()
	controller := &externalTestController{targets: []AgentControlTarget{target}, tail: "latest output"}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", nil, []Platform{platform}, "", LangEnglish)
	e.SetDataDir(dir)
	e.SetAdminFrom("user-1")
	e.SetAgentControllers(map[string]AgentController{"orca": controller})

	first, err := e.CreateExternalAgentGroup(context.Background(), target, "owner-1", "")
	if err != nil {
		t.Fatalf("CreateExternalAgentGroup() error = %v", err)
	}
	if !first.Created || first.ChatID != "chat-1" {
		t.Fatalf("first result = %#v", first)
	}
	second, err := e.CreateExternalAgentGroup(context.Background(), target, "owner-1", "")
	if err != nil {
		t.Fatalf("second CreateExternalAgentGroup() error = %v", err)
	}
	if second.Created || second.ChatID != "chat-1" || platform.createCalls != 1 {
		t.Fatalf("second result/calls = %#v/%d, want the existing chat and exactly one create", second, platform.createCalls)
	}

	msg := &Message{SessionKey: "feishu:chat-1:user-1", Platform: "feishu", ChannelKey: "chat-1", UserID: "user-1"}
	if !e.isExternalAgentGroup(msg) {
		t.Fatal("created group is not recognised as bound")
	}
	e.forwardExternalAgentGroupMessage(platform, msg, "ship it")
	if controller.sent != "ship it" {
		t.Fatalf("forwarded prompt = %q, want %q", controller.sent, "ship it")
	}

	// An unbound chat must stay with the local agent.
	other := &Message{SessionKey: "feishu:chat-9:user-1", Platform: "feishu", ChannelKey: "chat-9", UserID: "user-1"}
	if e.isExternalAgentGroup(other) {
		t.Fatal("unbound chat was claimed by the external route")
	}

	// Attachments cannot be typed into a terminal; the group must say so
	// instead of silently dropping them.
	e.forwardExternalAgentGroupMessage(platform, msg, "   ")
	if controller.sent != "ship it" {
		t.Fatalf("empty content was forwarded as %q", controller.sent)
	}
}

// A bound chat keeps working after the backend's opaque revision changes: the
// binding tracks the stable target ID, and the current revision is re-read
// before every send.
func TestExternalAgentGroupSurvivesRevisionChange(t *testing.T) {
	dir := t.TempDir()
	target := externalTestTarget()
	controller := &externalTestController{targets: []AgentControlTarget{target}}
	platform := &agentSessionGroupPlatform{
		agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
		chatID:                    "chat-1",
	}
	e := NewEngine("project-a", nil, []Platform{platform}, "", LangEnglish)
	e.SetDataDir(dir)
	e.SetAdminFrom("user-1")
	e.SetAgentControllers(map[string]AgentController{"orca": controller})

	if _, err := e.CreateExternalAgentGroup(context.Background(), target, "owner-1", ""); err != nil {
		t.Fatalf("CreateExternalAgentGroup() error = %v", err)
	}

	moved := target
	moved.Revision = "revision-2"
	controller.targets = []AgentControlTarget{moved}

	msg := &Message{SessionKey: "feishu:chat-1:user-1", Platform: "feishu", ChannelKey: "chat-1", UserID: "user-1"}
	e.forwardExternalAgentGroupMessage(platform, msg, "still there?")
	if controller.sent != "still there?" || controller.sentRef.Revision != "revision-2" {
		t.Fatalf("sent = %q at revision %q, want the message at revision-2", controller.sent, controller.sentRef.Revision)
	}
}

func TestCreateExternalAgentGroupRequiresOwnerAndCapablePlatform(t *testing.T) {
	target := externalTestTarget()
	plain := &agentSessionPlainPlatform{name: "plain"}
	e := NewEngine("project-a", nil, []Platform{plain}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetAgentControllers(map[string]AgentController{"orca": &externalTestController{targets: []AgentControlTarget{target}}})

	if _, err := e.CreateExternalAgentGroup(context.Background(), target, "  ", ""); !errors.Is(err, ErrAgentSessionOwnerRequired) {
		t.Fatalf("error = %v, want ErrAgentSessionOwnerRequired", err)
	}
	if _, err := e.CreateExternalAgentGroup(context.Background(), target, "owner-1", ""); !errors.Is(err, ErrAgentSessionGroupUnsupported) {
		t.Fatalf("error = %v, want ErrAgentSessionGroupUnsupported", err)
	}
}

// A real machine runs dozens of worktrees and panes. The listing must page
// instead of emitting one oversized card, and item numbers must stay absolute
// so `/orca show 25` addresses the same agent shown as "25." on page 2.
func TestExternalBackendListingPagesWithAbsoluteNumbers(t *testing.T) {
	targets := make([]AgentControlTarget, 0, listPageSize+5)
	for i := 0; i < listPageSize+5; i++ {
		targets = append(targets, AgentControlTarget{
			ID:       fmt.Sprintf("wt/%02d", i),
			Revision: fmt.Sprintf("rev-%02d", i),
			Backend:  "orca",
			Title:    fmt.Sprintf("agent-%02d", i),
		})
	}
	e := NewEngine("test", nil, nil, "", LangEnglish)
	e.SetAgentControllers(map[string]AgentController{"orca": &externalTestController{targets: targets}})

	first := e.renderExternalBackendCard("orca", "key").RenderText()
	if !strings.Contains(first, "agent-00") || strings.Contains(first, "agent-20") {
		t.Fatalf("page 1 holds the wrong window:\n%s", first)
	}
	second := e.renderExternalBackendCard("orca", "key", 2).RenderText()
	if strings.Contains(second, "agent-00") || !strings.Contains(second, "agent-20") {
		t.Fatalf("page 2 holds the wrong window:\n%s", second)
	}
	// Absolute numbering: the 21st agent is labelled 21 on page 2.
	if !strings.Contains(second, "**21.** agent-20") {
		t.Fatalf("page 2 renumbered its rows:\n%s", second)
	}
	_, target, err := e.resolveExternalTarget("orca", "21")
	if err != nil || target.ID != "wt/20" {
		t.Fatalf("selector 21 resolved to %q (err=%v), want wt/20", target.ID, err)
	}
	// An out-of-range page clamps to the last page instead of rendering empty.
	if last := e.renderExternalBackendCard("orca", "key", 99).RenderText(); !strings.Contains(last, "agent-24") {
		t.Fatalf("out-of-range page did not clamp:\n%s", last)
	}
	// Page navigation must round-trip through handleCardNav.
	nav := e.handleCardNav("nav:/orca page 2", "feishu:chat-1:user-1")
	if nav == nil || !strings.Contains(nav.RenderText(), "agent-20") {
		t.Fatalf("page nav card = %#v", nav)
	}
}

// The console reads live terminal output and can type into a local terminal,
// so it must be admin-gated and operator-disableable exactly like /shell.
// Backend commands carry no builtin cmdID, so they reach handleCommand's
// default branch and would otherwise bypass both checks entirely.
func TestExternalAgentConsoleRequiresAdminAndHonoursDisabledCmds(t *testing.T) {
	newEngine := func() (*Engine, *externalTestController, *stubPlatformEngine) {
		controller := &externalTestController{targets: []AgentControlTarget{externalTestTarget()}, tail: "secret output"}
		platform := &stubPlatformEngine{n: "feishu"}
		e := NewEngine("test", nil, []Platform{platform}, "", LangEnglish)
		e.SetAgentControllers(map[string]AgentController{"orca": controller})
		return e, controller, platform
	}
	msg := func() *Message {
		return &Message{SessionKey: "feishu:chat-1:user-1", Platform: "feishu", ChannelKey: "chat-1", UserID: "user-1", ReplyCtx: "ctx"}
	}

	t.Run("non-admin cannot list", func(t *testing.T) {
		e, _, platform := newEngine()
		e.SetAdminFrom("someone-else")
		if !e.handleCommand(platform, msg(), "/orca") {
			t.Fatal("command was not consumed")
		}
		sent := strings.Join(platform.getSent(), "\n")
		if strings.Contains(sent, "sol worker") {
			t.Fatalf("non-admin saw the agent listing: %s", sent)
		}
	})

	t.Run("non-admin cannot send", func(t *testing.T) {
		e, controller, platform := newEngine()
		e.SetAdminFrom("someone-else")
		e.handleCommand(platform, msg(), "/orca send 1 rm -rf /")
		if controller.sent != "" {
			t.Fatalf("non-admin injected %q into a live terminal", controller.sent)
		}
	})

	t.Run("non-admin cannot forward through a bound group", func(t *testing.T) {
		controller := &externalTestController{targets: []AgentControlTarget{externalTestTarget()}}
		platform := &agentSessionGroupPlatform{
			agentSessionPlainPlatform: agentSessionPlainPlatform{name: "feishu"},
			chatID:                    "chat-1",
		}
		e := NewEngine("test", nil, []Platform{platform}, "", LangEnglish)
		e.SetDataDir(t.TempDir())
		e.SetAgentControllers(map[string]AgentController{"orca": controller})
		if _, err := e.CreateExternalAgentGroup(context.Background(), externalTestTarget(), "owner-1", ""); err != nil {
			t.Fatalf("CreateExternalAgentGroup() error = %v", err)
		}
		e.SetAdminFrom("someone-else")
		e.forwardExternalAgentGroupMessage(platform, msg(), "rm -rf /")
		if controller.sent != "" {
			t.Fatalf("non-admin forwarded %q through a bound group", controller.sent)
		}
	})

	t.Run("admin can list", func(t *testing.T) {
		e, _, platform := newEngine()
		e.SetAdminFrom("user-1")
		e.handleCommand(platform, msg(), "/orca")
		if sent := strings.Join(platform.getSent(), "\n"); !strings.Contains(sent, "sol worker") {
			t.Fatalf("admin did not get the listing: %s", sent)
		}
	})

	t.Run("disabled_cmds agents blocks the whole console", func(t *testing.T) {
		e, controller, platform := newEngine()
		e.SetAdminFrom("*")
		e.disabledCmds = map[string]bool{"agents": true}
		e.handleCommand(platform, msg(), "/orca send 1 hello")
		if controller.sent != "" {
			t.Fatalf("a disabled console still sent %q", controller.sent)
		}
		if sent := strings.Join(platform.getSent(), "\n"); strings.Contains(sent, "sol worker") {
			t.Fatalf("a disabled console still listed agents: %s", sent)
		}
	})
}
