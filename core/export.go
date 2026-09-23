package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) cmdExport(p Platform, msg *Message, args []string) {
	format := "md"
	limit := 0

	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "json":
			format = "json"
		case "md", "markdown":
			format = "md"
		default:
			if n, err := strconv.Atoi(arg); err == nil && n > 0 {
				limit = n
			}
		}
	}

	agent, sessions, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	s := sessions.GetOrCreateActive(msg.SessionKey)

	var entries []HistoryEntry
	agentSID := s.GetAgentSessionID()

	if agentSID != "" {
		if hp, ok := agent.(HistoryProvider); ok {
			if agentEntries, err := hp.GetSessionHistory(e.ctx, agentSID, limit); err == nil {
				entries = agentEntries
			}
		}
	}
	if len(entries) == 0 {
		fetchLimit := limit
		if fetchLimit <= 0 {
			fetchLimit = 10000
		}
		entries = s.GetHistory(fetchLimit)
	}

	if len(entries) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgExportEmpty))
		return
	}

	if len(entries) > 10000 {
		entries = entries[len(entries)-10000:]
	}

	sessionName := s.GetName()
	project := e.name
	var content []byte
	var mimeType, ext string

	switch format {
	case "json":
		content = formatExportJSON(entries, agentSID, project, sessionName)
		mimeType = "application/json"
		ext = "json"
	default:
		content = formatExportMarkdown(entries, agentSID, project, sessionName)
		mimeType = "text/markdown; charset=utf-8"
		ext = "md"
	}

	const maxSize = 10 * 1024 * 1024
	if len(content) > maxSize {
		content = content[:maxSize]
	}

	timestamp := time.Now().Format("20060102_150405")
	sidSlug := agentSID
	if len(sidSlug) > 12 {
		sidSlug = sidSlug[:12]
	}
	filename := fmt.Sprintf("export_%s_%s.%s", sidSlug, timestamp, ext)

	fileSender, ok := p.(FileSender)
	if !ok {
		text := string(content)
		if len([]rune(text)) > 4000 {
			text = string([]rune(text)[:3997]) + "..."
		}
		e.reply(p, msg.ReplyCtx, text)
		return
	}

	_ = e.waitOutgoing(p)
	if err := fileSender.SendFile(e.ctx, msg.ReplyCtx, FileAttachment{
		MimeType: mimeType,
		Data:     content,
		FileName: filename,
	}); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
	}
}

func formatExportMarkdown(entries []HistoryEntry, sessionID, project, sessionName string) []byte {
	var sb strings.Builder
	sb.WriteString("# Conversation Export\n\n")
	sb.WriteString(fmt.Sprintf("**Session**: `%s`\n", sessionID))
	sb.WriteString(fmt.Sprintf("**Project**: `%s`\n", project))
	sb.WriteString(fmt.Sprintf("**Exported At**: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("---\n\n")
	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("- Message Count: %d\n", len(entries)))
	if sessionName != "" {
		sb.WriteString(fmt.Sprintf("- Session Name: %s\n", sessionName))
	}
	sb.WriteString("\n---\n\n## Conversation\n\n")

	for _, h := range entries {
		icon := "👤"
		if h.Role == "assistant" {
			icon = "🤖"
		}
		sb.WriteString(fmt.Sprintf("%s [%s]\n%s\n\n---\n\n", icon, h.Timestamp.Format("15:04:05"), h.Content))
	}
	return []byte(sb.String())
}

func formatExportJSON(entries []HistoryEntry, sessionID, project, sessionName string) []byte {
	type jsonMessage struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
	}
	type jsonExport struct {
		Metadata struct {
			SessionID  string `json:"session_id"`
			Project    string `json:"project"`
			ExportedAt string `json:"exported_at"`
		} `json:"metadata"`
		Session struct {
			Name         string `json:"name"`
			MessageCount int    `json:"message_count"`
		} `json:"session"`
		Messages []jsonMessage `json:"messages"`
	}

	var out jsonExport
	out.Metadata.SessionID = sessionID
	out.Metadata.Project = project
	out.Metadata.ExportedAt = time.Now().Format(time.RFC3339)
	out.Session.Name = sessionName
	out.Session.MessageCount = len(entries)
	out.Messages = make([]jsonMessage, len(entries))
	for i, h := range entries {
		out.Messages[i] = jsonMessage{
			Role:      h.Role,
			Content:   h.Content,
			Timestamp: h.Timestamp.Format(time.RFC3339),
		}
	}

	data, _ := json.MarshalIndent(out, "", "  ")
	return data
}
