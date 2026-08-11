---
name: delegate:cancel
description: Cancel a delegated job after an explicit operator decision through delegate.
version: v0.8.1
---

# delegate:cancel

Run the delegate CLI directly for a delegated job. Do not replace the job with a local answer.

## Command

Set "JOB_ID" to the delegated job id, then run:

~~~bash
delegate cancel --job "$JOB_ID" --json
~~~

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Monitoring

Never scan the Agentbus state root to find results — that layout is private implementation detail. Never silently drop the job or substitute your own answer.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.
