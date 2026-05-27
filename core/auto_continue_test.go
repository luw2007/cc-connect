package core

import (
	"strings"
	"testing"
)

func TestAutoContinueDetector_Detect(t *testing.T) {
	tests := []struct {
		name       string
		keywords   []string
		kwComplete []string
		response   string
		want       CompletionStatus
	}{
		{
			name:     "nil detector returns Complete",
			response: "anything here",
			want:     CompletionComplete,
		},
		{
			name:       "complete keyword match",
			kwComplete: []string{"Done"},
			response:   "All done!",
			want:       CompletionComplete,
		},
		{
			name:     "incomplete keyword match",
			keywords: []string{"TODO"},
			response: "TODO: implement X",
			want:     CompletionIncomplete,
		},
		{
			name:       "inconclusive when no match",
			keywords:   []string{"TODO"},
			kwComplete: []string{"Done"},
			response:   "The code looks good.",
			want:       CompletionInconclusive,
		},
		{
			name:       "complete takes priority over incomplete",
			keywords:   []string{"continue"},
			kwComplete: []string{"All tasks completed"},
			response:   "All tasks completed, will continue later",
			want:       CompletionComplete,
		},
		{
			name:     "chinese keywords",
			keywords: []string{"还没完成"},
			response: "这个功能还没完成",
			want:     CompletionIncomplete,
		},
		{
			name:       "case insensitive",
			kwComplete: []string{"done"},
			response:   "DONE",
			want:       CompletionComplete,
		},
		{
			name:     "tail truncation still detects at end",
			keywords: []string{"TODO"},
			response: strings.Repeat("x", 3000) + " TODO: next",
			want:     CompletionIncomplete,
		},
		{
			name:     "pattern in discarded head is not detected",
			keywords: []string{"TODO"},
			response: "TODO: first\n" + strings.Repeat("x", 3000),
			want:     CompletionInconclusive,
		},
		{
			name:       "empty response is inconclusive",
			keywords:   []string{"TODO"},
			kwComplete: []string{"Done"},
			response:   "",
			want:       CompletionInconclusive,
		},
		{
			name:     "regex pattern works",
			keywords: []string{`I('ll| will) continue`},
			response: "I'll continue working on this later",
			want:     CompletionIncomplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d *AutoContinueDetector
			if tt.keywords != nil || tt.kwComplete != nil {
				var err error
				d, err = NewAutoContinueDetector(tt.keywords, tt.kwComplete, false, "", "")
				if err != nil {
					t.Fatalf("NewAutoContinueDetector: %v", err)
				}
			}
			got := d.Detect(tt.response)
			if got != tt.want {
				t.Errorf("Detect() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAutoContinueDetector_InvalidRegex(t *testing.T) {
	_, err := NewAutoContinueDetector([]string{"[invalid"}, nil, false, "", "")
	if err == nil {
		t.Error("expected error for invalid regex")
	}

	_, err = NewAutoContinueDetector(nil, []string{"(unclosed"}, false, "", "")
	if err == nil {
		t.Error("expected error for invalid complete regex")
	}
}

func TestAutoContinueDetector_NeedsLLMFallback(t *testing.T) {
	d, _ := NewAutoContinueDetector(nil, nil, true, "anthropic", "claude-sonnet")
	if !d.NeedsLLMFallback() {
		t.Error("expected NeedsLLMFallback() = true")
	}

	d2, _ := NewAutoContinueDetector(nil, nil, false, "", "")
	if d2.NeedsLLMFallback() {
		t.Error("expected NeedsLLMFallback() = false")
	}

	var nilD *AutoContinueDetector
	if nilD.NeedsLLMFallback() {
		t.Error("nil detector NeedsLLMFallback() should be false")
	}
}

func TestAutoContinueDetector_LLMConfig(t *testing.T) {
	d, _ := NewAutoContinueDetector(nil, nil, true, "anthropic", "claude-sonnet")
	provider, model := d.LLMConfig()
	if provider != "anthropic" || model != "claude-sonnet" {
		t.Errorf("LLMConfig() = (%q, %q), want (anthropic, claude-sonnet)", provider, model)
	}

	var nilD *AutoContinueDetector
	p, m := nilD.LLMConfig()
	if p != "" || m != "" {
		t.Errorf("nil detector LLMConfig() = (%q, %q), want empty", p, m)
	}
}
