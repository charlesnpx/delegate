---
name: delegate
description: Submit asynchronous tasks through delegate and observe or continue them with Agentbus.
---

# delegate

Submit one task by piping its prompt to stdin:

```sh
printf '%s' "$PROMPT" | delegate task --backend <name> --cwd <abs> --prompt-file -
```

`--prompt-file -` reads the prompt from stdin. The submit receipt is JSON on stdout; notices and errors go to stderr.

## Steering active tasks and job control

`delegate task` jobs are steerable by default, but Agentbus accepts steering only while a job is running. Check `agentbus status --job "$JOB_ID" --json` and wait for `state=running` before sending a message. If Agentbus rejects a message with `not_running`, it stores that rejection under the message ID; send any retry with a new `--request-id`:

```sh
printf '%s' "$FOLLOWUP" | agentbus steer --job "$JOB_ID" \
  --request-id "$MESSAGE_ID" --message-file -
```

Receipts report `admitted`, `delivered`, `rejected`, or `unknown`. `admitted` means Agentbus accepted the message; `delivered` confirms provider delivery. Exit code 17 means `rejected`, and 18 means `unknown`. A successful write to Agentbus does not prove provider delivery.

`--no-steer` submits a fixed task. Reviews are always fixed; new review instructions or evidence need a new review. A generic `delegate task` used as a review must pass `--no-steer`. Cancel stops work and does not deliver a message.

Delegate sends the `inputMode` field and does not silently remove it or downgrade a task. The running Agentbus daemon must support this field; upgrade it if Delegate reports that it does not support steerable tasks. The daemon must also support requested `--verify` and `--local-ipc` fields; Delegate reports an unknown field and requires an upgrade without retrying without it. `--no-steer` selects fixed mode on a daemon that supports the field and is not an old-daemon compatibility option.

## Submit and continue tasks

Task flags (15 total) are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, `--resume`, `--retain-session`, `--no-steer`, `--verify`, `--local-ipc`, and `--tag`. Tasks are steerable by default, and `--no-steer` makes one fixed. This setting is independent of `--write` and `--resume`. `--resume <jobId>` resumes a terminal job and creates a new job with a fresh deadline. `--retain-session` keeps the backend session after completion so this job can be resumed; the session directory is left in place until removed.

`--verify` runs a Codex task read-only against a daemon-created disposable copy of the tracked and untracked, non-ignored files in `--cwd`, with private per-job writable caches and a shared Go module cache for reads. The submitted source is not writable. This protects the source through workflow arrangement and does not provide a stronger isolation boundary than the Codex sandbox. The job's `execution` posture fields `intent: verify`, `frozenCopy`, and `frozenSource` show that it ran in a copy.

`--local-ipc` grants Codex a private short socket directory and loopback TCP for test runners that need IPC, such as Node's test runner. It is available for `--write` or `--verify` tasks on macOS; Linux refuses it.

Timeouts require explicit units. Use values such as `--timeout 90m`, `--timeout 2400s`, or `--timeout 0s` for the Agentbus default. A bare value such as `2400` is rejected; Delegate suggests `2400s` and `40m`.

Continue the same backend conversation in two steps:

```sh
# submit, keeping the session for a follow-up
printf '%s' "$PROMPT" | delegate task --backend codex --retain-session --cwd <abs> --prompt-file -

# read the result, then continue the same conversation
printf '%s' "$FOLLOWUP" | delegate task --backend codex --resume <jobId> --cwd <abs> --prompt-file -
```

Without `--retain-session`, a completed job cannot be resumed. This is not a bug; the session is cleaned up when the job completes. `--resume` creates a new job with a fresh deadline and its own result; the original job's result is unchanged and remains final. A retained session directory stays on disk until an operator removes it. `--retain-session` is accepted only by `delegate task`; the review commands are schema/report workflows, not conversations to continue.

For a follow-up after a job becomes terminal, read its result and submit another task with `--resume <jobId>`. Resumption requires a retained session, a recorded backend session ID, and the same backend. If the session is unavailable, submit a fresh task with the relevant context. A fresh task starts a separate conversation.

Add `--retain-session` to each resumed task too if another follow-up will be needed after it completes. Retention cannot be added retroactively to a completed job whose session was cleaned up. Failed, canceled, and unknown terminal jobs retain session directories, but resumption still requires a recorded backend session ID; cancellation alone does not guarantee resumability.

## Review commands

Direct `delegate review` and `delegate adversarial-review` accept `--base`,
`--scope`, `--allow-live-repo-read`, and `--criteria-file`, along with backend,
cwd, model, effort, and timeout. Both accept `--resume`. They do not
take task flags such as `--write`, `--verify`, `--local-ipc`, `--prompt-file`, `--schema-file`,
`--request-id`, `--retain-session`, or `--tag`.

Direct review packets include the diff, full changed text files up to 128 KiB
each within a 512 KiB combined source-and-criteria budget, and a manifest with
path, original byte size, SHA-256, and `full`, `partial`, or `omitted` status.
Oversized files get changed-hunk context with up to 40 surrounding lines and a
32 KiB per-file excerpt limit. Binary files, secret paths, and deleted paths
are listed with an omission reason. Secret-like content is redacted.

Use `--criteria-file <path>` to include pre-fetched UTF-8 acceptance criteria
in the direct packet. The file is limited to 256 KiB; its content and hash
appear in the packet manifest. Delegate does not fetch criteria over the
network. The flag is not valid with contract mode. `--allow-live-repo-read` applies to direct reviews only; it
does not supplement exact-input contract reviews.

A working-tree review supports a repository with no commits by using Git's
empty tree as the comparison base without creating a dummy commit. A `--cwd`
inside a repository subdirectory still resolves to that parent repository.
Branch scope refuses changed paths that also differ in the working tree so its
diff and supplied source content describe the same committed snapshot.

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

Both forms require `--charter-file` to contain a plain `charter.json`
(`review-charter-v2`) input. Delegate rejects a frozen `charter.freeze.json`
envelope with an error naming the expected and received shapes. Delegate
freezes the plain charter for the contract workflow. Direct-mode packet
construction does not change contract prompts, digests, or artifact handling.
Delegate does not run secret-path, history, or content redaction on contract
inputs, so callers are responsible for screening them. Delegate only submits
and returns the receipt; it does not wait, poll, fetch a result, or construct a
completion document. A v2 submission's replay identity includes its reviewer,
so separate reviewers from one recipe are separate tasks.

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

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. Writable Codex task jobs, including Verify, get a managed per-job `GOCACHE` outside the repository and use the populated daemon `GOMODCACHE` for reads. Agentbus does not replace the module cache with an empty per-job directory; missing modules fail offline.
