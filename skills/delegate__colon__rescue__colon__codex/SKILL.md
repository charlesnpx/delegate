---
name: delegate:rescue:codex
description: Delegate a rescue task to codex through delegate and return the launch envelope verbatim.
version: v0.8.2
---

# delegate:rescue:codex

Use this when an orchestrator should delegate a rescue task to the codex backend through "delegate" and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks you to perform the task directly and locally, and delegate is unavailable in this environment, comply locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate task"/agentbus supervision, not an unmanaged background shell or a local substitute answer.
- shared fs: the parent harness, "delegate", agentbus, and the codex backend can see the same repo path and delegate state.
- exec: "delegate", "agentbus", and the codex backend executable are runnable.
- repo+state write access: the target repo and delegate/agentbus state roots are writable when the task needs writes.
- stdin handoff: sensitive prompt text can be piped to "delegate handoff create --json".
- backend reachability: "delegate setup --json --backend codex" shows agentbus capabilities and codex backend availability without unrelated backend model catalogues.

"delegate task" is read-only unless it has "--write". The worker sandbox is offline, and a write turn can write only inside the job "--cwd"; use it for repo-local edits/builds/tests and point "GOCACHE" and "GOMODCACHE" under that cwd. Route module downloads, other network work, and Git commits to the caller/orchestrator.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted. The "--timeout" flag is optional; omit it or pass "--timeout 0" to leave the deadline to the daemon default, then use the launch envelope's "timeout" field as the authoritative effective value.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id, job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt into "delegate handoff create --json" and capture the returned "handoff_path" as "HANDOFF_PATH".
3. Spawn the no-fork delegated job exactly through the CLI:

~~~bash
delegate task --backend codex --origin delegate:rescue:codex --cwd "$PWD" --handoff-prompt-file "$HANDOFF_PATH" --background --json
~~~

Each handoff prompt file is single-use: after the task consumes it, create a new handoff file before a relaunch of the same packet.

When the caller has a machine-readable output schema, pass it with "--output-schema-file" instead of placing it in prompt prose. Violations return as "<json-pointer>: <message>", and one corrective retry runs automatically.

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "timeout", or "result_sha256" fields.

If submission is unresolved after Agentbus accepted or may have accepted the request, preserve the reported "request_id" and run only the exact recovery command "delegate task --recover-request <request_id> --json". Do not create a replacement request unless the user explicitly asks for a new logical task.

Launch with "--background" so the host agent loop stays free. To await the job, start exactly ONE background "delegate result --job <id> --wait --json" task: a background "--wait" is the normal orchestration pattern — it blocks only its own small awaiter process, not a worker slot or the model. A FOREGROUND "--wait" ties up the current host tool call, so use a foreground "--wait" only for a short, explicitly bounded terminal check. Bound long waits with "--wait-timeout <duration>" (on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job). Do NOT write shell polling loops, and never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail, and filesystem salvage is an operator-only emergency after a confirmed CLI defect, not a supported path. Use one-shot "delegate status --job <id> --json" only for on-demand progress (for example when the user asks what the job is doing). Never silently drop the job or substitute your own answer for the delegated run.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes carry the same "timeout" resolution as launch envelopes and may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

Use delegate-report discipline in your own handoff: score criteria, label evidence as observed/inferred/assumed, separate changed from verified, state scope boundaries, and report divergences instead of hiding them.
