---
name: delegate:rescue:codex
description: Delegate a rescue task to codex through delegate and return the launch envelope verbatim.
version: v0.6.0
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
- backend reachability: "delegate setup --json" shows agentbus capabilities and codex backend availability.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

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

When the caller has a machine-readable output schema, pass it with "--output-schema-file" instead of placing it in prompt prose. Violations return as "<json-pointer>: <message>", and one corrective retry runs automatically.

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", or "result_sha256" fields.

If submission is unresolved after Agentbus accepted or may have accepted the request, preserve the reported "request_id" and run only the exact recovery command "delegate task --recover-request <request_id> --json". Do not create a replacement request unless the user explicitly asks for a new logical task.

Launch with "--background" and keep the host agent loop free to continue useful work. Long "--wait" calls can hold a host tool call for 100+ seconds and block that loop; use "--wait" only for a short, explicitly bounded terminal check. For an outstanding job, poll "delegate status --job <id>" instead.

## Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Never silently drop the job or substitute your own answer for the delegated run.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

Use delegate-report discipline in your own handoff: score criteria, label evidence as observed/inferred/assumed, separate changed from verified, state scope boundaries, and report divergences instead of hiding them.
