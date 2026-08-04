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
- **D6** ✅ `f0ffd59` — schema-2 envelope completion + orphaned first-class (exit 14, no fabricated result, status-fallback parity). review1 SHIP. gate green.
- **D7** ✅ `fa5d6fb` — cancel→status/result + gated cleanup; wait/poll reclassification (no infinite loops); ctx-cancel preserves intent. review1 SHIP (reviewer cross-checked against tagged agentbus v0.6.0 source). gate green.
- **D8** ✅ `fe2aa82` + fix `16b128a` — setup readiness (writable+strict), narrow codex sandbox roots, docs/skills/README rewrite (skill content v0.6.0), acceptance battery + real agentbus-v0.6 binary integration smoke (passes in socket-capable env). review: iter1 FIX (2 High: setup `ready` ignored writable checks; smoke skip-guard masked real failures — both accepted+fixed; +1 Low: over-broad public-surface guard, fixed), iter2 SHIP.

## CAMPAIGN COMPLETE — pushed to PR #17
All eight units D1–D8 implemented, independently gated (build/vet/gofmt/`go test ./...` = 0), and adversarially reviewed to SHIP (D5 and D8 took 2 review iterations; the rest 1). A **whole-tree integration review** returned SHIP (no Critical/High) with 3 Medium + 1 Low cross-unit seam findings, all applied and gate-verified: setup delegate-state readiness (D2↔D8), handoff-payload recovery ownership + truthful `local_artifacts_retained` (D3↔D5/D6), `--no-contract` recovery fidelity (D3↔D6), and a README `status --all` doc fix. The real agentbus v0.6.0 binary integration smoke drives a live identified submit→status→result and passes.

### D9 — drop committed vendoring (post-open, on request)
Removed committed `vendor/` (~199k lines, ~96% `golang.org/x/sys`) in favor of go.mod+go.sum version+hash locking with fetch-on-demand. Installer builds `-mod=readonly` (vendor fallback removed); offline installer tests warm a temp module cache then build `GOPROXY=off`; smoke uses `-modcacherw`. Corrected a stale agentbus `go.sum` hash (a `GONOSUMCHECK`-era value `-mod=vendor` never verified) to the canonical proxy/direct value; `go mod tidy` dropped unused indirect `bbolt`. Verified in a network+socket env: module-mode `go build/vet/test ./...` = 0, real binary smoke + offline installer tests pass with **no vendor present**. review1 SHIP. Branch history rewritten (filter-branch) so `vendor/` never appears in any branch commit.

Pushed to **PR #17** (`delegate-v0.6-protocol-v2-cut` → `main`), force-updated after the vendor-strip rewrite. Net PR diff is source-only (+~6k) plus the deletion of the inherited `main` vendor tree (removed on merge). CI (`.github/workflows/ci.yml`) uses module mode (no `-mod=vendor`), agentbus v0.6.0 is public → resolves from the proxy.

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

