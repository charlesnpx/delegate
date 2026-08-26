package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestProductionImportsOnlyAgentbusClientAndStableEngine(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	roots := []string{filepath.Join(repoRoot, "cmd/delegate"), filepath.Join(repoRoot, "internal")}
	allowed := map[string]struct{}{
		"github.com/charlesnpx/agentbus/client": {},
		"github.com/charlesnpx/agentbus/engine": {},
	}
	actual := map[string]struct{}{}
	var violations []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				if !strings.HasPrefix(importPath, "github.com/charlesnpx/agentbus/") {
					continue
				}
				actual[importPath] = struct{}{}
				if _, ok := allowed[importPath]; !ok {
					displayPath, err := filepath.Rel(repoRoot, path)
					if err != nil {
						displayPath = path
					}
					violations = append(violations, displayPath+": "+importPath)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("production files import unstable agentbus packages:\n%s", strings.Join(violations, "\n"))
	}
	var staleAllowances []string
	for importPath := range allowed {
		if _, ok := actual[importPath]; !ok {
			staleAllowances = append(staleAllowances, importPath)
		}
	}
	if len(staleAllowances) > 0 {
		sort.Strings(staleAllowances)
		t.Fatalf("agentbus import allowlist is broader than production imports:\n%s", strings.Join(staleAllowances, "\n"))
	}
}
