---
name: delegate:review:cursor
description: Delegate a code review to cursor through sanitized delegate review context and return the submit receipt verbatim.
version: v0.9.1
---

# delegate:review:cursor

Use this when an orchestrator should delegate a read-only code review to the cursor backend through delegate's sanitized review-context pipeline and return immediately with the submit receipt.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks for a direct local review and delegate is unavailable in this environment, perform the review locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate review"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: the parent harness, "delegate", agentbus, and the cursor backend can see the delegate state path. Using the private workspace as backend cwd is not OS isolation; a same-user backend can still read repository or other filesystem files when process permissions allow it.
- exec: "delegate", "agentbus", "git", and the cursor backend executable are runnable. Git is used by host-side delegate assembly only; it is not a review-worker preflight or input source.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. A per-review workspace under that state root holds sanitized repository content; the next review invocation removes it only after the job reaches a terminal state, so the most recent review's workspace persists until then. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.

"agentbus setup --json" is a one-time, installation-time check that every configured backend is usable, not a per-launch prerequisite. It covers all configured backends together and fails if any of them fails, so a failure does not by itself mean the selected cursor backend is unusable.

"delegate task" itself enforces the selected backend and required Agentbus capabilities at submission time; this review command uses the same Delegate submission gate. A launch failure reports its own connection or backend error directly.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

Review commands never pass "--write" and intentionally run the backend read-only. A read-only review worker cannot create a build/temp directory, compile, or run tests, so the caller must execute runtime verification and gates.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id, job record, contract stamps, and read-only profile.

Threat model: delegate's review context is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions.

## Review Context Discipline

Delegate performs Git collection on the host before the review worker starts and its composed review prompt supplies effective scope and, as applicable, resolved base, resolved base tip commit, merge-base comparison baseline, branch under review, and HEAD commit. The identifiers actually supplied are authoritative: the reviewer must report them as given, including in Scope boundary, rather than treating them as unavailable or inferring missing identifiers or a full commit list. In branch and auto scope, the supplied comparison baseline is the merge base used for the diff; the resolved base tip identifies the base ref. In working-tree scope, the supplied HEAD commit is the comparison baseline; a base tip applies only when supplied.

Reading the assembled context is the first and only required step. Do not instruct the review worker to probe for already-supplied metadata or context, and do not put the expressly unnecessary redundant metadata probe before "review.patch" with "&&". A sandbox denial of that expressly unnecessary probe must not stop the review; it should read the assembled context and complete the review. In live-repository mode, repository reads to validate or self-collect supplemental context remain permitted after that context read; supplied identifiers remain authoritative.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate review --backend cursor --cwd "$PWD"
~~~

Return the submit receipt verbatim. Do not wrap it in prose or rename its "requestId", "jobId", "state", "deduplicated", or "timeout" fields.

Review submission already returns immediately. Use Agentbus directly for job control: "agentbus status --job <id> --json", "agentbus result --job <id> --json", and "agentbus cancel --job <id> --json". A status or result exit code of 2 means the job is still running; callers that need completion use a plain shell loop around the Agentbus command. Never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail. Never silently drop the job or substitute your own answer for the delegated review.

## Review Result Discipline

Present findings first and keep them ordered by severity. Preserve the delegated review's file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Distinguish observed evidence from inferred risk and assumptions. If there are no findings, say so explicitly and keep residual risk brief. If the run failed or returned malformed output, show the actionable Agentbus failure and stop instead of guessing or substituting a local review.

Never auto-fix after presenting review findings. Ask the user which issues, if any, they want addressed.
