package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTargetMatrices(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   []string
	}{
		{
			target: TargetClaude,
			want:   expectedSkillNames(),
		},
		{
			target: TargetCodex,
			want:   expectedSkillNames(),
		},
	} {
		t.Run(tc.target, func(t *testing.T) {
			got, err := TargetNames(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TargetNames(%q) = %#v, want %#v", tc.target, got, tc.want)
			}
		})
	}
	claude, err := Generate(TargetClaude)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := Generate(TargetCodex)
	if err != nil {
		t.Fatal(err)
	}
	for i := range claude {
		if claude[i].Name != codex[i].Name || claude[i].Content != codex[i].Content {
			t.Fatalf("host matrices differ at %d: claude=%#v codex=%#v", i, claude[i], codex[i])
		}
	}
}

func expectedSkillNames() []string {
	return []string{
		"delegate:rescue:claude",
		"delegate:rescue:codex",
		"delegate:review:claude",
		"delegate:review:codex",
		"delegate:adversarial-review:claude",
		"delegate:adversarial-review:codex",
		"delegate:status",
		"delegate:result",
		"delegate:cancel",
		"delegate:setup",
		"delegate:config",
	}
}

func TestGeneratedSkillRequirements(t *testing.T) {
	all := allGeneratedSkills(t)
	for _, skill := range all {
		if strings.Contains(skill.Content, "--no-contract") {
			t.Fatalf("%s contains forbidden --no-contract", skill.Name)
		}
		if strings.Contains(strings.ToLower(skill.Content), "opus") || strings.Contains(strings.ToLower(skill.Content), "terra") {
			t.Fatalf("%s hardcodes a model name", skill.Name)
		}
		if DecodeName(skill.EscapedName) != skill.Name {
			t.Fatalf("DecodeName(%q) = %q, want %q", skill.EscapedName, DecodeName(skill.EscapedName), skill.Name)
		}
		if strings.Contains(skill.Name, ":") && !strings.Contains(skill.EscapedName, "__colon__") {
			t.Fatalf("%s escaped as %s, want __colon__", skill.Name, skill.EscapedName)
		}
		switch skill.Kind {
		case KindLaunch:
			requireFragments(t, skill, []string{
				"no-fork support",
				"shared fs",
				"exec:",
				"repo+state write access",
				"stdin handoff",
				"backend reachability",
				"delegate handoff create --json",
				`--cwd "$PWD"`,
				`--handoff-prompt-file "$HANDOFF_PATH"`,
				"Return the launch envelope verbatim",
			})
			requireStallGuidance(t, skill)
		case KindReview:
			requireFragments(t, skill, []string{
				"no-fork support",
				"shared fs",
				"exec:",
				"repo+state access",
				"backend reachability",
				`--cwd "$PWD"`,
				"Return the launch envelope verbatim",
				"findings first",
				"ordered by severity",
				"Preserve the delegated review's file paths, line numbers, evidence labels",
				"Never auto-fix",
				"accident prevention",
				"delete-and-recreate",
				"v0.2 OS isolation is the boundary fix",
			})
			action := strings.Split(skill.Name, ":")[1]
			if !strings.Contains(skill.Content, "delegate "+action+" --backend") {
				t.Fatalf("%s missing review command", skill.Name)
			}
			requireStallGuidance(t, skill)
		case KindJobControl:
			requireFragments(t, skill, []string{
				"Run the delegate CLI directly",
				"preserve the helper's verdict",
				"findings first",
				"ordered by severity",
				"Preserve file paths, line numbers, evidence labels",
				"do not auto-fix",
				"Do not replace the job with a local answer",
			})
			requireStallGuidance(t, skill)
		case KindSetup:
			requireFragments(t, skill, []string{
				"delegate setup --json",
				"agentbus reports the policy capabilities",
				"stop-review-gate",
			})
		case KindConfig:
			requireFragments(t, skill, []string{
				"delegate config list --json",
				"delegate config set <key> <value>",
				"all delegated tasks",
				"ergonomics control, not a security boundary",
				"supported delegation backends are explicitly \"claude\" and \"codex\"",
			})
		default:
			t.Fatalf("%s kind = %q", skill.Name, skill.Kind)
		}
	}
}

func TestTargetRootResolution(t *testing.T) {
	home := func() (string, error) { return "/home/delegate-user", nil }
	emptyEnv := func(string) string { return "" }
	root, err := TargetRoot(TargetClaude, "", emptyEnv, home)
	if err != nil {
		t.Fatal(err)
	}
	if root != "/home/delegate-user/.claude/skills" {
		t.Fatalf("claude root = %q", root)
	}
	root, err = TargetRoot(TargetCodex, "", func(key string) string {
		if key == "CODEX_HOME" {
			return "/opt/codex-home"
		}
		return ""
	}, home)
	if err != nil {
		t.Fatal(err)
	}
	if root != "/opt/codex-home/skills" {
		t.Fatalf("codex CODEX_HOME root = %q", root)
	}
	root, err = TargetRoot(TargetCodex, "/stage", func(key string) string {
		if key == "CODEX_HOME" {
			return "/stage/codex-home"
		}
		return ""
	}, home)
	if err != nil {
		t.Fatal(err)
	}
	if root != "/stage/codex-home/skills" {
		t.Fatalf("staged codex root = %q", root)
	}
}

