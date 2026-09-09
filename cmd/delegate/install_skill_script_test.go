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

func TestDelegatedInstallerLiveCodexInstallInvokesAgentbusSandboxConfiguration(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "agentbus-args")
	agentbusPath := filepath.Join(binDir, "agentbus")
	if err := os.WriteFile(agentbusPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AGENTBUS_ARGS_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateHome := filepath.Join(home, "state")
	delegateState := filepath.Join(stateHome, "delegate")

	runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", home}, []string{
		"HOME=" + home,
		"XDG_STATE_HOME=" + stateHome,
		"AGENTBUS_ARGS_FILE=" + argsFile,
		"PATH=" + binDir + string(os.PathListSeparator) + "/bin:/usr/bin",
	})

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "configure-codex-sandbox\n--writable-root\n" + delegateState + "\n"
	if string(args) != want {
		t.Fatalf("agentbus argv = %q, want %q", args, want)
	}
}

func TestDelegatedInstallerLiveCodexInstallSucceedsWithoutAgentbus(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateHome := filepath.Join(home, "state")
	delegateState := filepath.Join(stateHome, "delegate")
	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", home}, []string{
		"HOME=" + home,
		"XDG_STATE_HOME=" + stateHome,
		"PATH=" + t.TempDir() + string(os.PathListSeparator) + "/bin:/usr/bin",
	})
	if !containsInstallerWarning(installed.Warnings, "agentbus configure-codex-sandbox --writable-root "+delegateState) {
		t.Fatalf("install warnings = %#v, want remediation command for missing agentbus", installed.Warnings)
	}
}

func TestDelegatedInstallerCodexInstallDecodeAndUninstall(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	codexHome := filepath.Join(root, ".codex")
	env := []string{"CODEX_HOME=" + codexHome}
	for _, retired := range expectedRetiredSkillNames("codex") {
		path := filepath.Join(codexHome, "skills", retired, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("retired"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", root}, env)
	if containsInstallerWarning(installed.Warnings, "staged install root") {
		t.Fatalf("staged install warning = %#v, want no staged-root warning", installed.Warnings)
	}
	codexTarget := installed.Targets["codex"]
	if len(codexTarget.Files) != 1 {
		t.Fatalf("codex files = %d, want 1: %#v", len(codexTarget.Files), codexTarget.Files)
	}
	if len(codexTarget.Removed) != len(expectedRetiredSkillNames("codex")) {
		t.Fatalf("codex removed = %d: %#v", len(codexTarget.Removed), codexTarget.Removed)
	}
	var skillPath string
	retiredRemovals := map[string]bool{}
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
		if filepath.Base(filepath.Dir(file.Path)) == "delegate" {
			skillPath = file.Path
		}
	}
	for _, removal := range codexTarget.Removed {
		retiredRemovals[filepath.Base(filepath.Dir(removal.Path))] = true
	}
	if skillPath == "" {
		t.Fatalf("delegate skill missing from %#v", codexTarget.Files)
	}
	for _, retired := range expectedRetiredSkillNames("codex") {
		if !retiredRemovals[retired] {
			t.Fatalf("retired removal %q missing from %#v", retired, codexTarget.Removed)
		}
		if _, err := os.Stat(filepath.Join(codexHome, "skills", retired)); !os.IsNotExist(err) {
			t.Fatalf("retired skill %q remains after install: %v", retired, err)
		}
	}
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty", skillPath)
	}

	uninstalled := runDelegatedInstallerScript(t, []string{"--uninstall", "--target", "codex", "--json", "--install-root", root}, env)
	if len(uninstalled.Targets["codex"].Files) != 1 {
		t.Fatalf("uninstall files = %#v", uninstalled.Targets["codex"].Files)
	}
	if len(uninstalled.Targets["codex"].Removed) != len(expectedRetiredSkillNames("codex")) {
		t.Fatalf("uninstall removed = %#v", uninstalled.Targets["codex"].Removed)
	}
	if _, err := os.Stat(filepath.Dir(skillPath)); !os.IsNotExist(err) {
		t.Fatalf("delegate directory still exists or stat failed unexpectedly: %v", err)
	}
}

func TestDelegatedInstallerStagedCodexInstallIgnoresEscapingCodexHome(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "stage")
	outsideCodexHome := filepath.Join(tmp, "outside-codex")
	escapingCodexHome := root + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(outsideCodexHome)
	sentinel := filepath.Join(outsideCodexHome, "skills", "delegate:setup", "sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed := runDelegatedInstallerScript(t, []string{"--install", "--target", "codex", "--json", "--install-root", root}, []string{"CODEX_HOME=" + escapingCodexHome})
	files := installed.Targets["codex"].Files
	if len(files) != 1 {
		t.Fatalf("codex files = %#v", files)
	}
	wantSkillPath := filepath.Join(root, ".codex", "skills", "delegate", "SKILL.md")
	if files[0].Path != wantSkillPath {
		t.Fatalf("staged codex skill path = %q, want %q", files[0].Path, wantSkillPath)
	}
	if _, err := os.Stat(wantSkillPath); err != nil {
		t.Fatalf("staged codex skill missing at %q: %v", wantSkillPath, err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "sentinel" {
		t.Fatalf("outside sentinel = %q, %v; want untouched sentinel", got, err)
	}
}

func TestDelegatedInstallerPlanShowsLegacyRemovals(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	codexHome := filepath.Join(root, "codex-home")
	result := runDelegatedInstallerScript(t, []string{"--plan", "--target", "codex", "--json", "--install-root", root}, []string{"CODEX_HOME=" + codexHome})
	files := result.Targets["codex"].Files
	if len(files) != 1 {
		t.Fatalf("plan files = %#v", files)
	}
	removed := result.Targets["codex"].Removed
	if len(removed) != len(expectedRetiredSkillNames("codex")) {
		t.Fatalf("plan removed = %#v", removed)
	}
	for _, retired := range expectedRetiredSkillNames("codex") {
		found := false
		for _, removal := range removed {
			if filepath.Base(filepath.Dir(removal.Path)) == retired {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("plan did not identify retired removal %q: %#v", retired, removed)
		}
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

func expectedRetiredSkillNames(target string) []string {
	names := []string{
		"delegate:setup",
		"delegate:status",
		"delegate:result",
		"delegate:cancel",
		"delegate:config",
		"delegate:rescue:claude",
		"delegate:rescue:codex",
		"delegate:rescue:cursor",
		"delegate:review:claude",
		"delegate:review:codex",
		"delegate:review:cursor",
		"delegate:adversarial-review:claude",
		"delegate:adversarial-review:codex",
		"delegate:adversarial-review:cursor",
	}
	if target == "claude" {
		return append(names,
			"codex:rescue",
			"codex:review",
			"codex:adversarial-review",
			"codex:status",
			"codex:result",
			"codex:cancel",
		)
	}
	return append(names,
		"claude:rescue",
		"claude:review",
		"claude:adversarial-review",
		"claude:status",
		"claude:result",
		"claude:cancel",
	)
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
