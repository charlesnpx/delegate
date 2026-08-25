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
	"time"

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

func TestOrphanedTerminalWaitStopsEmitsEnvelopeAndRetainsArtifacts(t *testing.T) {
	jobID := "job_orphaned_unresolved"
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
		resultErr: errors.New("orphaned job has no result"),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:              jobID,
			SessionID:          "session_orphaned_unresolved",
			State:              engine.StateOrphaned,
			CleanupDisposition: cleanupDispositionUnresolved,
			LateFinalization:   true,
			Warnings:           []string{"lease expired before backend result was finalized"},
		}}},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return errors.New("unexpected poll sleep after orphaned terminal status")
	}
	defer func() { jobPollSleep = oldSleep }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--wait", "--json"}, nil, &stdout, &stderr)
	if code != engine.ExitCodeForState(engine.StateOrphaned) {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want orphaned exit", code, stderr.String(), stdout.String())
	}
	if code != 14 {
		t.Fatalf("orphaned exit code=%d, want 14", code)
	}
	if sleepCalls != 0 {
		t.Fatalf("poll sleep calls=%d, want wait to stop on orphaned", sleepCalls)
	}
	if len(fake.results) != 1 || len(fake.statuses) != 1 {
		t.Fatalf("RPC calls results=%#v statuses=%#v, want one result attempt and one status fallback", fake.results, fake.statuses)
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateOrphaned || env.CleanupDisposition != cleanupDispositionUnresolved || !env.LocalArtifactsRetained {
		t.Fatalf("orphaned envelope=%#v, want orphaned unresolved retained", env)
	}
	if env.ResultSHA256 != nil || env.ResultUnavailableReason != "orphaned_without_result" {
		t.Fatalf("orphaned result fields=%#v reason=%q, want no fabricated result", env.ResultSHA256, env.ResultUnavailableReason)
	}
	if !env.LateFinalization || len(env.AgentbusWarnings) != 1 || env.AgentbusWarnings[0] != "lease expired before backend result was finalized" {
		t.Fatalf("orphaned agentbus fields=%#v late=%v", env.AgentbusWarnings, env.LateFinalization)
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), cleanupDispositionUnresolvedWarning)
}

func TestTerminalEnvelopeSchema2FieldsFromResultAndStatus(t *testing.T) {
	jobID := "job_terminal_schema2"
	resultSHA := strings.Repeat("e", 64)
	startedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	heartbeatAt := startedAt.Add(time.Minute)
	updatedAt := heartbeatAt.Add(time.Minute)
	finalAttemptStartedAt := updatedAt.Add(time.Minute)
	finalAttemptEndedAt := finalAttemptStartedAt.Add(time.Minute)
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:     jobID,
			SessionID: "session_terminal_schema2",
			State:     engine.StateCompleted,
			Result:    &engine.ResultInfo{SHA256: resultSHA},
		},
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:                 jobID,
			SessionID:             "session_terminal_schema2",
			State:                 engine.StateCompleted,
			CleanupDisposition:    cleanupDispositionUnresolved,
			LateFinalization:      true,
			Warnings:              []string{"cleanup could not prove absence"},
			StartedAt:             &startedAt,
			HeartbeatAt:           &heartbeatAt,
			UpdatedAt:             &updatedAt,
			FinalAttemptStartedAt: &finalAttemptStartedAt,
			FinalAttemptEndedAt:   &finalAttemptEndedAt,
		}}},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
	}
	if len(fake.statuses) != 1 {
		t.Fatalf("JobStatus calls=%#v, want one reused terminal status lookup", fake.statuses)
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.CleanupDisposition != cleanupDispositionUnresolved || !env.LocalArtifactsRetained || !env.LateFinalization {
		t.Fatalf("terminal schema-2 fields=%#v", env)
	}
	if len(env.AgentbusWarnings) != 1 || env.AgentbusWarnings[0] != "cleanup could not prove absence" {
		t.Fatalf("agentbus_warnings=%#v", env.AgentbusWarnings)
	}
	if env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA || env.ResultUnavailableReason != "" {
		t.Fatalf("result fields sha=%#v reason=%q", env.ResultSHA256, env.ResultUnavailableReason)
	}
	if env.StartedAt == nil || !env.StartedAt.Equal(startedAt) || env.HeartbeatAt == nil || !env.HeartbeatAt.Equal(heartbeatAt) || env.UpdatedAt == nil || !env.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("terminal timestamps started=%v heartbeat=%v updated=%v", env.StartedAt, env.HeartbeatAt, env.UpdatedAt)
	}
	if env.FinalAttemptStartedAt == nil || !env.FinalAttemptStartedAt.Equal(finalAttemptStartedAt) || env.FinalAttemptEndedAt == nil || !env.FinalAttemptEndedAt.Equal(finalAttemptEndedAt) {
		t.Fatalf("terminal final-attempt timing started=%v ended=%v", env.FinalAttemptStartedAt, env.FinalAttemptEndedAt)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"final_attempt_started_at", "final_attempt_ended_at"} {
		if _, found := fields[field]; !found {
			t.Fatalf("terminal envelope JSON=%q, missing %q", stdout.String(), field)
		}
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), cleanupDispositionUnresolvedWarning)
}