func TestSourceFixturesMatchGeneratedTemplates(t *testing.T) {
	files, err := SourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 11 {
		t.Fatalf("SourceFiles count = %d, want 11", len(files))
	}
	for _, rel := range SortedSourcePaths(files) {
		if !strings.Contains(filepath.Dir(rel), "__colon__") {
			t.Fatalf("source fixture path %q does not use __colon__ escaping", rel)
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(raw) != files[rel] {
			t.Fatalf("source fixture %s drifted from generator", rel)
		}
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(filepath.Join("..", ".."), path)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, rel)
	}
	if !reflect.DeepEqual(got, SortedSourcePaths(files)) {
		t.Fatalf("skill source paths = %#v, want %#v", got, SortedSourcePaths(files))
	}
}

func TestColonEscapingRoundTripsMultiColonNames(t *testing.T) {
	const name = "delegate:adversarial-review:codex"
	if got := EncodeName(name); got != "delegate__colon__adversarial-review__colon__codex" {
		t.Fatalf("EncodeName(%q) = %q", name, got)
	}
	if got := DecodeName(EncodeName(name)); got != name {
		t.Fatalf("DecodeName(EncodeName(%q)) = %q", name, got)
	}
}

func TestPlanAndInstallRemoveLegacySkills(t *testing.T) {
	for _, target := range []string{TargetClaude, TargetCodex} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			rootForTarget, err := TargetRoot(target, root, func(string) string { return "" }, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, legacyName := range legacyNamesForTarget(target) {
				path := filepath.Join(rootForTarget, legacyName, "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			plan, err := Plan(target, root, func(string) string { return "" }, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := plan[target]
			if len(result.Files) != len(expectedSkillNames()) {
				t.Fatalf("plan files = %#v", result.Files)
			}
			if len(result.Removed) != len(legacyNamesForTarget(target)) {
				t.Fatalf("plan removed = %#v", result.Removed)
			}
			for _, legacyName := range legacyNamesForTarget(target) {
				path := filepath.Join(rootForTarget, legacyName, "SKILL.md")
				if !containsPlannedRemoval(result.Removed, path) {
					t.Fatalf("plan did not remove legacy skill %q: %#v", legacyName, result.Removed)
				}
			}

			installed, err := Install(target, root, func(string) string { return "" }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(installed[target].Files) != len(expectedSkillNames()) {
				t.Fatalf("installed files = %#v", installed[target].Files)
			}
			if len(installed[target].Removed) != len(legacyNamesForTarget(target)) {
				t.Fatalf("installed removed = %#v", installed[target].Removed)
			}
			for _, file := range installed[target].Files {
				if file.SHA256 == "" {
					t.Fatalf("installed file %q missing sha256", file.Path)
				}
			}
			for _, legacyName := range legacyNamesForTarget(target) {
				if _, err := os.Stat(filepath.Join(rootForTarget, legacyName)); !os.IsNotExist(err) {
					t.Fatalf("legacy skill %q remains after install: %v", legacyName, err)
				}
			}

			uninstalled, err := Uninstall(target, root, func(string) string { return "" }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(uninstalled[target].Files) != len(expectedSkillNames()) {
				t.Fatalf("uninstalled files = %#v", uninstalled[target].Files)
			}
			if len(uninstalled[target].Removed) != len(legacyNamesForTarget(target)) {
				t.Fatalf("uninstalled removed = %#v", uninstalled[target].Removed)
			}
		})
	}
}

func containsPlannedRemoval(removals []Removal, path string) bool {
	for _, removal := range removals {
		if removal.Path == path {
			return true
		}
	}
	return false
}

func allGeneratedSkills(t *testing.T) []GeneratedSkill {
	t.Helper()
	var all []GeneratedSkill
	for _, target := range []string{TargetClaude, TargetCodex} {
		generated, err := Generate(target)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, generated...)
	}
	return all
}

func TestReviewAndRescueSkillsKeepEscapeHatchAndStallDiscipline(t *testing.T) {
	for _, skill := range allGeneratedSkills(t) {
		if skill.Kind != KindReview && !strings.HasSuffix(skill.Name, ":rescue") {
			continue
		}
		requireFragments(t, skill, []string{"Superseding escape hatch", "explicitly asks", "delegate is unavailable", "supersedes this skill's delegation trigger"})
		requireStallGuidance(t, skill)
		if strings.Contains(skill.Content, "--no-contract") {
			t.Fatalf("%s contains forbidden --no-contract", skill.Name)
		}
	}
}

func requireFragments(t *testing.T, skill GeneratedSkill, fragments []string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(skill.Content, fragment) {
			t.Fatalf("%s missing fragment %q", skill.Name, fragment)
		}
	}
}

func requireStallGuidance(t *testing.T, skill GeneratedSkill) {
	t.Helper()
	requireFragments(t, skill, []string{
		"delegate status --job <id>",
		"every 2-5 minutes",
		"expired heartbeat lease",
		"immediate stall signal",
		"delegate status --job <id> --probe",
		"before any cancel",
		"ps -p <pid> -o %cpu,etime,stat",
		"lsof -p <pid> -iTCP -sTCP:ESTABLISHED",
		"log file size watched over 60 seconds",
		"Only if all three probes are flat",
		"delegate cancel --job <id>",
		"--resume-session",
		"Never silently drop the job",
		"never substitute your own answer",
		"30-minute patience cap",
	})
}
