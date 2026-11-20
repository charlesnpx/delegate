---
name: delegate
description: Submit asynchronous tasks and reviews through Delegate and observe or control them with Agentbus.
---

# delegate

Delegate 0.15.0 submits to Agentbus 0.18.0. Use Delegate for tasks and reviews, and Agentbus for status, result, transcript, cancel, steer, and session disposal. Save the JSON admission receipt with the result.

## Submit work

```sh
printf '%s' "$PROMPT" | delegate task --backend <name> --cwd <absolute-workspace> \
  --intent inspect --prompt-file - --request-id <stable-request-id>
```

Choose `--intent inspect` for investigation or `--intent edit` for requested changes; inspect is the default. Both use host execution. Inspect is a prompt contract, not a filesystem, network, credential, or tool boundary: ordinary builds, tests, caches, temporary files, and local services may write. After either intent, inspect repository status/diff and relevant observable side effects; lack of transcript file-change events does not prove no changes. Preserve unrelated user work. Allocate separate mutable workspaces for concurrent edits that could conflict.

Choose actual supported provider model and effort IDs deliberately for the task's complexity and duration. Do not guess a model catalogue or add an automatic router. Receipt `modelRequested` and `effortRequested` describe submitted values; status separately reports requested, starting-time selected, and provider-reported values.
Use `agentbus backends --json` to inspect the running daemon's executable and configured provider versions and defaults.
Delegate's stderr connection notice identifies the Agentbus client library and daemon releases, protocol, and daemon executable. An unknown development release is not a claimed match; a protocol-compatible release difference is informational.

Omitting `--timeout` uses the two-hour execution default. Explicit `--timeout 0` or `--timeout 0s` means unlimited; positive durations require units, whole milliseconds, and at most four hours. Queue time is separate; initialization and correction consume the same execution budget. At 90% of a finite budget Agentbus attempts one best-effort deadline notice during an eligible initial turn, including fixed reviews; after late initialization it attempts the notice as soon as control is accepted if time remains. The notice reports the remaining time and asks the worker to finish, without adding review evidence or changing the deadline. Unlimited jobs and correction or retired turns receive no notice. For large evidence, give the worker an explicit permitted artifact path and retrieve its final report via `agentbus result`. Do not use `transcript --last 1` as a full-report API.

Generic tasks are steerable unless `--no-steer` is set; caller steering is accepted only during an eligible initial turn. Fixed tasks, including all reviews, reject caller steering with `not_steerable` in every job state, including after completion. The daemon reserves the message ID `agentbus-deadline-warning`. New review instructions or evidence require a new review; never retry with fresh message IDs to bypass fixed-input rejection. Steering receipts say `admitted`, `delivered`, `rejected`, or `unknown`; admission does not confirm provider delivery.

`--retain-session` requests continuation; `--resume <job-id>` creates a new job and fresh deadline in an eligible conversation. A completed target must have requested retention; a failed, canceled, or unknown target can be eligible without it once the whole conversation is inactive and cleanup is proven. Each resume needs a recorded provider identity and an available conversation. Retain each continuation that needs a follow-up after success. Use `agentbus session discard <job-id>` for inactive owned Codex conversations when continuation is no longer needed. Disposal permanently revokes resume eligibility across the whole conversation, including retained descendants, while preserving jobs and results.

For replay, reuse the same explicit request ID, logical workspace, and immutable task specification after an ambiguous submission. A matching replay returns the existing job and `deduplicated: true`. Delegate never silently retries. A generated request ID may be lost if a process is killed before the receipt or error is visible; use a caller-owned ID when replay matters.

## Observe and control

```sh
agentbus status "$JOB_ID" --json
agentbus status --workspace-key "$WORKSPACE_KEY" --json
agentbus status --tag 'run=example' --json
agentbus transcript "$JOB_ID" --kind message --last 10 --json
agentbus result "$JOB_ID" --wait --wait-timeout 2h > report.txt
agentbus result "$JOB_ID" --json
agentbus cancel "$JOB_ID" --json
printf '%s' "$FOLLOWUP" | agentbus steer "$JOB_ID" --request-id "$MESSAGE_ID" --message-file -
agentbus session discard "$JOB_ID" --json
```

Job IDs are positional and options may follow them. `status` without an ID lists jobs, optionally filtered by workspace key, tags, or states. Successful status and transcript reads exit zero for any stored state; gaps, truncation, or pruning remain visible. Result is outcome-bearing: 0 completed, 2 queued/running, 3 schema noncompliant, 4 failed, 5 timeout, 6 interrupted, 7 canceled, 10 unknown job, 14 unknown outcome, 15 unavailable/corrupt result, and 16 observation timeout. `--wait-timeout` requires `--wait`; it is an observation limit (default 30 minutes), not the execution deadline, and expiry neither cancels nor extends the job. Steering exits 17 for rejected and 18 for unknown.

Set `AGENTBUS_NO_AUTOSTART=1` for both CLIs during stopped-state tests and scratch-root inspection. Empty or `0` enables autostart; other nonempty values fail configuration validation. `AGENTBUS_STATE_ROOT` selects the exact root, and a scratch root alone does not disable autostart. Agentbus defaults to `$XDG_STATE_HOME/agentbus/v2` or `~/.local/state/agentbus/v2`. Delegate selects the Agentbus executable from `PATH` once per invocation. Changing a client's environment does not reconfigure a daemon already running. A `daemon_draining` response does not authorize autostart or replaying a submission.

## Reviews

`delegate review` and `delegate adversarial-review` submit inspect intent with fixed input. Direct reviews accept backend, cwd, model, effort, timeout, resume, `--base`, `--scope auto|working-tree|branch`, `--allow-live-repo-read`, and `--criteria-file`. They capture a coherent Git view and submit sanitized diffs, source/excerpts, criteria, and a manifest. The direct receipt's `review.inputDigest` binds the exact submitted packet, including when delivered as a file. Working-tree mode supports unborn repositories; branch mode reads committed blobs from the captured HEAD and ignores working-tree edits. Secret redaction covers the assembled packet, not all files the host-access backend might read. Supplemental live reads enabled by `--allow-live-repo-read` are outside the captured-input guarantee.

Contract review uses `--request-file`, `--artifact-file`, and `--charter-file` together. The charter must be a plain `review-charter-v2` document. For a v2 request, also select `--reviewer` from the frozen recipe; its schema binds request, recipe, reviewer, charter, input, and consumer values. V1 produces `review-report-v1`; V2 produces `review-report-v2`. The caller screens contract inputs. The receipt records validated evidence and subject bindings, but does not establish report compliance or provider comprehension. Preserve report bytes and validate the required bindings.

## Evidence and coordinated work

- For parallel implementation units, name the unit responsible for every requirement and a single owner for shared enums, exit codes, API fields, and cross-unit cleanup. Resolve duplicate implementations explicitly before integration.
- Record every validation command, exit status, and tests that actually ran. A focused filter matching no tests is no evidence; an unsupported worker claim that tests passed remains unverified. Inspect a check's actual conclusion and available execution/test reports. Missing step or runner metadata alone proves neither success nor a specific failure, cancellation, or capacity problem.
- Rerun relevant integrated gates after clean merges and after conflict resolution. Run memory-intensive race suites sequentially on constrained hosts. Use the repository's declared package manager and toolchain; deleting dependency directories or changing package managers requires task authorization.
- For Bash pipelines where failure propagation matters use `bash -o pipefail -c` and inspect individual pipeline statuses where needed. Do not rely on another shell's splitting rules or assume Bash `PIPESTATUS` exists there.
