---
name: claude:adversarial-review
description: Delegate a refute-first adversarial code review from Codex to Claude Code through sanitized delegate review context and return the launch envelope verbatim.
version: v0.1.1
---

# claude:adversarial-review

Use this when Codex should delegate a read-only refute-first adversarial code review to Claude Code through delegate's sanitized review-context pipeline and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

- no-fork support: the job must run through "delegate adversarial-review"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: Codex, "delegate", agentbus, and Claude Code can see the delegate state path; the backend does not need the live repository in safe mode.
- exec: "delegate", "agentbus", "git", and the claude backend executable are runnable.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "delegate setup --json" shows agentbus capabilities and claude backend availability.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that it is not secret-safe.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate adversarial-review --backend claude --origin claude:adversarial-review --cwd "$PWD" --background --json
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
