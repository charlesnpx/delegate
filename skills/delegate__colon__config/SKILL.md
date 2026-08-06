---
name: delegate:config
description: View and change delegate user model and effort defaults.
version: v0.7.3
---

# delegate:config

View the effective user defaults and config path with:

~~~bash
delegate config list --json
~~~

Change one supported setting with:

~~~bash
delegate config set <key> <value>
~~~

Delegate user-config defaults apply to all delegated tasks. The supported keys are "overridable", "backend.claude.model", "backend.claude.effort", "backend.codex.model", "backend.codex.effort", "backend.cursor.model", and "backend.cursor.effort". Use "delegate config unset <key>" to remove a value.

The supported delegation backends are explicitly "claude", "codex", and "cursor".

When "overridable=false", configured model and effort values pin their respective dimensions against per-task "-model" and "-effort" flags. This is an ergonomics control, not a security boundary: an agent that can run "delegate config set" can change the setting.

Do not pass policy-bypass flags when using this skill.
