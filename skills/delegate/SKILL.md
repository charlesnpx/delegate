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

For a retry after an ambiguous submission, supply a stable `--request-id` and retry with that same identity. A replay returns `deduplicated: true` and the original job ID.

Submission is always asynchronous. Observe a job with Agentbus:

```sh
agentbus status --job <id> --json
agentbus result --job <id> --json
agentbus cancel --job <id> --json
```

For `status` or `result`, exit code 2 means the job is still running.

With `--write`, the worker has workspace-write access only inside its own `--cwd` and has no network. Put `GOCACHE` under that cwd because Go writes to it; leave the default `GOMODCACHE` alone because it is read-only to the worker and already populated, while a relocated empty cache cannot be refilled without network access. Without `--write`, the worker is read-only, has no network, and cannot create temporary directories.
