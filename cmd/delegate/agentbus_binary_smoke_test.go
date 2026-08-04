//go:build integration

package main

// Run with: go test -tags=integration ./cmd/delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
)

func TestAgentbusPinnedBinaryIntegrationSmoke(t *testing.T) {
	agentbusPath, source := agentbusPinnedSmokeBinary(t)
	base, err := os.MkdirTemp("/tmp", "delegate-agentbus-smoke-")
	if err != nil {
		t.Fatalf("create short smoke temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	binDir := filepath.Join(base, "bin")
	stateRoot := filepath.Join(base, "agentbus")
	delegateState := filepath.Join(base, "state")
	codexHome := filepath.Join(base, "codex-home")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{binDir, stateRoot, delegateState, codexHome, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSmokeCodexCLI(t, filepath.Join(binDir, "codex"))
	writeSmokeCodexModelsCache(t, codexHome)
	smokePath := binDir + string(os.PathListSeparator) + os.Getenv("PATH")

	serveCtx, stopServe := context.WithCancel(context.Background())
	defer stopServe()
	serve := exec.CommandContext(serveCtx, agentbusPath, "serve", "--foreground")
	serve.Env = append(os.Environ(),
		"AGENTBUS_STATE_ROOT="+stateRoot,
		"XDG_STATE_HOME="+delegateState,
		"CODEX_HOME="+codexHome,
		"PATH="+smokePath,
	)
	var serveStdout, serveStderr bytes.Buffer
	serve.Stdout = &serveStdout
	serve.Stderr = &serveStderr
	if err := serve.Start(); err != nil {
		t.Fatalf("start agentbus %s from %s: %v", agentbusPath, source, err)
	}
	serveDone := make(chan error, 1)
	serveExited := make(chan struct{})
	go func() {
		serveDone <- serve.Wait()
		close(serveExited)
	}()
	t.Cleanup(func() {
		stopServe()
		select {
		case <-serveExited:
		case <-time.After(5 * time.Second):
			if serve.Process != nil {
				_ = serve.Process.Kill()
			}
			<-serveExited
		}
	})
	waitForSmokeAgentbusReady(t, stateRoot, serveDone, &serveStdout, &serveStderr)

	restore := useRealAgentbusBinaryForDelegate(t, agentbusPath)
	defer restore()
	t.Setenv("AGENTBUS_STATE_ROOT", stateRoot)
	t.Setenv("XDG_STATE_HOME", delegateState)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", smokePath)

	// Prove the capability-negotiation path that regressed: the compat fix is about
	// delegate NOT rejecting the normal managed path at connect/Hello time, which
	// happens before any backend turn. `delegate setup` runs the exact
	// setupRequiredCapabilities gate against the real pinned binary. It must report
	// ready with capabilities OK even though the pinned agentbus advertises
	// policy.shape=false — the failure mode this whole fix addresses. This is
	// deliberately backend-turn-free so it is independent of the codex app-server
	// protocol and of result-handoff behavior.
	_ = workspace
	var setupOut, setupErr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &setupOut, &setupErr)
	if code != 0 {
		t.Fatalf("delegate setup against pinned agentbus failed with code %d stdout=%q stderr=%q serveStderr=%q", code, setupOut.String(), setupErr.String(), serveStderr.String())
	}
	var setup setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(setupOut.Bytes()), &setup); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, setupOut.String())
	}
	if !setup.AdmissionStrictContainment || !setup.Agentbus.CapabilitiesOK {
		t.Fatalf("setup=%#v, want strict containment + capabilities OK against pinned agentbus", setup)
	}
	// The regression was requiring policy.shape: it must not be required or missing,
	// and the real pinned binary must advertise it as false.
	if setup.Agentbus.Capabilities["policy.shape"] {
		t.Fatalf("pinned agentbus advertised policy.shape=true; want false (post-relocation build)")
	}
	for _, capName := range append(setup.Agentbus.Required, setup.Agentbus.Missing...) {
		if capName == "policy.shape" {
			t.Fatalf("policy.shape must not be required/missing; required=%#v missing=%#v", setup.Agentbus.Required, setup.Agentbus.Missing)
		}
	}
	t.Logf("agentbus pinned smoke ran using %s (%s)", agentbusPath, source)
}

