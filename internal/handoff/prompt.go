package handoff

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	// ErrNoPromptSource means no prompt source was supplied.
	ErrNoPromptSource = errors.New("no prompt source supplied")
	// ErrMultiplePromptSources means more than one mutually exclusive prompt source was supplied.
	ErrMultiplePromptSources = errors.New("multiple prompt sources supplied")
)

// PromptSourceKind records where a prompt was read from.
type PromptSourceKind string

const (
	SourcePrompt            PromptSourceKind = "prompt"
	SourcePromptFile        PromptSourceKind = "prompt_file"
	SourcePromptStdin       PromptSourceKind = "prompt_stdin"
	SourceHandoffPromptFile PromptSourceKind = "handoff_prompt_file"
	SourcePositional        PromptSourceKind = "positional"
)

// PromptSources are the mutually exclusive prompt input choices accepted by task-like commands.
type PromptSources struct {
	Prompt            string
	PromptSet         bool
	PromptFile        string
	PromptStdin       bool
	HandoffPromptFile string
	StateDir          string
	Positional        []string
	Stdin             io.Reader
}

// ResolvedPrompt contains the prompt bytes and source metadata needed for lifecycle cleanup.
type ResolvedPrompt struct {
	Prompt      string
	Source      PromptSourceKind
	HandoffPath string
}

// PromptSourceCount returns how many mutually exclusive prompt sources are set.
func PromptSourceCount(s PromptSources) int {
	count := 0
	if s.PromptSet || s.Prompt != "" {
		count++
	}
	if s.PromptFile != "" {
		count++
	}
	if s.PromptStdin {
		count++
	}
	if s.HandoffPromptFile != "" {
		count++
	}
	if len(s.Positional) > 0 {
		count++
	}
	return count
}

// ResolvePrompt reads exactly one prompt source.
func ResolvePrompt(s PromptSources) (ResolvedPrompt, error) {
	count := PromptSourceCount(s)
	if count == 0 {
		return ResolvedPrompt{}, ErrNoPromptSource
	}
	if count > 1 {
		return ResolvedPrompt{}, fmt.Errorf("%w: use only one of --prompt, --prompt-file, --prompt-stdin, --handoff-prompt-file, or positional prompt", ErrMultiplePromptSources)
	}
	if s.PromptSet || s.Prompt != "" {
		return ResolvedPrompt{Prompt: s.Prompt, Source: SourcePrompt}, nil
	}
	if s.PromptFile != "" {
		raw, err := os.ReadFile(s.PromptFile)
		if err != nil {
			return ResolvedPrompt{}, err
		}
		return ResolvedPrompt{Prompt: string(raw), Source: SourcePromptFile}, nil
	}
	if s.PromptStdin {
		reader := s.Stdin
		if reader == nil {
			reader = os.Stdin
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return ResolvedPrompt{}, err
		}
		return ResolvedPrompt{Prompt: string(raw), Source: SourcePromptStdin}, nil
	}
	if s.HandoffPromptFile != "" {
		raw, handoffPath, err := readHandoffPromptFile(s.HandoffPromptFile, s.StateDir)
		if err != nil {
			return ResolvedPrompt{}, err
		}
		return ResolvedPrompt{Prompt: string(raw), Source: SourceHandoffPromptFile, HandoffPath: handoffPath}, nil
	}
	return ResolvedPrompt{Prompt: strings.Join(s.Positional, " "), Source: SourcePositional}, nil
}
