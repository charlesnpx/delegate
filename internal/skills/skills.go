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
	Version = "v0.2.0"

	TargetClaude = "claude"
	TargetCodex  = "codex"
	TargetAll    = "all"

	KindLaunch     = "launch"
	KindReview     = "review"
	KindJobControl = "job-control"
	KindSetup      = "setup"
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

type skillSpec struct {
	Name         string
	Kind         string
	Backend      string
	OtherAgent   string
	HostAgent    string
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
	OtherAgent  string
	HostAgent   string
	Action      string
	Description string
	Command     string
}

var (
	launchTemplate = template.Must(template.New("launch").Parse(strings.TrimSpace(launchSkillTemplate) + "\n"))
	reviewTemplate = template.Must(template.New("review").Parse(strings.TrimSpace(reviewSkillTemplate) + "\n"))
	jobTemplate    = template.Must(template.New("job").Parse(strings.TrimSpace(jobControlSkillTemplate) + "\n"))
	setupTemplate  = template.Must(template.New("setup").Parse(strings.TrimSpace(setupSkillTemplate) + "\n"))
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
	specs := append(claudeSpecs(), codexSpecs()...)
	specs = append(specs, setupSpec(TargetClaude))
	seen := map[string]bool{}
	files := map[string]string{}
	for _, spec := range specs {
		if seen[spec.Name] {
			continue
		}
		seen[spec.Name] = true
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
func Plan(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string][]File, error) {
	return apply(target, installRoot, env, homeDir, "plan")
}

// Install writes generated skills for target.
func Install(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string][]File, error) {
	return apply(target, installRoot, env, homeDir, "install")
}

// Uninstall removes generated skill directories for target.
func Uninstall(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string][]File, error) {
	return apply(target, installRoot, env, homeDir, "uninstall")
}

func apply(target, installRoot string, env func(string) string, homeDir func() (string, error), op string) (map[string][]File, error) {
	targets, err := expandTargets(target)
	if err != nil {
		return nil, err
	}
	result := map[string][]File{}
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
			result[targetName] = append(result[targetName], file)
		}
	}
	return result, nil
}

