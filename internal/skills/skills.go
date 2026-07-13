// Package skills generates and installs delegate-managed orchestrator skills.
package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

const (
	// Version is the content version for the generated skill prose.
	Version = "v0.4.2"

	TargetClaude = "claude"
	TargetCodex  = "codex"
	TargetAll    = "all"

	KindLaunch     = "launch"
	KindReview     = "review"
	KindJobControl = "job-control"
	KindSetup      = "setup"
	KindConfig     = "config"
)

// GeneratedSkill is one rendered skill and its source/final naming metadata.
type GeneratedSkill struct {
	Target      string
	Name        string
	EscapedName string
	Kind        string
	Content     string
}

// File is one installed or planned skill file.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

// Removal is one legacy skill file removed or planned for removal.
type Removal struct {
	Path string `json:"path"`
}

// Result is the installed and removed skill files for one target.
type Result struct {
	Files   []File
	Removed []Removal
}

type skillSpec struct {
	Name         string
	Kind         string
	Backend      string
	Action       string
	Description  string
	Command      string
	SourceTarget string
}

type renderData struct {
	Version     string
	Name        string
	Kind        string
	Backend     string
	Action      string
	Description string
	Command     string
}

var supportedBackends = []string{"claude", "codex"} // Append gemini/grok when agentbus adapters ship.

var legacyNamesByTarget = map[string][]string{
	TargetClaude: {
		"codex:rescue",
		"codex:review",
		"codex:adversarial-review",
		"codex:status",
		"codex:result",
		"codex:cancel",
	},
	TargetCodex: {
		"claude:rescue",
		"claude:review",
		"claude:adversarial-review",
		"claude:status",
		"claude:result",
		"claude:cancel",
	},
}

var (
	launchTemplate = template.Must(template.New("launch").Parse(strings.TrimSpace(launchSkillTemplate) + "\n"))
	reviewTemplate = template.Must(template.New("review").Parse(strings.TrimSpace(reviewSkillTemplate) + "\n"))
	jobTemplate    = template.Must(template.New("job").Parse(strings.TrimSpace(jobControlSkillTemplate) + "\n"))
	setupTemplate  = template.Must(template.New("setup").Parse(strings.TrimSpace(setupSkillTemplate) + "\n"))
	configTemplate = template.Must(template.New("config").Parse(strings.TrimSpace(configSkillTemplate) + "\n"))
)

// DecodeName decodes source directory escaping used under skills/.
func DecodeName(name string) string {
	return strings.ReplaceAll(name, "__colon__", ":")
}

// EncodeName encodes a skill name for use as a source directory.
func EncodeName(name string) string {
	return strings.ReplaceAll(name, ":", "__colon__")
}

// TargetNames returns the concrete skill names installed for a target.
func TargetNames(target string) ([]string, error) {
	specs, err := targetSpecs(target)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names, nil
}

// Generate renders the skill matrix for a target.
func Generate(target string) ([]GeneratedSkill, error) {
	specs, err := targetSpecs(target)
	if err != nil {
		return nil, err
	}
	out := make([]GeneratedSkill, 0, len(specs))
	for _, spec := range specs {
		content, err := render(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, GeneratedSkill{
			Target:      spec.SourceTarget,
			Name:        spec.Name,
			EscapedName: EncodeName(spec.Name),
			Kind:        spec.Kind,
			Content:     content,
		})
	}
	return out, nil
}

// SourceFiles returns all source fixture files keyed by escaped source path.
func SourceFiles() (map[string]string, error) {
	specs, err := targetSpecs(TargetClaude)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, spec := range specs {
		content, err := render(spec)
		if err != nil {
			return nil, err
		}
		files[filepath.Join("skills", EncodeName(spec.Name), "SKILL.md")] = content
	}
	return files, nil
}

// TargetRoot resolves the target skill root directory.
func TargetRoot(target, installRoot string, env func(string) string, homeDir func() (string, error)) (string, error) {
	if env == nil {
		env = os.Getenv
	}
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	if target != TargetClaude && target != TargetCodex {
		return "", fmt.Errorf("unsupported skill target %q", target)
	}
	root := installRoot
	if root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("install root must be absolute")
		}
		root = filepath.Clean(root)
		switch target {
		case TargetClaude:
			return filepath.Join(root, ".claude", "skills"), nil
		case TargetCodex:
			if codexHome := env("CODEX_HOME"); codexHome != "" {
				if !filepath.IsAbs(codexHome) {
					return "", errors.New("CODEX_HOME must be absolute")
				}
				codexHome = filepath.Clean(codexHome)
				if isPathInside(root, codexHome) {
					return filepath.Join(codexHome, "skills"), nil
				}
			}
			return filepath.Join(root, ".codex", "skills"), nil
		}
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("home directory is empty")
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("home directory must be absolute")
	}
	home = filepath.Clean(home)
	switch target {
	case TargetClaude:
		return filepath.Join(home, ".claude", "skills"), nil
	case TargetCodex:
		codexHome := env("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		if !filepath.IsAbs(codexHome) {
			return "", errors.New("CODEX_HOME must be absolute")
		}
		return filepath.Join(filepath.Clean(codexHome), "skills"), nil
	default:
		return "", fmt.Errorf("unsupported skill target %q", target)
	}
}

