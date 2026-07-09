# delegate

The first client of [`agentbus`](https://github.com/charlesnpx/agentbus):
a delegation CLI plus orchestrator skills for cross-backend agent handoffs
(Claude Code orchestrating Codex, and Codex orchestrating Claude Code).

delegate owns all the delegation opinions as data it passes to agentbus,
never as engine code:

- The **delegate-report shape spec** (declarative, `go:embed`-ed) and the
  **digest text** synced from mise-en-place's `delegate-contract` skill.
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

delegate imports `agentbus/client` and `agentbus/engine` as pinned Go
modules; the dependency direction is one-way (delegate → agentbus).

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
