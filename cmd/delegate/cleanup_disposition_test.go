package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
)

func TestCompletedUnresolvedCleanupRetainsArtifactsAndPreservesSuccess(t *testing.T) {
	jobID := "job_completed_unresolved"
	resultSHA := strings.Repeat("d", 64)
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              jobID,
			SessionID:          "session_completed_unresolved",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionUnresolved,
			Result:             &engine.ResultInfo{SHA256: resultSHA},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCompleted || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA {
		t.Fatalf("terminal envelope=%#v, want completed result %s", env, resultSHA)
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), cleanupDispositionUnresolvedWarning)
}

func TestCanceledUnresolvedCleanupRetainsArtifacts(t *testing.T) {
	jobID := "job_canceled_unresolved"
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              jobID,
			SessionID:          "session_canceled_unresolved",
			State:              engine.StateCanceled,
			CleanupDisposition: cleanupDispositionUnresolved,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != engine.ExitCodeForState(engine.StateCanceled) {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want canceled exit", code, stderr.String(), stdout.String())
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), cleanupDispositionUnresolvedWarning)
}

func TestSafeCleanupDispositionsRemoveArtifacts(t *testing.T) {
	for _, disposition := range []string{cleanupDispositionVerifiedAbsent, cleanupDispositionNoExecutionPossible} {
		t.Run(disposition, func(t *testing.T) {
			jobID := "job_cleanup_" + disposition
			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				result: client.JobResult{
					JobID:              jobID,
					SessionID:          "session_" + disposition,
					State:              engine.StateCompleted,
					CleanupDisposition: disposition,
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

			var stdout, stderr bytes.Buffer
			code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("result code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
			}
			assertPathMissing(t, inputPath)
			assertPathMissing(t, workspace)
			if strings.Contains(stderr.String(), "retained local job artifacts") {
				t.Fatalf("stderr=%q, want no retention warning", stderr.String())
			}
			meta, found, err := loadJobMetadata("", jobID)
			if err != nil || !found {
				t.Fatalf("metadata found=%v err=%v", found, err)
			}
			if meta.JobInputPath != "" || meta.ReviewWorkspace != "" || meta.CleanupDisposition != disposition {
				t.Fatalf("metadata after cleanup=%#v, want removed artifacts and disposition %q", meta, disposition)
			}
		})
	}
}

func TestMissingCleanupDispositionOnTerminalJobRetainsArtifacts(t *testing.T) {
	jobID := "job_missing_cleanup_disposition"
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:     jobID,
			SessionID: "session_missing_cleanup_disposition",
			State:     engine.StateCompleted,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), "Agentbus did not report cleanupDisposition for a terminal job; delegate retained local job artifacts because backend absence is unproven")
}

func TestNonTerminalCleanupDispositionRetainsArtifacts(t *testing.T) {
	jobID := "job_running_verified_absent"
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:              jobID,
			SessionID:          "session_running_verified_absent",
			State:              engine.StateRunning,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
		}}},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	if strings.Contains(stderr.String(), "retained local job artifacts") {
		t.Fatalf("stderr=%q, want no terminal-retention warning", stderr.String())
	}
}

func prepareCleanupDispositionArtifacts(t *testing.T, jobID string) (string, string, string) {
	t.Helper()
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: stateDir,
		JobID:    jobID,
		Prompt: handoff.ResolvedPrompt{
			Prompt: "prompt for " + jobID,
			Source: handoff.SourcePrompt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(stateDir, "review-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "review.patch"), []byte("diff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:           jobID,
		Kind:            reviewKind,
		ContractKind:    contractKindShape,
		JobInputPath:    input.Path,
		ReviewWorkspace: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	return stateDir, input.Path, workspace
}

func assertCleanupWarning(t *testing.T, stderr, want string) {
	t.Helper()
	if !strings.Contains(stderr, "warning: "+want) {
		t.Fatalf("stderr=%q, want warning %q", stderr, want)
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s stat error=%v, want exists", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or stat error=%v, want missing", path, err)
	}
}
