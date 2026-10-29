# delegate

`delegate` is a thin, identified submitter for Agentbus. `delegate task` translates one convenient task invocation into one identified Agentbus `job.submit` call and prints the submit receipt; everything after task submission belongs to Agentbus.

## Depends on / Depended on by

**Depends on:**

- `github.com/charlesnpx/agentbus` — imports `client/`, pinned `v0.15.0`; the installer invokes `agentbus configure-codex-sandbox`.
- `github.com/charlesnpx/witness` — imports `contract/review`, the shared review contract; the pinned version is in `go.mod`.

**Depended on by:** `feature-implement`, which invokes the `delegate` CLI
(`task`, `review`, `adversarial-review`) and reads its submit receipts and the
Agentbus job records they point to. The command surface, flag names, receipt
fields, and exit codes are therefore a contract; removing or renaming any of
them is a breaking change for Feature Implement. The `configure-codex-sandbox`
removal in v0.11.0 is the recent example of exactly that kind of change.

## Install

Install Agentbus before Delegate: Delegate submits work through Agentbus and cannot submit a task until Agentbus is available.

```sh
mise-en-place install agentbus
mise-en-place install delegate
```

The repository installer can install the CLI plus its managed skill targets:

```sh
./install-skill.sh --plan --target all --json
./install-skill.sh --install --target all --json
```

`--target all` covers the command-line tool plus the Claude Code and Codex skill destinations. Go must be on `PATH` when installing the tool. The installer reports its work as JSON and warns if it cannot find Agentbus; install Agentbus first before using the skill or submitting a task.

## Commands

Public help lists four commands:

```text
delegate version [--json]

delegate task --backend <name> [--cwd <abs>] [--write] [--model <model>] [--effort <effort>]
              [--timeout <duration>] --prompt-file <path|-> [--schema-file <path>]
              [--request-id <id>] [--resume <jobId>] [--retain-session] [--no-steer]
              [--tag <key=value>]...

delegate review --backend <name> [--cwd <abs>] [--base <ref>]
                [--scope auto|working-tree|branch] [--allow-live-repo-read]
                [--criteria-file <path>]
                [--model <model>] [--effort <effort>] [--timeout <duration>] [--resume <jobId>]

delegate adversarial-review --backend <name> [--cwd <abs>] [--base <ref>]
                            [--scope auto|working-tree|branch] [--allow-live-repo-read]
                            [--criteria-file <path>]
                            [--model <model>] [--effort <effort>] [--timeout <duration>]
```

`version` prints the installed Delegate version; `--version`, `-version`, and `-V` are equivalent aliases. `--json` makes `version` emit an object with its `version` field.

`task` needs `--backend` and `--prompt-file`. Its thirteen flags are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, `--resume`, `--retain-session`, `--no-steer`, and `--tag`. `delegate task` submits steerable work by default; `--no-steer` submits a fixed-input task. This choice is independent of `--write` and `--resume`. `--cwd` must be absolute when supplied; otherwise the current directory is used. `--prompt-file -` reads the prompt from standard input, `--schema-file` supplies an optional JSON Schema output contract, and `--tag key=value` can be repeated. Timeout values require explicit units: use `--timeout 90m`, `--timeout 2400s`, or `--timeout 0s` to leave the deadline to Agentbus's default. A bare number such as `2400` is rejected with unit suggestions.

`task` and `review` accept `--resume <jobId>` to continue a prior backend thread. A resume creates a new job with a fresh deadline; it does not extend the named job. Agentbus validates the resume target, and changing `--resume` while reusing an explicit `--request-id` returns Agentbus's conflict response.

`--retain-session` is accepted only by `task`. It keeps the backend session after completion so the job can be resumed; the session directory is left in place until an operator removes it. Without `--retain-session`, a completed job cannot be resumed: its session is cleaned up when the job completes. `--resume` creates a new job with a fresh deadline and its own result; the original job's result is unchanged and remains final.

