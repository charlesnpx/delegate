package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestAgentbusV06BinaryIntegrationSmoke(t *testing.T) {
	agentbusPath, source := agentbusV06SmokeBinary(t)
	base, err := os.MkdirTemp("/tmp", "delegate-agentbus-smoke-")
	if err != nil {
		t.Skipf("create short smoke temp dir: %v", err)
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

	var launchOut, launchErr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", workspace,
		"--prompt", "delegate real agentbus smoke",
		"--no-contract", "--background", "--json",
	}, nil, &launchOut, &launchErr)
	if code != 0 {
		t.Fatalf("delegate task submit failed with code %d stdout=%q stderr=%q serveStderr=%q", code, launchOut.String(), launchErr.String(), serveStderr.String())
	}
	jobID := smokeJobIDFromLaunch(t, launchOut.Bytes())
	if jobID == "" {
		t.Fatalf("launch output did not include job_id: %q", launchOut.String())
	}

	waitForSmokeTerminalStatus(t, jobID)

	var resultOut, resultErr bytes.Buffer
	code = run([]string{"result", "--job", jobID, "--json"}, nil, &resultOut, &resultErr)
	if code != 0 {
		t.Fatalf("delegate result failed with code %d stdout=%q stderr=%q serveStderr=%q", code, resultOut.String(), resultErr.String(), serveStderr.String())
	}
	var terminal TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(resultOut.Bytes()), &terminal); err != nil {
		t.Fatalf("terminal JSON invalid: %v; raw=%q", err, resultOut.String())
	}
	if terminal.Schema != envelopeSchema || terminal.JobID != jobID || terminal.Status != engine.StateCompleted {
		t.Fatalf("terminal envelope=%#v, want schema %d completed job %s", terminal, envelopeSchema, jobID)
	}
	if terminal.RequestID == "" || terminal.ResultSHA256 == nil {
		t.Fatalf("terminal request/result fields=%#v, want request id and result hash", terminal)
	}
	t.Logf("agentbus v0.6 smoke ran using %s (%s)", agentbusPath, source)
}

func agentbusV06SmokeBinary(t *testing.T) (string, string) {
	t.Helper()
	if path, err := exec.LookPath("agentbus"); err == nil {
		version, versionErr := smokeAgentbusVersion(path)
		if versionErr == nil && version == "0.6.0" {
			return path, "PATH"
		}
	}

	repo := "/Users/Charles.Anderson/c/agentbus"
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Skipf("agentbus v0.6.0 source unavailable at %s: %v", repo, err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "v0.6.0^{commit}").CombinedOutput(); err != nil {
		t.Skipf("agentbus v0.6.0 tag unavailable at %s: %v: %s", repo, err, strings.TrimSpace(string(out)))
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "agentbus-v0.6.0.tar")
	if out, err := exec.Command("git", "-C", repo, "archive", "--format=tar", "-o", archive, "v0.6.0").CombinedOutput(); err != nil {
		t.Skipf("archive agentbus v0.6.0: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("tar", "-xf", archive, "-C", src).CombinedOutput(); err != nil {
		t.Skipf("extract agentbus v0.6.0 archive: %v: %s", err, strings.TrimSpace(string(out)))
	}
	rawVersion, err := os.ReadFile(filepath.Join(src, "VERSION"))
	if err != nil {
		t.Skipf("read agentbus v0.6.0 VERSION: %v", err)
	}
	tagVersion := strings.TrimSpace(string(rawVersion))
	if tagVersion != "0.6.0" {
		t.Skipf("agentbus VERSION = %q, want 0.6.0", tagVersion)
	}
	bin := filepath.Join(base, "agentbus")
	build := exec.Command("go", "build", "-modcacherw", "-trimpath", "-ldflags", "-X main.version="+tagVersion, "-o", bin, "./cmd/agentbus")
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
		t.Skipf("build agentbus v0.6.0 binary: %v: %s", err, strings.TrimSpace(string(out)))
	}
	version, err := smokeAgentbusVersion(bin)
	if err != nil || version != "0.6.0" {
		t.Skipf("built agentbus version = %q err=%v, want 0.6.0", version, err)
	}
	return bin, "local v0.6.0 tag"
}

func smokeAgentbusVersion(path string) (string, error) {
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, field := range strings.Fields(string(out)) {
		field = strings.TrimPrefix(field, "v")
		if field == "0.6.0" {
			return field, nil
		}
	}
	return "", fmt.Errorf("version output %q did not include v0.6.0", strings.TrimSpace(string(out)))
}

func writeSmokeCodexCLI(t *testing.T, path string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.143.0"; exit 0; fi
if [ "$1" = "--help" ]; then echo "codex help"; exit 0; fi
input=$(cat)
if [ -z "$input" ]; then input="delegate smoke"; fi
printf '%s\n' '{"type":"thread.started","thread_id":"codex-smoke-session"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"delegate smoke result"}}'
printf '%s\n' '{"type":"turn.completed","last_agent_message":"delegate smoke result"}'
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
			return
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	if agentbusSmokeStartupBindDenied(stateRoot, stdout.String()+"\n"+stderr.String()) {
		t.Skipf("agentbus smoke skipped because sandbox denied daemon startup unix-socket bind: last=%v stderr=%s", last, strings.TrimSpace(stderr.String()))
	}
	t.Fatalf("agentbus did not become ready: last=%v stdout=%s stderr=%s", last, stdout.String(), stderr.String())
}

func agentbusSmokeServeFailOrSkip(t *testing.T, stateRoot string, err error, stdout, stderr string) {
	t.Helper()
	if agentbusSmokeStartupBindDenied(stateRoot, stdout+"\n"+stderr+"\n"+fmt.Sprint(err)) {
		t.Skipf("agentbus smoke skipped because sandbox denied daemon startup unix-socket bind: %v stderr=%s", err, strings.TrimSpace(stderr))
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

func smokeJobIDFromLaunch(t *testing.T, raw []byte) string {
	t.Helper()
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &launch); err == nil && launch.JobID != "" {
		return launch.JobID
	}
	var terminal TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &terminal); err == nil && terminal.JobID != "" {
		return terminal.JobID
	}
	return ""
}

func waitForSmokeTerminalStatus(t *testing.T, jobID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"status", "--job", jobID, "--json"}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("delegate status failed with code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		state, terminal, err := smokeStateFromStatus(stdout.Bytes())
		if err != nil {
			t.Fatalf("status JSON invalid: %v; raw=%q", err, stdout.String())
		}
		if terminal {
			if state != engine.StateCompleted {
				t.Fatalf("terminal status = %q, want completed; raw=%q", state, stdout.String())
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach terminal status in smoke timeout", jobID)
}

func smokeStateFromStatus(raw []byte) (engine.JobState, bool, error) {
	var terminal TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &terminal); err == nil && terminal.JobID != "" && terminal.Status != "" {
		return terminal.Status, true, nil
	}
	var status client.JobStatusResult
	if err := json.Unmarshal(bytes.TrimSpace(raw), &status); err != nil {
		return "", false, err
	}
	if len(status.Jobs) == 0 {
		return "", false, errors.New("status contained no jobs")
	}
	state := status.Jobs[0].State
	return state, engine.IsTerminal(state), nil
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
