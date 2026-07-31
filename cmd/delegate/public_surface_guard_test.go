package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestRetiredLaunchVocabularyAbsentFromPublicDocsSkillsAndTests(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	files := []string{filepath.Join(repoRoot, "README.md")}
	for _, root := range []string{filepath.Join(repoRoot, "skills"), filepath.Join(repoRoot, "cmd"), filepath.Join(repoRoot, "internal")} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			switch {
			case strings.HasPrefix(path, filepath.Join(repoRoot, "skills")+string(os.PathSeparator)) && strings.HasSuffix(path, ".md"):
				files = append(files, path)
			case (strings.HasPrefix(path, filepath.Join(repoRoot, "cmd")+string(os.PathSeparator)) || strings.HasPrefix(path, filepath.Join(repoRoot, "internal")+string(os.PathSeparator))) && strings.HasSuffix(path, "_test.go"):
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	join := func(parts ...string) string { return strings.Join(parts, "") }
	flags := []string{
		"--" + join("em", "bedded"),
		"--" + join("fr", "esh"),
		"--" + join("re", "sume"),
		"--" + join("re", "sume") + "-" + join("se", "ssion"),
	}
	phrases := []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			name:    join("re", "sume") + " " + join("se", "ssion"),
			pattern: regexp.MustCompile(`(?i)\b` + join("re", "sume") + `\s+` + join("se", "ssion") + `\b`),
		},
		{
			name:    join("se", "ssion") + " " + join("re", "launch"),
			pattern: regexp.MustCompile(`(?i)\b` + join("se", "ssion") + `\s+` + join("re", "launch") + `\b`),
		},
	}

	var violations []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		for _, flag := range flags {
			if strings.Contains(content, flag) {
				violations = append(violations, relativeTestPath(repoRoot, path)+": flag "+flag)
				break
			}
		}
		for _, phrase := range phrases {
			if match := phrase.pattern.FindString(content); match != "" {
				violations = append(violations, relativeTestPath(repoRoot, path)+": phrase "+phrase.name+" ("+match+")")
				break
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("retired launch public surface found:\n%s", strings.Join(violations, "\n"))
	}
}

func relativeTestPath(repoRoot, path string) string {
	display, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return path
	}
	return display
}
