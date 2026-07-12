---
name: delegate:review:codex
description: Delegate a code review to codex through sanitized delegate review context and return the launch envelope verbatim.
version: v0.4.0
---

# delegate:review:codex

Use this when an orchestrator should delegate a read-only code review to the codex backend through delegate's sanitized review-context pipeline and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks for a direct local review and delegate is unavailable in this environment, perform the review locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate review"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: the parent harness, "delegate", agentbus, and the codex backend can see the delegate state path. Using the private workspace as backend cwd is not OS isolation; a same-user backend can still read repository or other filesystem files when process permissions allow it.
- exec: "delegate", "agentbus", "git", and the codex backend executable are runnable.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "delegate setup --json" shows agentbus capabilities and codex backend availability.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

When the parent uses the same harness as the selected backend, this launches a fresh supervised session—not a native subagent. It has its own job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

Threat model: v0.1 is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope. v0.2 OS isolation is the boundary fix for that class.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions. A container/sandbox profile for OS-level isolation is planned for v0.2.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate review --backend codex --origin delegate:review:codex --cwd "$PWD" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call.

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over 60 seconds, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Review Result Discipline

Present findings first and keep them ordered by severity. Preserve the delegated review's file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Distinguish observed evidence from inferred risk and assumptions. If there are no findings, say so explicitly and keep residual risk brief. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing or substituting a local review.

Never auto-fix after presenting review findings. Ask the user which issues, if any, they want addressed.
