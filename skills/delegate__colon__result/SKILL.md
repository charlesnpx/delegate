---
name: delegate:result
description: Fetch and present a delegated job result through delegate.
version: v0.6.0
---

# delegate:result

Run the delegate CLI directly for a delegated job. Do not replace the job with a local answer.

## Command

Set "JOB_ID" to the delegated job id, then run:

~~~bash
delegate result --job "$JOB_ID" --json
~~~

For a non-terminal job, do not use "delegate result --wait" as the normal host-agent-loop control flow. Long "--wait" calls can hold a host tool call for 100+ seconds and block the host agent loop. Use "--wait" only for a short, explicitly bounded terminal check; otherwise poll "delegate status --job <id>" and fetch the result after the job is terminal.

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Plain "delegate status --json --job <id>" is the cheap call; "--probe" blocks for roughly one to three sampling intervals (~10-30s at the default 10s interval, configurable with "--probe-interval").

Use "delegate status --job <id> --probe --json" only as an observational diagnostic. Its verdict is one of "activity_observed", "no_activity_observed", "inconclusive", or "terminal", and its output includes "authority_state", "cleanup_disposition", and "authority_warnings". These fields report what Delegate observed; they do not override Agentbus state or prove backend absence.

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over the probe interval, because progress can land without a command event.

A flat CPU sample, no TCP socket, unchanged log size, or expired lease is not cancellation authority. Report the job id, "authority_state", probe verdict, and any "authority_warnings"; then either keep waiting or ask the user before cancelling. Never silently drop the job or substitute your own answer for the delegated run.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.
