# Delegate v0.6.0 — clean protocol-v2 cut (campaign ledger)

Branch: `delegate-v0.6-protocol-v2-cut` (off `main` @ v0.5.0). Target: **breaking Delegate v0.6.0, Agentbus protocol v2 only. No v1/v2 compatibility layer.**

Product boundary (the invariant every unit serves):

> Agentbus owns identified admission, replay, execution, cancellation, terminal outcome, and cleanup evidence. Delegate owns prompt preparation, delegation policy, durable client request identity, review artifacts, audit metadata, and user-facing envelopes.

Execution doctrine: the standing loop from `~/tmp/orchestrator.md` — gpt-5.5 xhigh workers implement (`--write`), gpt-5.6-sol high reviewers refute (SHA-bound), max 4 review iterations per unit (stop early on a no-High/no-Critical iteration after sweeping worthwhile Low/Medium), orchestrator gates (build/vet/gofmt/full test + real v0.6 binary integration smoke) and commits between units. Be **discriminating** of gpt-5.6-sol findings — accept only concrete reachable defects or spec-rule violations; reject taxonomy, speculative hardening, new frameworks, and coverage asks on already-proven branches.

---

## D0 — PREREQUISITE (external, BLOCKING; not Delegate work)

Delegate cannot bump its dependency until Agentbus **actually tags `v0.6.0`**.

