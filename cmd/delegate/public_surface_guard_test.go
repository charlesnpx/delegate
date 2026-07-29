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
	words := []string{
		join("re", "sume"),
		join("fr", "esh"),
		join("em", "bedded"),
		join("em", "bedding"),
	}
	wordPattern := regexp.MustCompile(`\b(` + strings.Join(words, "|") + `)\b`)
	flags := []string{
		"--" + join("re", "sume"),
		"--" + join("re", "sume") + "-session",
		"--" + join("fr", "esh"),
		"--" + join("em", "bedded"),
	}

	var violations []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		if match := wordPattern.FindString(content); match != "" {
			violations = append(violations, relativeTestPath(repoRoot, path)+": word "+match)
			continue
		}
		for _, flag := range flags {
			if strings.Contains(content, flag) {
				violations = append(violations, relativeTestPath(repoRoot, path)+": flag "+flag)
				break
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("retired launch vocabulary found:\n%s", strings.Join(violations, "\n"))
	}
}

func relativeTestPath(repoRoot, path string) string {
	display, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return path
	}
	return display
}
