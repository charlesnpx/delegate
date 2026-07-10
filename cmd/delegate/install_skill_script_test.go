package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type delegatedInstallerResult struct {
	Schema    int                                 `json:"schema"`
	Name      string                              `json:"name"`
	Version   string                              `json:"version"`
	Operation string                              `json:"operation"`
	Kind      string                              `json:"kind"`
	Setup     []delegatedInstallerSetup           `json:"setup"`
	Targets   map[string]delegatedInstallerTarget `json:"targets"`
	Warnings  []string                            `json:"warnings"`
}

type delegatedInstallerSetup struct {
	Kind        string `json:"kind"`
	Executable  string `json:"executable"`
	Remediation string `json:"remediation"`
}

type delegatedInstallerTarget struct {
	Files []delegatedInstallerFile `json:"files"`
}

type delegatedInstallerFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

func TestDelegatedInstallerPlanReportsSetupWhenToolsMissing(t *testing.T) {
	result := runDelegatedInstallerScript(t, []string{"--plan", "--target", "all", "--json"}, []string{"PATH=" + t.TempDir()})
	if result.Schema != 1 || result.Name != "delegate" || result.Operation != "plan" || result.Kind != "delegated" {
		t.Fatalf("unexpected delegated result: %#v", result)
	}
	for _, target := range []string{"tools", "claude", "codex"} {
		if _, ok := result.Targets[target]; !ok {
			t.Fatalf("target %q missing from %#v", target, result.Targets)
		}
	}
	setup := map[string]bool{}
	for _, requirement := range result.Setup {
		if requirement.Kind == "executable" {
			setup[requirement.Executable] = true
		}
	}
	if !setup["go"] || !setup["agentbus"] {
		t.Fatalf("setup = %#v, want go and agentbus executable requirements", result.Setup)
	}
}

func TestDelegatedInstallerCodexInstallDecodeAndUninstall(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	codexHome := filepath.Join(root, "codex-home")
	env := []string{"CODEX_HOME=" + codexHome}
	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", root}, env)
	codexTarget := installed.Targets["codex"]
	if len(codexTarget.Files) != 7 {
		t.Fatalf("codex files = %d, want 7: %#v", len(codexTarget.Files), codexTarget.Files)
	}
	var rescuePath string
	installedNames := map[string]bool{}
	for _, file := range codexTarget.Files {
		if !strings.HasPrefix(file.Path, root+string(os.PathSeparator)) {
			t.Fatalf("installed path %q escapes install root %q", file.Path, root)
		}
		if strings.Contains(file.Path, "__colon__") {
			t.Fatalf("installed path %q was not colon-decoded", file.Path)
		}
		if file.SHA256 == "" {
			t.Fatalf("installed file %q missing sha256", file.Path)
		}
		if strings.Contains(file.Path, "claude:rescue") {
			rescuePath = file.Path
		}
		installedNames[filepath.Base(filepath.Dir(file.Path))] = true
	}
	if rescuePath == "" {
		t.Fatalf("claude:rescue file missing from %#v", codexTarget.Files)
	}
	for _, name := range []string{"claude:review", "claude:adversarial-review"} {
		if !installedNames[name] {
			t.Fatalf("%s file missing from %#v", name, codexTarget.Files)
		}
	}
	raw, err := os.ReadFile(rescuePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "--no-contract") {
		t.Fatalf("%s contains forbidden --no-contract", rescuePath)
	}

	uninstalled := runDelegatedInstallerScript(t, []string{"--uninstall", "--target", "codex", "--json", "--install-root", root}, env)
	if len(uninstalled.Targets["codex"].Files) != 7 {
		t.Fatalf("uninstall files = %#v", uninstalled.Targets["codex"].Files)
	}
	if _, err := os.Stat(filepath.Dir(rescuePath)); !os.IsNotExist(err) {
		t.Fatalf("rescue directory still exists or stat failed unexpectedly: %v", err)
	}
}

func TestDelegatedInstallerToolsInstallBuildsDelegate(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	gocache := privateTmpDir(t, "delegate-gocache-*")
	env := []string{
		"GOCACHE=" + gocache,
		"GOPROXY=off",
		"GOSUMDB=off",
	}

	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "tools", "--json", "--install-root", root}, env)
	if installed.Version != "0.2.0" {
		t.Fatalf("installer version = %q, want 0.2.0", installed.Version)
	}
	toolsTarget := installed.Targets["tools"]
	if len(toolsTarget.Files) != 1 {
		t.Fatalf("tools files = %d, want 1: %#v", len(toolsTarget.Files), toolsTarget.Files)
	}
	toolFile := toolsTarget.Files[0]
	wantPath := filepath.Join(root, ".local", "bin", "delegate")
	if toolFile.Path != wantPath {
		t.Fatalf("tool path = %q, want %q", toolFile.Path, wantPath)
	}
	if _, err := hex.DecodeString(toolFile.SHA256); err != nil || len(toolFile.SHA256) != 64 {
		t.Fatalf("tool sha256 = %q, want 64 hex chars (decode err %v)", toolFile.SHA256, err)
	}
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != toolFile.SHA256 {
		t.Fatalf("reported sha256 = %q, computed %q", toolFile.SHA256, got)
	}
	cmd := exec.Command(wantPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("staged delegate version failed: %v\n%s", err, out)
	}
	if got, want := string(out), "delegate 0.2.0\n"; got != want {
		t.Fatalf("staged delegate version = %q, want %q", got, want)
	}
}

func runDelegatedInstallerScript(t *testing.T, args, extraEnv []string) delegatedInstallerResult {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "install-skill.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("installer failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, exit.Stderr)
		}
		t.Fatal(err)
	}
	var result delegatedInstallerResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("installer output is not JSON: %v\n%s", err, out)
	}
	return result
}

func privateTmpDir(t *testing.T, pattern string) string {
	t.Helper()
	base := "/private/tmp"
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
