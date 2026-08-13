---
name: delegate:adversarial-review:codex
description: Delegate a refute-first adversarial code review to codex through sanitized delegate review context and return the launch envelope verbatim.
version: v0.9.0
---

# delegate:adversarial-review:codex

Use this when an orchestrator should delegate a read-only refute-first adversarial code review to the codex backend through delegate's sanitized review-context pipeline and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks for a direct local review and delegate is unavailable in this environment, perform the review locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate adversarial-review"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: the parent harness, "delegate", agentbus, and the codex backend can see the delegate state path. Using the private workspace as backend cwd is not OS isolation; a same-user backend can still read repository or other filesystem files when process permissions allow it.
- exec: "delegate", "agentbus", "git", and the codex backend executable are runnable. Git is used by host-side delegate assembly only; it is not a review-worker preflight or input source.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "delegate setup --json --backend codex" shows agentbus capabilities and codex backend availability without unrelated backend model catalogues.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

Review commands never pass "--write" and intentionally run the backend read-only. A read-only review worker cannot create a build/temp directory, compile, or run tests, so the caller must execute runtime verification and gates. The launch and terminal envelope's "backend_profile" reports the effective Agentbus sandbox mode as "read-only" or "workspace-write"; use it to route runtime gates.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id, job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

Threat model: delegate's review context is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions.

## Review Context Discipline

Delegate performs Git collection on the host before the review worker starts and its composed review prompt supplies effective scope and, as applicable, resolved base, resolved base tip commit, merge-base comparison baseline, branch under review, and HEAD commit. The identifiers actually supplied are authoritative: the reviewer must report them as given, including in Scope boundary, rather than treating them as unavailable or inferring missing identifiers or a full commit list. In branch and auto scope, the supplied comparison baseline is the merge base used for the diff; the resolved base tip identifies the base ref. In working-tree scope, the supplied HEAD commit is the comparison baseline; a base tip applies only when supplied.

Reading the assembled context is the first and only required step. Do not instruct the review worker to probe for already-supplied metadata or context, and do not put the expressly unnecessary redundant metadata probe before "review.patch" with "&&". A sandbox denial of that expressly unnecessary probe must not stop the review; it should read the assembled context and complete the review. In live-repository mode, repository reads to validate or self-collect supplemental context remain permitted after that context read; supplied identifiers remain authoritative.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate adversarial-review --backend codex --origin delegate:adversarial-review:codex --cwd "$PWD" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "backend_profile", "timeout", or "result_sha256" fields.

If submission is unresolved after Agentbus accepted or may have accepted the request, preserve the reported "request_id" and run only the exact recovery command "delegate task --recover-request <request_id> --json". Do not create a replacement request unless the user explicitly asks for a new logical review.

Launch with "--background" so the host agent loop stays free. To await the job, start exactly ONE background "delegate result --job <id> --wait --json" task: a background "--wait" is the normal orchestration pattern — it blocks only its own small awaiter process, not a worker slot or the model. A FOREGROUND "--wait" ties up the current host tool call, so use a foreground "--wait" only for a short, explicitly bounded terminal check. Bound long waits with "--wait-timeout <duration>" (on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job). Do NOT write shell polling loops, and never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail, and filesystem salvage is an operator-only emergency after a confirmed CLI defect, not a supported path. Use one-shot "delegate status --job <id> --json" only for on-demand progress (for example when the user asks what the job is doing). Never silently drop the job or substitute your own answer for the delegated review.

## Review Result Discipline

Present findings first and keep them ordered by severity. Preserve the delegated review's file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Distinguish observed evidence from inferred risk and assumptions. Terminal envelopes carry the same "timeout" resolution as launch envelopes and may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say so explicitly and keep residual risk brief. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing or substituting a local review.

Never auto-fix after presenting review findings. Ask the user which issues, if any, they want addressed.
