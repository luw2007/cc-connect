package core

import (
	"fmt"
	"strings"
)

func (e *Engine) notesStorage() *MemoryStorage {
	if e.memoryExtractor == nil {
		return nil
	}
	return e.memoryExtractor.storage
}

func (e *Engine) cmdNotes(p Platform, msg *Message, args []string) {
	storage := e.notesStorage()
	if storage == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesNotAvailable))
		return
	}

	if len(args) == 0 {
		e.notesListAll(p, msg, storage)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"list", "user", "project", "search", "delete", "clear"})
	switch sub {
	case "list":
		e.notesListAll(p, msg, storage)
	case "user":
		e.notesListByType(p, msg, storage, MemoryTypeUser)
	case "project":
		e.notesListByType(p, msg, storage, MemoryTypeProject)
	case "search":
		query := strings.TrimSpace(strings.Join(args[1:], " "))
		if query == "" {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesSearchUsage))
			return
		}
		e.notesSearch(p, msg, storage, query)
	case "delete":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesDeleteUsage))
			return
		}
		e.notesDelete(p, msg, storage, args[1])
	case "clear":
		e.notesClear(p, msg, storage)
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesHelp))
	}
}

func (e *Engine) notesListAll(p Platform, msg *Message, storage *MemoryStorage) {
	projectPath := e.resolveProjectPath()
	userEntries, _ := storage.List(msg.UserID, MemoryTypeUser, "", 10)
	projectEntries, _ := storage.List(msg.UserID, MemoryTypeProject, projectPath, 10)

	if len(userEntries) == 0 && len(projectEntries) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.T(MsgNotesListHeader))
	sb.WriteString("\n")

	if len(projectEntries) > 0 {
		sb.WriteString("\n📁 Project:\n")
		for _, entry := range projectEntries {
			sb.WriteString(formatMemoryLine(entry))
		}
	}
	if len(userEntries) > 0 {
		sb.WriteString("\n👤 User:\n")
		for _, entry := range userEntries {
			sb.WriteString(formatMemoryLine(entry))
		}
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) notesListByType(p Platform, msg *Message, storage *MemoryStorage, memType MemoryType) {
	projectPath := ""
	if memType == MemoryTypeProject {
		projectPath = e.resolveProjectPath()
	}
	entries, err := storage.List(msg.UserID, memType, projectPath, 20)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesError), err))
		return
	}
	if len(entries) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgNotesTypeHeader), string(memType)))
	sb.WriteString("\n")
	for _, entry := range entries {
		sb.WriteString(formatMemoryLine(entry))
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) notesSearch(p Platform, msg *Message, storage *MemoryStorage, query string) {
	entries, err := storage.Search(msg.UserID, query)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesError), err))
		return
	}
	if len(entries) == 0 {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesSearchEmpty), query))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgNotesSearchHeader), query, len(entries)))
	sb.WriteString("\n")
	for _, entry := range entries {
		sb.WriteString(formatMemoryLine(entry))
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) notesDelete(p Platform, msg *Message, storage *MemoryStorage, id string) {
	entry, err := storage.Get(msg.UserID, id)
	if err != nil || entry == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesNotFound), id))
		return
	}
	if err := storage.Delete(msg.UserID, id); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesError), err))
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesDeleted), entry.Title))
}

func (e *Engine) notesClear(p Platform, msg *Message, storage *MemoryStorage) {
	projectPath := e.resolveProjectPath()
	if projectPath == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesClearNoProject))
		return
	}
	if err := storage.Clear(msg.UserID, MemoryTypeProject, projectPath); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNotesError), err))
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNotesClearDone))
}

func (e *Engine) resolveProjectPath() string {
	if e.baseWorkDir != "" {
		return e.baseWorkDir
	}
	if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
		return wd.GetWorkDir()
	}
	return ""
}

func formatMemoryLine(entry MemoryEntry) string {
	title := entry.Title
	if len([]rune(title)) > 60 {
		title = string([]rune(title)[:57]) + "..."
	}
	tags := ""
	if len(entry.Tags) > 0 {
		tags = " (" + strings.Join(entry.Tags, ", ") + ")"
	}
	return fmt.Sprintf("  [%s] %s%s\n    ID: %s\n", entry.Category, title, tags, entry.ID)
}
