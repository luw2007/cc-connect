package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) renderSessionInfoCard(sessionKey string) *Card {
	agent, sessions := e.sessionContextForKey(sessionKey)
	session := sessions.GetActive(sessionKey)
	if session == nil {
		cb := NewCard().Title(e.i18n.T(MsgInfoCardTitle), "grey")
		cb.Markdown(e.i18n.T(MsgInfoNoSession))
		cb.Buttons(e.cardBackButton())
		return cb.Build()
	}

	agentName := "unknown"
	if agent != nil {
		agentName = agent.Name()
	}

	workDir := ""
	if switcher, ok := agent.(WorkDirSwitcher); ok {
		workDir = switcher.GetWorkDir()
		if absDir, err := filepath.Abs(workDir); err == nil {
			workDir = absDir
		}
	}

	duration := session.Duration()
	durationStr := formatDuration(duration)

	session.mu.Lock()
	msgCount := len(session.History)
	sessionID := session.ID
	sessionName := session.Name
	session.mu.Unlock()

	headerTitle := "📊 " + agentName
	if e.name != "" {
		headerTitle += " · " + e.name
	}
	if sessionName != "" {
		headerTitle += " — " + sessionName
	}

	cb := NewCard().Title(headerTitle, "blue")

	var infoLines []string
	infoLines = append(infoLines, e.i18n.Tf(MsgInfoAgent, agentName))
	if workDir != "" {
		infoLines = append(infoLines, e.i18n.Tf(MsgInfoWorkspace, workDir))
	}
	infoLines = append(infoLines, e.i18n.Tf(MsgInfoDuration, durationStr))
	infoLines = append(infoLines, e.i18n.Tf(MsgInfoMessages, msgCount))
	infoLines = append(infoLines, e.i18n.Tf(MsgInfoSessionID, sessionID))
	cb.Markdown(strings.Join(infoLines, "\n"))

	cb.Divider()

	// Action buttons row 1: Screenshot, Terminal, Files
	cb.Buttons(
		DefaultBtn(e.i18n.T(MsgInfoBtnScreenshot), "act:/screenshot"),
		PrimaryBtn(e.i18n.T(MsgInfoBtnTerminal), "act:/open-terminal"),
		DefaultBtn(e.i18n.T(MsgInfoBtnFiles), "nav:/dir"),
	)

	// Action buttons row 2: Commands, Resources
	cb.Buttons(
		DefaultBtn(e.i18n.T(MsgInfoBtnCommands), "nav:/info commands"),
		DefaultBtn(e.i18n.T(MsgInfoBtnResources), "nav:/info resources"),
	)

	cb.Buttons(e.cardBackButton())
	return cb.Build()
}

func (e *Engine) renderSessionCommandsCard(sessionKey string) *Card {
	_, sessions := e.sessionContextForKey(sessionKey)
	session := sessions.GetActive(sessionKey)

	cb := NewCard().Title(e.i18n.T(MsgInfoCommandsTitle), "turquoise")

	if session == nil {
		cb.Markdown(e.i18n.T(MsgInfoNoSession))
		cb.Buttons(e.cardBackButton())
		return cb.Build()
	}

	session.mu.Lock()
	cmds := make([]string, len(session.CommandHistory))
	copy(cmds, session.CommandHistory)
	session.mu.Unlock()

	if len(cmds) == 0 {
		cb.Markdown(e.i18n.T(MsgInfoCommandsEmpty))
	} else {
		var lines []string
		for i := len(cmds) - 1; i >= 0; i-- {
			lines = append(lines, fmt.Sprintf("%d. `%s`", len(cmds)-i, cmds[i]))
		}
		cb.Markdown(strings.Join(lines, "\n"))
	}

	cb.Buttons(
		DefaultBtn("← Back", "nav:/info"),
		e.cardBackButton(),
	)
	return cb.Build()
}

func (e *Engine) renderSessionResourcesCard(sessionKey string) *Card {
	_, sessions := e.sessionContextForKey(sessionKey)
	session := sessions.GetActive(sessionKey)

	cb := NewCard().Title(e.i18n.T(MsgInfoResourcesTitle), "violet")

	if session == nil {
		cb.Markdown(e.i18n.T(MsgInfoNoSession))
		cb.Buttons(e.cardBackButton())
		return cb.Build()
	}

	duration := session.Duration()

	session.mu.Lock()
	msgCount := len(session.History)
	cmdCount := len(session.CommandHistory)
	session.mu.Unlock()

	var lines []string
	lines = append(lines, fmt.Sprintf("**Uptime:** %s", formatDuration(duration)))
	lines = append(lines, fmt.Sprintf("**Messages:** %d", msgCount))
	lines = append(lines, fmt.Sprintf("**Commands:** %d", cmdCount))
	cb.Markdown(strings.Join(lines, "\n"))

	cb.Buttons(
		DefaultBtn("← Back", "nav:/info"),
		e.cardBackButton(),
	)
	return cb.Build()
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
