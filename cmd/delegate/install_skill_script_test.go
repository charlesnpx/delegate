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
	Files   []delegatedInstallerFile    `json:"files"`
	Removed []delegatedInstallerRemoval `json:"removed"`
}

type delegatedInstallerFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

type delegatedInstallerRemoval struct {
	Path string `json:"path"`
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
	for _, legacy := range []string{"claude:rescue", "claude:review", "claude:adversarial-review", "claude:status", "claude:result", "claude:cancel"} {
		path := filepath.Join(codexHome, "skills", legacy, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", root}, env)
	codexTarget := installed.Targets["codex"]
	if len(codexTarget.Files) != 11 {
		t.Fatalf("codex files = %d, want 11: %#v", len(codexTarget.Files), codexTarget.Files)
	}
	if len(codexTarget.Removed) != 6 {
		t.Fatalf("codex removed = %d, want 6: %#v", len(codexTarget.Removed), codexTarget.Removed)
	}
	var rescuePath string
	installedNames := map[string]bool{}
	legacyRemovals := map[string]bool{}
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
		if strings.Contains(file.Path, "delegate:rescue:claude") {
			rescuePath = file.Path
		}
		installedNames[filepath.Base(filepath.Dir(file.Path))] = true
	}
	for _, removal := range codexTarget.Removed {
		legacyRemovals[filepath.Base(filepath.Dir(removal.Path))] = true
	}
	if rescuePath == "" {
		t.Fatalf("delegate:rescue:claude file missing from %#v", codexTarget.Files)
	}
	for _, name := range []string{"delegate:review:claude", "delegate:adversarial-review:claude", "delegate:rescue:codex"} {
		if !installedNames[name] {
			t.Fatalf("%s file missing from %#v", name, codexTarget.Files)
		}
	}
	for _, legacy := range []string{"claude:rescue", "claude:review", "claude:adversarial-review", "claude:status", "claude:result", "claude:cancel"} {
		if !legacyRemovals[legacy] {
			t.Fatalf("legacy removal %q missing from %#v", legacy, codexTarget.Files)
		}
		if _, err := os.Stat(filepath.Join(codexHome, "skills", legacy)); !os.IsNotExist(err) {
			t.Fatalf("legacy skill %q remains after install: %v", legacy, err)
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
	if len(uninstalled.Targets["codex"].Files) != 11 {
		t.Fatalf("uninstall files = %#v", uninstalled.Targets["codex"].Files)
	}
	if len(uninstalled.Targets["codex"].Removed) != 6 {
		t.Fatalf("uninstall removed = %#v", uninstalled.Targets["codex"].Removed)
	}
	if _, err := os.Stat(filepath.Dir(rescuePath)); !os.IsNotExist(err) {
		t.Fatalf("rescue directory still exists or stat failed unexpectedly: %v", err)
	}
}

func TestDelegatedInstallerPlanShowsLegacyRemovals(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	codexHome := filepath.Join(root, "codex-home")
	result := runDelegatedInstallerScript(t, []string{"--plan", "--target", "codex", "--json", "--install-root", root}, []string{"CODEX_HOME=" + codexHome})
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
			t.Fatalf("plan did not identify legacy removal %q: %#v", legacy, removed)
		}
	}
}

func TestDelegatedInstallerLiveCodexInstallConfiguresSandbox(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(home, "codex-home")
	stateHome := filepath.Join(home, "state")
	gocache := privateTmpDir(t, "delegate-gocache-*")
	gomodcache := privateTmpDir(t, "delegate-gomodcache-*")
	warmDelegateModuleCache(t, gomodcache, gocache)
	env := []string{
		"HOME=" + home,
		"CODEX_HOME=" + codexHome,
		"XDG_STATE_HOME=" + stateHome,
		"GOCACHE=" + gocache,
		"GOMODCACHE=" + gomodcache,
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS=-modcacherw",
	}

	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json"}, env)
	if !containsInstallerWarning(installed.Warnings, "codex sandbox writable_roots configured") {
		t.Fatalf("install warnings = %#v, want configured sandbox", installed.Warnings)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := decodeCodexSandboxConfig(raw)
	if err != nil {
		t.Fatalf("configured config does not parse: %v\n%s", err, raw)
	}
	agentbusRoot, err := canonicalizeAgentbusStateRoot("test agentbus root", filepath.Join(stateHome, "agentbus"))
	if err != nil {
		t.Fatal(err)
	}
	wantRoots := []string{agentbusRoot, filepath.Join(stateHome, "delegate")}
	if !allWritableRootsPresent(parsed.SandboxWorkspaceWrite.WritableRoots, wantRoots) {
		t.Fatalf("writable roots = %#v, want %#v", parsed.SandboxWorkspaceWrite.WritableRoots, wantRoots)
	}

	beforeUninstall := append([]byte(nil), raw...)
	uninstalled := runDelegatedInstallerScript(t, []string{"--uninstall", "--target", "codex", "--json"}, env)
	if !containsInstallerWarning(uninstalled.Warnings, "entries left in place") {
		t.Fatalf("uninstall warnings = %#v, want leave-in-place note", uninstalled.Warnings)
	}
	afterUninstall, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterUninstall) != string(beforeUninstall) {
		t.Fatal("uninstall changed Codex sandbox configuration")
	}
}

func containsInstallerWarning(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}

func TestDelegatedInstallerToolsInstallBuildsDelegate(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	gocache := privateTmpDir(t, "delegate-gocache-*")
	gomodcache := privateTmpDir(t, "delegate-gomodcache-*")
	warmDelegateModuleCache(t, gomodcache, gocache)
	env := []string{
		"GOCACHE=" + gocache,
		"GOMODCACHE=" + gomodcache,
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS=-modcacherw",
	}

	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "tools", "--json", "--install-root", root}, env)
	versionBytes, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	expectedVersion := strings.TrimSpace(string(versionBytes))
	if installed.Version != expectedVersion {
		t.Fatalf("installer version = %q, want %s", installed.Version, expectedVersion)
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
	if got, want := string(out), "delegate "+expectedVersion+"\n"; got != want {
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

func warmDelegateModuleCache(t *testing.T, gomodcache, gocache string) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "mod", "download", "all")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GOCACHE="+gocache,
		"GOMODCACHE="+gomodcache,
		"GOFLAGS=-modcacherw",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	msg := strings.TrimSpace(string(out))
	if moduleCacheWarmupEnvironmentBlocked(msg) {
		t.Skipf("warm module cache for offline installer test: %v: %s", err, msg)
	}
	t.Fatalf("warm module cache for offline installer test: %v\n%s", err, msg)
}

func moduleCacheWarmupEnvironmentBlocked(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"goproxy=off",
		"module lookup disabled",
		"operation not permitted",
		"network is unreachable",
		"no route to host",
		"no such host",
		"could not resolve host",
		"temporary failure in name resolution",
		"i/o timeout",
		"tls handshake timeout",
		"connection refused",
		"connection reset",
		"proxyconnect tcp",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
