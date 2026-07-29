package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestRunReadCommandsOnlyCleanTheRequestedJob(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		fake := &fakeAgentbusClient{
			hello:  helloWithCapabilities(),
			status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_requested", State: engine.StateRunning}}},
		}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()

		var stdout, stderr bytes.Buffer
		if code := run([]string{"status", "--job", "job_requested", "--json"}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("status code = %d, stderr = %q", code, stderr.String())
		}
		assertOnlyRequestedStatusCall(t, fake.statuses, "job_requested")
	})

	t.Run("result", func(t *testing.T) {
		fake := &fakeAgentbusClient{
			hello:  helloWithCapabilities(),
			result: client.JobResult{JobID: "job_requested", SessionID: "session_requested", State: engine.StateCompleted},
			status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_requested", State: engine.StateCompleted}}},
		}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()

		var stdout, stderr bytes.Buffer
		if code := run([]string{"result", "--job", "job_requested"}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("result code = %d, stderr = %q", code, stderr.String())
		}
		assertOnlyRequestedStatusCall(t, fake.statuses, "job_requested")
	})
}

func assertOnlyRequestedStatusCall(t *testing.T, calls []client.JobStatusParams, jobID string) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("JobStatus calls = %#v, want one requested-job call", calls)
	}
	if calls[0].JobID != jobID || calls[0].All {
		t.Fatalf("JobStatus call = %#v, want JobID %q without All", calls[0], jobID)
	}
}

func TestWaitForJobResultBacksOffAndChecksStatusForCleanup(t *testing.T) {
	base := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake := &scriptedPollingClient{
		fakeAgentbusClient: base,
		results: []jobResultStep{
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{err: errors.New("result not ready")},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateRunning}},
			{result: client.JobResult{JobID: "job_poll", State: engine.StateCompleted}},
		},
		statuses: []client.JobStatusResult{
			{Jobs: []client.JobStatus{{JobID: "job_poll", State: engine.StateRunning}}},
			{Jobs: []client.JobStatus{{JobID: "job_poll", State: engine.StateCompleted}}},
		},
	}
	oldSleep := jobPollSleep
	var delays []time.Duration
	jobPollSleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	defer func() { jobPollSleep = oldSleep }()

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := waitForJobResult(context.Background(), fake, stateDir, "job_poll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != engine.StateCompleted {
		t.Fatalf("result state = %q, want completed", result.State)
	}
	if want := []string{"result", "result", "status", "result", "result", "result", "result", "result", "result", "result", "result", "status"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("RPC order = %#v, want %#v", fake.calls, want)
	}
	if len(base.statuses) != 2 {
		t.Fatalf("JobStatus calls = %#v, want fallback and terminal cleanup calls", base.statuses)
	}
	for _, call := range base.statuses {
		if call.JobID != "job_poll" || call.All {
			t.Fatalf("JobStatus call = %#v, want JobID %q without All", call, "job_poll")
		}
	}

	wantDelays := []time.Duration{
		100 * time.Millisecond,
		150 * time.Millisecond,
		225 * time.Millisecond,
		337500 * time.Microsecond,
		506250 * time.Microsecond,
		759375 * time.Microsecond,
		1139062500 * time.Nanosecond,
		1708593750 * time.Nanosecond,
		2 * time.Second,
	}
	if !reflect.DeepEqual(delays, wantDelays) {
		t.Fatalf("poll delays = %#v, want %#v", delays, wantDelays)
	}
	if got := nextJobPollInterval(2 * time.Second); got != 2*time.Second {
		t.Fatalf("capped next interval = %s, want 2s", got)
	}
}

