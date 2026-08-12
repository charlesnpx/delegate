---
name: delegate:status
description: Check a delegated job status through delegate.
version: v0.8.2
---

# delegate:status

Run the delegate CLI directly for a delegated job. Do not replace the job with a local answer.

## Command

Set "JOB_ID" to the delegated job id, then run:

~~~bash
delegate status --job "$JOB_ID" --json
~~~

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Monitoring

Awaiting a job: "delegate result --job <id> --wait --json" is the canonical await-and-fetch primitive — normally launched as ONE background task. "delegate status --job <id> --wait --json" is a terminal barrier when you do not need the body yet; also background it. A FOREGROUND "--wait" blocks the current host tool call, so reserve it for short bounded checks. Bound long waits with "--wait-timeout <duration>"; on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job. Use one-shot "delegate status --job <id> --json" only for on-demand progress.

Never scan the Agentbus state root to find results — that layout is private implementation detail. Never silently drop the job or substitute your own answer.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.