- Envelope **schema 2** adds `request_id`, `cleanup_disposition`, `late_finalization`, `agentbus_warnings`, and `local_artifacts_retained`. The `JobStatus`-derived terminal fallback must copy the same fields (today's fallback drops them). Since `JobResult` lacks warnings, terminal rendering fetches/reuses `JobStatus` to capture them (warnings on status; cleanup/finalization on both).
- **`orphaned` first-class terminal:** stop waits; emit terminal envelope; report no result when none exists; preserve cleanup disposition; exit code **14**; retain local artifacts when cleanup unresolved.
- Make `result_unavailable_reason` state-specific, not one generic reason.
- **Tests:** schema-2 fields populated on terminal + status-fallback paths; orphaned → exit 14, no fabricated result, retained; warnings surfaced from status.

## D7 — Cancel + wait/poll behavior
Spec §8. Depends on D2 + D5 + D6.

- **Cancel:** `JobCancelResult` carries only job ID + state (no disposition). After `JobCancel`: fetch `JobStatus` → fetch `JobResult` when terminal → emit terminal/status envelope w/ disposition + warnings → clean local artifacts **only** through the D5 disposition gate (required for `canceled+unresolved`, exit 7).
- **Wait loops:** remove `waitForTurnResult`; all waiting polls identified jobs via `JobResult`/`JobStatus`. Classify poll outcomes (today it effectively retries after every error): retryable (transport loss, synthetic blank-cause `backend_unavailable`); immediate permanent (`unknown_job`, protocol mismatch, root sealed/corrupt/identity-mismatch/fail-stopped); context-cancel (return immediately, preserve pending intent). `delegate result --wait` must not loop forever on unknown job or fail-stopped root.
- **Tests:** post-cancel status/result before cleanup; `canceled+unresolved` retention + exit 7; unknown-job and fail-stop terminate promptly, not infinite poll; ctx-cancel preserves intent.

## D8 — Setup / sandbox + docs / skills + acceptance battery (closure)
Spec §12 (sandbox+setup), file-level docs/skills, full acceptance battery. Depends on all prior.

- **`setup`** reports: resolved Agentbus state root + writable?; resolved autostart-lock root (`<UserCacheDir>/agentbus/start-locks`) + writable?; protocol version; `admission.strictContainment`; pending submission-intent count; unresolved-cleanup artifact count. Missing `admission.strictContainment` fails launch/setup clearly.
- **`codex_sandbox.go`:** add narrow `<UserCacheDir>/agentbus` (not the whole cache); replace hard-coded default state root with the resolved `AGENTBUS_STATE_ROOT` when configured.
- **Docs/skills:** `internal/skills/skills.go` + generated `skills/**` — remove resume/embedded language, add request-recovery + cleanup-disposition rules, bump skill content version; rewrite the managed status/cancel skill prose. `README.md` — rewrite CLI, envelopes, monitoring, recovery, state-root, cleanup docs.
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

---

## D10 — Post-PR-#17 review hardening (accepted subset)
Base SHA: `e1b03246` (branch `delegate-v0.6-protocol-v2-cut`). Risk: medium. Follow-up to an external refute-first review of PR #17. Orchestrator triaged the review; the five items below are accepted (all narrow, real failure-path defects, no new abstractions). Every "High" in the review was downgraded to Medium — none corrupts an execution outcome; they drop artifacts or safety margins on rare paths.

- **F1 — workspace preservation on post-in-flight persist failures.** `submitIntentWithRetry` (submission_intent.go / task.go) moves the intent to `in_flight` before the first `JobSubmit`. After that point the daemon may have accepted/started executing in the review CWD. Today the persist-failure sub-branches (retry-branch L~507-511 and the terminal branch L~530-533) return a *raw* error; `submissionErrorPreservesReviewWorkspace` (review.go:178) only preserves on `submissionUnresolvedError`, so review cleanup can delete a CWD the daemon is executing in. Fix: once a submit has been attempted and the classification is retryable/preserve-intent, every subsequent return (including persist/connect-persist failures) must be a `submissionUnresolvedError` so the workspace is preserved. Do NOT preserve on a genuinely definitive rejection (job never ran).
- **F2 — bounded `JobResult` retry before status-only fallback.** `waitForTerminalJobResult` (jobcontrol.go:385-401): a *retryable* result error + terminal status short-circuits to `terminalJobResultFromStatus`, discarding the result SHA + contract on a transient blip. Fix: on a retryable result error, keep retrying `JobResult` (bounded) before falling back; use the status-only envelope only when the result is definitively unavailable or the terminal state is legitimately resultless (e.g. `orphaned`). Outcome/exit code stay correct on all paths.
- **F3 — cleanup failure = warning + `local_artifacts_retained=true`, never suppress the outcome.** Restores the D5 invariant and fixes a wait/non-wait inconsistency: `--wait` (jobcontrol.go:379) returns the cleanup error and *suppresses the terminal envelope*; non-wait `runResult` (jobcontrol.go:244) *ignores* it and then reports `retained=false` despite a failed delete. Fix: `cleanupJobInput` (jobmeta.go:225) FS-delete failures (`DeleteJobInputOnTerminalState`, `CleanupWorkspace`) become cleanup warnings, artifacts stay recorded, and `local_artifacts_retained` reflects artifacts actually still on disk (not merely `!localCleanupSafe`). The authoritative terminal envelope is always emitted. Apply consistently across every cleanup site (runStatus:82, submittedTerminalJob, terminalJobFromTerminalStatus, wait + non-wait result).
- **F4 — recorded-root-first routing.** `agentbusStateRootForJob` (jobcontrol.go:140-155) resolves the *current* env root first, so an invalid current `AGENTBUS_STATE_ROOT` blocks status/result/cancel of a job whose recorded root is valid. Fix: when a jobID is present, consult `recordedAgentbusStateRootForJob` first; fall back to the current-env root only when no recorded root is found.
- **F5 — fail-closed identity guard on recovery.** `loadSubmissionIntent` (submission_intent.go:112-138) verifies the outer request_id but not that the embedded `Params` identity agrees. Fix: after unmarshal, require `intent.Params` request-id and workspace-key to equal `intent.RequestID`/`intent.WorkspaceKey`; error out (fail closed) on mismatch before any resubmission.

**Deliberately rejected from the review (overengineering / excessive tests / low-value):** a "supported schema version" recovery check (no v2 intent schema exists — YAGNI); rebuilding the agentbus binary smoke into a hermetic checkout (accepted-scope local dev smoke; only fix the PR-body wording that overstates its skip guard); wiring/removing the dead-code `cmd/delegate` `sweepTerminalJobInputs` wrapper (low-priority disk hygiene, deferred); installer `--plan` state-root/lock-root accuracy (low, deferred). The upstream note (agentbus v0.6 authority projection not yet populating warnings/contract/model) is forward-compatible plumbing, not a delegate defect.

- **Gates:** `go build ./...`=0, `gofmt -l` empty, `go vet ./...`, full `go test ./...`; focused regression tests for each of F1-F5. Review: gpt-5.6-sol high, refute-first, SHA-bound, max 4 iterations.

### D10 — COMPLETE (pushed to PR #17)
- **Impl:** gpt-5.5 xhigh worker → all five fixes F1-F5 in `cmd/delegate/*.go` (+8 focused regression tests). Commit `1a36102`.
- **Review round 1** (gpt-5.6-sol high, SHA-bound `9c3b9de..1a36102`): FIX — 3 High, all accepted as reachable defects introduced by the diff:
  - H1: retry-branch reconnect+persist double-failure could return a plain error → review.go deletes a daemon-owned CWD.
  - H2: warning-writer failure (stderr EPIPE) propagated as fatal → re-suppressed the terminal envelope.
  - H3: successful delete + failed metadata-save → stale reload → false `local_artifacts_retained=true`.
- **Fix round 1** (gpt-5.5 xhigh): H1 always-preserve in that branch; H2 warnings best-effort (never fatal); H3 single save-retry after successful cleanup. Commit `456fad8`.
- **Review round 2** (gpt-5.6-sol high, SHA-bound `1a36102..456fad8`, full unit `9c3b9de..456fad8`): **SHIP**, no Critical/High/Medium/worthwhile-Low. Loop closed at iteration 2 of max 4.
- **Gates (orchestrator, network+socket env):** `go build ./...`=0, `gofmt -l cmd/delegate` empty, `go vet ./...`=0, full `go test ./...`=ok. Scope strictly `cmd/delegate/*.go`.
- **Residual (accepted):** H3 persistent metadata-save double-failure leaves stale on-disk paths (false retained=true) — degraded-disk case; terminal outcome always preserved + warned. Deferred lows (unchanged): dead-code `sweepTerminalJobInputs` wrapper; installer `--plan` root accuracy; PR-body smoke-skip wording.
- **Pushed:** commits `9c3b9de` (ledger), `1a36102`, `456fad8`, `21b6f01` (ledger) fast-forwarded to `origin/delegate-v0.6-protocol-v2-cut` (`e1b0324..21b6f01`) under user approval; PR #17 body updated with the D10 section and corrected binary-smoke wording.

---

## D11 — Second-review hardening (result fidelity, fail-stop bound, metadata resilience, schema fail-closed)
Base SHA: `21b6f01` (branch `delegate-v0.6-protocol-v2-cut`). Risk: medium. Follow-up to the round-2 external review of PR #17. Two Highs are extensions of D10 fixes I under-scoped; two Mediums are cheap fail-closed/resilience hardening. Orchestrator triage recorded; the upstream "agentbus should expose a typed startup/fail-stop error" ask is a separate-repo enhancement and is OUT OF SCOPE (noted as an upstream follow-up).

- **H1 — result fidelity on the non-wait paths (completes D10 F2).** F2 only fixed the `--wait` loop. The single-shot paths still replace ANY `JobResult` error with a status-only, resultless envelope: `terminalJobResultFallbackWithStatus` (jobcontrol.go:495, used by `result` no-wait), `runStatus` terminal branch (jobcontrol.go:72-88), cancel's `terminalJobFromTerminalStatus`, and dedup/replay's `submittedTerminalJob` (jobcontrol.go:455-460). A transient blip on a `completed` job then exits 0 as `completed_without_result`, dropping the real hash+contract. Fix: status-only fallback is permitted ONLY for intrinsically resultless terminal states (reuse `terminalStatusDoesNotExpectJobResult` = orphaned); for a normally-resultful terminal, surface the observation error (single-shot → non-zero exit) instead of a false resultless success. Also gate `--wait`'s post-retry-cap fallback on the same predicate.
- **H2 — bound consecutive transport failures so a real fail-stop can't loop `--wait` forever.** `agentbusOperationError` turns every non-RPC/startup error into retryable transport; the wait loops (`waitForTerminalJobResult`, `waitForJobStatus`) have no total transport budget. On a permanent fail-stop where BOTH result and status fail with untyped errors, the loop never terminalizes. Fix: add a consecutive-transport-failure budget (reset on any successful observation; generous enough to survive an ordinary daemon restart) after which the wait returns the transport error (exit 11). Deterministic count-based cap for testability.
- **M1 — corrupt/unreadable local job metadata must not suppress the authoritative outcome.** `terminalEnvelopeFromJobResultWithOptions` hard-fails on a metadata read/parse error (jobcontrol.go:631-634) and `cleanupJobInput` returns it (jobmeta.go:239-243). Local bookkeeping is enrichment, not the source of truth. Fix (narrow): treat a metadata read/parse error like not-found — best-effort warn, proceed with defaults, still emit the authoritative terminal envelope; `cleanupJobInput` warns and returns nil on an unreadable-metadata load error.
- **M2 — fail closed on unsupported submission-intent schema (revises the earlier YAGNI rejection).** The intent file is durable state that outlives the binary version, so `loadSubmissionIntent` must reject an unsupported `schema` rather than silently treating 0/2/any as schema 1 and resubmitting with possibly-misread params. Fix: reject `intent.Schema != submissionIntentSchema` with a clear fail-closed error. ~2 lines + a test.
- **Cleanups:** delete the dead, unreferenced `job_<hex>` generator (`cmd/delegate/jobid.go` — `newJobID`/`randomJobID`, no callers). Fix the now-stale roadmap D10 "local, unpushed" note (done, orchestrator).

**Deliberately deferred (non-blocking debt, unchanged):** wiring/removing the dead-code `sweepTerminalJobInputs` wrapper (latent feature, separate unit); installer `--plan` sandbox-root accuracy (low). **Declined:** upstream agentbus typed startup/fail-stop error (separate repo).

- **Gates:** `go build ./...`=0, `gofmt -l cmd/delegate` empty, `go vet ./...`, full `go test ./...`; focused regression per H1/H2/M1/M2. Review: gpt-5.6-sol high, refute-first, SHA-bound, max 4 iterations.

### D11 — COMPLETE (pushed)
- Impl: gpt-5.5 xhigh worker → H1/H2/M1/M2 + dead-code delete. Commit `cefe172`.
- Orchestrator gate (module env): build/vet/gofmt clean, full `go test ./...`=ok. H1 assumption (JobResult error = observation failure, not result-absence) verified against agentbus v0.6 `authorityResult` (returns successful nil-payload JobResult for resultless terminals; errors only on unknown-job/corruption/fail-stop) → H1 safe for all terminal states, not just orphaned.
- Review round 1 (gpt-5.6-sol high, SHA-bound `32a1300..cefe172`): **SHIP**, no Critical/High/Medium/worthwhile-Low. Loop closed at iteration 1.
- Review round 2 (external refute-first, whole unit `21b6f01..fb22354`): no Critical/High. Confirmed all D11 items closed: result errors no longer fabricate resultless successes (H1), fail-stop polling bounded (H2), intent schema fails closed (M2), dead job-ID generator gone. One Medium remained — malformed job metadata still hard-aborts status/result/cancel before AgentBus is contacted, so the D11 M1 corruption-tolerant cleanup/envelope code never runs through the actual CLI (only its helpers were tested). Accepted → spawned unit D12 below.
- Pushed: `32a1300`, `cefe172`, `fb22354` (ledger) on `origin/delegate-v0.6-protocol-v2-cut`.

---

## D12 — Corruption-tolerant recorded-root resolution for job commands
Base SHA: `fb22354` (branch `delegate-v0.6-protocol-v2-cut`). Risk: low/medium. Source: the accepted Medium from the D11 round-2 external review (above). `status`/`result`/`cancel` resolve the recorded AgentBus state root first (`agentbusStateRootForJob` → `recordedAgentbusStateRootForJob`, jobcontrol.go:146-171), and a metadata read/parse error or a recorded-root canonicalize error propagated as fatal — each command aborted at the top, before ever contacting AgentBus. This defeated D11 M1's corruption-tolerant paths for exactly the corruption they were built to tolerate and contradicted the D5 invariant (a known authoritative outcome must not be suppressed by local metadata uncertainty). Absent metadata and invalid job IDs already fell back correctly and were out of scope.

- **Fix (accepted, narrow):** treat unusable job metadata as "no usable recorded root" — emit a one-line stderr warning and fall back to `resolveAgentbusStateRoot()`; a genuine default-root resolve failure stays fatal. Warning honestly discloses the degraded routing (default root, so the job may resolve differently). Routing-seam tests T1-T4 in `agentbus_state_root_test.go` (corrupt metadata ⇒ fallback+warn; uncanonicalizable recorded root ⇒ fallback+warn; valid recorded root ⇒ used, no warning; absent metadata unchanged). No live-daemon harness — rejected as disproportionate.

### D12 — COMPLETE (local, unpushed)
- **Impl** (gpt-5.5 xhigh worker): warn+fallback in jobcontrol.go, all three call sites; T1-T4 seam tests. Commit `0fcee56`.
- **Review round 1** (gpt-5.6-sol high, SHA-bound `fb22354..0fcee56`): FIX — 1 High accepted: AgentBus job IDs are sequential **per state root** (`job-%020d` from a per-root NextJobSequence), so a corrupt-metadata `cancel` redirected to the default root can cancel an unrelated same-ID job. Warn+fallback is acceptable only for reads.
- **Fix round 1** (gpt-5.5 xhigh): `agentbusStateRootForJob` gains `allowCorruptRootFallback` — `status`/`result` pass true (warn+fallback; warning now discloses the returned status/result may belong to a different same-ID job or be not-found), `cancel` passes false (corrupt/unreadable/uncanonicalizable recorded root is fatal, aborting before connect/JobCancel — restores safe pre-D12 cancel behavior). Absent-metadata/invalid-job-id fallback unchanged for all commands. Strict-path tests added. Commit `5bdf9df`.
- **Review round 2** (gpt-5.6-sol high, SHA-bound `0fcee56..5bdf9df`, whole unit `fb22354..5bdf9df`): **SHIP** — no Critical/High. One Low: the resolver-level test pins `false`⇒fatal, but not `runCancel`'s actual call site (jobcontrol.go:289) — a future flip to `true` would stay green while wrong-root cancellation reopened. Disposition: call-site comment guard documenting the invariant added (commit `3934fc7`); the suggested seam-level `runCancel` command harness (asserting zero connect/JobCancel calls) rejected as disproportionate for a Low and recorded here as the **deferred guard**. Loop closed at iteration 2 of max 4.
- **Gates (orchestrator, at head `3934fc7`):** `go vet ./...`=0, `go test ./cmd/delegate/... -count=1`=ok, `gofmt -l cmd/delegate` empty. Scope strictly `cmd/delegate/jobcontrol.go` + `agentbus_state_root_test.go`.
- **Pushed:** `0fcee56`, `5bdf9df`, `3934fc7` on `origin/delegate-v0.6-protocol-v2-cut` (user-approved push `fb22354..1fb5961`).

---

## D13 — Deferred-debt closure (dead sweep wrapper; installer --plan sandbox-root accuracy)
Base SHA: `3558120` (branch `delegate-v0.6-protocol-v2-cut`). Risk: low. Closes the two items the D10/D11 ledger deferred as non-blocking debt (the remaining backlog after D12).

- **Item A — remove dead `sweepTerminalJobInputs` wrapper.** jobcontrol.go:662 had zero callers (grep-verified at base); deleted along with the now-unused `handoff` import. `internal/handoff.SweepTerminalJobInputs` deliberately retained as a tested latent API. No replacement wiring — a new command would be feature work, out of scope.
- **Item B — installer `--plan` codex sandbox root accuracy.** The shell plan branch reported `{$XDG_STATE_HOME/agentbus, $XDG_STATE_HOME/delegate}`, diverging from what `--install` actually configures via Go `codexSandboxPaths` (codex_sandbox.go): it ignored the `AGENTBUS_STATE_ROOT` override and omitted the agentbus user-cache (autostart-lock) root entirely. Fix: plan now mirrors the configurator's three roots in order — AGENTBUS state root (env override honored, absolute-guarded), agentbus cache root (Darwin `~/Library/Caches/agentbus`, else `${XDG_CACHE_HOME:-~/.cache}/agentbus`), `$XDG_STATE_HOME/delegate`; non-absolute env values route to the skipped warning. One focused test pins the would-configure line under a fully pinned env. No go shell-out for plan (must work without go), no parity harness.

### D13 — COMPLETE (local, unpushed)
- **Impl** (gpt-5.5 xhigh worker): both items, +1 focused plan test. Commit `ba7d8d7`.
- **Review round 1** (gpt-5.6-sol high, refute-first, SHA-bound `3558120..ba7d8d7`): **SHIP** — no Critical/High. Two Lows: (1) `uname` failure silently takes the Linux branch on Darwin (reproduced with broken PATH) — accepted; (2) the new test inherits host PATH while the script invokes `uname` — resolved by elimination via fix (1). Reviewer confirmed Item A safe, normal Darwin/Linux parity in configurator order, and ruled the relative-`XDG_CACHE_HOME`-on-Darwin conservatism acceptable.
- **Fix round 1** (gpt-5.5 xhigh): one hunk — `$(uname -s) == "Darwin"` → `[[ "${OSTYPE:-}" == darwin* ]]` (bash builtin, no subprocess; moots Low 2, so no test change). Commit `97fc933`. Loop closed at iteration 1 of max 4 per the stop rule.
- **Gates (orchestrator):** `gofmt -l cmd/delegate` empty, `go vet ./...`=0, `go test ./cmd/delegate/... ./internal/handoff/... -count=1`=ok, `bash -n install-skill.sh`=0.
- **Deferred debt now closed:** dead sweep wrapper (D10/D11) and installer `--plan` root accuracy (D10). Remaining recorded debt: the D12 deferred guard (runCancel command harness, Low) only.
- **Pushed:** `ba7d8d7`, `97fc933` on `origin/delegate-v0.6-protocol-v2-cut` (user-approved push `fb22354..1fb5961`).