func TestCancelFetchesStatusResultAndRetainsCanceledUnresolved(t *testing.T) {
	jobID := "job_cancel_unresolved"
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		cancel: client.JobCancelResult{JobID: jobID, State: engine.StateCanceled},
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:              jobID,
			SessionID:          "session_cancel_status",
			State:              engine.StateCanceled,
			CleanupDisposition: cleanupDispositionUnresolved,
			LateFinalization:   true,
			Warnings:           []string{"cleanup unresolved after cancellation"},
		}}},
		result: client.JobResult{
			JobID:     jobID,
			SessionID: "session_cancel_result",
			State:     engine.StateCanceled,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"cancel", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != engine.ExitCodeForState(engine.StateCanceled) {
		t.Fatalf("cancel code=%d stderr=%q stdout=%q, want canceled exit", code, stderr.String(), stdout.String())
	}
	if len(fake.cancels) != 1 || fake.cancels[0].JobID != jobID || len(fake.statuses) != 1 || len(fake.results) != 1 {
		t.Fatalf("RPC calls cancels=%#v statuses=%#v results=%#v, want cancel then status/result", fake.cancels, fake.statuses, fake.results)
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCanceled || env.CleanupDisposition != cleanupDispositionUnresolved || !env.LocalArtifactsRetained || !env.LateFinalization {
		t.Fatalf("cancel envelope=%#v, want canceled unresolved retained", env)
	}
	if len(env.AgentbusWarnings) != 1 || env.AgentbusWarnings[0] != "cleanup unresolved after cancellation" {
		t.Fatalf("agentbus_warnings=%#v", env.AgentbusWarnings)
	}
	assertPathExists(t, inputPath)
	assertPathExists(t, workspace)
	assertCleanupWarning(t, stderr.String(), cleanupDispositionUnresolvedWarning)
}

func TestCancelRaceCompletedUsesTerminalResultAndCleanupDisposition(t *testing.T) {
	jobID := "job_cancel_raced_completed"
	resultSHA := strings.Repeat("f", 64)
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		cancel: client.JobCancelResult{JobID: jobID, State: engine.StateCanceled},
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:              jobID,
			SessionID:          "session_cancel_completed",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
		}}},
		result: client.JobResult{
			JobID:     jobID,
			SessionID: "session_cancel_completed",
			State:     engine.StateCompleted,
			Result:    &engine.ResultInfo{SHA256: resultSHA},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	_, inputPath, workspace := prepareCleanupDispositionArtifacts(t, jobID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"cancel", "--job", jobID, "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cancel code=%d stderr=%q stdout=%q, want completed success", code, stderr.String(), stdout.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
	}
	if env.Status != engine.StateCompleted || env.ResultSHA256 == nil || *env.ResultSHA256 != resultSHA || env.CleanupDisposition != cleanupDispositionVerifiedAbsent {
		t.Fatalf("cancel race envelope=%#v, want completed terminal result", env)
	}
	if env.LocalArtifactsRetained {
		t.Fatalf("local_artifacts_retained=true, want safe cleanup")
	}
	assertPathMissing(t, inputPath)
	assertPathMissing(t, workspace)
	if len(fake.cancels) != 1 || len(fake.statuses) != 1 || len(fake.results) != 1 {
		t.Fatalf("RPC calls cancels=%#v statuses=%#v results=%#v, want cancel/status/result", fake.cancels, fake.statuses, fake.results)
	}
}

func TestWaitForJobResultPermanentPollErrorsReturnPromptly(t *testing.T) {
	for _, tc := range []struct {
		name               string
		err                error
		wantCode           int
		wantCodeField      string
		wantAdmissionCause string
	}{
		{
			name:          "unknown_job",
			err:           testRPCError(t, agentbusErrorUnknownJob, "job not found", "", "job_missing", nil),
			wantCode:      agentbusExitUnknownJob,
			wantCodeField: agentbusErrorUnknownJob,
		},
		{
			name:          "protocol_mismatch",
			err:           &client.ProtocolVersionMismatchError{Expected: 2, Received: 1},
			wantCode:      agentbusExitDaemonRuntime,
			wantCodeField: agentbusErrorProtocolMismatch,
		},
		{
			name:               "root_fail_stopped",
			err:                testRPCError(t, agentbusErrorBackendUnavailable, "root fail-stopped", admissionCauseRootFailStopped, "job_fail_stopped", nil),
			wantCode:           agentbusExitAuthority,
			wantCodeField:      agentbusErrorBackendUnavailable,
			wantAdmissionCause: admissionCauseRootFailStopped,
		},
		{
			name:               "root_sealed",
			err:                testRPCError(t, agentbusErrorBackendUnavailable, "root sealed", admissionCauseRootSealed, "job_sealed", nil),
			wantCode:           agentbusExitAuthority,
			wantCodeField:      agentbusErrorBackendUnavailable,
			wantAdmissionCause: admissionCauseRootSealed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities(), resultErr: tc.err}
			restore := stubAgentbusClientGlobals(t, fake)
			defer restore()
			oldSleep := jobPollSleep
			sleepCalls := 0
			jobPollSleep = func(context.Context, time.Duration) error {
				sleepCalls++
				return errors.New("unexpected poll sleep after permanent error")
			}
			defer func() { jobPollSleep = oldSleep }()

			var stdout, stderr bytes.Buffer
			code := run([]string{"result", "--job", "job_permanent", "--wait", "--json"}, nil, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("result code=%d stderr=%q stdout=%q, want %d", code, stderr.String(), stdout.String(), tc.wantCode)
			}
			if sleepCalls != 0 || len(fake.results) != 1 || len(fake.statuses) != 0 {
				t.Fatalf("poll calls sleep=%d results=%#v statuses=%#v, want one result and no retry/status", sleepCalls, fake.results, fake.statuses)
			}
			var env agentbusErrorEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("error JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Code != tc.wantCodeField || env.AdmissionCause != tc.wantAdmissionCause {
				t.Fatalf("error envelope=%#v, want code=%q cause=%q", env, tc.wantCodeField, tc.wantAdmissionCause)
			}
		})
	}
}

