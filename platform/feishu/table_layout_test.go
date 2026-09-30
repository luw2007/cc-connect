package feishu

import (
	"strings"
	"testing"
)

func TestCardTableUsesVerticalLayout(t *testing.T) {
	input := "概览\n| 项目 | 说明 |\n| --- | --- |\n| 长名称 | 内容较长，避免窄屏列折行 |\n| 第二项 | 正常 |\n结尾"
	got := sanitizeCardMarkdownForCard(input)
	for _, want := range []string{"概览", "- **项目**: 长名称", "- **说明**: 内容较长，避免窄屏列折行", "- **项目**: 第二项", "结尾"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "| --- |") {
		t.Fatalf("table remains: %q", got)
	}
}

func TestCardTableLeavesFencedCodeAlone(t *testing.T) {
	input := "```\n| a | b |\n| --- | --- |\n| x | y |\n```"
	if got := sanitizeCardMarkdownForCard(input); got != input {
		t.Fatalf("code changed: %q", got)
	}
}