Continue the same backend conversation in two steps:

```sh
# submit, keeping the session for a follow-up
printf '%s' "$PROMPT" | delegate task --backend codex --retain-session --cwd <abs> --prompt-file -

# read the result, then continue the same conversation
printf '%s' "$FOLLOWUP" | delegate task --backend codex --resume <jobId> --cwd <abs> --prompt-file -
```

The retained session directory stays on disk until an operator removes it.

### Follow-up instructions while a job is running

`delegate task` jobs are steerable by default. Send a message to a queued or running steerable job through Agentbus:

```sh
printf '%s' "$FOLLOWUP" | agentbus steer --job "$JOB_ID" \
  --request-id "$MESSAGE_ID" --message-file -
```

The receipt status is `admitted`, `delivered`, `rejected`, or `unknown`. `admitted` means Agentbus accepted the message; `delivered` confirms provider delivery. Exit code 17 means `rejected`, and 18 means `unknown`. A successful write to Agentbus is not itself proof that the provider received the message.

`--no-steer` submits a fixed job. Reviews are always fixed; new review instructions or evidence require a new review. If a review is submitted through generic `delegate task`, pass `--no-steer`. A fixed job cannot receive steering; `agentbus cancel` stops it but does not deliver instructions or schedule a follow-up.

Delegate sends the `inputMode` field to Agentbus and does not silently remove it or downgrade a steerable task. The running Agentbus daemon must support this field; upgrade it if Delegate reports that it does not support steerable tasks. `--no-steer` selects fixed input mode on a daemon that supports the field; it is not an old-daemon compatibility option.

For a terminal job, a follow-up in the same conversation uses `delegate task --backend <same-backend> --resume <jobId>` with the follow-up prompt. Agentbus requires a retained session and a recorded backend session ID. Add `--retain-session` to each resumed task if further follow-ups will be needed. Completed jobs without retention cannot be resumed; submit a fresh task with the relevant context instead. Failed, canceled, and unknown terminal jobs retain their session directories, but still need a recorded backend session ID to resume. Cancellation alone does not guarantee resumability, and resumption always creates a new job.

For example, submit a prompt without placing it in the process arguments:

```sh
printf '%s' 'Investigate the failing check and report the evidence.' |
  delegate task --backend codex --cwd "$PWD" --prompt-file - \
    --tag 'ticket=ABC-123'
```

The command returns as soon as Agentbus accepts the work. It does not wait for, poll, or collect the result.

## Submit receipt and request identity

Each successful `task`, `review`, or `adversarial-review` submission writes one JSON receipt to standard output. This is the receipt shape emitted by a task with an explicit request ID, model, effort, and Agentbus default timeout:

```json
{
  "requestId": "automation/retry-1",
  "workspaceKey": "delegate-v1-<sha256>",
  "jobId": "job_receipt",
  "state": "queued",
  "deduplicated": false,
  "inputMode": "steerable",
  "model": "unadvertised-model",
  "effort": "unadvertised-effort",
  "timeout": {
    "effectiveMs": 1800000,
    "source": "daemon_default"
  }
}
```

`model` and `effort` are omitted when their flags were not supplied. The timeout values are Agentbus's returned values, not a local interpretation. `workspaceKey` lets an operator scope Agentbus status queries to the workspace that submitted the job with `agentbus status --workspace-key <key>`.

For replay safety, Agentbus's replay key is the pair of request ID and working directory: after an ambiguous submission, reuse that exact `--request-id` and run against the same working directory. If it has already been accepted, the replay receipt has `deduplicated: true` and carries the original job ID. Agentbus also hashes the immutable task specification, so enabling `--retain-session` changes the replay identity and is a conflict rather than a replay; leaving it absent preserves the existing hash. Without the flag, Delegate generates an ID and includes it in the receipt. That is convenient for a normal one-off invocation, but it has a deliberate trade-off: if a manually run command is hard-killed before its generated receipt is visible, Delegate has not retained that generated ID for a later replay. Use an explicit request ID whenever replay matters.