// Plan returns the files that would exist for target.
func Plan(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "plan")
}

// Install writes generated skills for target.
func Install(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "install")
}

// Uninstall removes generated skill directories for target.
func Uninstall(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "uninstall")
}

func apply(target, installRoot string, env func(string) string, homeDir func() (string, error), op string) (map[string]Result, error) {
	targets, err := expandTargets(target)
	if err != nil {
		return nil, err
	}
	result := map[string]Result{}
	for _, targetName := range targets {
		root, err := TargetRoot(targetName, installRoot, env, homeDir)
		if err != nil {
			return nil, err
		}
		generated, err := Generate(targetName)
		if err != nil {
			return nil, err
		}
		for _, skill := range generated {
			dir := filepath.Join(root, DecodeName(skill.EscapedName))
			path := filepath.Join(dir, "SKILL.md")
			file := File{Path: path}
			switch op {
			case "plan":
			case "install":
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(path, []byte(skill.Content), 0o644); err != nil {
					return nil, err
				}
				file.SHA256 = sha256Text(skill.Content)
			case "uninstall":
				if err := os.RemoveAll(dir); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unsupported operation %q", op)
			}
			targetResult := result[targetName]
			targetResult.Files = append(targetResult.Files, file)
			result[targetName] = targetResult
		}
		for _, legacyName := range legacyNamesForTarget(targetName) {
			dir := filepath.Join(root, legacyName)
			removal := Removal{Path: filepath.Join(dir, "SKILL.md")}
			switch op {
			case "plan":
			case "install", "uninstall":
				if err := os.RemoveAll(dir); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unsupported operation %q", op)
			}
			targetResult := result[targetName]
			targetResult.Removed = append(targetResult.Removed, removal)
			result[targetName] = targetResult
		}
	}
	return result, nil
}

func legacyNamesForTarget(target string) []string {
	return append([]string(nil), legacyNamesByTarget[target]...)
}

func targetSpecs(target string) ([]skillSpec, error) {
	switch target {
	case TargetClaude:
		return namespaceSpecs(TargetClaude), nil
	case TargetCodex:
		return namespaceSpecs(TargetCodex), nil
	default:
		return nil, fmt.Errorf("unsupported skill target %q", target)
	}
}

func expandTargets(target string) ([]string, error) {
	switch target {
	case TargetClaude, TargetCodex:
		return []string{target}, nil
	case TargetAll:
		return []string{TargetClaude, TargetCodex}, nil
	default:
		return nil, fmt.Errorf("target must be claude, codex, or all")
	}
}

func namespaceSpecs(target string) []skillSpec {
	specs := make([]skillSpec, 0, len(supportedBackends)*3+5)
	for _, backend := range supportedBackends {
		specs = append(specs, launchSpec(target, backend))
	}
	for _, backend := range supportedBackends {
		specs = append(specs, reviewSpec(target, backend, "review"))
	}
	for _, backend := range supportedBackends {
		specs = append(specs, reviewSpec(target, backend, "adversarial-review"))
	}
	specs = append(specs,
		jobSpec(target, "status", "Check a delegated job status", `delegate status --job "$JOB_ID" --json`),
		jobSpec(target, "result", "Fetch and present a delegated job result", `delegate result --job "$JOB_ID" --json`),
		jobSpec(target, "cancel", "Cancel a delegated job after confirming it is stalled", `delegate cancel --job "$JOB_ID" --json`),
		setupSpec(target),
		configSpec(target),
	)
	return specs
}

func launchSpec(target, backend string) skillSpec {
	return skillSpec{
		Name:         "delegate:rescue:" + backend,
		Kind:         KindLaunch,
		Backend:      backend,
		Description:  fmt.Sprintf("Delegate a rescue task to %s through delegate and return the launch envelope verbatim.", backend),
		SourceTarget: target,
	}
}

