# delegate

`delegate` is the first client of [agentbus](https://github.com/charlesnpx/agentbus): a delegation CLI and managed skill matrix for handing work between Claude Code and Codex. Version 0.1.0 ships `task`, rescue, and job-control workflows in both directions.

agentbus owns execution, supervision, and generic policy enforcement. delegate owns the delegation-specific data and decisions it passes to agentbus: the embedded `delegate-report` contract, the delegate-contract digest, policy tiers, handoff lifecycle, skill matrix, and result envelopes.

## Install

delegate is a private, experimental mise-en-place entry. Install **agentbus first**: mise-en-place does not infer this dependency, and delegate cannot launch work until agentbus is installed and set up.

```sh
mise-en-place install agentbus
mise-en-place install delegate

agentbus setup --json
delegate setup --json
```

The delegated installers build from source, so Go must be on `PATH`. `delegate` installs its binary in `~/.local/bin` and its managed skills for the selected target. The installer reports the prerequisite explicitly in its JSON plan:

```sh
./install-skill.sh --plan --target all --json
./install-skill.sh --install --target all --json --install-root /tmp/delegate-stage
./install-skill.sh --uninstall --target all --json --install-root /tmp/delegate-stage
```

`agentbus` must be installed before using delegate skills; installing it first also ensures that `delegate setup --json` can discover its binary and validate required capabilities.

## CLI

```text
delegate version [--json]
delegate setup [--json]
delegate install-skills [--plan|--install|--uninstall] [--target claude|codex|all] [--json] [--install-root <abs>]

delegate handoff create --json

delegate task --backend claude|codex [--background|--wait] [--json] [--cwd <abs>]
              [--resume|--resume-session <id>|--fresh] [--model <model>] [--effort <effort>]
              [--timeout <duration>] [--write] [--strict-contract|--no-contract]
              [--origin <skill>] [--embedded] [prompt source]

delegate status [--job <id>] [--wait|--probe] [--json]
delegate result --job <id> [--wait] [--json]
delegate cancel --job <id> [--json]
```

Prompt sources are mutually exclusive: `--prompt`, `--prompt-file`, `--prompt-stdin`, `--handoff-prompt-file`, or positional text. `--prompt` and positional text are visible in process arguments and shell history; use stdin, a prompt file, or a handoff file for sensitive input.

Use `--resume-session <id>` to resume an explicit agentbus session. `--resume` selects the most recent session recorded in delegate job metadata for the selected backend and cwd. Omitting the resume flags, or passing `--fresh` explicitly, starts a new session.

Daemon mode is the default. It connects through `agentbus/client`, checks the protocol capabilities required by the selected policy, and returns a launch envelope unless `--wait` is set. `--embedded --wait` uses the vendored `agentbus/engine` for tests and foreground-only local execution; it intentionally cannot supervise a background job after the CLI exits.

## Rescue workflow

The rescue skills use a durable stdin handoff instead of exposing the delegated prompt in argv:

```sh
HANDOFF_PATH=$(
  printf '%s' 'Investigate the issue and report evidence.' |
    delegate handoff create --json |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["handoff_path"])'
)

delegate task --backend codex --origin codex:rescue --cwd "$PWD" \
  --handoff-prompt-file "$HANDOFF_PATH" --background --json
```

`delegate handoff create --json` creates a private `0600` handoff file in delegate state. `task` copies it to the job input, fsyncs it, and removes the handoff before launch. The job input is removed when a backend session is recorded or when a terminal job state is observed.

## Managed skills

The source directories escape `:` as `__colon__`; the installer decodes the names when it writes skills.

| Installed for | Skill names | Purpose |
| --- | --- | --- |
| Claude Code (`~/.claude/skills`) | `codex:rescue`, `codex:status`, `codex:result`, `codex:cancel`, `delegate:setup` | Delegate rescue work to Codex and control the returned job. |
| Codex (`${CODEX_HOME:-~/.codex}/skills`) | `claude:rescue`, `claude:status`, `claude:result`, `claude:cancel`, `delegate:setup` | Delegate rescue work to Claude Code and control the returned job. |

Launch skills preflight shared filesystem and state access, stdin handoff, no-fork execution, agentbus capabilities, and target-backend reachability. They launch only through `delegate task`, return the launch envelope verbatim, and never add `--no-contract`. Job-control skills use the same status, result, cancellation, evidence-preservation, and no-substitute-answer discipline.

## Contract tiers

`delegate-report` is structural validation, not proof that the task is correct or complete.

| Invocation | Contract result | Retry behavior |
| --- | --- | --- |
| Default read-only task | Inject digest, validate, stamp | No corrective retry; a malformed result is `completed_noncompliant`. |
| `--write` or `--strict-contract` | Inject digest, validate, stamp | At most one corrective resume. The resume is always read-only and instructs the backend to emit only the corrected report and make no further changes. |
| `--no-contract` | Enforcement disabled | No retry; terminal envelope has `contract.status: "disabled"` and `reason: "no_contract_flag"`. This is for direct CLI use only, never managed skills. |

## Envelope reference

Every envelope has `schema: 1` and `sha256`, where `sha256` is the SHA-256 of canonical JSON for that envelope with its own hash field excluded. `result_sha256` is agentbus's SHA-256 over the raw final assistant message bytes.

Launch envelopes are returned by a non-waiting task:

```json
{
  "schema": 1,
  "job_id": "job_…",
  "status": "queued",
  "result_sha256": null,
  "sha256": "…"
}
```

Terminal envelopes are returned by `delegate result --job <id>` and `delegate task --wait`:

```json
{
  "schema": 1,
  "job_id": "job_…",
  "status": "completed",
  "kind": "task",
  "contractKind": "shape",
  "contract": {
    "status": "compliant",
    "missing": [],
    "reason": "",
    "attempts": 1,
    "retryUsed": false,
    "contractSha256": "sha256:…",
    "validatedAt": "2026-01-01T00:00:00Z"
  },
  "result_sha256": "…",
  "sha256": "…"
}
```

Possible contract statuses are `compliant`, `retried`, `noncompliant`, `skipped`, and `disabled`. A terminal `result_sha256` is mandatory. `kind` is `task` in v0.1.0; review kinds arrive with v0.1.1.

## Setup and stall monitoring

`delegate setup --json` reports:

- agentbus discovery (`found`, path, version, protocol, and advertised backends);
- the full capability map, required delegate capabilities, and `capabilitiesOK` result;
- a status for every managed skill (`installed`, `missing`, `outdated`, or `unreadable`); and
- `stop_review_gate: "not available (planned v0.2)"`.

The non-JSON setup output always includes this exact line:

```text
stop-review-gate: not available (planned v0.2)
```

While a job is outstanding, poll `delegate status --job <id>` every 2–5 minutes rather than blocking indefinitely. An expired heartbeat lease is an immediate stall signal. Otherwise run `delegate status --job <id> --probe`: it samples child-process CPU/elapsed state twice, checks established TCP sockets twice, and compares captured-log size over 60 seconds. Only cancel after all three probes are flat. Report the job ID and last phase, then cancel and relaunch (fresh or `--resume-session`) or continue waiting; never silently drop the job or replace it with an orchestrator-authored answer. Escalate after 30 minutes without progress.

## Development and release

```sh
GOCACHE=/private/tmp/delegate-gocache go test -race ./...
GOCACHE=/private/tmp/delegate-gocache go vet ./...
bash -n install-skill.sh scripts/*.sh
scripts/release-check.sh v0.1.0
```

The release check requires the requested tag to point exactly at `HEAD`, requires `VERSION` to match `v<version>`, JSON-decodes installer and CLI output, verifies every staged binary/skill hash, and confirms the staged binary reports the release version.

## Roadmap

### v0.1.1

- `delegate review` and `delegate adversarial-review`, including sanitized review-artifact assembly and their Claude/Codex skill pairs: `codex:review`, `codex:adversarial-review`, `claude:review`, and `claude:adversarial-review`.

### v0.2

- `stop-review-gate`: a small Claude Code Stop-hook client of agentbus that gates a turn through an `ALLOW`/`BLOCK` shape contract and `policy.validate`, replacing the vendor plugin's gate without making it a delegate runtime no-op.