func TestWaitForJobResultRetryablePollErrorsKeepPollingThenSucceed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "transport", err: errors.New("connection reset")},
		{name: "blank_cause_backend_unavailable", err: testRPCError(t, agentbusErrorBackendUnavailable, "socket closed", "", "job_retryable", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &fakeAgentbusClient{hello: helloWithCapabilities()}
			fake := &scriptedPollingClient{
				fakeAgentbusClient: base,
				results: []jobResultStep{
					{err: tc.err},
					{result: client.JobResult{JobID: "job_retryable", State: engine.StateCompleted}},
				},
				statuses: []client.JobStatusResult{
					{Jobs: []client.JobStatus{{JobID: "job_retryable", State: engine.StateRunning}}},
					{Jobs: []client.JobStatus{{JobID: "job_retryable", State: engine.StateCompleted, CleanupDisposition: cleanupDispositionVerifiedAbsent}}},
				},
			}
			restore := stubAgentbusClientGlobals(t, fake)
			defer restore()
			oldSleep := jobPollSleep
			sleepCalls := 0
			jobPollSleep = func(context.Context, time.Duration) error {
				sleepCalls++
				return nil
			}
			defer func() { jobPollSleep = oldSleep }()

			var stdout, stderr bytes.Buffer
			code := run([]string{"result", "--job", "job_retryable", "--wait", "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("result code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
			}
			if sleepCalls != 1 {
				t.Fatalf("sleep calls=%d, want one retry delay", sleepCalls)
			}
			if want := []string{"result", "status", "result", "status"}; !reflect.DeepEqual(fake.calls, want) {
				t.Fatalf("RPC order=%#v, want %#v", fake.calls, want)
			}
			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Status != engine.StateCompleted {
				t.Fatalf("terminal status=%q, want completed", env.Status)
			}
		})
	}
}

func TestWaitContextCancelReturnsImmediatelyAndPreservesSubmissionIntent(t *testing.T) {
	jobID := "job_wait_context_cancel"
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
		result:    client.JobResult{JobID: jobID},
		resultErr: context.Canceled,
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return errors.New("unexpected poll sleep after context cancellation")
	}
	defer func() { jobPollSleep = oldSleep }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "wait context cancel", "--wait", "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task unexpectedly succeeded; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if sleepCalls != 0 || len(fake.results) != 1 || len(fake.statuses) != 0 {
		t.Fatalf("poll calls sleep=%d results=%#v statuses=%#v, want immediate context return", sleepCalls, fake.results, fake.statuses)
	}
	intents, err := listSubmissionIntents("")
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Fatalf("submission intents=%#v, want one preserved intent", intents)
	}
	if intents[0].Phase != submissionPhaseAcknowledged || intents[0].JobID != jobID {
		t.Fatalf("intent=%#v, want acknowledged preserved job %s", intents[0], jobID)
	}
}

type jobResultStep struct {
	result client.JobResult
	err    error
}

type scriptedPollingClient struct {
	*fakeAgentbusClient
	results  []jobResultStep
	statuses []client.JobStatusResult
	calls    []string
}

func (c *scriptedPollingClient) JobResult(context.Context, client.JobResultParams) (client.JobResult, error) {
	c.calls = append(c.calls, "result")
	if len(c.results) == 0 {
		return client.JobResult{}, errors.New("unexpected JobResult")
	}
	step := c.results[0]
	c.results = c.results[1:]
	return step.result, step.err
}

func (c *scriptedPollingClient) JobStatus(_ context.Context, params client.JobStatusParams) (client.JobStatusResult, error) {
	c.calls = append(c.calls, "status")
	c.fakeAgentbusClient.statuses = append(c.fakeAgentbusClient.statuses, params)
	if len(c.statuses) == 0 {
		return client.JobStatusResult{}, errors.New("unexpected JobStatus")
	}
	status := c.statuses[0]
	c.statuses = c.statuses[1:]
	return status, nil
}
