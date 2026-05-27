package core

import (
	"strings"
	"testing"
)

func defaultI18n() *I18n {
	return NewI18n("en")
}

func allButtonValues(card *Card) []string {
	var vals []string
	for _, el := range card.Elements {
		if row, ok := el.(CardActions); ok {
			for _, btn := range row.Buttons {
				vals = append(vals, btn.Value)
			}
		}
	}
	return vals
}

func containsValue(vals []string, target string) bool {
	for _, v := range vals {
		if v == target {
			return true
		}
	}
	return false
}

func TestRenderSessionActionCard_HeaderColors(t *testing.T) {
	i18n := defaultI18n()
	tests := []struct {
		status SessionCardStatus
		color  string
	}{
		{SessionStatusWaiting, "green"},
		{SessionStatusRunning, "blue"},
		{SessionStatusDone, "grey"},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			card := RenderSessionActionCard(i18n, "claude", "myproject", tt.status, false, KbPageBasic)
			if card.Header == nil {
				t.Fatal("card.Header is nil")
			}
			if card.Header.Color != tt.color {
				t.Fatalf("header color = %q, want %q", card.Header.Color, tt.color)
			}
		})
	}
}

func TestRenderSessionActionCard_DefaultStatus(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "", "custom-status", false, KbPageBasic)
	if card.Header.Color != "blue" {
		t.Fatalf("default status color = %q, want %q", card.Header.Color, "blue")
	}
	if !strings.Contains(card.Header.Title, "custom-status") {
		t.Fatalf("title %q does not contain status text", card.Header.Title)
	}
}

func TestRenderSessionActionCard_HasTerminalTrue(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "proj", SessionStatusRunning, true, KbPageBasic)
	vals := allButtonValues(card)
	if !containsValue(vals, "act:/open-terminal") {
		t.Error("expected act:/open-terminal button when hasTerminal=true")
	}
	if !containsValue(vals, "act:/screenshot") {
		t.Error("expected act:/screenshot button")
	}
	if !containsValue(vals, "act:/get-link") {
		t.Error("expected act:/get-link button when hasTerminal=true")
	}
}

func TestRenderSessionActionCard_HasTerminalFalse(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "proj", SessionStatusRunning, false, KbPageBasic)
	vals := allButtonValues(card)
	if containsValue(vals, "act:/open-terminal") {
		t.Error("act:/open-terminal should not appear when hasTerminal=false")
	}
	if !containsValue(vals, "act:/screenshot") {
		t.Error("expected act:/screenshot button")
	}
}

func TestRenderSessionActionCard_QuickKeys(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "", SessionStatusWaiting, false, KbPageBasic)
	vals := allButtonValues(card)
	for _, key := range []string{
		"act:/key Escape",
		"act:/key C-c",
		"act:/key Enter",
	} {
		if !containsValue(vals, key) {
			t.Errorf("expected quick key button %q", key)
		}
	}
}

func TestRenderSessionActionCard_TitleIncludesProject(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "myrepo", SessionStatusDone, false, KbPageNone)
	if !strings.Contains(card.Header.Title, "myrepo") {
		t.Fatalf("title %q does not contain project name", card.Header.Title)
	}
}

func TestRenderSessionActionCard_TitleNoProject(t *testing.T) {
	card := RenderSessionActionCard(defaultI18n(), "claude", "", SessionStatusDone, false, KbPageNone)
	if strings.Contains(card.Header.Title, "·") {
		t.Fatalf("title %q should not contain · when project is empty", card.Header.Title)
	}
}

func TestRenderSessionActionCard_KeyboardPages(t *testing.T) {
	i18n := defaultI18n()

	t.Run("none shows keyboard button", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusRunning, false, KbPageNone)
		vals := allButtonValues(card)
		if !containsValue(vals, "nav:/kb basic") {
			t.Error("collapsed mode should show 'Keyboard' button with nav:/kb basic")
		}
		if containsValue(vals, "act:/key Escape") {
			t.Error("collapsed mode should not show key buttons")
		}
	})

	t.Run("basic page shows keys", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusRunning, false, KbPageBasic)
		vals := allButtonValues(card)
		if !containsValue(vals, "act:/key Escape") {
			t.Error("basic page should show Escape key")
		}
		if !containsValue(vals, "act:/key Left") {
			t.Error("basic page should show arrow keys")
		}
	})

	t.Run("modifier page shows ctrl keys", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusRunning, false, KbPageModifier)
		vals := allButtonValues(card)
		if !containsValue(vals, "act:/key C-a") {
			t.Error("modifier page should show C-a")
		}
		if !containsValue(vals, "act:/key M-f") {
			t.Error("modifier page should show M-f")
		}
	})

	t.Run("nav page shows navigation keys", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusRunning, false, KbPageNav)
		vals := allButtonValues(card)
		if !containsValue(vals, "act:/key Home") {
			t.Error("nav page should show Home key")
		}
		if !containsValue(vals, "act:/key End") {
			t.Error("nav page should show End key")
		}
	})

	t.Run("command page shows agent commands", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusRunning, false, KbPageCommand)
		vals := allButtonValues(card)
		if !containsValue(vals, "act:/key y") {
			t.Error("command page should show y key")
		}
		if !containsValue(vals, "act:/cmd compact") {
			t.Error("command page should show /compact command")
		}
	})

	t.Run("done status hides keyboard", func(t *testing.T) {
		card := RenderSessionActionCard(i18n, "claude", "p", SessionStatusDone, false, KbPageBasic)
		vals := allButtonValues(card)
		if containsValue(vals, "act:/key Escape") {
			t.Error("done status should not show keyboard keys")
		}
		if containsValue(vals, "nav:/kb basic") {
			t.Error("done status should not show keyboard tab")
		}
	})
}
