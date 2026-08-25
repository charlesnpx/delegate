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
	Version = "v0.9.1"

	TargetClaude = "claude"
	TargetCodex  = "codex"
	TargetAll    = "all"

	KindLaunch     = "launch"
	KindReview     = "review"
	KindJobControl = "job-control"
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

var supportedBackends = []string{"claude", "codex", "cursor"} // Append gemini/grok when agentbus adapters ship.

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
	specs := make([]skillSpec, 0, len(supportedBackends)*3+4)
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
		jobSpec(target, "cancel", "Cancel a delegated job after an explicit operator decision", `delegate cancel --job "$JOB_ID" --json`),
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
- backend reachability: "agentbus setup --json" shows agentbus capabilities and {{.Backend}} backend availability without unrelated backend model catalogues.

"delegate task" is read-only unless it has "--write". The worker sandbox is offline, and a write turn can write only inside the job "--cwd"; use it for repo-local edits/builds/tests and point "GOCACHE" and "GOMODCACHE" under that cwd. Route module downloads, other network work, and Git commits to the caller/orchestrator. The launch and terminal envelope's "backend_profile" reports the effective Agentbus sandbox mode as "read-only" or "workspace-write"; use it to route runtime gates.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted. The "--timeout" flag is optional; omit it or pass "--timeout 0" to leave the deadline to the daemon default, then use the launch envelope's "timeout" field as the authoritative effective value.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id, job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt into "delegate handoff create --json" and capture the returned "handoff_path" as "HANDOFF_PATH".
3. Spawn the no-fork delegated job exactly through the CLI:

~~~bash
delegate task --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --handoff-prompt-file "$HANDOFF_PATH" --background --json
~~~

Each handoff prompt file is single-use: after the task consumes it, create a new handoff file before a relaunch of the same packet.

When the caller has a machine-readable output schema, pass it with "--output-schema-file" instead of placing it in prompt prose. Violations return as "<json-pointer>: <message>", and one corrective retry runs automatically.

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "backend_profile", "timeout", or "result_sha256" fields.

If submission is unresolved after Agentbus accepted or may have accepted the request, preserve the reported "request_id" and run only the exact recovery command "delegate task --recover-request <request_id> --json". Do not create a replacement request unless the user explicitly asks for a new logical task.

Launch with "--background" so the host agent loop stays free. To await the job, start exactly ONE background "delegate result --job <id> --wait --json" task: a background "--wait" is the normal orchestration pattern — it blocks only its own small awaiter process, not a worker slot or the model. A FOREGROUND "--wait" ties up the current host tool call, so use a foreground "--wait" only for a short, explicitly bounded terminal check. Bound long waits with "--wait-timeout <duration>" (on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job). Do NOT write shell polling loops, and never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail, and filesystem salvage is an operator-only emergency after a confirmed CLI defect, not a supported path. Use one-shot "delegate status --job <id> --json" only for on-demand progress (for example when the user asks what the job is doing). Never silently drop the job or substitute your own answer for the delegated run.

## Result Discipline

When the delegated run returns, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes carry the same "timeout" resolution as launch envelopes and may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

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
- exec: "delegate", "agentbus", "git", and the {{.Backend}} backend executable are runnable. Git is used by host-side delegate assembly only; it is not a review-worker preflight or input source.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "agentbus setup --json" shows agentbus capabilities and {{.Backend}} backend availability without unrelated backend model catalogues.

The "-model" and "-effort" flags are optional. User-config defaults apply when those flags are omitted.

Review commands never pass "--write" and intentionally run the backend read-only. A read-only review worker cannot create a build/temp directory, compile, or run tests, so the caller must execute runtime verification and gates. The launch and terminal envelope's "backend_profile" reports the effective Agentbus sandbox mode as "read-only" or "workspace-write"; use it to route runtime gates.

When the parent uses the same harness as the selected backend, this launches a new supervised Agentbus job rather than a native subagent. It has its own request id, job record, contract stamps, and read-only profile.

## Parent Audit Linkage

Delegate records the originating skill plus best-effort parent session identity and depth in its job tags and launch/terminal envelopes. If a harness cannot expose a parent identity through its environment, pass "--parent-client <client>" and "--parent-session <id>"; explicit values override automatic capture.

Threat model: delegate's review context is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions.

## Review Context Discipline

