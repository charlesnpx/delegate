package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestInstallSkillsPlanJSONShowsLegacyRemovals(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"install-skills", "--plan", "--target", "codex", "--install-root", root, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%q", code, stderr.String())
	}
	var result installSkillsResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("plan JSON = %q: %v", stdout.String(), err)
	}
	files := result.Targets["codex"].Files
	if len(files) != 11 {
		t.Fatalf("plan files = %#v", files)
	}
	removed := result.Targets["codex"].Removed
	if len(removed) != 6 {
		t.Fatalf("plan removed = %#v", removed)
	}
	for _, legacy := range []string{"claude:rescue", "claude:review", "claude:adversarial-review", "claude:status", "claude:result", "claude:cancel"} {
		found := false
		for _, removal := range removed {
			if filepath.Base(filepath.Dir(removal.Path)) == legacy {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("plan lacks removal for %q: %#v", legacy, removed)
		}
	}
}
