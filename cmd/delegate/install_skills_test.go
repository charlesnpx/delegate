package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkillsIsNotARuntimeCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"install-skills"}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "install-skills"`) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestConfigureCodexSandboxCommand(t *testing.T) {
	home := t.TempDir()
	stateHome := filepath.Join(home, "state")
	codexHome := filepath.Join(home, "codex-home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("CODEX_HOME", codexHome)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"configure-codex-sandbox", "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("configure code=%d stderr=%q", code, stderr.String())
	}
	var configured codexSandboxResult
	if err := json.Unmarshal(stdout.Bytes(), &configured); err != nil {
		t.Fatal(err)
	}
	if configured.Action != codexSandboxConfigured {
		t.Fatalf("configure result=%#v", configured)
	}
	paths, err := resolveCodexSandboxPaths(os.Getenv, os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := decodeCodexSandboxConfig(raw)
	if err != nil || !allWritableRootsPresent(parsed.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots) {
		t.Fatalf("configured roots = %#v, err=%v", parsed.SandboxWorkspaceWrite.WritableRoots, err)
	}

	t.Setenv("CODEX_HOME", "relative-codex-home")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"configure-codex-sandbox", "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("relative CODEX_HOME code=%d stderr=%q", code, stderr.String())
	}
	var skipped codexSandboxResult
	if err := json.Unmarshal(stdout.Bytes(), &skipped); err != nil {
		t.Fatal(err)
	}
	if skipped.Action != codexSandboxSkipped || !strings.Contains(skipped.Warning, "must be absolute") {
		t.Fatalf("relative CODEX_HOME result=%#v", skipped)
	}
}
