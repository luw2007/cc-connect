package core

import (
	"strings"
	"testing"
)

func TestListAllCardSwitch_StableIDPreservesHistory(t *testing.T) {
	const sessionKey = "test:list-all-user"
	current := AgentSessionInfo{ID: "session-shared-prefix-current", Summary: "Current workspace session"}
	target := AgentSessionInfo{ID: "session-shared-prefix-all-only", Summary: "Other workspace session"}
	agent := &agentSessionAllTestAgent{
		agentSessionTestAgent: &agentSessionTestAgent{name: "list-all-test", sessions: []AgentSessionInfo{current}},
		allSessions:           []AgentSessionInfo{target, current},
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	stored := e.sessions.SwitchToAgentSession(sessionKey, target.ID, agent.Name(), target.Summary)
	stored.AddHistory("user", "Keep the original request")
	stored.AddHistory("assistant", "Keep the original answer")
	e.sessions.SwitchToAgentSession(sessionKey, current.ID, agent.Name(), current.Summary)

	card, err := e.renderListCard(sessionKey, 1, true)
	if err != nil {
		t.Fatalf("render all sessions: %v", err)
	}
	button, ok := findCardAction(card, "act:/switch --all-id "+target.ID)
	if !ok {
		t.Fatal("all-only session has no stable switch action")
	}
	currentCard, err := e.renderListCard(sessionKey, 1, false)
	if err != nil {
		t.Fatalf("render current sessions: %v", err)
	}
	if _, ok := findCardAction(currentCard, "act:/switch --id "+current.ID); !ok {
		t.Fatal("current session has no stable switch action")
	}

	// The captured button must survive a new ordering and a different current list.
	agent.allSessions = []AgentSessionInfo{current, target}
	agent.sessions = []AgentSessionInfo{{ID: "new-current-session", Summary: "New current session"}, current}
	updated := e.handleCardNav(button.Value, sessionKey)
	active := e.sessions.GetOrCreateActive(sessionKey)
	if active.ID != stored.ID || active.GetAgentSessionID() != target.ID {
		t.Fatalf("selected session = %q/%q, want stored session %q/%q", active.ID, active.GetAgentSessionID(), stored.ID, target.ID)
	}
	history := active.GetHistory(0)
	if len(history) != 2 || history[0].Content != "Keep the original request" || history[1].Content != "Keep the original answer" {
		t.Fatalf("history after card switch = %#v, want original request and answer", history)
	}
	if updated == nil {
		t.Fatal("switch did not return an updated card")
	}
	selected, ok := findCardAction(updated, button.Value)
	if !ok || selected.Type != "primary" {
		t.Fatal("updated card does not retain the selected all-only session")
	}
	if _, ok := findCardAction(updated, "nav:/list 1"); !ok {
		t.Fatal("switch did not retain the all-sessions view")
	}
}

func TestListAll_TopicOwnershipOverridesBroadEnumeration(t *testing.T) {
	const (
		ownKey   = "test:shared-chat:topic-a"
		otherKey = "test:shared-chat:topic-b"
	)
	own := AgentSessionInfo{ID: "owned-by-topic-a", Summary: "Own topic conversation"}
	other := AgentSessionInfo{ID: "owned-by-topic-b", Summary: "Other topic conversation"}
	external := AgentSessionInfo{ID: "unowned-broad-session", Summary: "Unowned external conversation"}
	agent := &agentSessionAllTestAgent{
		agentSessionTestAgent: &agentSessionTestAgent{name: "list-all-test", sessions: []AgentSessionInfo{own, other}},
		allSessions:           []AgentSessionInfo{other, external, own},
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.multiWorkspace = true
	e.SetFilterExternalSessions(true)
	e.sessions.SwitchToAgentSession(ownKey, own.ID, agent.Name(), own.Summary)
	e.sessions.SwitchToAgentSession(otherKey, other.ID, agent.Name(), other.Summary)

	e.cmdList(p, &Message{Platform: p.Name(), SessionKey: ownKey}, []string{"all"})
	reply := strings.Join(p.getSent(), "\n")
	if !strings.Contains(reply, own.Summary) || !strings.Contains(reply, external.Summary) || strings.Contains(reply, other.Summary) {
		t.Fatalf("all-session text must include own and external conversations only: %q", reply)
	}
	card, err := e.renderListCard(ownKey, 1, true)
	if err != nil {
		t.Fatalf("render all sessions: %v", err)
	}
	if _, ok := findCardAction(card, "act:/switch --all-id "+own.ID); !ok {
		t.Fatal("all-session card hides the current topic's conversation")
	}
	staleButton, ok := findCardAction(card, "act:/switch --all-id "+external.ID)
	if !ok {
		t.Fatal("all-session card hides an unowned session returned only by ListAllSessions")
	}
	if _, ok := findCardAction(card, "act:/switch --all-id "+other.ID); ok {
		t.Fatal("all-session card exposes another topic's conversation")
	}
	if got := countCardActionValues(card, "act:/switch "); got != 2 {
		t.Fatalf("visible all-session rows = %d, want own and external only", got)
	}

	e.handleCardNav("act:/switch --all-id "+other.ID, ownKey)
	if got := e.sessions.GetOrCreateActive(ownKey).GetAgentSessionID(); got != own.ID {
		t.Fatalf("forged action selected %q, want own conversation %q", got, own.ID)
	}
	// An unowned row can become owned after rendering; clicks must recheck ownership.
	e.sessions.SwitchToAgentSession(otherKey, external.ID, agent.Name(), external.Summary)
	e.handleCardNav(staleButton.Value, ownKey)
	if got := e.sessions.GetOrCreateActive(ownKey).GetAgentSessionID(); got != own.ID {
		t.Fatalf("stale action selected %q after another topic claimed it, want %q", got, own.ID)
	}
}