## Observe jobs with Agentbus

After the receipt, use Agentbus directly:

```sh
agentbus status --job "$JOB_ID" --json
agentbus status --tag 'run=example' --json
agentbus transcript --job "$JOB_ID" --json
agentbus result --job "$JOB_ID" --json
agentbus cancel --job "$JOB_ID" --json
```

Tag related `delegate task` submissions with `--tag run=<label>`, then use `agentbus status --tag run=<label>` to watch the group. Use `agentbus transcript --job "$JOB_ID"` to see job activity: without `--kind` it returns a digest with counts by kind and a short tail, and `--json` adds the first and last item timestamps; `--kind message` returns only the agent's messages.

For `status --job` and `result`, exit code `2` means the job is still running. A tag or workspace listing exits `0` once it has printed the list, whatever state its members are in, so do not read a successful listing as a finished fleet. The receipt's `jobId` is the value to pass to each command.

## Worker sandbox rules

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. For Go builds, set `GOCACHE` under `/tmp` and leave it out of the reviewed workspace; leave `GOMODCACHE` at its default.

## Review commands

`review` delegates a sanitized code review and `adversarial-review` uses refute-first framing. Direct reviews accept `--base`, `--scope`, `--allow-live-repo-read`, `--criteria-file`, model/effort/timeout, and `--resume`. They do not accept task flags such as `--write`, `--prompt-file`, `--schema-file`, `--request-id`, `--retain-session`, or `--tag`. A review `--timeout` also requires an explicit unit.

Direct mode includes the diff, full post-change text for files up to 128 KiB each within a 512 KiB combined source-and-criteria budget, and a manifest with each supplied path's original byte size, SHA-256, and `full`, `partial`, or `omitted` status. Oversized files receive a partial excerpt around changed hunks (up to 32 KiB per file); binary files, secret paths, and deleted paths are listed without source content and with an omission reason. Existing secret scanning also redacts secret-like content. These limits cover added source context; the diff remains included.

Use `--criteria-file <path>` to supply acceptance criteria that were fetched beforehand. Delegate reads up to 256 KiB of UTF-8 text locally and includes its bytes and hash in the packet; it does not fetch criteria from a network service. This flag is for direct mode only. `--allow-live-repo-read` affects direct mode's backend working directory and does not add live repository bytes to exact-input contract reviews.

Working-tree review works before the first commit and uses Git's empty-tree object as its base; it does not create a commit. `--cwd` may name a repository subdirectory; Delegate discovers and reviews its parent repository. Branch scope supplies the committed branch snapshot and refuses changed paths that also differ in the working tree, so the source text and diff describe the same revision.

Contract mode uses `--request-file`, `--artifact-file`, and `--charter-file` together. The charter file must be a plain `charter.json` input document (`review-charter-v2`); a frozen `charter.freeze.json` envelope is rejected with a shape-specific error. Delegate freezes the plain input for the review contract. A v1 request produces a schema-bound `review-report-v1`; a v2 request additionally takes `--reviewer <identifier>`, selects that identifier from the frozen recipe's `required_outputs`, and produces a recipe-bound `review-report-v2` with request, recipe, and reviewer constants in its output schema. Contract mode does not accept `--criteria-file`.

Contract mode submits the caller's request, artifact, and plain charter inputs through its existing contract path without direct-mode secret-path, history, or content redaction; screening remains the caller's responsibility. The contract prompts and digests bind those supplied inputs. Resubmitting the same request file from the same canonical `--cwd` replays the same job (`deduplicated: true`) instead of paying for a second review.

## Managed skill

The repository has one managed skill: [`skills/delegate/SKILL.md`](skills/delegate/SKILL.md). It sends a task prompt through standard input and tells the worker how to use the receipt and Agentbus observation commands. Install it with `./install-skill.sh --install --target all --json`, or use `--target claude` or `--target codex` when only one host needs the skill.