func reviewSpec(target, backend, command string) skillSpec {
	label := "code review"
	if command == "adversarial-review" {
		label = "refute-first adversarial code review"
	}
	return skillSpec{
		Name:         "delegate:" + command + ":" + backend,
		Kind:         KindReview,
		Backend:      backend,
		Action:       command,
		Description:  fmt.Sprintf("Delegate a %s to %s through sanitized delegate review context and return the launch envelope verbatim.", label, backend),
		Command:      command,
		SourceTarget: target,
	}
}

func jobSpec(target, action, description, command string) skillSpec {
	return skillSpec{
		Name:         "delegate:" + action,
		Kind:         KindJobControl,
		Action:       action,
		Description:  description + " through delegate.",
		Command:      command,
		SourceTarget: target,
	}
}

func setupSpec(target string) skillSpec {
	return skillSpec{
		Name:         "delegate:setup",
		Kind:         KindSetup,
		Description:  "Verify delegate, agentbus, backend availability, and the current stop-review-gate status.",
		Command:      "delegate setup --json",
		SourceTarget: target,
	}
}

func configSpec(target string) skillSpec {
	return skillSpec{
		Name:         "delegate:config",
		Kind:         KindConfig,
		Description:  "View and change delegate user model and effort defaults.",
		SourceTarget: target,
	}
}

