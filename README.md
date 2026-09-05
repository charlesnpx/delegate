# delegate

`delegate` is a thin, identified submitter for Agentbus. `delegate task` translates one convenient task invocation into one identified Agentbus `job.submit` call and prints the submit receipt; everything after task submission belongs to Agentbus.

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
              [--request-id <id>] [--resume <jobId>] [--tag <key=value>]...

delegate review --backend <name> [--cwd <abs>] [--base <ref>]
                [--scope auto|working-tree|branch] [--allow-live-repo-read]
                [--model <model>] [--effort <effort>] [--timeout <duration>] [--resume <jobId>]

delegate adversarial-review --backend <name> [--cwd <abs>] [--base <ref>]
                            [--scope auto|working-tree|branch] [--allow-live-repo-read]
                            [--model <model>] [--effort <effort>] [--timeout <duration>]
```

`version` prints the installed Delegate version; `--version`, `-version`, and `-V` are equivalent aliases. `--json` makes `version` emit an object with its `version` field.

`task` needs `--backend` and `--prompt-file`. Its eleven flags are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, `--resume`, and `--tag`. `--cwd` must be absolute when supplied; otherwise the current directory is used. `--prompt-file -` reads the prompt from standard input, `--schema-file` supplies an optional JSON Schema output contract, and `--tag key=value` can be repeated. `--timeout 0` leaves the deadline to Agentbus's default.

`task` and `review` accept `--resume <jobId>` to continue a prior backend thread. A resume creates a new job with a fresh deadline; it does not extend the named job. Agentbus validates the resume target, and changing `--resume` while reusing an explicit `--request-id` returns Agentbus's conflict response.

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
  "model": "unadvertised-model",
  "effort": "unadvertised-effort",
  "timeout": {
    "effectiveMs": 1800000,
    "source": "daemon_default"
  }
}
```

`model` and `effort` are omitted when their flags were not supplied. The timeout values are Agentbus's returned values, not a local interpretation. `workspaceKey` lets an operator scope Agentbus status queries to the workspace that submitted the job with `agentbus status --workspace-key <key>`.

For replay safety, Agentbus's replay key is the pair of request ID and working directory: after an ambiguous submission, reuse that exact `--request-id` and run against the same working directory. If it has already been accepted, the replay receipt has `deduplicated: true` and carries the original job ID. Without the flag, Delegate generates an ID and includes it in the receipt. That is convenient for a normal one-off invocation, but it has a deliberate trade-off: if a manually run command is hard-killed before its generated receipt is visible, Delegate has not retained that generated ID for a later replay. Use an explicit request ID whenever replay matters.

## Observe jobs with Agentbus

After the receipt, use Agentbus directly:

```sh
agentbus status --job "$JOB_ID" --json
agentbus status --tag 'run=example' --json
agentbus transcript --job "$JOB_ID" --json
agentbus result --job "$JOB_ID" --json
agentbus cancel --job "$JOB_ID" --json
```

Tag related `delegate task` submissions with `--tag run=<label>`, then use `agentbus status --tag run=<label>` to watch the group. Use `agentbus transcript --job "$JOB_ID"` to see job activity: without `--kind` it returns a digest with counts by kind, first and last timestamps, and a short tail; `--kind message` returns only the agent's messages.

For `status` and `result`, exit code `2` means the job is still running. The receipt's `jobId` is the value to pass to each command.

## Worker sandbox rules

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. For Go builds, set `GOCACHE` inside `--cwd` and leave `GOMODCACHE` at its default.

## Review commands

Today `review` delegates a sanitized code review and `adversarial-review` delegates a refute-first review; both also support contract mode with caller-frozen `--request-file`, `--artifact-file`, and `--charter-file` inputs against the shared review contract and return the submit-receipt shape.

Contract mode submits the caller's frozen bytes verbatim: Delegate does not run its secret-path, history, or content redaction on them, so screening is the caller's responsibility. Resubmitting the same request file from the same canonical `--cwd` replays the same job (`deduplicated: true`) instead of paying for a second review.

## Managed skill

The repository has one managed skill: [`skills/delegate/SKILL.md`](skills/delegate/SKILL.md). It sends a task prompt through standard input and tells the worker how to use the receipt and Agentbus observation commands. Install it with `./install-skill.sh --install --target all --json`, or use `--target claude` or `--target codex` when only one host needs the skill.