func targetSpecs(target string) ([]skillSpec, error) {
	switch target {
	case TargetClaude:
		return append(claudeSpecs(), setupSpec(TargetClaude)), nil
	case TargetCodex:
		return append(codexSpecs(), setupSpec(TargetCodex)), nil
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

func claudeSpecs() []skillSpec {
	return crossAgentSpecs(TargetClaude, "codex", "Codex", "Claude Code")
}

func codexSpecs() []skillSpec {
	return crossAgentSpecs(TargetCodex, "claude", "Claude Code", "Codex")
}

func crossAgentSpecs(target, backend, otherAgent, hostAgent string) []skillSpec {
	prefix := backend
	return []skillSpec{
		{
			Name:         prefix + ":rescue",
			Kind:         KindLaunch,
			Backend:      backend,
			OtherAgent:   otherAgent,
			HostAgent:    hostAgent,
			Description:  fmt.Sprintf("Delegate a rescue task from %s to %s through delegate and return the launch envelope verbatim.", hostAgent, otherAgent),
			SourceTarget: target,
		},
		reviewSpec(target, prefix+":review", backend, otherAgent, hostAgent, "review"),
		reviewSpec(target, prefix+":adversarial-review", backend, otherAgent, hostAgent, "adversarial-review"),
		jobSpec(target, prefix+":status", backend, otherAgent, hostAgent, "status", "Check a delegated job status", fmt.Sprintf("delegate status --job \"$JOB_ID\" --json")),
		jobSpec(target, prefix+":result", backend, otherAgent, hostAgent, "result", "Fetch and present a delegated job result", fmt.Sprintf("delegate result --job \"$JOB_ID\" --json")),
		jobSpec(target, prefix+":cancel", backend, otherAgent, hostAgent, "cancel", "Cancel a delegated job after confirming it is stalled", fmt.Sprintf("delegate cancel --job \"$JOB_ID\" --json")),
	}
}

func reviewSpec(target, name, backend, otherAgent, hostAgent, command string) skillSpec {
	label := "code review"
	if command == "adversarial-review" {
		label = "refute-first adversarial code review"
	}
	return skillSpec{
		Name:         name,
		Kind:         KindReview,
		Backend:      backend,
		OtherAgent:   otherAgent,
		HostAgent:    hostAgent,
		Action:       command,
		Description:  fmt.Sprintf("Delegate a %s from %s to %s through sanitized delegate review context and return the launch envelope verbatim.", label, hostAgent, otherAgent),
		Command:      command,
		SourceTarget: target,
	}
}

func jobSpec(target, name, backend, otherAgent, hostAgent, action, description, command string) skillSpec {
	return skillSpec{
		Name:         name,
		Kind:         KindJobControl,
		Backend:      backend,
		OtherAgent:   otherAgent,
		HostAgent:    hostAgent,
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

func render(spec skillSpec) (string, error) {
	data := renderData{
		Version:     Version,
		Name:        spec.Name,
		Kind:        spec.Kind,
		Backend:     spec.Backend,
		OtherAgent:  spec.OtherAgent,
		HostAgent:   spec.HostAgent,
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

Use this when {{.HostAgent}} should delegate a rescue task to {{.OtherAgent}} through "delegate" and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks you to perform the task directly and locally, and delegate is unavailable in this environment, comply locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate task"/agentbus supervision, not an unmanaged background shell or a local substitute answer.
- shared fs: {{.HostAgent}}, "delegate", agentbus, and {{.OtherAgent}} can see the same repo path and delegate state.
- exec: "delegate", "agentbus", and the {{.Backend}} backend executable are runnable.
- repo+state write access: the target repo and delegate/agentbus state roots are writable when the task needs writes.
- stdin handoff: sensitive prompt text can be piped to "delegate handoff create --json".
- backend reachability: "delegate setup --json" shows agentbus capabilities and {{.Backend}} backend availability.

## Launch

1. Create a prompt for the delegated task. Include the acceptance criteria, repo path, current state, constraints, and what the subagent must report back.
2. Pipe that prompt into "delegate handoff create --json" and capture the returned "handoff_path" as "HANDOFF_PATH".
3. Spawn the no-fork delegated job exactly through the CLI:

~~~bash
delegate task --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --handoff-prompt-file "$HANDOFF_PATH" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call.

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over 60 seconds, because progress can land without a command event.

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

Use this when {{.HostAgent}} should delegate a read-only {{if eq .Action "adversarial-review"}}refute-first adversarial {{end}}code review to {{.OtherAgent}} through delegate's sanitized review-context pipeline and return immediately with the launch envelope.

## Preflight

Before launching, check the subagent context and stop with a clear setup error if any item is missing:

Superseding escape hatch: if the requester explicitly asks for a direct local review and delegate is unavailable in this environment, perform the review locally. That explicit request supersedes this skill's delegation trigger; do not refuse merely because delegate cannot run.

- no-fork support: the job must run through "delegate {{.Action}}"/agentbus supervision, not an unmanaged background shell or a local substitute review.
- shared fs: {{.HostAgent}}, "delegate", agentbus, and {{.OtherAgent}} can see the delegate state path. Using the private workspace as backend cwd is not OS isolation; a same-user backend can still read repository or other filesystem files when process permissions allow it.
- exec: "delegate", "agentbus", "git", and the {{.Backend}} backend executable are runnable.
- repo+state access: delegate can read the target Git repository and write its private state root for sanitized review artifacts. Delegate applies path/history redaction and a final content scan to every assembled inline or spilled diff payload.
- cwd: resolve and forward the parent repository path as an absolute, quoted "--cwd" value.
- backend reachability: "delegate setup --json" shows agentbus capabilities and {{.Backend}} backend availability.

Threat model: v0.1 is accident prevention, not a security boundary against an adversarial repository or history. Deliberate history shuffles such as delete-and-recreate sequences intended to evade the heuristics are out of scope. v0.2 OS isolation is the boundary fix for that class.

Do not add "--allow-live-repo-read" unless the user explicitly requests live-repository access after being told that using the repository as backend cwd makes backend file reads easier. It does not change OS filesystem permissions. A container/sandbox profile for OS-level isolation is planned for v0.2.

## Launch

Spawn the no-fork delegated review exactly through the CLI. Add "--base" or "--scope" only when the requested review scope requires it:

~~~bash
delegate {{.Command}} --backend {{.Backend}} --origin {{.Name}} --cwd "$PWD" --background --json
~~~

Return the launch envelope verbatim. Do not wrap it in prose, do not rename fields, and do not omit the "job_id", "status", "result_sha256", or "sha256" fields.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call.

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over 60 seconds, because progress can land without a command event.

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

Run the delegate CLI directly for a {{.OtherAgent}} job. Do not replace the job with a local answer.

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

For result handling, preserve the helper's verdict, summary, findings, and next steps structure. For review-style output, present findings first and keep them ordered by severity. Preserve file paths, line numbers, evidence labels, uncertainty labels, and follow-up questions exactly. If there are no findings, say that explicitly and keep residual risk brief. If the run failed or returned malformed output, include the actionable stderr or envelope fields and stop instead of guessing. After presenting review findings, do not auto-fix; ask the user which issues to address.

## Stall Monitoring

While the delegated job is outstanding, poll "delegate status --job <id>" every 2-5 minutes. Do not wait indefinitely on a single blocking call.

An expired heartbeat lease in "delegate status" is an immediate stall signal. Otherwise, distinguish a long agent turn from a genuine stall before cancelling: run "delegate status --job <id> --probe" before any cancel. The probe automates all three checks and reports per-probe results:

- "ps -p <pid> -o %cpu,etime,stat" sampled twice for child process activity.
- "lsof -p <pid> -iTCP -sTCP:ESTABLISHED" to confirm a live API socket.
- captured log file size watched over 60 seconds, because progress can land without a command event.

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