func render(spec skillSpec) (string, error) {
	data := renderData{
		Version:     Version,
		Name:        spec.Name,
		Kind:        spec.Kind,
		Backend:     spec.Backend,
		Action:      spec.Action,
		Description: spec.Description,
		Command:     spec.Command,
	}
	var tmpl *template.Template
	switch spec.Kind {
	case KindLaunch:
		tmpl = launchTemplate
	case KindReview:
		tmpl = reviewTemplate
	case KindJobControl:
		tmpl = jobTemplate
	case KindSetup:
		tmpl = setupTemplate
	case KindConfig:
		tmpl = configTemplate
	default:
		return "", fmt.Errorf("unsupported skill kind %q", spec.Kind)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func isPathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// SortedSourcePaths returns stable source fixture paths.
func SortedSourcePaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

const launchSkillTemplate = `---
name: {{.Name}}
description: {{.Description}}
version: {{.Version}}
---

# {{.Name}}

Use this when an orchestrator should delegate a rescue task to the {{.Backend}} backend through "delegate" and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks you to perform the task directly and locally, and delegate is unavailable in this environment, comply locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate task"/agentbus supervision, not an unmanaged background shell or a local substitute answer.
- shared fs: the parent harness, "delegate", agentbus, and the {{.Backend}} backend can see the same repo path and delegate state.
- exec: "delegate", "agentbus", and the {{.Backend}} backend executable are runnable.
- repo+state write access: the target repo and delegate/agentbus state roots are writable when the task needs writes.
- stdin handoff: sensitive prompt text can be piped to "delegate handoff create --json".
- backend reachability: "delegate setup --json" shows agentbus capabilities and {{.Backend}} backend availability.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

When the parent uses the same harness as the selected backend, this launches a fresh supervised session—not a native subagent. It has its own job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt into "delegate handoff create --json" and capture the returned "handoff_path" as "HANDOFF_PATH".
3. Spawn the no-fork delegated job exactly through the CLI:

~~~bash
delegate task --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --handoff-prompt-file "$HANDOFF_PATH" --background --json
~~~

When the caller has a machine-readable output schema, pass it with "--output-schema-file" instead of embedding it in prompt prose. Violations return as "<json-pointer>: <message>", and one corrective retry runs automatically.

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

Launch with "--background" and keep the host agent loop free to continue useful work. Long "--wait" calls can hold a host tool call for 100+ seconds and block that loop; use "--wait" only for a short, explicitly bounded terminal check. For an outstanding job, poll "delegate status --job <id>" instead.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Plain "delegate status --json --job <id>" is the cheap call; "--probe" blocks for roughly one to three sampling intervals (~10-30s at the default 10s interval, configurable with "--probe-interval").

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over the probe interval, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

Use delegate-report discipline in your own handoff: score criteria, label evidence as observed/inferred/assumed, separate changed from verified, state scope boundaries, and report divergences instead of hiding them.`

const reviewSkillTemplate = `---
name: {{.Name}}
description: {{.Description}}
version: {{.Version}}
---

# {{.Name}}

Use this when an orchestrator should delegate a read-only {{if eq .Action "adversarial-review"}}refute-first adversarial {{end}}code review to the {{.Backend}} backend through delegate's sanitized review-context pipeline and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks for a direct local review and delegate is unavailable in this environment, perform the review locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate {{.Action}}"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: the parent harness, "delegate", agentbus, and the {{.Backend}} backend can see the delegate state path. Using the private workspace as backend cwd is not OS isolation; a same-user backend can still read repository or other filesystem files when process permissions allow it.
- exec: "delegate", "agentbus", "git", and the {{.Backend}} backend executable are runnable.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "delegate setup --json" shows agentbus capabilities and {{.Backend}} backend availability.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

When the parent uses the same harness as the selected backend, this launches a fresh supervised session—not a native subagent. It has its own job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

Threat model: v0.1 is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope. v0.2 OS isolation is the boundary fix for that class.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions. A container/sandbox profile for OS-level isolation is planned for v0.2.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate {{.Command}} --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

Launch with "--background" and keep the host agent loop free to continue useful work. Long "--wait" calls can hold a host tool call for 100+ seconds and block that loop; use "--wait" only for a short, explicitly bounded terminal check. For an outstanding job, poll "delegate status --job <id>" instead.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Plain "delegate status --json --job <id>" is the cheap call; "--probe" blocks for roughly one to three sampling intervals (~10-30s at the default 10s interval, configurable with "--probe-interval").

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over the probe interval, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Review Result Discipline

Present findings first and keep them ordered by severity. Preserve the delegated review's file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Distinguish observed evidence from inferred risk and assumptions. If there are no findings, say so explicitly and keep residual risk brief. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing or substituting a local review.

Never auto-fix after presenting review findings. Ask the user which issues, if any, they want addressed.`

const jobControlSkillTemplate = `---
name: {{.Name}}
description: {{.Description}}
version: {{.Version}}
---

# {{.Name}}

Run the delegate CLI directly for a delegated job. Do not replace the job with a local answer.

## Command

Set "JOB_ID" to the delegated job id, then run:

{{if eq .Action "cancel" -}}
Before running the cancel command, confirm the expired-lease signal or that all three probes are flat:

~~~bash
delegate status --job "$JOB_ID" --probe --json
~~~

If the probe is active or inconclusive, report that state instead of cancelling.

{{end -}}
~~~bash
{{.Command}}
~~~

{{if eq .Action "result" -}}
For a non-terminal job, do not use "delegate result --wait" as the normal host-agent-loop control flow. Long "--wait" calls can hold a host tool call for 100+ seconds and block the host agent loop. Use "--wait" only for a short, explicitly bounded terminal check; otherwise poll "delegate status --job <id>" and fetch the result after the job is terminal.

{{end -}}

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call. Plain "delegate status --json --job <id>" is the cheap call; "--probe" blocks for roughly one to three sampling intervals (~10-30s at the default 10s interval, configurable with "--probe-interval").

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over the probe interval, because progress can land without a command event.

Only if all three probes are flat is the job stalled. On confirmed stall, report the job id and last-known phase, then either "delegate cancel --job <id>" and relaunch fresh or with "--resume-session", or keep waiting. Never silently drop the job, never substitute your own answer for the delegated run, and escalate after a 30-minute patience cap without progress.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.`

const setupSkillTemplate = `---
name: {{.Name}}
description: {{.Description}}
version: {{.Version}}
---

# {{.Name}}

Run:

~~~bash
{{.Command}}
~~~

Use this before launching delegated work. Confirm that "delegate" and "agentbus" are executable, agentbus reports the policy capabilities delegate requires, the intended backend is available, the repo and delegate state are writable when needed, and stdin handoff through "delegate handoff create --json" is viable. Report the "stop-review-gate" line exactly as delegate prints it.

If setup fails, report the failing prerequisite and stop. Do not improvise alternate auth, install, or execution flows.`

const configSkillTemplate = `---
name: {{.Name}}
description: {{.Description}}
version: {{.Version}}
---

# {{.Name}}

View the effective user defaults and config path with:

~~~bash
delegate config list --json
~~~

Change one supported setting with:

~~~bash
delegate config set <key> <value>
~~~

Delegate user-config defaults apply to all delegated tasks. The supported keys are "overridable", "backend.claude.model", "backend.claude.effort", "backend.codex.model", and "backend.codex.effort". Use "delegate config unset <key>" to remove a value.

The supported delegation backends are explicitly "claude" and "codex".

When "overridable=false", configured model and effort values pin their respective dimensions against per-task "-model" and "-effort" flags. This is an ergonomics control, not a security boundary: an agent that can run "delegate config set" can change the setting.

Do not pass policy-bypass flags when using this skill.`
