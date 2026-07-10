---
name: claude:status
description: Check a delegated job status through delegate.
version: v0.2.0
---

# claude:status

Run the delegate CLI directly for a Claude Code job. Do not replace the job with a local answer.

## Command

Set "JOB_ID" to the delegated job id, then run:

~~~bash
delegate status --job "$JOB_ID" --json
~~~

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call.

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over 60 seconds, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.