Delegate performs Git collection on the host before the review worker starts and its composed review prompt supplies effective scope and, as applicable, resolved base, resolved base tip commit, merge-base comparison baseline, branch under review, and HEAD commit. The identifiers actually supplied are authoritative: the reviewer must report them as given, including in Scope boundary, rather than treating them as unavailable or inferring missing identifiers or a full commit list. In branch and auto scope, the supplied comparison baseline is the merge base used for the diff; the resolved base tip identifies the base ref. In working-tree scope, the supplied HEAD commit is the comparison baseline; a base tip applies only when supplied.

Reading the assembled context is the first and only required step. Do not instruct the review worker to probe for already-supplied metadata or context, and do not put the expressly unnecessary redundant metadata probe before "review.patch" with "&&". A sandbox denial of that expressly unnecessary probe must not stop the review; it should read the assembled context and complete the review. In live-repository mode, repository reads to validate or self-collect supplemental context remain permitted after that context read; supplied identifiers remain authoritative.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate {{.Command}} --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "backend_profile", "timeout", or "result_sha256" fields.

If submission is unresolved after Agentbus accepted or may have accepted the request, preserve the reported "request_id" and run only the exact recovery command "delegate task --recover-request <request_id> --json". Do not create a replacement request unless the user explicitly asks for a new logical review.

Launch with "--background" so the host agent loop stays free. To await the job, start exactly ONE background "delegate result --job <id> --wait --json" task: a background "--wait" is the normal orchestration pattern — it blocks only its own small awaiter process, not a worker slot or the model. A FOREGROUND "--wait" ties up the current host tool call, so use a foreground "--wait" only for a short, explicitly bounded terminal check. Bound long waits with "--wait-timeout <duration>" (on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job). Do NOT write shell polling loops, and never locate results by scanning the Agentbus state root (for example ~/.local/state/agentbus): that storage layout is private implementation detail, and filesystem salvage is an operator-only emergency after a confirmed CLI defect, not a supported path. Use one-shot "delegate status --job <id> --json" only for on-demand progress (for example when the user asks what the job is doing). Never silently drop the job or substitute your own answer for the delegated review.

## Review Result Discipline

Present findings first and keep them ordered by severity. Preserve the delegated review's file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Distinguish observed evidence from inferred risk and assumptions. Terminal envelopes carry the same "timeout" resolution as launch envelopes and may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say so explicitly and keep residual risk brief. If the run failed or returned malformed output, show the actionable failure and stop instead of guessing or substituting a local review.

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

~~~bash
{{.Command}}
~~~

{{if eq .Action "result" -}}
For an outstanding job, "delegate result --job <id> --wait --json" is the primary command and is normally launched as ONE background task. Optionally add "--wait-timeout <duration>" to bound the wait; on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job. A FOREGROUND "--wait" blocks the current host tool call, so reserve it for a short, explicitly bounded terminal check.

{{end -}}

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. Terminal envelopes may include "cleanup_disposition" and "local_artifacts_retained"; when cleanup is "unresolved", local artifacts were intentionally retained because backend absence is unproven, and a successful result remains successful. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Monitoring

{{if ne .Action "cancel" -}}
Awaiting a job: "delegate result --job <id> --wait --json" is the canonical await-and-fetch primitive — normally launched as ONE background task. "delegate status --job <id> --wait --json" is a terminal barrier when you do not need the body yet; also background it. A FOREGROUND "--wait" blocks the current host tool call, so reserve it for short bounded checks. Bound long waits with "--wait-timeout <duration>"; on expiry the job keeps running and stays retrievable by id; on a timeout, re-arm one background waiter or fetch the terminal result once it is ready — do not abandon the job. Use one-shot "delegate status --job <id> --json" only for on-demand progress.

{{end -}}
Never scan the Agentbus state root to find results — that layout is private implementation detail. Never silently drop the job or substitute your own answer.

## Operating Discipline

Use repo-discipline and stuck-protocol habits: verify paths and writable state before acting, classify denied/transient/ambiguous failures, preserve evidence boundaries, and report scope boundaries.`

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

Delegate user-config defaults apply to all delegated tasks. The supported keys are "overridable", "backend.claude.model", "backend.claude.effort", "backend.codex.model", "backend.codex.effort", "backend.cursor.model", and "backend.cursor.effort". Use "delegate config unset <key>" to remove a value.

Delegate ships managed delegation skills and configurable model/effort defaults for "claude", "codex", and "cursor". Delegate also accepts any other backend that agentbus advertises via "delegate task --backend <name>".

When "overridable=false", configured model and effort values pin their respective dimensions against per-task "-model" and "-effort" flags. This is an ergonomics control, not a security boundary: an agent that can run "delegate config set" can change the setting.

Do not pass policy-bypass flags when using this skill.`
