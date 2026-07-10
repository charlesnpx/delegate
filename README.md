# delegate

The first client of [`agentbus`](https://github.com/charlesnpx/agentbus):
a delegation CLI plus orchestrator skills for cross-backend agent handoffs
(Claude Code orchestrating Codex, and Codex orchestrating Claude Code).

delegate owns all the delegation opinions as data it passes to agentbus,
never as engine code:

- The **delegate-report shape spec** (declarative, `go:embed`-ed) and the
  **digest text** synced from mise-en-place's `delegate-contract` skill. The
  digest source of truth stays in the mise-en-place checkout; run
  `scripts/sync-digest.sh` to refresh delegate's embedded copy.
- **Tiered policy resolution** — CLI flags (`--write`, `--strict-contract`,
  `--no-contract`) resolved to a `TurnPolicy` attached CLI-side, so skills
  stay simple one-liners and agents never see or pass the shape/digest
  directly.
- **Review context assembly** (v0.1.1) — a sanitized-artifact model that
  redacts secrets before a backend ever sees a diff, rather than relying on
  sandboxing alone.
- **Envelopes** — a launch-time envelope and a richer terminal-result
  envelope (with contract stamp and result hash), returned by `delegate task`,
  `delegate review`, and `delegate adversarial-review`.
- **Prompt handoff** — `--prompt`, `--prompt-file`, `--prompt-stdin`,
  `--handoff-prompt-file`, and positional prompts are mutually exclusive.
  `--prompt` and positional prompt text are visible in process argv and may be
  stored in shell history; use file, stdin, or handoff sources for sensitive
  prompts.

delegate imports `agentbus/client` and `agentbus/engine` as pinned Go
modules; the dependency direction is one-way (delegate → agentbus).

## Task execution modes

`delegate task` uses daemon mode by default. It connects through
`agentbus/client`, lets the client autostart the local daemon when needed, gates
on `protocol.hello` capabilities, submits the task with the CLI-resolved
`TurnPolicy`, and returns the launch envelope unless `--wait` is set.

`delegate task --embedded --wait` runs the same policy/envelope pipeline through
the vendored `agentbus/engine` store and backend interface. The v0.1.0 vendored
engine does not include real Claude/Codex adapter constructors or a daemon to
supervise work after the CLI exits, so embedded background mode is intentionally
rejected; use daemon mode for background jobs.

This repo is currently private and pre-release. See
`~/tmp/agent-server-delegate-plan.md` (or the equivalent planning doc) for the
full design and delivery plan.

## Roadmap / v0.2

- **stop-review-gate**: today, replacing the vendor `openai-codex` plugin
  means losing its stop-review gate in v1. The planned v0.2 home for this is
  a small Claude Code Stop-hook client of agentbus (gating a turn via
  `turn.start` with an ALLOW/BLOCK shape contract plus `policy.validate`),
  not a delegate-owned feature. Until then, `delegate setup` reports
  `stop-review-gate: not available (planned v0.2)`.
