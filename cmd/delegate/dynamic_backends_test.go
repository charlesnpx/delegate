package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestTerminalEnvelopeWithoutResultAcrossTerminalStates(t *testing.T) {
	states := []engine.JobState{engine.StateFailed, engine.StateTimedOut, engine.StateInterrupted, engine.StateCanceled, engine.StateReaped, engine.StateQuarantined}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			env, err := newTerminalEnvelope("job_terminal", state, taskKind, contractKindShape, skippedDelegateContractStamp(engine.SkipResultUnavailable), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if env.ResultSHA256 != nil || env.ResultUnavailableReason != "result_unavailable" || env.Contract.Status != engine.ContractSkipped {
				t.Fatalf("terminal envelope = %#v", env)
			}
		})
	}
}

func TestCaptureBackendErrorPersistsAndSurfacesStderr(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(t.TempDir(), "backend.stderr")
	if err := os.WriteFile(stderrPath, []byte("launch failed: credential helper unavailable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{JobID: "job_backend_error", Kind: reviewKind, ContractKind: contractKindShape}); err != nil {
		t.Fatal(err)
	}
	if err := captureBackendError(stateDir, client.JobStatus{JobID: "job_backend_error", State: engine.StateFailed, LogPaths: engine.LogPaths{Stderr: stderrPath}}); err != nil {
		t.Fatal(err)
	}
	env, err := terminalEnvelopeFromJobResult(stateDir, client.JobResult{JobID: "job_backend_error", State: engine.StateFailed})
	if err != nil {
		t.Fatal(err)
	}
	if env.BackendError != "launch failed: credential helper unavailable" || env.Contract.Reason != string(engine.SkipBackendError) || env.ResultSHA256 != nil {
		t.Fatalf("backend error envelope = %#v", env)
	}
}

func TestCaptureBackendErrorSanitizesPromptSecretsAndBoundsDiagnostic(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := "inspect the private customer incident"
	promptPath := filepath.Join(stateDir, "prompt")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "token = zK7pQ2vM9xR4tW8nL3cF6hJ1"
	stderrPath := filepath.Join(t.TempDir(), "backend.stderr")
	raw := prompt + "\n" + secret + "\n" + strings.Repeat("diagnostic ", 1000)
	if err := os.WriteFile(stderrPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{JobID: "job_sanitized", Kind: taskKind, ContractKind: contractKindShape, JobInputPath: promptPath}); err != nil {
		t.Fatal(err)
	}
	job := client.JobStatus{JobID: "job_sanitized", State: engine.StateFailed, LogPaths: engine.LogPaths{Stderr: stderrPath}}
	if err := captureBackendError(stateDir, job); err != nil {
		t.Fatal(err)
	}
	meta, found, err := loadJobMetadata(stateDir, job.JobID)
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if strings.Contains(meta.BackendError, prompt) || strings.Contains(meta.BackendError, "zK7pQ2vM9xR4tW8nL3cF6hJ1") {
		t.Fatalf("metadata leaked sensitive input: %q", meta.BackendError)
	}
	if len(meta.BackendError) > backendDiagnosticMaxBytes || !strings.HasSuffix(meta.BackendError, backendDiagnosticTruncated) {
		t.Fatalf("metadata diagnostic length/suffix = %d %q", len(meta.BackendError), meta.BackendError)
	}
	env, err := terminalEnvelopeFromJobResult(stateDir, client.JobResult{JobID: job.JobID, State: job.State})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env.BackendError, prompt) || strings.Contains(env.BackendError, "zK7pQ2vM9xR4tW8nL3cF6hJ1") || len(env.BackendError) > backendDiagnosticMaxBytes {
		t.Fatalf("envelope leaked or exceeded bound: %#v", env)
	}
}

func TestWaitForJobResultSynthesizesEnvelopeInputAcrossTerminalStates(t *testing.T) {
	states := []engine.JobState{engine.StateFailed, engine.StateTimedOut, engine.StateInterrupted, engine.StateCanceled, engine.StateReaped, engine.StateQuarantined}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.Chmod(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			fake := &fakeAgentbusClient{resultErr: errors.New("quarantined record cannot be loaded"), status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_fallback", SessionID: "session_fallback", State: state}}}}
			result, err := waitForJobResult(context.Background(), fake, stateDir, "job_fallback", nil)
			if err != nil {
				t.Fatal(err)
			}
			env, err := terminalEnvelopeFromJobResult(stateDir, result)
			if err != nil {
				t.Fatal(err)
			}
			if env.JobID != "job_fallback" || env.Status != state || env.ResultUnavailableReason != "result_unavailable" {
				t.Fatalf("fallback envelope = %#v", env)
			}
		})
	}
}

func TestValidateBackendUsesHelloDiscoveryAndMetadata(t *testing.T) {
	hello := client.HelloResult{Backends: []string{"gemini", "codex"}, BackendMetadata: []client.BackendInfo{{Name: "gemini", Models: []string{"gemini-2.5-pro"}, Efforts: []string{"high"}}}}
	var stderr bytes.Buffer
	if err := validateBackend(hello, "gemini", "gemini-2.5-pro", "high", &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("known values warning = %q", stderr.String())
	}
	if err := validateBackend(hello, "claude", "", "", &stderr); err == nil || !strings.Contains(err.Error(), "available backends: codex, gemini") {
		t.Fatalf("unknown backend error = %v", err)
	}
	stderr.Reset()
	if err := validateBackend(hello, "gemini", "wrong-model", "wrong-effort", &stderr); err != nil {
		t.Fatalf("unadvertised catalog values error = %v", err)
	}
	for _, want := range []string{
		`warning: model "wrong-model" is not advertised by agentbus for backend "gemini" (advertised: gemini-2.5-pro); passing through — the backend is authoritative`,
		`warning: effort "wrong-effort" is not advertised by agentbus for backend "gemini" (advertised: high); passing through — the backend is authoritative`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("unadvertised values warning = %q, want substring %q", stderr.String(), want)
		}
	}

	stderr.Reset()
	emptyCatalog := client.HelloResult{Backends: []string{"gemini"}, BackendMetadata: []client.BackendInfo{{Name: "gemini"}}}
	if err := validateBackend(emptyCatalog, "gemini", "unadvertised-model", "unadvertised-effort", &stderr); err != nil {
		t.Fatalf("empty catalog error = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("empty catalog warning = %q", stderr.String())
	}
}
