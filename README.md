# delegate

`delegate` is the first client of [agentbus](https://github.com/charlesnpx/agentbus): a delegation CLI and managed skill matrix for handing work between Claude Code and Codex. Version 0.7.3 ships `task`, rescue, sanitized review, adversarial-review, job-control workflows, and parent-session audit linkage.

agentbus owns execution, supervision, and generic policy enforcement. delegate owns the delegation-specific data and decisions it passes to agentbus: the bundled `delegate-report` contract, the delegate-contract digest, policy tiers, handoff lifecycle, skill matrix, and result envelopes.

## Install

delegate is an experimental mise-en-place entry. Install **agentbus first**: mise-en-place does not infer this dependency, and delegate cannot launch work until agentbus is installed and set up.

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

On a live Codex install, delegate minimally updates `${CODEX_HOME:-~/.codex}/config.toml` so the default `workspace-write` sandbox can write the resolved Agentbus state root, the narrow Agentbus cache subtree used for autostart locks, and the Delegate state root. It adds only missing values under `[sandbox_workspace_write].writable_roots`, preserving unrelated TOML text and comments. The Agentbus state root is `AGENTBUS_STATE_ROOT` when set, otherwise `${XDG_STATE_HOME:-~/.local/state}/agentbus`; the cache root is `<UserCacheDir>/agentbus`, not the whole user cache; the Delegate state root is `${XDG_STATE_HOME:-~/.local/state}/delegate`. `--plan` reports the intended change without writing it; staged `--install-root` invocations do not touch the live Codex config. Uninstall intentionally leaves these security settings in place rather than trying to remove possibly user-managed entries.

## CLI

```text
delegate version [--json]
delegate setup [--json]
delegate install-skills [--plan|--install|--uninstall] [--target claude|codex|all] [--json] [--install-root <abs>]

delegate handoff create --json

delegate task --backend claude|codex|cursor [--background|--wait] [--json] [--cwd <abs>]
              [--model <model>] [--effort <effort>]
              [--timeout <duration>] [--write] [--strict-contract|--no-contract]
              [--output-schema-file <path>] [--origin <skill>] [--parent-client <client>] [--parent-session <id>] [prompt source]
delegate task --recover-request <request-id> [--background|--wait] [--json]

delegate review|adversarial-review --backend claude|codex|cursor [--background|--wait] [--json] [--cwd <abs>]
              [--base <ref>] [--scope auto|working-tree|branch] [--allow-live-repo-read]
              [--model <model>] [--effort <effort>] [--timeout <duration>]
              [--strict-contract] [--origin <skill>] [--parent-client <client>] [--parent-session <id>]

delegate status [--job <id>] [--wait] [--json]
delegate result --job <id> [--wait] [--json]
delegate cancel --job <id> [--json]
```

Prompt sources are mutually exclusive: `--prompt`, `--prompt-file`, `--prompt-stdin`, `--handoff-prompt-file`, or positional text. `--prompt` and positional text are visible in process arguments and shell history; use stdin, a prompt file, or a handoff file for sensitive input.

For `delegate task`, `--output-schema-file <path>` reads a JSON Schema output contract from a file. Violations return as `<json-pointer>: <message>` with one corrective retry.

Delegate connects through `agentbus/client`, requires `admission.strictContainment` plus the policy capabilities used by the selected contract, persists a durable request identity before submission, and returns a launch envelope unless `--wait` is set.

## Rescue workflow

The rescue skills use a durable stdin handoff instead of exposing the delegated prompt in argv:

```sh
HANDOFF_PATH=$(
  printf '%s' 'Investigate the issue and report evidence.' |
    delegate handoff create --json |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["handoff_path"])'
)

delegate task --backend codex --origin delegate:rescue:codex --cwd "$PWD" \
  --handoff-prompt-file "$HANDOFF_PATH" --background --json
```

`delegate handoff create --json` creates a private `0600` handoff file in delegate state. `task` persists the exact Agentbus submission parameters, copies the prompt to the job input after acknowledgement, fsyncs it, and removes the handoff. A handoff prompt file is single-use: create a new one before relaunching the same packet. The job input is removed only when Agentbus reports `cleanupDisposition` of `verified_absent` or `no_execution_possible`; it is retained when cleanup is `unresolved` or absent.

## Worker sandbox boundaries

The following are Agentbus v0.10.0 worker-sandbox contract rules. These rules are unchanged since v0.9.1; the v0.10.0 label marks the current contract version rather than a sandbox-policy change. Route work accordingly:

- A `--write` task runs as `workspaceWrite` with only its job `--cwd` writable. Keep task outputs there; a write worker can build and test only when `GOCACHE` and `GOMODCACHE` also point under that cwd, because the usual Go cache locations are outside the worker's writable roots.
- Write workers have no outbound network (`networkAccess: false`). Run `go get`, proxy-dependent `go mod tidy`, toolchain downloads, and other network-bound work in the orchestrator.
- A task without `--write` and every review worker run read-only with no network. A read-only worker cannot create a temporary or build directory, so the caller must run compile, test, and other runtime gates for review verification.
- Approval policy is `never`, so approval requests are auto-declined. A denied worker write cannot be escalated; change the routing or stop the worker.
- Writes in `.git` are commonly denied, including index locks. The orchestrator owns commits and other Git metadata changes.

Observed behavior, not an Agentbus guarantee: the OS temporary directory is writable, so ordinary Go builds and tests work with only `GOCACHE` and `GOMODCACHE` relocated under the job cwd. A worker may nevertheless be denied deletion of a temp file; do not treat its cleanup as required or fail a task merely because temp artifacts remain.

## Review workflow and security model

The review context pipeline is **accident prevention**. It reduces the chance that ordinary repository secrets are copied into delegated review context; it is not a security boundary against a repository or history crafted to evade the heuristics. Adversarial repository-history shuffles, including delete-and-recreate sequences intended to break path or content ancestry, are explicitly out of scope.

`review` and `adversarial-review` are always read-only. Delegate canonicalizes `--cwd`, discovers the repository from that directory, and assembles the change context before starting the backend. `--scope auto` combines the current branch diff with the working-tree overlay, including untracked files. `--scope working-tree` compares tracked changes to `HEAD` and includes untracked files. `--scope branch` compares the merge base of `HEAD` and the resolved base. Base selection follows `--base`, the locally recorded default remote branch (such as `origin/HEAD`), then an upstream behind `HEAD` for an unpushed-commits comparison. An upstream equal to `HEAD` is never used as the branch base. If no usable base exists, delegate stops with setup guidance and never contacts a remote.

Before collecting content, delegate applies a case-insensitive secret-path heuristic to tracked, untracked, rename-source, and rename-destination paths. It covers `.env*`/`*.env`, `.netrc`, `.npmrc`, credential/password/token names, kubeconfig and service-account files, common key stores and private-key names, and `.aws`, `.ssh`, and `.gnupg` path segments. Matching paths are represented only by path and status; delegate never includes their content in the prompt or patch it assembles, including binary patches.

After every diff has been assembled, a final gitleaks-style content gate scans the shared payload used for both inline prompts and spilled artifacts. It detects AWS access keys, high-entropy API-key/token/secret assignments, private-key headers, JWTs, GitHub and Slack tokens, password-bearing connection strings, and high-entropy base64 or hex assignment values longer than 32 characters. A matching hunk is replaced with `[redacted: secret-like content]` while its path and status remain visible. These path, history, and content heuristics implement the accident-prevention model; they do not expand it into an adversarial security boundary.

Sanitized context for at most 10 files and 256 KiB is included in the prompt. Larger changesets are written to a private `0600` `review.patch` in a per-review `0700` delegate-state workspace. By default that workspace—not the live repository—is the backend cwd. This only limits the context delegate assembles: a same-user backend can still read repository or other filesystem files itself when its process permissions allow it. The workspace and artifact remain available for a background job and are removed only when Agentbus cleanup disposition proves local cleanup is safe.

`--allow-live-repo-read` is an explicit escape hatch. It makes the live repository the backend cwd and permits self-collection, making backend reads of `.env` and other sensitive files easier. Delegate still applies its path/history redaction and final content scan to the context it assembles; the flag does not add or remove OS filesystem permissions. Delegate emits a warning whenever this flag is used. Managed review skills do not add it unless the user explicitly requests it.

## Managed skills

The source directories escape `:` as `__colon__`; the installer decodes the names when it writes skills.

| Installed for | Skill names | Purpose |
| --- | --- | --- |
| Claude Code (`~/.claude/skills`) | `delegate:rescue:claude`, `delegate:rescue:codex`, `delegate:rescue:cursor`, `delegate:review:claude`, `delegate:review:codex`, `delegate:review:cursor`, `delegate:adversarial-review:claude`, `delegate:adversarial-review:codex`, `delegate:adversarial-review:cursor`, `delegate:status`, `delegate:result`, `delegate:cancel`, `delegate:setup`, `delegate:config` | Launch any managed backend (claude, codex, cursor) and control any delegated job. |
| Codex (`${CODEX_HOME:-~/.codex}/skills`) | `delegate:rescue:claude`, `delegate:rescue:codex`, `delegate:rescue:cursor`, `delegate:review:claude`, `delegate:review:codex`, `delegate:review:cursor`, `delegate:adversarial-review:claude`, `delegate:adversarial-review:codex`, `delegate:adversarial-review:cursor`, `delegate:status`, `delegate:result`, `delegate:cancel`, `delegate:setup`, `delegate:config` | Launch any managed backend (claude, codex, cursor) and control any delegated job. |

Launch skills preflight shared filesystem and state access, no-fork execution, agentbus capabilities, and target-backend reachability. Rescue skills launch through `delegate task`; review skills launch through the sanitized `delegate review` commands. All return the launch envelope verbatim and never add `--no-contract`. Job-control skills use the same status, result, cancellation, evidence-preservation, and no-substitute-answer discipline. Review prose requires findings ordered by severity, preservation of evidence labels, and no automatic fixes after review.

v0.7.3 retains the breaking namespace rename. On install or upgrade, the managed installer removes the legacy `codex:{rescue,review,adversarial-review,status,result,cancel}` names from Claude Code and the corresponding `claude:{...}` names from Codex; `--plan --json`, `--install --json`, and `--uninstall --json` report them in each target's additive `removed` array (entries of `{"path": ...}`); the `files` array contains only installed skill files.

## Contract tiers

`delegate-report` is structural validation, not proof that the task is correct or complete.

| Invocation | Contract result | Retry behavior |
| --- | --- | --- |
| Default delegate-report task | Inject digest, append the generated output format, validate, stamp | At most one corrective retry. The retry is always read-only and instructs the backend to emit only the corrected report and make no further changes. |
| `--write` or `--strict-contract` | Same delegate-report contract behavior | `--write` controls backend write permission; `--strict-contract` is retained for compatibility. |
| `--no-contract` | Enforcement disabled | No retry; terminal envelope has `contract.status: "disabled"` and `reason: "no_contract_flag"`. This is for direct CLI use only, never managed skills. |

## State Roots And Recovery

Delegate resolves Agentbus state with the same rule for setup, launch, recovery, status, result, cancel, and Codex sandbox configuration: `AGENTBUS_STATE_ROOT` when set, otherwise `${XDG_STATE_HOME:-~/.local/state}/agentbus`. The resolved root is persisted in submission intents and acknowledged job metadata. `delegate task --recover-request <request-id>` reconnects to the request's recorded root and resubmits the exact persisted `job.submit` parameters; it does not reconstruct prompts, timeouts, policy, model, effort, or tags from current flags.

`status`, `result`, and `cancel` use the job metadata's recorded Agentbus root when available and otherwise use the current resolved root. Running `delegate status` without `--job` lists all jobs from only the current resolved root. Delegate never scans arbitrary roots and never performs Agentbus admission recovery, reset, seal, or fail-stop clearing automatically.

## Envelope Reference

Every envelope has `schema: 2`. `request_id` is Delegate's durable logical request identity; `deduplicated` is true when Agentbus accepted the same request earlier and returned the existing job. `result_sha256` is Agentbus's SHA-256 over the raw final assistant message bytes when a result exists. When captured, the additive `origin` block carries `skill`, `parent_client`, `parent_session_id`, `parent_agent`, and `depth`.

Launch envelopes are returned by a non-waiting task:

```json
{
  "schema": 2,
  "job_id": "opaque-agentbus-id",
  "request_id": "delegate-0123456789abcdef0123456789abcdef",
  "status": "queued",
  "deduplicated": false,
  "result_sha256": null
}
```

Terminal envelopes are returned by `delegate result --job <id>`, `cancel`, and `delegate task --wait`. `delegate status --json` is the exception: it always returns a `JobStatusResult` (`{"jobs": [...]}`), for both running and terminal jobs, so a poller sees one stable shape across a job's lifetime. Running admission jobs carry `startedAt`, `heartbeatAt`, and `updatedAt` liveness fields (`heartbeatAt` is the last provider-event time, an activity signal, not a process lease); those liveness fields are absent from terminal status jobs. Terminal status jobs instead may carry the complete `finalAttemptStartedAt`/`finalAttemptEndedAt` pair for the FINAL attempt. That pair is not a whole-job duration, and a retry replaces its start time.

The terminal envelope shape:

```json
{
  "schema": 2,
  "job_id": "opaque-agentbus-id",
  "request_id": "delegate-0123456789abcdef0123456789abcdef",
  "status": "completed",
  "kind": "task",
  "contractKind": "shape",
  "cleanup_disposition": "verified_absent",
  "late_finalization": false,
  "agentbus_warnings": [],
  "local_artifacts_retained": false,
  "contract": {
    "status": "compliant",
    "missing": [],
    "reason": "",
    "attempts": 1,
    "retryUsed": false,
    "contractSha256": "sha256:...",
    "validatedAt": "2026-01-01T00:00:00Z"
  },
  "result_sha256": "..."
}
```

Possible contract statuses are `compliant`, `retried`, `noncompliant`, `skipped`, and `disabled`. `kind` is `task`, `review`, or `adversarial_review`. `orphaned` is a first-class terminal state with exit code 14; it does not fabricate a result, and `result_unavailable_reason` explains the missing result.

For `failed`, `interrupted`, and `quarantined` terminals, Agentbus may also supply a redacted, length-bounded `failure_reason` and closed-set `failure_class`. They answer what went wrong and are independent of `result_unavailable_reason`, which only explains why no result is present. They are omitted when Agentbus supplies neither. `delegate status --json` exposes the same metadata within each JobStatus using Agentbus's `failureReason` and `failureClass` names; terminal envelopes use Delegate's snake_case names. Terminal envelopes likewise expose the complete FINAL-attempt pair as `final_attempt_started_at` and `final_attempt_ended_at`; callers can subtract them when needed, but Delegate does not emit a derived duration.

- `backend_not_started`: Agentbus could not start backend work, so no backend work was possible.
- `backend_error`: a launched backend turn failed; work may have happened before it failed.
- `backend_interrupted`: a backend turn stopped without Agentbus requesting the interruption.
- `finalization_error`: the backend succeeded and Agentbus lost the result; inspect the worktree and salvage completed work before retrying.
- `internal_error`: Agentbus has no more specific failure category.

## Cleanup Disposition

Agentbus reports terminal outcome and cleanup proof separately. Delegate removes job inputs and review workspaces only when `cleanup_disposition` is `verified_absent` or `no_execution_possible`. When the disposition is `unresolved` or missing on a terminal job, Delegate retains local artifacts and warns that backend absence is unproven. A successful job remains successful when cleanup is unresolved.

## Setup And Monitoring

`delegate setup --json` reports:

- Agentbus discovery (`found`, path, version, protocol, advertised backends, backend metadata);
- the full capability map, required delegate capabilities, missing capabilities, `capabilitiesOK`, and explicit `admissionStrictContainment`;
- resolved `agentbusStateRoot` plus `agentbusStateRootWritable`;
- resolved `agentbusAutostartLockRoot` (`<UserCacheDir>/agentbus/start-locks`) plus `agentbusAutostartLockRootWritable`;
- `pendingSubmissionIntentCount` for prepared, in-flight, and blocked local submission intents;
- `unresolvedCleanupArtifactCount` for retained terminal local artifacts whose cleanup is not proven safe;
- `stateRootWritable`, `daemonReachable`, `ready`, and managed skill statuses.

Missing `admission.strictContainment` makes setup not ready and returns nonzero.

Launch with `--background` so the host agent loop stays free. For an outstanding job, start exactly ONE background `delegate result --job <id> --wait --json` task: a background `--wait` blocks only its small awaiter process, not a worker slot or the model. A foreground `--wait` blocks the current host tool call, so reserve it for a short, explicitly bounded terminal check. Bound long waits with `--wait-timeout <duration>`; on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job. Do not write shell polling loops or scan the Agentbus state root for results: that storage layout is a private implementation detail, and filesystem salvage is an operator-only emergency after a confirmed CLI defect, not a supported path.

The cheap one-shot `delegate status --json --job <id>` request reports a real terminal `state` (`completed`, `completed_noncompliant`, `failed`, ...) once the job finishes — sourced from the durable authority record, not a transient in-memory map — so a watcher keyed on `engine.IsTerminal(state)` observes termination without any separate call. Use it only for on-demand progress. `delegate status --job <id> --wait` (and `delegate result --job <id> --wait --json`) returns only once the job is terminal and exits with the job's status code; it is the terminal-wait primitive. Run a long terminal wait as one background awaiter rather than tying up the host loop.

## Development and release

```sh
GOCACHE=/private/tmp/delegate-gocache go test -race ./...
GOCACHE=/private/tmp/delegate-gocache go vet ./...
bash -n install-skill.sh scripts/*.sh
scripts/release-check.sh v0.7.3
```

The release check requires a clean worktree, including no modified tracked files and no untracked files outside ignored paths. Manually inspect the same gate with `git status --short --untracked-files=all`. It also requires the requested tag to point exactly at `HEAD`, requires `VERSION` to match `v<version>`, JSON-decodes installer and CLI output, verifies every staged binary/skill hash, and confirms the staged binary reports the release version. `--allow-dirty` is an unsafe escape hatch and always prints a loud warning when used.