func TestTerminalEnvelopeSchema2FieldsFromOrphanedStatusFallback(t *testing.T) {
	jobID := "job_status_fallback_schema2"
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
		resultErr: errors.New("terminal result unavailable"),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:              jobID,
			SessionID:          "session_status_fallback_schema2",
			State:              engine.StateOrphaned,
			CleanupDisposition: cleanupDispositionUnresolved,
			LateFinalization:   true,
			Warnings:           []string{"terminal status was recovered without a result"},
		}}},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != engine.ExitCodeForState(engine.StateOrphaned) {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want orphaned exit", code, stderr.String(), stdout.String())
	}
	if len(fake.results) != 1 || len(fake.statuses) != 1 {
		t.Fatalf("RPC calls results=%#v statuses=%#v, want one result attempt and one status fallback", fake.results, fake.statuses)
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateOrphaned || env.CleanupDisposition != cleanupDispositionUnresolved || !env.LocalArtifactsRetained || !env.LateFinalization {
		t.Fatalf("fallback schema-2 fields=%#v", env)
	}
	if len(env.AgentbusWarnings) != 1 || env.AgentbusWarnings[0] != "terminal status was recovered without a result" {
		t.Fatalf("fallback agentbus_warnings=%#v", env.AgentbusWarnings)
	}
	if env.ResultSHA256 != nil || env.ResultUnavailableReason != "orphaned_without_result" {
		t.Fatalf("fallback result fields sha=%#v reason=%q", env.ResultSHA256, env.ResultUnavailableReason)
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

func TestSafeCleanupFailureWarnsRetainsArtifactsAndPreservesOutcome(t *testing.T) {
	jobID := "job_safe_cleanup_delete_failure"
	resultSHA := strings.Repeat("6", 64)
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              jobID,
			SessionID:          "session_safe_cleanup_delete_failure",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
			Result:             &engine.ResultInfo{SHA256: resultSHA},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	deleteErr := errors.New("delete job input failed")
	reviewErr := errors.New("delete review workspace failed")
	oldDelete := deleteJobInputOnTerminalState
	oldCleanup := cleanupReviewWorkspace
	deleteJobInputOnTerminalState = func(handoff.JobInput, engine.JobState, string, handoff.Hooks) (bool, error) {
		return false, deleteErr
	}
	cleanupReviewWorkspace = func(string, string) error {
		return reviewErr
	}
	t.Cleanup(func() {
		deleteJobInputOnTerminalState = oldDelete
		cleanupReviewWorkspace = oldCleanup
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want success despite cleanup failure", code, stderr.String(), stdout.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCompleted || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA {
		t.Fatalf("terminal envelope=%#v, want completed result %s", env, resultSHA)
	}
	if env.CleanupDisposition != cleanupDispositionVerifiedAbsent || !env.LocalArtifactsRetained {
		t.Fatalf("cleanup fields=%#v, want verified_absent with local_artifacts_retained=true", env)
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), "Delegate could not remove local job input")
	assertCleanupWarning(t, stderr.String(), "Delegate could not remove local review workspace")
	meta, found, err := loadJobMetadata("", jobID)
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if meta.JobInputPath == "" || meta.ReviewWorkspace == "" || meta.CleanupDisposition != cleanupDispositionVerifiedAbsent {
		t.Fatalf("metadata after failed cleanup=%#v, want artifact paths retained and disposition recorded", meta)
	}

	jobID = "job_safe_cleanup_warning_writer_failure"
	resultSHA = strings.Repeat("7", 64)
	fake.result = client.JobResult{
		JobID:              jobID,
		SessionID:          "session_safe_cleanup_warning_writer_failure",
		State:              engine.StateCompleted,
		CleanupDisposition: cleanupDispositionVerifiedAbsent,
		Result:             &engine.ResultInfo{SHA256: resultSHA},
	}
	_, inputPath, workspace = prepareCleanupDispositionArtifacts(t, jobID)

	stdout.Reset()
	failingStderr := &failingCleanupWarningWriter{err: errors.New("warning writer failed")}
	code = run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, failingStderr)
	if code != 0 {
		t.Fatalf("result code=%d stdout=%q, want success despite cleanup and warning-writer failures", code, stdout.String())
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCompleted || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA {
		t.Fatalf("terminal envelope=%#v, want completed result %s", env, resultSHA)
	}
	if env.CleanupDisposition != cleanupDispositionVerifiedAbsent || !env.LocalArtifactsRetained {
		t.Fatalf("cleanup fields=%#v, want verified_absent with local_artifacts_retained=true", env)
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	if failingStderr.writes == 0 {
		t.Fatal("warning writer writes=0, want cleanup warnings attempted")
	}
}

func TestSafeCleanupMetadataSaveRetryKeepsRetainedFlagAccurate(t *testing.T) {
	jobID := "job_safe_cleanup_save_retry"
	resultSHA := strings.Repeat("8", 64)
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              jobID,
			SessionID:          "session_safe_cleanup_save_retry",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
			Result:             &engine.ResultInfo{SHA256: resultSHA},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	realSave := saveJobMetadata
	cleanedSaveCalls := 0
	saveJobMetadata = func(stateDir string, meta jobMetadata) error {
		if meta.JobID == jobID && meta.JobInputPath == "" && meta.ReviewWorkspace == "" {
			cleanedSaveCalls++
			if cleanedSaveCalls == 1 {
				return errors.New("persist cleaned metadata failed")
			}
		}
		return realSave(stateDir, meta)
	}
	defer func() { saveJobMetadata = realSave }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q, want success despite transient metadata save failure", code, stderr.String(), stdout.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCompleted || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA {
		t.Fatalf("terminal envelope=%#v, want completed result %s", env, resultSHA)
	}
	if env.CleanupDisposition != cleanupDispositionVerifiedAbsent || env.LocalArtifactsRetained {
		t.Fatalf("cleanup fields=%#v, want verified_absent with local_artifacts_retained=false", env)
	}
	if cleanedSaveCalls != 2 {
		t.Fatalf("cleaned metadata save calls=%d, want initial failure plus retry", cleanedSaveCalls)
	}
	assertPathMissing(t, inputPath)
	assertPathMissing(t, workspace)
	meta, found, err := loadJobMetadata("", jobID)
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if meta.JobInputPath != "" || meta.ReviewWorkspace != "" || meta.CleanupDisposition != cleanupDispositionVerifiedAbsent {
		t.Fatalf("metadata after cleanup retry=%#v, want removed artifacts and disposition recorded", meta)
	}
}

type failingCleanupWarningWriter struct {
	err    error
	writes int
}

func (w *failingCleanupWarningWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
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
