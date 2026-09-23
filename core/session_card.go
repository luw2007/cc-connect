package core

// SessionCardStatus represents the current session state for card rendering.
type SessionCardStatus string

const (
	SessionStatusWaiting SessionCardStatus = "waiting"
	SessionStatusRunning SessionCardStatus = "running"
	SessionStatusDone    SessionCardStatus = "done"
)

// Keyboard page identifiers for the terminal control card.
const (
	KbPageNone     = ""
	KbPageBasic    = "basic"
	KbPageModifier = "modifier"
	KbPageNav      = "nav"
	KbPageCommand  = "command"
)

// RenderSessionActionCard builds an interactive card with action buttons
// and optional keyboard pages for terminal control.
// keyboardPage selects which keyboard section to display ("", "basic", "modifier", "nav", "command").
func RenderSessionActionCard(i18n *I18n, agentName, project string, status SessionCardStatus, hasTerminal bool, keyboardPage string) *Card {
	var headerColor string
	var statusText string
	switch status {
	case SessionStatusWaiting:
		headerColor = "green"
		statusText = i18n.T(MsgSessionStatusWaiting)
	case SessionStatusRunning:
		headerColor = "blue"
		statusText = i18n.T(MsgSessionStatusRunning)
	case SessionStatusDone:
		headerColor = "grey"
		statusText = i18n.T(MsgSessionStatusDone)
	default:
		headerColor = "blue"
		statusText = string(status)
	}

	title := "🖥 " + agentName
	if project != "" {
		title += " · " + project
	}
	title += " — " + statusText

	b := NewCard().Title(title, headerColor)

	// Action buttons row
	actionBtns := []CardButton{
		DefaultBtn(i18n.T(MsgSessionScreenshot), "act:/screenshot"),
		DefaultBtn(i18n.T(MsgSessionExportText), "act:/export-text"),
	}
	if hasTerminal {
		actionBtns = append(actionBtns,
			PrimaryBtn(i18n.T(MsgSessionOpenTerminal), "act:/open-terminal"),
			DefaultBtn(i18n.T(MsgSessionGetLink), "act:/get-link"),
		)
	}
	b.Buttons(actionBtns...)

	// Keyboard section (only for non-done sessions)
	if status != SessionStatusDone {
		b.Divider()
		renderKeyboardSection(b, keyboardPage)
	}

	// Close / restart button
	if status == SessionStatusDone {
		b.Buttons(PrimaryBtn("🔄 "+i18n.T(MsgSessionRestarting), "act:/restart-session"))
	} else {
		b.Buttons(DangerBtn(i18n.T(MsgSessionClose), "act:/close-session"))
	}

	return b.Build()
}

// renderKeyboardSection adds the keyboard tab bar and the selected page's keys.
func renderKeyboardSection(b *CardBuilder, page string) {
	if page == KbPageNone {
		// Collapsed mode: just show a "show keyboard" button
		b.Buttons(DefaultBtn("⌨️ Keyboard", "nav:/kb basic"))
		return
	}

	// Tab bar
	tabBtn := func(label, key string) CardButton {
		if key == page {
			return PrimaryBtn("·"+label+"·", "nav:/kb "+key)
		}
		return DefaultBtn(label, "nav:/kb "+key)
	}
	b.Buttons(
		tabBtn("Basic", KbPageBasic),
		tabBtn("Ctrl", KbPageModifier),
		tabBtn("Nav", KbPageNav),
		tabBtn("Cmd", KbPageCommand),
		DefaultBtn("✕", "nav:/kb hide"),
	)

	// Page content
	switch page {
	case KbPageBasic:
		renderKbBasic(b)
	case KbPageModifier:
		renderKbModifier(b)
	case KbPageNav:
		renderKbNav(b)
	case KbPageCommand:
		renderKbCommand(b)
	}
}

func renderKbBasic(b *CardBuilder) {
	b.ButtonsEqual(
		DefaultBtn("Esc", "act:/key Escape"),
		DefaultBtn("^C", "act:/key C-c"),
		DefaultBtn("Tab", "act:/key Tab"),
		DefaultBtn("Space", "act:/key Space"),
		DefaultBtn("Enter", "act:/key Enter"),
	)
	b.ButtonsEqual(
		DefaultBtn("←", "act:/key Left"),
		DefaultBtn("↑", "act:/key Up"),
		DefaultBtn("↓", "act:/key Down"),
		DefaultBtn("→", "act:/key Right"),
		DefaultBtn("⇞", "act:/key PPage"),
		DefaultBtn("⇟", "act:/key NPage"),
	)
}

func renderKbModifier(b *CardBuilder) {
	b.ButtonsEqual(
		DefaultBtn("^A", "act:/key C-a"),
		DefaultBtn("^B", "act:/key C-b"),
		DefaultBtn("^D", "act:/key C-d"),
		DefaultBtn("^E", "act:/key C-e"),
		DefaultBtn("^F", "act:/key C-f"),
	)
	b.ButtonsEqual(
		DefaultBtn("^K", "act:/key C-k"),
		DefaultBtn("^L", "act:/key C-l"),
		DefaultBtn("^N", "act:/key C-n"),
		DefaultBtn("^P", "act:/key C-p"),
		DefaultBtn("^R", "act:/key C-r"),
	)
	b.ButtonsEqual(
		DefaultBtn("^U", "act:/key C-u"),
		DefaultBtn("^W", "act:/key C-w"),
		DefaultBtn("^Z", "act:/key C-z"),
		DefaultBtn("M-b", "act:/key M-b"),
		DefaultBtn("M-f", "act:/key M-f"),
	)
}

func renderKbNav(b *CardBuilder) {
	b.ButtonsEqual(
		DefaultBtn("Home", "act:/key Home"),
		DefaultBtn("End", "act:/key End"),
		DefaultBtn("PgUp", "act:/key PPage"),
		DefaultBtn("PgDn", "act:/key NPage"),
	)
	b.ButtonsEqual(
		DefaultBtn("←", "act:/key Left"),
		DefaultBtn("→", "act:/key Right"),
		DefaultBtn("↑", "act:/key Up"),
		DefaultBtn("↓", "act:/key Down"),
	)
}

func renderKbCommand(b *CardBuilder) {
	b.ButtonsEqual(
		PrimaryBtn("y (yes)", "act:/key y"),
		DangerBtn("n (no)", "act:/key n"),
		DangerBtn("^C interrupt", "act:/key C-c"),
	)
	b.ButtonsEqual(
		DefaultBtn("/compact", "act:/cmd compact"),
		DefaultBtn("/clear", "act:/cmd clear"),
		DefaultBtn("/diff", "act:/cmd diff"),
	)
}
