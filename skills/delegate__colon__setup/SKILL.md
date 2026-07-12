---
name: delegate:setup
description: Verify delegate, agentbus, backend availability, and the current stop-review-gate status.
version: v0.4.0
---

# delegate:setup

Run:

~~~bash
delegate setup --json
~~~

Use this before launching delegated work. Confirm that "delegate" and "agentbus" are executable, agentbus reports the policy capabilities delegate requires, the intended backend is available, the repo and delegate state are writable when needed, and stdin handoff through "delegate handoff create --json" is viable. Report the "stop-review-gate" line exactly as delegate prints it.

If setup fails, report the failing prerequisite and stop. Do not improvise alternate auth, install, or execution flows.
