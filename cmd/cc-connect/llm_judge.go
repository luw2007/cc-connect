package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

type cliLLMJudger struct{}

func (j *cliLLMJudger) JudgeCompletion(ctx context.Context, response string, provider, model string) (bool, error) {
	if model == "" {
		model = "haiku"
	}

	tail := response
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}

	prompt := fmt.Sprintf(
		"Analyze the following AI agent response and determine if the agent has COMPLETED its task or STOPPED mid-task. "+
			"Reply with exactly one word: COMPLETE or INCOMPLETE.\n\n---\n%s", tail)

	args := []string{"-p", model, "--no-input", prompt}
	if provider != "" {
		args = []string{"-p", model, "--provider", provider, "--no-input", prompt}
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("claude CLI: %w", err)
	}

	result := strings.TrimSpace(string(out))
	return strings.EqualFold(result, "COMPLETE"), nil
}

var _ core.LLMJudger = (*cliLLMJudger)(nil)
