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
}
