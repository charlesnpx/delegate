package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillpkg "github.com/charlesnpx/delegate/internal/skills"
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
	if len(files) != 13 {
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

func TestInstallSkillsCodexSandboxWarningsAndLiveGuard(t *testing.T) {
	home := t.TempDir()
	stateHome := filepath.Join(home, "state")
	codexHome := filepath.Join(home, "codex-home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("CODEX_HOME", codexHome)

	var planOut, planErr bytes.Buffer
	if code := run([]string{"install-skills", "--plan", "--target", "codex", "--json"}, nil, &planOut, &planErr); code != 0 {
		t.Fatalf("plan code=%d stderr=%q", code, planErr.String())
	}
	var plan installSkillsResult
	if err := json.Unmarshal(planOut.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "would-configure") {
		t.Fatalf("plan warnings = %#v", plan.Warnings)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("plan wrote Codex config: %v", err)
	}

	var installOut, installErr bytes.Buffer
	if code := run([]string{"install-skills", "--install", "--target", "codex", "--json"}, nil, &installOut, &installErr); code != 0 {
		t.Fatalf("install code=%d stderr=%q", code, installErr.String())
	}
	var installed installSkillsResult
	if err := json.Unmarshal(installOut.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	if len(installed.Warnings) != 1 || !strings.Contains(installed.Warnings[0], codexSandboxConfigured) {
		t.Fatalf("install warnings = %#v", installed.Warnings)
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

	beforeUninstall := append([]byte(nil), raw...)
	var uninstallOut, uninstallErr bytes.Buffer
	if code := run([]string{"install-skills", "--uninstall", "--target", "codex", "--json"}, nil, &uninstallOut, &uninstallErr); code != 0 {
		t.Fatalf("uninstall code=%d stderr=%q", code, uninstallErr.String())
	}
	var uninstalled installSkillsResult
	if err := json.Unmarshal(uninstallOut.Bytes(), &uninstalled); err != nil {
		t.Fatal(err)
	}
	if len(uninstalled.Warnings) != 1 || !strings.Contains(uninstalled.Warnings[0], "left in place") {
		t.Fatalf("uninstall warnings = %#v", uninstalled.Warnings)
	}
	afterUninstall, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeUninstall, afterUninstall) {
		t.Fatal("uninstall modified Codex sandbox configuration")
	}

	stagedRoot := filepath.Join(t.TempDir(), "stage")
	var stagedOut, stagedErr bytes.Buffer
	if code := run([]string{"install-skills", "--install", "--target", "codex", "--install-root", stagedRoot, "--json"}, nil, &stagedOut, &stagedErr); code != 0 {
		t.Fatalf("staged install code=%d stderr=%q", code, stagedErr.String())
	}
	var staged installSkillsResult
	if err := json.Unmarshal(stagedOut.Bytes(), &staged); err != nil {
		t.Fatal(err)
	}
	if len(staged.Warnings) != 0 {
		t.Fatalf("staged warnings = %#v, want none for expected staged install", staged.Warnings)
	}

	t.Setenv("CODEX_HOME", "relative-codex-home")
	unexpected := codexSandboxInstallerWarnings("install", skillpkg.TargetCodex, "")
	if len(unexpected) != 1 || !strings.Contains(unexpected[0], "writable_roots skipped") {
		t.Fatalf("unexpected live-root skip warnings = %#v", unexpected)
	}
}
