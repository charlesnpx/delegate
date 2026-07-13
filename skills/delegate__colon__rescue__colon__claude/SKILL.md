---
name: delegate:rescue:claude
description: Delegate a rescue task to claude through delegate and return the launch envelope verbatim.
version: v0.4.2
---

# delegate:rescue:claude

Use this when an orchestrator should delegate a rescue task to the claude backend through "delegate" and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks you to perform the task directly and locally, and delegate is unavailable in this environment, comply locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate task"/agentbus supervision, not an unmanaged background shell or a local substitute answer.
- shared fs: the parent harness, "delegate", agentbus, and the claude backend can see the same repo path and delegate state.
- exec: "delegate", "agentbus", and the claude backend executable are runnable.
- repo+state write access: the target repo and delegate/agentbus state roots are writable when the task needs writes.
- stdin handoff: sensitive prompt text can be piped to "delegate handoff create --json".
- backend reachability: "delegate setup --json" shows agentbus capabilities and claude backend availability.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

When the parent uses the same harness as the selected backend, this launches a fresh supervised session—not a native subagent. It has its own job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt into "delegate handoff create --json" and capture the returned "handoff_path" as "HANDOFF_PATH".
3. Spawn the no-fork delegated job exactly through the CLI:

~~~bash
delegate task --backend claude --origin delegate:rescue:claude --cwd "$PWD" --handoff-prompt-file "$HANDOFF_PATH" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

Launch with "--background" and keep the host agent loop free to continue useful work. Long "--wait" calls can hold a host tool call for 100+ seconds and block that loop; use "--wait" only for a short, explicitly bounded terminal check. For an outstanding job, poll "delegate status --job <id>" instead.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Plain "delegate status --json --job <id>" is the cheap call; "--probe" blocks for roughly two sampling intervals (default ~10s+, configurable with "--probe-interval").

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over the probe interval, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

Use delegate-report discipline in your own handoff: score criteria, label evidence as observed/inferred/assumed, separate changed from verified, state scope boundaries, and report divergences instead of hiding them.
