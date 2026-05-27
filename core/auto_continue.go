package core

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

type CompletionStatus int

const (
	CompletionComplete     CompletionStatus = iota
	CompletionIncomplete
	CompletionInconclusive
)

type AutoContinueDetector struct {
	incomplete  []*regexp.Regexp
	complete    []*regexp.Regexp
	llmFallback bool
	llmProvider string
	llmModel    string
	mu          sync.RWMutex
}

func NewAutoContinueDetector(keywords, keywordsComplete []string, llmFallback bool, llmProvider, llmModel string) (*AutoContinueDetector, error) {
	d := &AutoContinueDetector{
		llmFallback: llmFallback,
		llmProvider: llmProvider,
		llmModel:    llmModel,
	}
	for _, pat := range keywords {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return nil, fmt.Errorf("invalid incomplete keyword pattern %q: %w", pat, err)
		}
		d.incomplete = append(d.incomplete, re)
	}
	for _, pat := range keywordsComplete {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return nil, fmt.Errorf("invalid complete keyword pattern %q: %w", pat, err)
		}
		d.complete = append(d.complete, re)
	}
	return d, nil
}

const detectTailLen = 2000

func (d *AutoContinueDetector) Detect(response string) CompletionStatus {
	if d == nil {
		return CompletionComplete
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	tail := response
	if len(tail) > detectTailLen {
		tail = tail[len(tail)-detectTailLen:]
	}

	for _, re := range d.complete {
		if re.MatchString(tail) {
			return CompletionComplete
		}
	}
	for _, re := range d.incomplete {
		if re.MatchString(tail) {
			return CompletionIncomplete
		}
	}
	return CompletionInconclusive
}

func (d *AutoContinueDetector) NeedsLLMFallback() bool {
	if d == nil {
		return false
	}
	return d.llmFallback
}

func (d *AutoContinueDetector) LLMConfig() (provider, model string) {
	if d == nil {
		return "", ""
	}
	return d.llmProvider, d.llmModel
}

type LLMJudger interface {
	JudgeCompletion(ctx context.Context, response string, provider, model string) (bool, error)
}
