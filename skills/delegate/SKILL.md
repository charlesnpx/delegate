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

Task flags are `--backend`, `--cwd`, `--write`, `--model`, `--effort`, `--timeout`, `--prompt-file`, `--schema-file`, `--request-id`, and `--tag`.

Contract mode for `delegate review` and `delegate adversarial-review` uses `--request-file <request.json> --artifact-file <review.patch> --charter-file <charter.json>`; the command returns the asynchronous submit receipt, its schema-enforced `review-report-v1` result is later available through `agentbus result --job <id> --json`, and Delegate does not run secret-path, history, or content redaction on those caller-frozen inputs, so callers are responsible for screening them.

Submission is asynchronous. After an ambiguous submission, reuse the same `--request-id` and run from the same canonical `--cwd` (the replay key is their pair); a replay returns `deduplicated: true` and the original job ID. Observe a job with Agentbus:

```sh
agentbus status --job <id> --json
agentbus result --job <id> --json
agentbus cancel --job <id> --json
```

For `status` or `result`, exit code 2 means the job is still running.

With `--write`, workspace-write access only inside its `--cwd` and no network are Codex-specific guarantees; enforcement depends on the selected Agentbus backend: Claude runs without a filesystem or network sandbox, and Cursor uses agent-mode permissions. For Go builds, set `GOCACHE` inside `--cwd` and leave `GOMODCACHE` at its default.
