package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateSourceFixtures(t *testing.T) {
	if os.Getenv("UPDATE_SKILL_FIXTURES") != "1" {
		t.Skip("set UPDATE_SKILL_FIXTURES=1 to refresh generated source fixtures")
	}
	files, err := SourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		path := filepath.Join("..", "..", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{TargetClaude, TargetCodex} {
		for _, name := range legacyNamesForTarget(target) {
			path := filepath.Join("..", "..", "skills", EncodeName(name), "SKILL.md")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"delegate:status", "delegate:result", "delegate:cancel", "delegate:config"} {
		if err := os.RemoveAll(filepath.Join("..", "..", "skills", EncodeName(name))); err != nil {
			t.Fatal(err)
		}
	}
}
