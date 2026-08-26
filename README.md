# delegate

`delegate` is the first client of [agentbus](https://github.com/charlesnpx/agentbus): a delegation CLI and one managed skill for handing work between Claude Code and Codex. Version 0.8.1 ships `task`, sanitized review, and adversarial-review workflows.

agentbus owns execution, supervision, policy enforcement, and job control. delegate owns the delegation CLI surface and managed skill.

## Install

delegate is an experimental mise-en-place entry. Install **agentbus first**: mise-en-place does not infer this dependency, and delegate cannot launch work until agentbus is installed and set up.

```sh
mise-en-place install agentbus
mise-en-place install delegate

agentbus setup --json
```

The delegated installers build from source, so Go must be on `PATH`. `delegate` installs its binary in `~/.local/bin` and its managed skills for the selected target. The installer reports the prerequisite explicitly in its JSON plan:

```sh
./install-skill.sh --plan --target all --json
./install-skill.sh --install --target all --json --install-root /tmp/delegate-stage
./install-skill.sh --uninstall --target all --json --install-root /tmp/delegate-stage
```

`agentbus` must be installed before using delegate skills. `agentbus setup --json` is a one-time, installation-time check that every configured backend is usable, not a per-launch prerequisite. It covers all configured backends together and fails if any of them fails, so a failure does not by itself mean the selected backend is unusable. `delegate task` enforces the selected backend and required Agentbus capabilities at submission time, and a launch failure reports its own connection or backend error directly.

On a live Codex install, delegate minimally updates `${CODEX_HOME:-~/.codex}/config.toml` so the default `workspace-write` sandbox can write the resolved Agentbus state root, the narrow Agentbus cache subtree used for autostart locks, and the Delegate state root. It adds only missing values under `[sandbox_workspace_write].writable_roots`, preserving unrelated TOML text and comments. The Agentbus state root is `AGENTBUS_STATE_ROOT` when set, otherwise `${XDG_STATE_HOME:-~/.local/state}/agentbus`; the cache root is `<UserCacheDir>/agentbus`, not the whole user cache; the Delegate state root is `${XDG_STATE_HOME:-~/.local/state}/delegate`. `--plan` reports the intended change without writing it; staged `--install-root` invocations do not touch the live Codex config. Uninstall intentionally leaves these security settings in place rather than trying to remove possibly user-managed entries.

## CLI

```text
delegate version [--json]

delegate task --backend <name> [--cwd <abs>] [--write] [--model <model>] [--effort <effort>]
              [--timeout <duration>] --prompt-file <path|-> [--schema-file <path>]
              [--request-id <id>] [--tag <key=value>]...

delegate review|adversarial-review --backend claude|codex|cursor [--cwd <abs>]
              [--base <ref>] [--scope auto|working-tree|branch] [--allow-live-repo-read]
              [--model <model>] [--effort <effort>] [--timeout <duration>]
```

For `task`, `review`, and `adversarial-review`, `--timeout 0` leaves the deadline to the daemon default. Their submit receipts carry Agentbus's authoritative millisecond `timeout` resolution.

`task` accepts only `--prompt-file` for prompt input. Use `--prompt-file -` to read stdin, or give a file path. There is no positional prompt, prompt flag, handoff file, or stdin flag alias.

For `delegate task`, `--schema-file <path>` reads a JSON Schema output contract from a file. A supplied schema receives one Agentbus corrective retry; any violation is reported by Agentbus as `<json-pointer>: <message>`. Repeat `--tag key=value` to populate `TaskSpec.Tags`; malformed tags are usage errors.

Delegate connects through `agentbus/client`, requires `admission.strictContainment` plus the policy capabilities used by the selected contract, forwards model and effort values without local catalog resolution, and always returns after submission with a JSON receipt. Model and effort are per-invocation flags; when omitted, the backend supplies its default. Delegate persists no task submission intent, request cache, or job metadata.

## Task workflow

The managed skill pipes the prompt directly to task stdin instead of exposing it in argv:

```sh
printf '%s' 'Investigate the issue and report evidence.' |
  delegate task --backend codex --cwd "$PWD" --prompt-file - \
    --tag 'skill=delegate'
```

`task` writes no local submission state. Automation that needs replay safety supplies `--request-id <id>` and retries the same command with the same id after an ambiguous transport result. A manual invocation without that flag receives a generated ID in its receipt.

## Worker sandbox boundaries

The following are Agentbus v0.10.0 worker-sandbox contract rules. These rules are unchanged since v0.9.1; the v0.10.0 label marks the current contract version rather than a sandbox-policy change. Route work accordingly:

- A `--write` task runs as `workspace-write` with only its job `--cwd` writable. Keep task outputs there; a write worker must point `GOCACHE` under that cwd because Go writes to it. Leave the default `GOMODCACHE` alone: it is read-only from the worker's perspective and already populated, while relocating it breaks builds because the worker has no network to refill it.
- Write workers have no outbound network (`networkAccess: false`). Run `go get`, proxy-dependent `go mod tidy`, toolchain downloads, and other network-bound work in the orchestrator.
- A task without `--write` and every review worker run read-only with no network. A read-only worker cannot create a temporary or build directory, so the caller must run compile, test, and other runtime gates for review verification. Submit receipts intentionally have no backend-profile wrapper; route runtime gates from the submitted `--write` choice.
- Approval policy is `never`, so approval requests are auto-declined. A denied worker write cannot be escalated; change the routing or stop the worker.
- Writes in `.git` are commonly denied, including index locks. The orchestrator owns commits and other Git metadata changes.

Observed behavior, not an Agentbus guarantee: the OS temporary directory is writable, so ordinary Go builds and tests work with only `GOCACHE` relocated under the job cwd; the default `GOMODCACHE` remains a readable populated cache. A worker may nevertheless be denied deletion of a temp file; do not treat its cleanup as required or fail a task merely because temp artifacts remain.

## Review workflow and security model

The review context pipeline is **accident prevention**. It reduces the chance that ordinary repository secrets are copied into delegated review context; it is not a security boundary against a repository or history crafted to evade the heuristics. Adversarial repository-history shuffles, including delete-and-recreate sequences intended to break path or content ancestry, are explicitly out of scope.

`review` and `adversarial-review` are always read-only. Delegate canonicalizes `--cwd`, discovers the repository from that directory, and assembles the change context before starting the backend. `--scope auto` combines the current branch diff with the working-tree overlay, including untracked files. `--scope working-tree` compares tracked changes to `HEAD` and includes untracked files. `--scope branch` compares the merge base of `HEAD` and the resolved base. Base selection follows `--base`, the locally recorded default remote branch (such as `origin/HEAD`), then an upstream behind `HEAD` for an unpushed-commits comparison. An upstream equal to `HEAD` is never used as the branch base. If no usable base exists, delegate stops with setup guidance and never contacts a remote.

Before collecting content, delegate applies a case-insensitive secret-path heuristic to tracked, untracked, rename-source, and rename-destination paths. It covers `.env*`/`*.env`, `.netrc`, `.npmrc`, credential/password/token names, kubeconfig and service-account files, common key stores and private-key names, and `.aws`, `.ssh`, and `.gnupg` path segments. Matching paths are represented only by path and status; delegate never includes their content in the prompt or patch it assembles, including binary patches.

After every diff has been assembled, a final gitleaks-style content gate scans the shared payload used for both inline prompts and spilled artifacts. It detects AWS access keys, high-entropy API-key/token/secret assignments, private-key headers, JWTs, GitHub and Slack tokens, password-bearing connection strings, and high-entropy base64 or hex assignment values longer than 32 characters. A matching hunk is replaced with `[redacted: secret-like content]` while its path and status remain visible. These path, history, and content heuristics implement the accident-prevention model; they do not expand it into an adversarial security boundary.

Sanitized context for at most 10 files and 256 KiB is included in the prompt. Larger changesets are written to a private `0600` `review.patch` in a per-review `0700` workspace under the Delegate state root that holds sanitized repository content. By default that workspace—not the live repository—is the backend cwd. This only limits the context delegate assembles: a same-user backend can still read repository or other filesystem files itself when its process permissions allow it. Delegate records the workspace with its Agentbus job ID and state root; a later review invocation removes it only after a one-shot Agentbus status read confirms the job is terminal. The most recent review's workspace persists until then.

`--allow-live-repo-read` is an explicit escape hatch. It makes the live repository the backend cwd and permits self-collection, making backend reads of `.env` and other sensitive files easier. Delegate still applies its path/history redaction and final content scan to the context it assembles; the flag does not add or remove OS filesystem permissions. Delegate emits a warning whenever this flag is used. The managed skill does not add it unless the user explicitly requests it.

## Managed skills

The installer writes one hand-authored skill named `delegate` to each host: `~/.claude/skills/delegate/SKILL.md` for Claude Code and `${CODEX_HOME:-~/.codex}/skills/delegate/SKILL.md` for Codex. It selects a runtime backend with `--backend <name>` instead of encoding backend names in skill identities.

On install or upgrade, the installer removes retired `delegate:*` skills and the pre-existing cross-namespace legacy names. `--plan --json`, `--install --json`, and `--uninstall --json` report their paths in each target's additive `removed` array; `files` contains only the installed `delegate` skill.

## Contract tiers

Tasks submit a nil policy unless the caller supplies a JSON Schema with `--schema-file`. In that case Delegate passes the source bytes as `Contract.JSONSchema` and requests one Agentbus corrective retry. There is no default Markdown report contract. Review commands submit a nil policy.

## State Roots And Job Control

Delegate resolves Agentbus state with the same rule for task submission, review cleanup checks, and Codex sandbox configuration: `AGENTBUS_STATE_ROOT` when set, otherwise `${XDG_STATE_HOME:-~/.local/state}/agentbus`. Task uses that root to connect but stores no local submission intent or acknowledgement metadata.

Use `agentbus status --job <id> --json`, `agentbus result --job <id> --json`, and `agentbus cancel --job <id> --json` directly. A status or result exit code of `2` means the job is still running, so callers poll with a plain shell loop. Delegate's review cleanup uses only the recorded root for the submitted review job and never performs Agentbus admission recovery, reset, seal, or fail-stop clearing automatically.

## Submit receipts

`delegate task`, `delegate review`, and `delegate adversarial-review` return the same submit receipt. `requestId` is caller-owned when supplied and generated otherwise; `deduplicated` is Agentbus's direct admission value. On a failed submission, the error includes the request ID so the caller can retry with it.

```json
{
  "requestId": "delegate-0123456789abcdef0123456789abcdef",
  "jobId": "opaque-agentbus-id",
  "state": "queued",
  "deduplicated": false,
  "model": "backend-model",
  "effort": "backend-effort",
  "timeout": {
    "effectiveMs": 1800000,
    "source": "daemon_default"
  }
}
```

`model` and `effort` are omitted when their flags were not supplied.

## Setup And Monitoring

`agentbus setup --json` is a one-time, installation-time check that every configured backend is usable, not a per-launch prerequisite. It covers all configured backends together and fails if any of them fails, so a failure does not by itself mean the selected backend is unusable. The per-launch gate is `delegate task`: it enforces the selected backend and required Agentbus capabilities at submission time, and a launch failure reports its own connection or backend error directly.

All submissions are asynchronous. Delegate has no wait, poll, status, result, or cancel command. For an outstanding job, use Agentbus directly; a status or result exit code of `2` means the job is still running, so callers poll with a plain shell loop. Do not scan the Agentbus state root for results: that layout is a private implementation detail.

Review workspaces are private directories under the Delegate state root that hold sanitized repository content. Delegate records only the job ID, workspace path, and Agentbus state root. On the next `review` or `adversarial-review` invocation, it makes one Agentbus status read for each retained workspace and removes the workspace and its record only after a terminal state. The most recent review's workspace persists until a later review invocation makes that terminal-status check; a running, unreadable, or unreachable recorded state root leaves the workspace for a later invocation. Delegate does not wait or poll.

## Development and release

```sh
GOCACHE=/private/tmp/delegate-gocache go test -race ./...
GOCACHE=/private/tmp/delegate-gocache go vet ./...
bash -n install-skill.sh scripts/*.sh
scripts/release-check.sh v0.8.1
```

The release check requires a clean worktree, including no modified tracked files and no untracked files outside ignored paths. Manually inspect the same gate with `git status --short --untracked-files=all`. It also requires the requested tag to point exactly at `HEAD`, requires `VERSION` to match `v<version>`, JSON-decodes installer and CLI output, verifies every staged binary/skill hash, and confirms the staged binary reports the release version. `--allow-dirty` is an unsafe escape hatch and always prints a loud warning when used.
