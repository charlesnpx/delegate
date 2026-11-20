# delegate

Delegate submits identified asynchronous tasks and reviews to Agentbus 0.18.0. It returns a JSON admission receipt; Agentbus owns execution, status, results, steering, cancellation, and conversation disposal. Delegate 0.15.0 and Agentbus 0.18.0 use protocol 4 and are a coordinated breaking release.

## Installation

Install Agentbus and then Delegate:

```sh
mise-en-place install agentbus
mise-en-place install delegate
```

The repository installer can install the CLI and managed skills for Codex and Claude Code:

```sh
./install-skill.sh --plan --target all --json
./install-skill.sh --install --target all --json
```

Go must be on `PATH` to install the CLI. The installer reports required executables and does not configure provider permissions.

## Submit tasks

```sh
printf '%s' 'Investigate the failing check and report the evidence.' |
  delegate task --backend codex --cwd "$PWD" --prompt-file - \
    --intent inspect --tag 'ticket=ABC-123'
```

`--backend` and `--prompt-file <path|->` are required; `-` reads stdin. Omitted `--cwd` uses the current directory; an explicitly supplied path must be absolute. `--intent` accepts `inspect` or `edit` and defaults to `inspect`. Inspect instructs the worker not to edit project source or configuration; normal build and test artifacts, caches, temporary files, and local services are allowed. Both intents run with host access. Inspect is an instruction, not an enforced filesystem, network, credential, or tool-permission boundary. The caller is responsible for separate mutable workspaces when concurrent edits could conflict.

Tasks are steerable by default; `--no-steer` makes a task fixed. Optional `--model` and `--effort` are requests, not proof of actual provider routing. `--schema-file` supplies an output JSON Schema, `--tag key=value` may be repeated, and `--request-id` specifies the caller's replay identity. Otherwise Delegate generates one once per invocation. For safe replay after an ambiguous error, reuse the same request ID, logical workspace, and immutable task specification. Delegate does not automatically retry submissions.

Omitting `--timeout` uses the Agentbus two-hour execution default for either intent. `--timeout 0` or `--timeout 0s` explicitly disables the execution deadline. Positive values require units, must be whole milliseconds, and cannot exceed four hours. The execution deadline starts when a slot is granted; queue time does not consume it. Initialization and any schema correction share the same deadline.

For a finite deadline, Agentbus attempts one best-effort notice at 90% of the execution budget during an eligible initial turn, including for fixed-input tasks and reviews. If initialization passes that point, it attempts the notice as soon as the turn can accept it and time remains. The notice reports the remaining time and asks the worker to finish; it adds no task or review evidence and does not change the deadline. Unlimited jobs and correction or retired turns receive no notice.

`--retain-session` requests retention for continuation. `--resume <job-id>` creates a new job in an eligible conversation with a fresh deadline and result. A completed target must have requested retention; a failed, canceled, or unknown target can be eligible without it once the whole conversation is inactive and cleanup is proven. Every continuation needs a recorded provider identity and an available conversation. If further continuation after success is needed, request retention on the new task too. Use `agentbus session discard <job-id>` to remove an inactive owned Codex conversation and permanently revoke resume eligibility across the whole conversation, including retained descendants; this preserves its jobs and results.

## Admission receipts

Each successful submission writes one JSON receipt to stdout, with diagnostics on stderr. The connection notice identifies the Agentbus client library release and the connected daemon's release, protocol, and executable. A local development replacement reports an unknown client release; a compatible release difference is informational. For example:

```json
{
  "requestId": "automation/retry-1",
  "workspaceKey": "delegate-v1-<sha256>",
  "jobId": "job_receipt",
  "state": "queued",
  "backend": "codex",
  "intent": "inspect",
  "deduplicated": false,
  "inputMode": "steerable",
  "modelRequested": "provider-model-id",
  "effortRequested": "high",
  "timeout": {"effectiveMs": 7200000, "source": "daemon_default"}
}
```

