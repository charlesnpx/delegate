---
name: delegate
description: Submit one asynchronous task through delegate.
---

# delegate

Submit one task by piping its prompt to stdin:

```sh
printf '%s' "$PROMPT" | delegate task --backend <name> --cwd <abs> --prompt-file -
```

`--prompt-file -` reads the prompt from stdin. The submit receipt is JSON on stdout; notices and errors go to stderr.

Task flags are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, `--resume`, `--retain-session`, and `--tag`. `--resume <jobId>` resumes a prior job and creates a new job with a fresh deadline. `--retain-session` keeps the backend session after completion so this job can be resumed; the session directory is left in place until removed.

Continue the same backend conversation in two steps:

```sh
# submit, keeping the session for a follow-up
printf '%s' "$PROMPT" | delegate task --backend codex --retain-session --cwd <abs> --prompt-file -

# read the result, then continue the same conversation
printf '%s' "$FOLLOWUP" | delegate task --backend codex --resume <jobId> --cwd <abs> --prompt-file -
```

Without `--retain-session`, a completed job cannot be resumed. This is not a bug; the session is cleaned up when the job completes. `--resume` creates a new job with a fresh deadline and its own result; the original job's result is unchanged and remains final. A retained session directory stays on disk until an operator removes it. `--retain-session` is accepted only by `delegate task`; the review commands are schema/report workflows, not conversations to continue.

Contract mode has two request forms. The v1 request/artifact form uses
`--request-file <review-request-v1.json> --artifact-file <review.patch>
--charter-file <charter.json>`; the separate charter file is still required.
It returns an asynchronous submit receipt whose schema-enforced
`review-report-v1` result is later available through
`agentbus result --job <id> --json`; v1 has no `--reviewer` selection.

The v2 form uses the same three files plus
`--reviewer <identifier>`:

```sh
delegate review --backend <name> --cwd <abs> \
  --request-file <review-request-v2.json> \
  --artifact-file <review.patch> --charter-file <charter.json> \
  --reviewer <declared-reviewer>
```

Delegate detects the form from the request document's `schema_version`. The
selected reviewer must occur in the request's frozen recipe
`required_outputs`; an undeclared identifier is refused. The submitted v2
schema binds `request_digest`, `recipe_digest`, `reviewer`, `charter_hash`,
`review_input_digest`, and the requesting consumer identity. The prompt carries
the frozen recipe instructions verbatim, so reviewer names are open data rather
than a Delegate registry. The v2 result is a `review-report-v2` object and is
later read by the host through Agentbus.

Both forms carry caller-frozen request, artifact, and charter inputs verbatim.
Delegate does not run secret-path, history, or content redaction on them, so
callers are responsible for screening them. Delegate only submits and returns
the receipt; it does not wait, poll, fetch a result, or construct a completion
document. A v2 submission's replay identity includes its reviewer, so separate
reviewers from one recipe are separate tasks.

Submission is asynchronous. After an ambiguous submission, reuse the same `--request-id` and run from the same canonical `--cwd` (the replay key is their pair); a replay returns `deduplicated: true` and the original job ID. The immutable task specification is hashed too, so enabling `--retain-session` is a different replay identity, while omitting it preserves existing replay behavior. Observe a job with Agentbus:

```sh
agentbus status --job <id> --json
agentbus status --workspace-key <workspaceKey> --json
agentbus status --tag <key=value> --json
agentbus status --state <state> --json
agentbus transcript --job <id> --kind message --last <n> --json
agentbus result --job <id> --json
agentbus cancel --job <id> --json
```

Without `--job`, `status` lists summaries from every workspace unless filtered. Its optional filters are `--workspace-key`, repeatable `--tag`, and repeatable `--state`, and different filters combine with AND, while `--job` selects one record. Repeating a flag differs by flag: repeated `--tag` takes distinct keys and requires every tag to match, and repeating a key is rejected, whereas repeated `--state` matches any listed state. Use the submit receipt's `workspaceKey` value verbatim for `--workspace-key`. Valid states are `queued`, `running`, `completed`, `failed`, `canceled`, and `unknown`. A summary has no result; list to select, then inspect with `--job`. Without any transcript filter, `transcript` returns a digest of messages and errors. To follow messages as they arrive, page forward with `--kind message --since-ordinal <n> --limit <page-size>`, starting at `0` and feeding back the highest returned `ordinal`; a short or empty page means nothing more is readable yet, so keep polling while `state` is `queued` or `running`. Once `state` is terminal, `gap: false` means you have every captured message, while `gap: true` means capture or reading was incomplete and the unseen items cannot be paged in; a running job is always gapped, so `gap` only carries that meaning once terminal. `--last <n>` is a one-time tail view that discards earlier items; there is no backwards paging, so it is not a polling cursor.

For a selected job, `status`/`result` exit with the job-state code, not a command success/failure code: 0=completed, 2=queued/running, 3=completed-noncompliant, 4=failed, 5=timeout, 6=interrupted, 7=canceled, 14=unknown, and 15=result-artifact-unavailable (completed, but its artifact is unavailable). Codes 10=unknown-job, 11=daemon-startup-failure, and 13=shutdown-deadline are CLI/daemon failures. A listing exits 0 once printed, regardless of member states.

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. For Go builds, set `GOCACHE` under `/tmp` and leave it out of the reviewed workspace; leave `GOMODCACHE` at its default.
