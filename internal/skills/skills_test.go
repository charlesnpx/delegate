package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTargetMatrices(t *testing.T) {
	for _, target := range []string{TargetClaude, TargetCodex} {
		t.Run(target, func(t *testing.T) {
			got, err := TargetNames(target)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expectedSkillNames()) {
				t.Fatalf("TargetNames(%q) = %#v, want %#v", target, got, expectedSkillNames())
			}
			if len(got) != 10 {
				t.Fatalf("TargetNames(%q) count = %d, want 10", target, len(got))
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
		"delegate:rescue:cursor",
		"delegate:review:claude",
		"delegate:review:codex",
		"delegate:review:cursor",
		"delegate:adversarial-review:claude",
		"delegate:adversarial-review:codex",
		"delegate:adversarial-review:cursor",
		"delegate:config",
	}
}

func TestGeneratedSkillsUseAgentbusForJobControl(t *testing.T) {
	for _, skill := range allGeneratedSkills(t) {
		for _, removed := range []string{"delegate status", "delegate result", "delegate cancel", "launch envelope", "terminal envelope"} {
			if strings.Contains(skill.Content, removed) {
				t.Fatalf("%s contains removed wording %q", skill.Name, removed)
			}
		}
		switch skill.Kind {
		case KindLaunch, KindReview:
			fragments := []string{
				"agentbus status --job <id> --json",
				"agentbus result --job <id> --json",
				"agentbus cancel --job <id> --json",
				"exit code of 2 means the job is still running",
				"plain shell loop",
				"Agentbus state root",
				"silently drop the job",
				"substitute your own answer",
			}
			requireFragments(t, skill, []string{"Return the submit receipt verbatim"})
			for _, fragment := range fragments {
				if !strings.Contains(skill.Content, fragment) {
					t.Fatalf("%s missing %q", skill.Name, fragment)
				}
			}
			if skill.Kind == KindReview {
				action := strings.Split(skill.Name, ":")[1]
				if !strings.Contains(skill.Content, "delegate "+action+" --backend") {
					t.Fatalf("%s missing review command", skill.Name)
				}
			}
		case KindConfig:
			requireFragments(t, skill, []string{"delegate config list --json", "delegate config set <key> <value>"})
		default:
			t.Fatalf("unexpected skill kind %q for %s", skill.Kind, skill.Name)
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
}

func TestSourceFixturesMatchGeneratedTemplates(t *testing.T) {
	files, err := SourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range SortedSourcePaths(files) {
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
			if len(plan[target].Files) != 10 {
				t.Fatalf("plan files = %#v", plan[target].Files)
			}
			if len(plan[target].Removed) != len(legacyNamesForTarget(target)) {
				t.Fatalf("plan removed = %#v", plan[target].Removed)
			}

			installed, err := Install(target, root, func(string) string { return "" }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(installed[target].Files) != 10 {
				t.Fatalf("installed files = %#v", installed[target].Files)
			}
			for _, legacyName := range legacyNamesForTarget(target) {
				if _, err := os.Stat(filepath.Join(rootForTarget, legacyName)); !os.IsNotExist(err) {
					t.Fatalf("legacy skill %q remains after install: %v", legacyName, err)
				}
			}
		})
	}
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

func requireFragments(t *testing.T, skill GeneratedSkill, fragments []string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(skill.Content, fragment) {
			t.Fatalf("%s missing fragment %q", skill.Name, fragment)
		}
	}
}
