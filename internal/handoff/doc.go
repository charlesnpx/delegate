// Package handoff manages private prompt handoff files and job-input cleanup.
package handoff

const packageRole = "handoff"

// PromptFlagUsage documents the security caveat for --prompt.
const PromptFlagUsage = "--prompt text is visible in process argv and may be stored in shell history; use --prompt-file, --prompt-stdin, or --handoff-prompt-file for sensitive prompts"

// PositionalPromptUsage documents the security caveat for positional prompts.
const PositionalPromptUsage = "positional prompt text is visible in process argv and may be stored in shell history; use --prompt-file, --prompt-stdin, or --handoff-prompt-file for sensitive prompts"
