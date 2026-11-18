package feishu

import "testing"

func TestCardTableLeavesFencedCodeAlone(t *testing.T) {
	input := "```\n| a | b |\n| --- | --- |\n| x | y |\n```"
	if got := sanitizeCardMarkdownForCard(input); got != input {
		t.Fatalf("code changed: %q", got)
	}
}