`modelRequested` and `effortRequested` appear when supplied; status distinguishes requested, selected, and reported values. A `queue` object appears when Agentbus returns queue information. Review receipts also contain a `review` evidence stamp. The receipt identifies submitted inputs and does not certify that the provider read them or produced a compliant report. Save the receipt alongside the result.

## Observe and control jobs

Use the receipt's `jobId` with positional Agentbus commands:

```sh
agentbus status "$JOB_ID" --json
agentbus status --tag 'run=example' --json
agentbus transcript "$JOB_ID" --json
agentbus result "$JOB_ID" --wait --wait-timeout 2h > result.txt
agentbus result "$JOB_ID" --json
agentbus cancel "$JOB_ID" --json
printf '%s' "$FOLLOWUP" | agentbus steer "$JOB_ID" --request-id "$MESSAGE_ID" --message-file -
```

A successful status or transcript read exits zero regardless of job state. A result command reports the job outcome: completed 0, queued/running 2, schema noncompliant 3, failed 4, timeout 5, interrupted 6, canceled 7, unknown job 10, unknown outcome 14, unavailable/corrupt required result 15, and observation timeout 16. `result --wait` observes for 30 minutes by default; `--wait-timeout` changes only that observation limit and never cancels or extends the execution. Plain result output is the authoritative result bytes. Transcript previews can be truncated or incomplete and are not a full-report API.

Steering receipts distinguish `admitted`, `delivered`, `rejected`, and `unknown`; admission does not prove delivery. A fixed task, including every review, rejects caller steering with `not_steerable` in every job state. Steerable jobs accept caller input only during an eligible initial turn. The message ID `agentbus-deadline-warning` is reserved for the daemon. New review instructions or evidence require a new review; do not retry with a fresh message ID to bypass a refusal.

Set `AGENTBUS_NO_AUTOSTART=1` for either CLI when testing a stopped daemon or inspecting a scratch root without starting a daemon. Empty or `0` enables ordinary autostart; other values are invalid. `AGENTBUS_STATE_ROOT` selects the exact root, while the default is `$XDG_STATE_HOME/agentbus/v2` or `~/.local/state/agentbus/v2`. A scratch root alone does not disable autostart. Delegate selects the `agentbus` executable from `PATH` once per invocation; an already-running daemon retains its own environment and provider configuration. A `daemon_draining` response is not a reason to autostart or replay a submission.

Use `agentbus backends --json` to inspect the running daemon and its configured provider executables, versions, and defaults.

## Review commands

`delegate review` submits a sanitized review; `delegate adversarial-review` uses refute-first framing. Both submit explicit inspect intent and fixed input. Direct mode accepts `--backend`, `--cwd`, `--base`, `--scope auto|working-tree|branch`, `--allow-live-repo-read`, `--criteria-file`, `--model`, `--effort`, `--timeout`, and `--resume`.

Direct review captures a coherent Git view and submits a sanitized packet with diffs, selected source or excerpts, a manifest, and optional pre-fetched criteria. The receipt's `review.inputDigest` hashes the exact packet submitted, whether inline or as an artifact; it records scope and captured comparison metadata. Working-tree reviews support unborn repositories; branch reviews refuse changed paths whose working-tree content conflicts with the captured branch view. Secret-matched content is redacted from the packet, but host access means a backend can still read files directly; `--allow-live-repo-read` makes supplemental live repository reads easier and those reads are outside the captured-input guarantee.

Contract mode accepts `--request-file`, `--artifact-file`, and `--charter-file` together. The charter must be a plain `review-charter-v2` document, not a frozen envelope. A v1 request produces a schema-bound `review-report-v1`; a v2 request also requires `--reviewer` from the frozen recipe and produces a bound `review-report-v2`. Contract inputs are submitted as supplied and the caller must screen them. Contract receipts record the validated artifact, request, charter, recipe, reviewer, and subject bindings as applicable. A receipt does not replace report validation.

The repository skill is [skills/delegate/SKILL.md](skills/delegate/SKILL.md).
