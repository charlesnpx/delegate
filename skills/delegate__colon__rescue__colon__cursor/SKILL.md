---
name: delegate:rescue:cursor
description: Delegate a rescue task to cursor through delegate and return the submit receipt verbatim.
version: v0.9.1
---

# delegate:rescue:cursor

Use this when an orchestrator should delegate a rescue task to the cursor backend through "delegate" and return immediately with the submit receipt.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks you to perform the task directly and locally, and delegate is unavailable in this environment, comply locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate task"/agentbus supervision, not an unmanaged background shell or a local substitute answer.
- shared fs: the parent harness, "delegate", agentbus, and the cursor backend can see the same repo path and delegate state.
- exec: "delegate", "agentbus", and the cursor backend executable are runnable.
- repo+state write access: the target repo and delegate/agentbus state roots are writable when the task needs writes.
- stdin prompt: sensitive prompt text can be piped directly to "delegate task --prompt-file -".

"agentbus setup --json" is a one-time, installation-time check that every configured backend is usable, not a per-launch prerequisite. It covers all configured backends together and fails if any of them fails, so a failure does not by itself mean the selected cursor backend is unusable.

The per-launch gate is "delegate task": it enforces the selected backend and required Agentbus capabilities at submission time, and a launch failure reports its own connection or backend error directly.

"delegate task" is read-only unless it has "--write". The worker sandbox is offline, and a write turn can write only inside the job "--cwd"; use it for repo-local edits/builds/tests and point "GOCACHE" and "GOMODCACHE" under that cwd. Route module downloads, other network work, and Git commits to the caller/orchestrator. A read-only task notice is written to stderr; the submit receipt on stdout remains JSON only.

The "--model" and "--effort" flags are optional and are passed directly to Agentbus when supplied; otherwise Agentbus or the backend chooses. The "--timeout" flag is optional; omit it or pass "--timeout 0" to leave the deadline to the daemon default. The submit receipt's "timeout" field is Agentbus's authoritative effective millisecond value and source.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id and job record.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt directly into the no-fork delegated job:

~~~bash
printf '%s' "$PROMPT" | delegate task --backend cursor --cwd "$PWD" --prompt-file - --tag "skill=delegate:rescue:cursor"
~~~

When the caller has a machine-readable output schema, pass it with "--schema-file" instead of placing it in prompt prose. Violations return as "<json-pointer>: <message>", and one corrective retry runs automatically.

Return the submit receipt verbatim. Do not wrap it in prose, rename fields, or emit non-JSON text on stdout. Its identity and lookup fields are "requestId", "jobId", "state", "deduplicated", and "timeout".

Automation that needs replay safety must supply "--request-id <id>" and retry the same command with the same id after an ambiguous transport result. Delegate stores no local submission state. A manual invocation without "--request-id" receives a generated id in its receipt.

Task submission already returns immediately. Use Agentbus directly for job control: "agentbus status --job <id> --json", "agentbus result --job <id> --json", and "agentbus cancel --job <id> --json". A status or result exit code of 2 means the job is still running; callers that need completion use a plain shell loop around the Agentbus command. Never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail. Never silently drop the job or substitute your own answer for the delegated run.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If the run failed or returned malformed output, show the actionable Agentbus failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

Use delegate-report discipline in your own handoff: score criteria, label evidence as observed/inferred/assumed, separate changed from verified, state scope boundaries, and report divergences instead of hiding them.
