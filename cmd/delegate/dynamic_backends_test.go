package main

import (
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

func TestValidateBackendUsesHelloDiscoveryAndMetadata(t *testing.T) {
	hello := client.HelloResult{Backends: []string{"gemini", "codex"}, BackendMetadata: []client.BackendInfo{{Backend: "gemini", Models: []string{"gemini-2.5-pro"}, Efforts: []string{"high"}}}}
	if err := validateBackend(hello, "gemini", "gemini-2.5-pro", "high"); err != nil {
		t.Fatal(err)
	}
	if err := validateBackend(hello, "claude", "", ""); err == nil || !strings.Contains(err.Error(), "available backends: codex, gemini") {
		t.Fatalf("unknown backend error = %v", err)
	}
	if err := validateBackend(hello, "gemini", "wrong", ""); err == nil || !strings.Contains(err.Error(), "available models") {
		t.Fatalf("unknown model error = %v", err)
	}
}
