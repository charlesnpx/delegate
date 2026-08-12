---
name: delegate:setup
description: Verify delegate, agentbus, and backend availability.
version: v0.8.2
---

# delegate:setup

Run:

~~~bash
delegate setup --json
~~~

Use this before launching delegated work. Confirm that "delegate" and "agentbus" are executable, agentbus reports "admission.strictContainment" plus the policy capabilities delegate requires, the intended backend is available, the repo and delegate state are writable when needed, and stdin handoff through "delegate handoff create --json" is viable.

Report these setup fields when they are relevant: "agentbusStateRoot", "agentbusStateRootWritable", "agentbusAutostartLockRoot", "agentbusAutostartLockRootWritable", "pendingSubmissionIntentCount", "pendingSubmissionIntents", "unresolvedCleanupArtifactCount", "admissionStrictContainment", and "ready". "pendingSubmissionIntents" contains up to 20 oldest pending intents (oldest first); its request IDs can go directly to "delegate task --recover-request <request_id> --json". "pendingSubmissionIntentCount" remains the authoritative total when that array is capped. A nonzero "unresolvedCleanupArtifactCount" means Delegate retained local artifacts because Agentbus did not prove backend absence.

If setup fails, report the failing prerequisite and stop. Do not improvise alternate auth, install, or execution flows.
