package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestTerminalEnvelopeWithoutResultAcrossTerminalStates(t *testing.T) {
	cases := []struct {
		state  engine.JobState
		reason string
	}{
		{state: engine.StateFailed, reason: "failed_without_result"},
		{state: engine.StateTimedOut, reason: "timed_out_without_result"},
		{state: engine.StateInterrupted, reason: "interrupted_without_result"},
		{state: engine.StateCanceled, reason: "canceled_without_result"},
		{state: engine.StateOrphaned, reason: "orphaned_without_result"},
		{state: engine.StateReaped, reason: "reaped_without_result"},
		{state: engine.StateQuarantined, reason: "quarantined_without_result"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			env, err := newTerminalEnvelope("job_terminal", tc.state, taskKind, contractKindShape, skippedDelegateContractStamp(engine.SkipResultUnavailable), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if env.ResultSHA256 != nil || env.ResultUnavailableReason != tc.reason || env.Contract.Status != engine.ContractSkipped {
				t.Fatalf("terminal envelope = %#v", env)
			}
			raw, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"failure_reason", "failure_class"} {
				if _, found := fields[field]; found {
					t.Fatalf("terminal envelope JSON=%s unexpectedly contains %q", raw, field)
				}
			}
		})
	}
}

func TestResultUnavailableReasonDiffersByTerminalState(t *testing.T) {
	orphaned, err := newTerminalEnvelope("job_orphaned", engine.StateOrphaned, taskKind, contractKindShape, skippedDelegateContractStamp(engine.SkipResultUnavailable), "", "")
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := newTerminalEnvelope("job_canceled", engine.StateCanceled, taskKind, contractKindShape, skippedDelegateContractStamp(engine.SkipResultUnavailable), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.ResultUnavailableReason != "orphaned_without_result" || canceled.ResultUnavailableReason != "canceled_without_result" || orphaned.ResultUnavailableReason == canceled.ResultUnavailableReason {
		t.Fatalf("reasons orphaned=%q canceled=%q, want distinct state-specific reasons", orphaned.ResultUnavailableReason, canceled.ResultUnavailableReason)
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
	if err := captureBackendError(stateDir, client.JobStatus{JobID: "job_backend_error", State: engine.StateFailed, LogPaths: &engine.LogPaths{Stderr: stderrPath}}); err != nil {
		t.Fatal(err)
	}
	env, err := terminalEnvelopeFromJobResult(stateDir, client.JobResult{
		JobID:         "job_backend_error",
		State:         engine.StateFailed,
		FailureReason: "credential helper unavailable",
		FailureClass:  engine.FailureClassBackendError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.BackendError != "launch failed: credential helper unavailable" || env.Contract.Reason != string(engine.SkipBackendError) || env.ResultSHA256 != nil || env.ResultUnavailableReason != "failed_without_result" || env.FailureReason != "credential helper unavailable" || env.FailureClass != engine.FailureClassBackendError {
		t.Fatalf("backend error envelope = %#v", env)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"failure_reason", "failure_class"} {
		if _, found := fields[field]; !found {
			t.Fatalf("terminal envelope JSON=%s, missing %q", raw, field)
		}
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
	job := client.JobStatus{JobID: "job_sanitized", State: engine.StateFailed, LogPaths: &engine.LogPaths{Stderr: stderrPath}}
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

func TestTerminalEnvelopeAndCleanupIgnoreCorruptLocalMetadata(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "job_corrupt_metadata"
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if err := cleanupJobInput(stateDir, jobID, "session_corrupt", engine.StateCompleted, cleanupDispositionVerifiedAbsent, newLocalCleanupWarnings(&stderr)); err != nil {
		t.Fatalf("cleanupJobInput err=%v, want corrupt metadata warning only", err)
	}
	if !strings.Contains(stderr.String(), "Delegate could not read local job metadata") {
		t.Fatalf("cleanup warning=%q, want corrupt metadata warning", stderr.String())
	}

	resultSHA := strings.Repeat("8", 64)
	env, err := terminalEnvelopeFromJobResult(stateDir, client.JobResult{
		JobID:  jobID,
		State:  engine.StateCompleted,
		Result: &engine.ResultInfo{SHA256: resultSHA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.JobID != jobID || env.Status != engine.StateCompleted || engine.ExitCodeForState(env.Status) != 0 {
		t.Fatalf("terminal envelope=%#v, want completed authoritative result", env)
	}
	if env.Kind != taskKind || env.ContractKind != contractKindShape || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA {
		t.Fatalf("terminal enrichment/result fields=%#v, want defaults with result sha %s", env, resultSHA)
	}
}

func TestWaitForJobResultSynthesizesEnvelopeInputForOrphanedTerminalState(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{resultErr: errors.New("orphaned record cannot be loaded"), status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_fallback", SessionID: "session_fallback", State: engine.StateOrphaned}}}}
	result, err := waitForJobResult(context.Background(), fake, stateDir, "job_fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	env, err := terminalEnvelopeFromJobResult(stateDir, result)
	if err != nil {
		t.Fatal(err)
	}
	if env.JobID != "job_fallback" || env.Status != engine.StateOrphaned || env.ResultUnavailableReason != resultUnavailableReason(engine.StateOrphaned) {
		t.Fatalf("fallback envelope = %#v", env)
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