// agentbusPinnedSmokeBinary builds the EXACT agentbus commit delegate's go.mod
// pins, not a release tag. agentbus main still reports version 0.6.0, so a
// version string cannot distinguish the post-relocation build (policy.shape=false)
// from the old v0.6.0 tag — building the pinned commit guarantees the smoke
// exercises the code delegate actually compiles against.
func agentbusPinnedSmokeBinary(t *testing.T) (string, string) {
	t.Helper()
	commit := agentbusPinnedCommit(t)

	repo := os.Getenv("DELEGATE_AGENTBUS_REPO")
	if repo == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			repo = filepath.Join(home, "WebstormProjects", "agentbus")
		}
	}
	if repo == "" {
		t.Fatal("agentbus source unavailable: set DELEGATE_AGENTBUS_REPO to an agentbus checkout")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatalf("agentbus source unavailable at %s: %v", repo, err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", commit+"^{commit}").CombinedOutput(); err != nil {
		t.Fatalf("pinned agentbus commit %s unavailable at %s: %v: %s", commit, repo, err, strings.TrimSpace(string(out)))
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "agentbus-pinned.tar")
	if out, err := exec.Command("git", "-C", repo, "archive", "--format=tar", "-o", archive, commit).CombinedOutput(); err != nil {
		t.Fatalf("archive agentbus %s: %v: %s", commit, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("tar", "-xf", archive, "-C", src).CombinedOutput(); err != nil {
		t.Fatalf("extract agentbus %s archive: %v: %s", commit, err, strings.TrimSpace(string(out)))
	}
	rawVersion, err := os.ReadFile(filepath.Join(src, "VERSION"))
	if err != nil {
		t.Fatalf("read agentbus VERSION: %v", err)
	}
	version := strings.TrimSpace(string(rawVersion))
	bin := filepath.Join(base, "agentbus")
	build := exec.Command("go", "build", "-modcacherw", "-trimpath", "-ldflags", "-X main.version="+version, "-o", bin, "./cmd/agentbus")
	build.Dir = src
	build.Env = append(os.Environ(),
		"GOFLAGS=-mod=readonly",
		"CGO_ENABLED=0",
		"GOCACHE="+filepath.Join(base, "gocache"),
	)
	if os.Getenv("GOMODCACHE") == "" {
		build.Env = append(build.Env, "GOMODCACHE="+filepath.Join(base, "gomodcache"))
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentbus pinned binary: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return bin, "pinned commit " + commit
}

// agentbusPinnedCommit extracts the 12-hex commit from the agentbus pseudo-version
// in delegate's go.mod, so the smoke builds exactly the pinned dependency.
func agentbusPinnedCommit(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read delegate go.mod: %v", err)
	}
	re := regexp.MustCompile(`github\.com/charlesnpx/agentbus\s+\S+-([0-9a-f]{12})`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("could not find pinned agentbus pseudo-version commit in go.mod:\n%s", data)
	}
	return string(m[1])
}

func writeSmokeCodexCLI(t *testing.T, path string) {
	t.Helper()
	// A minimal codex stub is enough: the smoke asserts capability negotiation via
	// `delegate setup` (backend discovery calls --version/--help), and runs no turn.
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.143.0"; exit 0; fi
if [ "$1" = "--help" ]; then echo "codex help"; exit 0; fi
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeSmokeCodexModelsCache(t *testing.T, codexHome string) {
	t.Helper()
	raw := []byte(`{"fetched_at":"2026-07-29T00:00:00Z","client_version":"0.143.0","models":[{"slug":"gpt-5","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]}]}` + "\n")
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForSmokeAgentbusReady(t *testing.T, stateRoot string, done <-chan error, stdout, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			agentbusSmokeServeFailOrSkip(t, stateRoot, err, stdout.String(), stderr.String())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		c, err := client.Connect(ctx, client.Options{StateRoot: stateRoot, DisableAutoStart: true})
		cancel()
		if err == nil {
			hello := c.HelloResult()
			_ = c.Close()
			if hello.ProtocolVersion != 2 || !hello.Capabilities["admission.strictContainment"] {
				t.Fatalf("agentbus hello=%#v, want protocol 2 strict containment", hello)
			}
			// Prove this is the post-relocation build: shape validation moved to
			// delegate, so agentbus advertises policy.shape=false. This is the exact
			// capability whose stale client-side requirement blocked the normal path.
			if hello.Capabilities["policy.shape"] {
				t.Fatalf("agentbus hello advertises policy.shape=true; want false for the post-relocation pinned build")
			}
			return
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	if agentbusSmokeStartupBindDenied(stateRoot, stdout.String()+"\n"+stderr.String()) {
		t.Fatalf("agentbus smoke failed because sandbox denied daemon startup unix-socket bind: last=%v stderr=%s", last, strings.TrimSpace(stderr.String()))
	}
	t.Fatalf("agentbus did not become ready: last=%v stdout=%s stderr=%s", last, stdout.String(), stderr.String())
}

func agentbusSmokeServeFailOrSkip(t *testing.T, stateRoot string, err error, stdout, stderr string) {
	t.Helper()
	if agentbusSmokeStartupBindDenied(stateRoot, stdout+"\n"+stderr+"\n"+fmt.Sprint(err)) {
		t.Fatalf("agentbus smoke failed because sandbox denied daemon startup unix-socket bind: %v stderr=%s", err, strings.TrimSpace(stderr))
	}
	t.Fatalf("agentbus serve exited before readiness: %v stdout=%s stderr=%s", err, stdout, stderr)
}

func useRealAgentbusBinaryForDelegate(t *testing.T, path string) func() {
	t.Helper()
	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
		opts.DisableAutoStart = true
		return client.Connect(ctx, opts)
	}
	lookPath = func(name string) (string, error) {
		if name == "agentbus" {
			return path, nil
		}
		return exec.LookPath(name)
	}
	commandOutput = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}
}

func agentbusSmokeStartupBindDenied(stateRoot, text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "unix socket bind denied by sandbox") {
		return true
	}
	bindDenied := strings.Contains(lower, "bind: operation not permitted") ||
		strings.Contains(lower, "bind: permission denied")
	if !bindDenied {
		return false
	}
	socketPath := strings.ToLower(filepath.Join(stateRoot, "agentbus.sock"))
	return strings.Contains(lower, socketPath) || strings.Contains(lower, "agentbus: bind:")
}

func TestAgentbusSmokeStartupBindDeniedIsNarrow(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "agentbus")
	socketPath := filepath.Join(stateRoot, "agentbus.sock")
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "daemon cli bind operation", text: "agentbus: bind: operation not permitted", want: true},
		{name: "daemon cli bind permission", text: "agentbus: bind: permission denied", want: true},
		{name: "socket path bind permission", text: socketPath + ": bind: permission denied", want: true},
		{name: "explicit sandbox bind", text: "unix socket bind denied by sandbox", want: true},
		{name: "backend permission denied", text: "job failed: backend reported permission denied", want: false},
		{name: "generic sandbox text", text: "sandbox unavailable for backend", want: false},
		{name: "strict admission", text: "strict admission support unavailable", want: false},
		{name: "other bind failure", text: "/tmp/other/agentbus.sock: bind: permission denied", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentbusSmokeStartupBindDenied(stateRoot, tt.text); got != tt.want {
				t.Fatalf("agentbusSmokeStartupBindDenied(%q)=%t, want %t", tt.text, got, tt.want)
			}
		})
	}
}
