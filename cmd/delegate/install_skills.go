package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	skillpkg "github.com/charlesnpx/delegate/internal/skills"
)

type installSkillsResult struct {
	Schema    int                           `json:"schema"`
	Name      string                        `json:"name"`
	Version   string                        `json:"version"`
	Operation string                        `json:"operation"`
	Kind      string                        `json:"kind"`
	Targets   map[string]installSkillTarget `json:"targets"`
	Warnings  []string                      `json:"warnings"`
}

type installSkillTarget struct {
	Files   []skillpkg.File    `json:"files"`
	Removed []skillpkg.Removal `json:"removed"`
}

func runInstallSkills(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate install-skills", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plan := fs.Bool("plan", false, "plan skill installation")
	install := fs.Bool("install", false, "install skills")
	uninstall := fs.Bool("uninstall", false, "uninstall skills")
	target := fs.String("target", skillpkg.TargetAll, "target agent: claude, codex, or all")
	jsonOut := fs.Bool("json", false, "emit JSON")
	installRoot := fs.String("install-root", "", "absolute install root")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate install-skills does not accept positional arguments")
	}
	operation := ""
	for name, set := range map[string]bool{"plan": *plan, "install": *install, "uninstall": *uninstall} {
		if !set {
			continue
		}
		if operation != "" {
			return 0, fmt.Errorf("exactly one operation flag is allowed")
		}
		operation = name
	}
	if operation == "" {
		operation = "install"
	}
	if *installRoot != "" && !filepath.IsAbs(*installRoot) {
		return 0, fmt.Errorf("--install-root must be absolute")
	}
	results, err := applySkillOperation(operation, *target, *installRoot)
	if err != nil {
		return 0, err
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, installSkillsJSON(operation, results))
	}
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(stdout, "%s %s %d file(s)\n", operation, name, len(results[name].Files)); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func applySkillOperation(operation, target, installRoot string) (map[string]skillpkg.Result, error) {
	switch operation {
	case "plan":
		return skillpkg.Plan(target, installRoot, nil, nil)
	case "install":
		return skillpkg.Install(target, installRoot, nil, nil)
	case "uninstall":
		return skillpkg.Uninstall(target, installRoot, nil, nil)
	default:
		return nil, fmt.Errorf("unsupported operation %q", operation)
	}
}

func installSkillsJSON(operation string, results map[string]skillpkg.Result) installSkillsResult {
	targets := make(map[string]installSkillTarget, len(results))
	for name, result := range results {
		targets[name] = installSkillTarget{Files: result.Files, Removed: result.Removed}
	}
	return installSkillsResult{
		Schema:    envelopeSchema,
		Name:      "delegate",
		Version:   Version,
		Operation: operation,
		Kind:      "delegate-skills",
		Targets:   targets,
		Warnings:  []string{},
	}
}