- As of writing, agentbus `VERSION` reads `0.6.0` but **no `v0.6.0` git tag exists** (tags: `v0.5.0`, `v0.5.1`; work is on branch `abf-custody`, PR #30 → `abd-authority` stacked under #29). The v2 identified-only daemon + two-axis outcome/cleanup + `orphaned`/exit-14 + `cleanupDisposition` all land through that stack.
- **Gate to start D1:** agentbus `abf-custody`/`abd-authority` merged to its release line and `v0.6.0` tagged and fetchable by `go get github.com/charlesnpx/agentbus@v0.6.0`. Confirm the tagged tree exposes: `client.HelloResult`, `client.JobSubmitParams{WorkspaceKey,RequestID,TaskSpec}`, `JobSubmitResult{JobID,State,Deduplicated}`, `*client.RPCError{code,jobId,admissionCause,runtimeSupport}`, `client.Options.StateRoot`, `engine.JobState` incl. `orphaned`, and that the session/turn wire surface is gone.
- Tagging / merging agentbus PRs are **external writes → require explicit user approval.** Not autonomous.

Until D0 clears, this campaign is decomposition + groundwork only. No worker delegation on D1+.

---

## Dependency graph & parallelism

```
D0 (external tag) ─▶ D1 ─▶ D2 ─▶ D3 ─┬─▶ D4 ─┐
                                     └─▶ D5 ─┴─▶ D6 ─▶ D7 ─▶ D8
```

- D1 → D2 → D3 are strictly serial (compile, then client-boundary foundations, then the identified-submit core).
- **D4 and D5 can run in parallel** after D3 (D4 = submit-path dedup/terminal; D5 = jobmeta cleanup gate) — separate worktrees, merge before D6.
- D6 depends on both D4 and D5; D7 on D2+D5+D6; D8 is the closure/docs/battery pass.

Each unit lands with its own behavioral tests. D8 is the black-box closure, not the first test of new behavior.

---

## STATUS
- **D0** ✅ agentbus `v0.6.0` tagged/pushed on `main` (content-identical to reviewed AB-F tip); go-resolvable.
- **D1** ✅ `c3e50f0` — compile cut. gate green (build/vet/gofmt/`go test ./...` all 0 under `-mod=vendor`). review1 SHIP (gpt-5.6-sol, no findings). Provisional path + `TODO(D3)` placeholder retained by design.
- **D2** ✅ `ce75ba7` — state-root resolver + typed RPC classification. gate green.
- **D3** ✅ `c7ac49d` — durable submission intents + identities + opaque job IDs + exact timeout. gate green (incl. protocol-v2 socket fake-server replay test running+passing). `TODO(D4)` for dedup/terminal rendering.
- **D2+D3** ✅ combined integration review SHIP at `c7ac49d`.
- **D4** ✅ `76b246b` — dedup/already-terminal + schema-2 envelope base. gate green.
- **D5** ✅ `87cb872` + fix `99ddc1d` — cleanup-disposition safety gate. D4+D5 combined review: iter1 FIX (1 High: wait-loop cleanup ignored status-disposition fallback → over-retention on `--wait`; accepted+fixed), iter2 SHIP. gate green.
- **Review-backend note:** gpt-5.6-sol hit a sustained capacity outage mid-campaign; reviews were batched into integration passes (D2+D3, D4+D5) and resumed across capacity retries. Codex is single-active-task → workers and reviews serialize; the D4∥D5 parallel plan was dropped (also overlapping files).
- **Next:** D6 (envelope schema-2 completion + orphaned exit-14) → D7 → D8.

---

## D1 — Compile cut: dependency bump + client-surface & foreground/embedded removal
Spec §1 (dep + client surface), §2 (remove resume), §13 (remove embedded). **Foundational — repo will not compile until session/turn types are gone.**

- `go.mod` v0.5.1 → v0.6.0; `go mod tidy`; refresh committed `vendor/` (agentbus now pulls bbolt + `x/sys` into the graph; both repos are Go 1.26 — no toolchain change). `VERSION` → `0.6.0`.
- Reduce `agentbusClient` to exactly: `Close`, `HelloResult`, `JobSubmit`, `JobStatus`, `JobResult`, `JobCancel`. Remove `Hello`, `SessionStart/Resume/List`, `TurnStart`, and every `SessionInfo`/`SessionStartParams`/`TurnNotification` + fake-client impl.
- Remove the fallback second `Hello` call — `client.Connect` already performs/validates hello; a second hello on the connection is invalid in v2. Rely on `HelloResult()` post-connect.
- Delete `--resume`, `--resume-session`, `--fresh`; delete `runDaemonSessionTask`, `resumableSessionInfo`, `validateResumeTarget`, `mostRecentDelegateSession`, `delegateSessionMetadata`, session model/effort resolution, turn-notification waiting, and all resume tests/docs/skill prose. Do **not** reinterpret `--resume` as request replay. Keep `--parent-session` (unrelated audit field). Keep contract corrective retry (an Agentbus policy op, not the deleted foreground session).
- Delete `--embedded` and `cmd/delegate/embedded.go` from task/review/adversarial-review.
- Capability checks: every **launch** requires `admission.strictContainment` **plus** the policy capabilities the chosen contract actually uses (today it checks only policy caps; setup baseline only `policy.shape`/`policy.retry`). `status`/`result`/`cancel` must **not** require policy caps — keep them usable for diagnosis.
- **Tests:** architecture guard (Delegate imports only agentbus `client` + stable `engine` types, never `engine/execution`/authority/custodian/repository/served); public-surface guard (resume/fresh/embedded absent from help + tests); strict-capability launch gating; status/result/cancel usable without policy caps.

## D2 — State-root binding + typed RPC rejection classification
Spec §11 (state root), §9 (typed rejections). Cross-cutting client-boundary foundations consumed by D3/D5/D7/D8.

- **Shared root resolver:** `AGENTBUS_STATE_ROOT` if set → require absolute + canonicalize; else agentbus standard default; pass via `client.Options.StateRoot` (today `client.Options` carries only `CommandPath`). Use the same root for setup preflight and sandbox config. Persist the resolved root in submission intents (D3) and acknowledged job metadata.
- Routing: `--recover-request` → intent's recorded root; `status/result/cancel --job` → job metadata's recorded root when present, else current configured root; `status --all` → current configured root only. **Never** scan/aggregate arbitrary roots; never auto-switch a pending request to a new root. Delegate never invokes `admission recover`/reset/seal/clear-fail-stop automatically.
- **Typed rejections:** `errors.As` to `*client.RPCError`; preserve `code`, `jobId`, `admissionCause`, `runtimeSupport`. Handle the stable causes per the spec table:
  | Cause | Delegate behavior |
  |---|---|
  | `missing_identity` | internal invariant failure |
  | `admission_closing` | reconnect + retry same identity, bounded |
  | `replay_conflict` | fail hard; preserve intent; never mint new ID |
  | `request_expired` | fail hard; new logical request needs explicit resubmission |
  | `request_fingerprint_unsupported` | fail hard; preserve exact request for diagnosis |
  | root fail-stop/corrupt/identity/sealed | preserve intent + operator guidance |
  | unsupported/unfenceable/runtime/config | definitive rejection; no job admitted |
  | transport / blank-cause `backend_unavailable` | ambiguous; replay same persisted request |
- Exit meanings preserved where possible: unknown job **10**, daemon/runtime **11**, authority fail-stop **12**, orphaned **14**. `--json` returns a **structured error envelope**, not a bare Go error string.
- **Tests:** each cause → correct classification/exit; root resolver (env/absolute/canonical/default) + routing rules; structured `--json` error shape.

## D3 — Durable submission intents + identities + opaque IDs + exact-param timeout
Spec §3 (durable intents — **the central change**), §4 (opaque job IDs), §10 (timeout). Depends on D1, D2.

- New `cmd/delegate/submission_intent.go`: durable `submissionIntent` keyed by request ID (schema, request/workspace/root, exact `JobSubmitParams`, kind/contract/model/effort/origin/review-workspace, phase `prepared|in_flight|acknowledged|blocked|rejected`, job_id, deduplicated, last_error, timestamps). Store at `$DELEGATE_STATE/submissions/<base64url(request-id)>.json`, mode `0600`, atomic replace + file fsync + dir fsync.
- **Identity generation:** ≥128 random bits → `requestId` as `delegate-<hex>`; domain-separated `workspaceKey = delegate-v1-<sha256(canonical-logical-workspace)>`; **review jobs use the original logical repo identity, not the temp review dir**. Persist both before first send; **never recompute during recovery**. Persist exact `JobSubmitParams` incl. nil-vs-present timeout/policy/tags/model/effort/prompt/CWD/write-mode/schema (agentbus fingerprints `taskSpec`; omitted≠null≠zero≠populated — replay persisted params, never reconstruct).
- Tags: add `delegate.request_id=<requestId>`; remove `delegate.provisional_job_id` and the provisional-adoption scan over `job.status --all` (replay is the authoritative recovery mechanism now).
- **Send sequence (closes all crash windows):** resolve → persist intent `prepared` → persist `in_flight` → `JobSubmit(persisted params)` → on success atomically persist acknowledged job metadata (job ID + request identity) → only then remove prompt payload / mark acknowledged.
- **Submission retry:** on transport/connection/malformed/ctx-cancel-after-send/blank-cause `backend_unavailable` — retain intent, reconnect, bounded retry with **same exact params**, never auto-mint a replacement request ID (the client reconnects but won't replay the logical op — Delegate must re-call `JobSubmit`).
- **Recovery surface:** `delegate task --recover-request <request-id>` — accepts no prompt/backend/model/effort/CWD/policy/schema/write flags; loads persisted request; reconnects to persisted root; resubmits same params → recovers job or reports typed rejection; missing local intent is an error (no arbitrary caller-supplied ID reuse). Unresolved-submission errors include the request ID + exact recovery command.
- **Opaque job IDs (§4):** delete `validateDelegateJobID`'s `job_` prefix requirement; store job metadata at `jobs/<base64url(job-id)>.json` and verify the in-file `job_id` matches the requested ID. No migration of pre-v2 jobs.
- **Timeout (§10):** track `Timeout`/`TimeoutSet`; omitted→`TimeoutMs=nil`, explicit zero→`*int64(0)`, positive→ms, negative→local usage error, above agentbus max→local usage error. Apply to task, review, adversarial-review.
- **Tests:** lost-response replay (server accepts+closes before responding → one job ID, one backend exec); crash-after-response recovery restores same job ID; replay-conflict (same ID, changed task → fail, no new ID); opaque + filename-hostile IDs; five timeout cases distinct.

## D4 — Deduplicated & already-terminal submission handling
Spec §5. Depends on D3. **Parallelizable with D5.**

- `JobSubmitResult{JobID, State, Deduplicated}`; a replay returns the job's **current** state (may already be terminal). Today Delegate collapses every non-queued state to `"running"` — must stop misreporting a deduplicated terminal job.
- Preserve `queued`/`starting`/`running`/`retrying` exactly. If returned state is terminal, immediately fetch `job.result` + `job.status` and emit a terminal envelope **even without `--wait`**. Include `deduplicated` + `request_id`.
- Local acknowledgement **idempotent** — reprocessing the same request/job association succeeds (metadata/renamed input already existing is not an error).
- Envelope schema bump to include `request_id`, `deduplicated` (feeds D6).
- **Tests:** deduplicated completed job → terminal envelope (never `"running"`); idempotent re-ack.

## D5 — Cleanup-disposition safety gate
Spec §6 — **second major correctness change.** Depends on D3 (jobmeta carries `state`+`cleanupDisposition`). **Parallelizable with D4.**

- Agentbus reports `cleanupDisposition ∈ {no_execution_possible, verified_absent, unresolved}` independently of terminal outcome (`completed|canceled|orphaned + unresolved` all possible). A successful result stays successful even when cleanup is unproven.
- Replace the `engine.IsTerminal(state)` cleanup gate with `localCleanupSafe(disp) = disp=="no_execution_possible" || disp=="verified_absent"`. Rules: `verified_absent`/`no_execution_possible` → remove prompt input + review workspace; `unresolved` → retain all execution-dependent artifacts; missing disposition on terminal job → retain conservatively; nonterminal → retain.
- Update all cleanup sites so their lookup contract carries **both** `state` and `cleanupDisposition` (terminal state alone insufficient): `cleanupJobInput`, `SweepTerminalJobInputs`, review-workspace cleanup, cancellation cleanup, status-triggered, result-triggered, immediate-terminal-replay cleanup, and `internal/handoff/job_input.go`.
- On retention, warn: `Agentbus reported cleanupDisposition=unresolved; delegate retained local job artifacts because backend absence is unproven`. **Never** turn a successful result into a failure because cleanup is unresolved.
- **Tests:** `completed+unresolved` → exit 0, result preserved, workspace retained; `verified_absent`/`no_execution_possible` → artifacts removed; missing-disposition terminal → retained.

## D6 — Terminal/status envelopes schema 2 + orphaned first-class
Spec §7. Depends on D4 + D5.

- Envelope **schema 2** adds `request_id`, `cleanup_disposition`, `late_finalization`, `agentbus_warnings`, `local_artifacts_retained`, `submission_deduplicated`. The `JobStatus`-derived terminal fallback must copy the same fields (today's fallback drops them). Since `JobResult` lacks warnings, terminal rendering fetches/reuses `JobStatus` to capture them (warnings on status; cleanup/finalization on both).
- **`orphaned` first-class terminal:** stop waits; emit terminal envelope; report no result when none exists; preserve cleanup disposition; exit code **14**; retain local artifacts when cleanup unresolved.
- Make `result_unavailable_reason` state-specific, not one generic reason.
- **Tests:** schema-2 fields populated on terminal + status-fallback paths; orphaned → exit 14, no fabricated result, retained; warnings surfaced from status.

## D7 — Cancel + wait/poll behavior
Spec §8. Depends on D2 + D5 + D6.

- **Cancel:** `JobCancelResult` carries only job ID + state (no disposition). After `JobCancel`: fetch `JobStatus` → fetch `JobResult` when terminal → emit terminal/status envelope w/ disposition + warnings → clean local artifacts **only** through the D5 disposition gate (required for `canceled+unresolved`, exit 7).
- **Wait loops:** remove `waitForTurnResult`; all waiting polls identified jobs via `JobResult`/`JobStatus`. Classify poll outcomes (today it effectively retries after every error): retryable (transport loss, synthetic blank-cause `backend_unavailable`); immediate permanent (`unknown_job`, protocol mismatch, root sealed/corrupt/identity-mismatch/fail-stopped); context-cancel (return immediately, preserve pending intent). `delegate result --wait` must not loop forever on unknown job or fail-stopped root.
- **Tests:** post-cancel status/result before cleanup; `canceled+unresolved` retention + exit 7; unknown-job and fail-stop terminate promptly, not infinite poll; ctx-cancel preserves intent.

## D8 — Setup / sandbox / probe + docs / skills + acceptance battery (closure)
Spec §12 (sandbox+setup), §14 (probe), file-level docs/skills, full acceptance battery. Depends on all prior.

- **`setup`** reports: resolved Agentbus state root + writable?; resolved autostart-lock root (`<UserCacheDir>/agentbus/start-locks`) + writable?; protocol version; `admission.strictContainment`; pending submission-intent count; unresolved-cleanup artifact count. Missing `admission.strictContainment` fails launch/setup clearly.
- **`codex_sandbox.go`:** add narrow `<UserCacheDir>/agentbus` (not the whole cache); replace hard-coded default state root with the resolved `AGENTBUS_STATE_ROOT` when configured.
- **`probe.go`:** verdicts become observational — `activity_observed | no_activity_observed | inconclusive | terminal` (drop `stalled`/`stalled_expired_lease`); include `authority_state`, `cleanup_disposition`, `authority_warnings`. A flat CPU / no socket / unchanged log / expired legacy lease must never override agentbus state, prove absence, trigger deletion, or authorize cancel.
- **Docs/skills:** `internal/skills/skills.go` + generated `skills/**` — remove resume/embedded language, add request-recovery + cleanup-disposition rules, bump skill content version; rewrite the managed status/cancel skill prose that treats the probe as stall confirmation. `README.md` — rewrite CLI, envelopes, monitoring, recovery, state-root, cleanup docs.
- **Acceptance battery** (all rows from the spec): compile cut; identified submit (nonempty persisted identities); lost-response replay (one job ID / one exec); crash-after-response recovery; replay conflict; opaque IDs; dedup terminal ≠ running; timeout five-way; completed+unresolved (exit 0, retained); canceled+unresolved (exit 7, retained); orphaned (exit 14, no fabricated result, retained); verified/no-exec cleanup removes; cancel→status/result→cleanup order; wait failures terminate promptly; root binding uses original root; codex sandbox both roots writable; strict setup fails clearly; public-surface absence; architecture guard. Plus: fake `agentbusClient` unit tests, a **protocol-v2 fake-server test** for connection-loss + replay, and an **exact Agentbus v0.6 binary integration smoke test**.

---

## Deliberately out of scope (do not add)
Replacement backend-session continuation; v1 compatibility; export/migration of old jobs; fallback reads of old JSON state; multi-root aggregation; automatic root recover/seal/reset/fail-stop-clear; direct imports of agentbus authority/custodian/storage internals; a second local execution authority behind `--embedded`.

## Gates (per unit)
`go build ./...` = 0; `gofmt -l` empty; `go vet`; full `go test ./...`; and (D8, and spot-checks earlier) the real Agentbus v0.6 binary integration smoke. Flake rule: isolated `-count` rerun suffices for ordinary flakes, but any submit/replay/recovery/cleanup/cancel test failure needs root-cause, not dismissal.

## Approval-gated (cannot run autonomously)
- **D0:** merging agentbus PRs + tagging/pushing `v0.6.0` — external writes.
- Any push of this branch / PR creation via `gh` — external write.
- Hidden-file writes (none anticipated; `.github/workflows` if ever added).
- Local edits/commits on this branch are covered by standing approval.
