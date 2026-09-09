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

Task flags are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, `--resume`, and `--tag`. `--resume <jobId>`: resume a prior job; creates a new job with a fresh deadline.

Contract mode for `delegate review` and `delegate adversarial-review` uses `--request-file <request.json> --artifact-file <review.patch> --charter-file <charter.json>`; the command returns the asynchronous submit receipt, its schema-enforced `review-report-v1` result is later available through `agentbus result --job <id> --json`, and Delegate does not run secret-path, history, or content redaction on those caller-frozen inputs, so callers are responsible for screening them.

Submission is asynchronous. After an ambiguous submission, reuse the same `--request-id` and run from the same canonical `--cwd` (the replay key is their pair); a replay returns `deduplicated: true` and the original job ID. Observe a job with Agentbus:

```sh
agentbus status --job <id> --json
agentbus status --workspace-key <workspaceKey> --json
agentbus status --tag <key=value> --json
agentbus status --state <state> --json
agentbus transcript --job <id> --kind message --last <n> --json
agentbus result --job <id> --json
agentbus cancel --job <id> --json
```

Without `--job`, `status` lists summaries from every workspace unless filtered. Its optional filters are `--workspace-key`, repeatable `--tag`, and repeatable `--state`, and different filters combine with AND, while `--job` selects one record. Repeating a flag differs by flag: repeated `--tag` takes distinct keys and requires every tag to match, and repeating a key is rejected, whereas repeated `--state` matches any listed state. Use the submit receipt's `workspaceKey` value verbatim for `--workspace-key`. Valid states are `queued`, `running`, `completed`, `failed`, `canceled`, and `unknown`. A summary has no result; list to select, then inspect with `--job`. Without any transcript filter, `transcript` returns a digest of messages and errors; any filter, including `--last`, replaces that digest with a raw tail that can be all tool items, so pair `--last <n>` with `--kind message` when messages are what you want.

For a selected job, `status`/`result` exit with the job-state code, not a command success/failure code: 0=completed, 2=queued/running, 3=completed-noncompliant, 4=failed, 5=timeout, 6=interrupted, 7=canceled, 14=unknown, and 15=result-artifact-unavailable (completed, but its artifact is unavailable). Codes 10=unknown-job, 11=daemon-startup-failure, and 13=shutdown-deadline are CLI/daemon failures. A listing exits 0 once printed, regardless of member states.

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. For Go builds, set `GOCACHE` inside `--cwd` and leave `GOMODCACHE` at its default.
