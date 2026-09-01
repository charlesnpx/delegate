package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProductionImportsOnlyPinnedAgentbusAndWitnessContracts(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	roots := []string{filepath.Join(repoRoot, "cmd/delegate"), filepath.Join(repoRoot, "internal")}
	allowedAgentbus := map[string]struct{}{
		"github.com/charlesnpx/agentbus/client": {},
		"github.com/charlesnpx/agentbus/engine": {},
	}
	allowedWitness := map[string]struct{}{
		"github.com/charlesnpx/witness/contract/charter": {},
		"github.com/charlesnpx/witness/contract/review":  {},
	}
	var agentbusViolations, witnessViolations []string
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
				displayPath, err := filepath.Rel(repoRoot, path)
				if err != nil {
					displayPath = path
				}
				switch {
				case strings.HasPrefix(importPath, "github.com/charlesnpx/agentbus/"):
					if _, ok := allowedAgentbus[importPath]; !ok {
						agentbusViolations = append(agentbusViolations, displayPath+": "+importPath)
					}
				case strings.HasPrefix(importPath, "github.com/charlesnpx/witness/"):
					if _, ok := allowedWitness[importPath]; !ok {
						witnessViolations = append(witnessViolations, displayPath+": "+importPath)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(agentbusViolations) > 0 {
		t.Fatalf("production files import unstable agentbus packages:\n%s", strings.Join(agentbusViolations, "\n"))
	}
	if len(witnessViolations) > 0 {
		t.Fatalf("production files import unpinned witness contracts:\n%s", strings.Join(witnessViolations, "\n"))
	}
}
