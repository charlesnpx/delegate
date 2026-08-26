package skills

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestInstallRemovesAndReportsRetiredSkills(t *testing.T) {
	root := t.TempDir()
	env := func(string) string { return "" }
	for _, target := range []string{TargetClaude, TargetCodex} {
		targetRoot, err := TargetRoot(target, root, env, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range expectedRetiredNames(target) {
			path := filepath.Join(targetRoot, name, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("retired"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	installed, err := Install(TargetAll, root, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{TargetClaude, TargetCodex} {
		targetRoot, err := TargetRoot(target, root, env, nil)
		if err != nil {
			t.Fatal(err)
		}
		result := installed[target]
		if len(result.Files) != 1 {
			t.Fatalf("%s installed files = %#v", target, result.Files)
		}
		skillPath := filepath.Join(targetRoot, "delegate", "SKILL.md")
		if result.Files[0].Path != skillPath || result.Files[0].SHA256 == "" {
			t.Fatalf("%s installed file = %#v", target, result.Files[0])
		}
		content, err := os.ReadFile(skillPath)
		if err != nil || len(content) == 0 {
			t.Fatalf("%s installed static skill: content=%q err=%v", target, content, err)
		}

		removed := make(map[string]bool, len(result.Removed))
		for _, removal := range result.Removed {
			removed[filepath.Base(filepath.Dir(removal.Path))] = true
		}
		want := expectedRetiredNames(target)
		if len(result.Removed) != len(want) {
			t.Fatalf("%s removed = %#v", target, result.Removed)
		}
		for _, name := range want {
			if !removed[name] {
				t.Fatalf("%s did not report retired skill %q: %#v", target, name, result.Removed)
			}
			if _, err := os.Stat(filepath.Join(targetRoot, name)); !os.IsNotExist(err) {
				t.Fatalf("%s retired skill %q remains after install: %v", target, name, err)
			}
		}
	}
}

func expectedRetiredNames(target string) []string {
	names := []string{
		"delegate:setup",
		"delegate:status",
		"delegate:result",
		"delegate:cancel",
		"delegate:config",
		"delegate:rescue:claude",
		"delegate:rescue:codex",
		"delegate:rescue:cursor",
		"delegate:review:claude",
		"delegate:review:codex",
		"delegate:review:cursor",
		"delegate:adversarial-review:claude",
		"delegate:adversarial-review:codex",
		"delegate:adversarial-review:cursor",
	}
	switch target {
	case TargetClaude:
		return append(names,
			"codex:rescue",
			"codex:review",
			"codex:adversarial-review",
			"codex:status",
			"codex:result",
			"codex:cancel",
		)
	case TargetCodex:
		return append(names,
			"claude:rescue",
			"claude:review",
			"claude:adversarial-review",
			"claude:status",
			"claude:result",
			"claude:cancel",
		)
	default:
		return names
	}
}
