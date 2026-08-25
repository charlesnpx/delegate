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

func TestRunWaitTimeoutRequiresWait(t *testing.T) {
	for _, command := range []string{"status", "result"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{command, "--job", "job_wait_timeout_requires_wait", "--wait-timeout", "5s"}, nil, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("%s code=%d stderr=%q, want usage exit 2", command, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--wait-timeout requires --wait") {
				t.Fatalf("%s stderr=%q, want wait-timeout usage error", command, stderr.String())
			}
		})
	}
}

func TestRunWaitTimeoutRejectsNegative(t *testing.T) {
	for _, command := range []string{"status", "result"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{command, "--job", "job_wait_timeout_negative", "--wait", "--wait-timeout", "-1s"}, nil, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("%s code=%d stderr=%q, want usage exit 2", command, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--wait-timeout must not be negative") {
				t.Fatalf("%s stderr=%q, want negative-duration usage error", command, stderr.String())
			}
		})
	}
}

func TestParseWaitTimeout(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{name: "bare integer is seconds", input: "240", want: 240 * time.Second},
		{name: "go duration", input: "30m", want: 30 * time.Minute},
		{name: "invalid value", input: "not-a-duration", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWaitTimeout(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWaitTimeout(%q) error = nil, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWaitTimeout(%q) error = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("parseWaitTimeout(%q) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

func TestRunWaitTimeoutReportsObservationTimeoutWithoutCancel(t *testing.T) {
	for _, tc := range []struct {
		command string
		fake    *fakeAgentbusClient
	}{
		{
			command: "status",
			fake: &fakeAgentbusClient{
				hello:  helloWithCapabilities(),
				status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_status_wait_timeout", State: engine.StateRunning}}},
			},
		},
		{
			command: "result",
			fake: &fakeAgentbusClient{
				hello:  helloWithCapabilities(),
				result: client.JobResult{JobID: "job_result_wait_timeout", State: engine.StateRunning},
			},
		},
	} {
		t.Run(tc.command, func(t *testing.T) {
			restore := stubAgentbusGlobals(t, tc.fake)
			defer restore()

			jobID := "job_" + tc.command + "_wait_timeout"
			var stdout, stderr bytes.Buffer
			code := run([]string{tc.command, "--job", jobID, "--wait", "--wait-timeout", "50ms", "--json"}, nil, &stdout, &stderr)
			if code != exitCodeWaitObservationTimeout {
				t.Fatalf("%s code=%d stderr=%q stdout=%q, want observation timeout exit %d", tc.command, code, stderr.String(), stdout.String(), exitCodeWaitObservationTimeout)
			}
			var result waitObservationTimeoutJSON
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("%s timeout JSON invalid: %v; raw=%q", tc.command, err, stdout.String())
			}
			if result.JobID != jobID || result.Wait != "timed_out" || result.ObservationTimeout != "50ms" || !strings.Contains(result.Message, "still running") || !strings.Contains(result.Message, "`delegate "+tc.command+" --job "+jobID+"`") {
				t.Fatalf("%s timeout JSON=%#v, want requested job, timed_out wait, 50ms observation timeout, and retrieval message", tc.command, result)
			}
			if len(tc.fake.cancels) != 0 {
				t.Fatalf("%s JobCancel calls=%#v, want no job cancellation", tc.command, tc.fake.cancels)
			}
		})
	}
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

func TestRunResultNoWaitStatusFallbackRequiresResultlessTerminal(t *testing.T) {
	resultErr := errors.New("temporary result transport failure")
	for _, tc := range []struct {
		name           string
		state          engine.JobState
		wantError      bool
		wantExitCode   int
		wantReason     string
		wantErrorCode  string
		forbiddenField string
	}{
		{
			name:           "completed surfaces result error",
			state:          engine.StateCompleted,
			wantError:      true,
			wantExitCode:   agentbusExitDaemonRuntime,
			wantErrorCode:  agentbusErrorTransport,
			forbiddenField: "completed_without_result",
		},
		{
			name:         "orphaned uses status-only fallback",
			state:        engine.StateOrphaned,
			wantExitCode: engine.ExitCodeForState(engine.StateOrphaned),
			wantReason:   "orphaned_without_result",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "job_nowait_" + strings.ReplaceAll(tc.name, " ", "_")
			fake := &fakeAgentbusClient{
				hello:     helloWithCapabilities(),
				resultErr: resultErr,
				status: client.JobStatusResult{Jobs: []client.JobStatus{{
					JobID:     jobID,
					SessionID: "session_" + jobID,
					State:     tc.state,
				}}},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr)
			if code != tc.wantExitCode {
				t.Fatalf("result code=%d stderr=%q stdout=%q, want %d", code, stderr.String(), stdout.String(), tc.wantExitCode)
			}
			if len(fake.results) != 1 || len(fake.statuses) != 1 {
				t.Fatalf("RPC calls results=%#v statuses=%#v, want result then status", fake.results, fake.statuses)
			}
			if tc.wantError {
				if strings.Contains(stdout.String(), tc.forbiddenField) {
					t.Fatalf("stdout=%q contains forbidden resultless terminal field %q", stdout.String(), tc.forbiddenField)
				}
				var env agentbusErrorEnvelope
				if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
					t.Fatalf("error JSON: %v; raw=%q", err, stdout.String())
				}
				if env.Code != tc.wantErrorCode {
					t.Fatalf("error envelope=%#v, want code %q", env, tc.wantErrorCode)
				}
				return
			}
			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Status != tc.state || env.ResultSHA256 != nil || env.ResultUnavailableReason != tc.wantReason {
				t.Fatalf("terminal envelope=%#v, want status %s without fabricated result", env, tc.state)
			}
		})
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

func TestRunResultWaitPermanentStatusErrorsReturnPromptly(t *testing.T) {
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
			fake := &fakeAgentbusClient{hello: helloWithCapabilities(), statusErr: tc.err}
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
			if sleepCalls != 0 || len(fake.results) != 0 || len(fake.statuses) != 1 {
				t.Fatalf("poll calls sleep=%d results=%#v statuses=%#v, want one status and no result fetch", sleepCalls, fake.results, fake.statuses)
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

func TestRunResultWaitPollsStatusThenFetchesTerminalResultOnce(t *testing.T) {
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
					{result: client.JobResult{JobID: "job_retryable", State: engine.StateCompleted}},
				},
				statusSteps: []jobStatusStep{
					{err: tc.err},
					{status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_retryable", State: engine.StateCompleted, CleanupDisposition: cleanupDispositionVerifiedAbsent}}}},
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
			if want := []string{"status", "status", "result"}; !reflect.DeepEqual(fake.calls, want) {
				t.Fatalf("RPC order=%#v, want %#v", fake.calls, want)
			}
			if len(fake.submits) != 0 {
				t.Fatalf("JobSubmit calls=%d, want 0", len(fake.submits))
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

func TestWaitForTerminalJobResultTransportFailuresStopAfterBudget(t *testing.T) {
	resultErr := errors.New("result connection reset")
	statusErr := errors.New("status connection reset")
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
		resultErr: resultErr,
		statusErr: statusErr,
	}
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	}
	defer func() { jobPollSleep = oldSleep }()

	terminalJob, err := waitForTerminalJobResult(context.Background(), fake, t.TempDir(), "job_unreachable", nil)
	if err == nil {
		t.Fatalf("waitForTerminalJobResult err=nil terminal=%#v, want transport error", terminalJob)
	}
	classification, ok := classifyAgentbusError(agentbusOperationError(err))
	if !ok || classification.Code != agentbusErrorTransport || classification.ExitCode != agentbusExitDaemonRuntime {
		t.Fatalf("classified err=%#v ok=%v, want transport exit %d", classification, ok, agentbusExitDaemonRuntime)
	}
	if len(fake.results) != maxConsecutiveTransportFailures || len(fake.statuses) != maxConsecutiveTransportFailures {
		t.Fatalf("poll calls results=%d statuses=%d, want %d each", len(fake.results), len(fake.statuses), maxConsecutiveTransportFailures)
	}
	if sleepCalls != maxConsecutiveTransportFailures-1 {
		t.Fatalf("sleep calls=%d, want %d before budget return", sleepCalls, maxConsecutiveTransportFailures-1)
	}
}

func TestWaitForTerminalJobResultTransportBudgetAllowsTransientFailuresThenResult(t *testing.T) {
	jobID := "job_transient_transport_then_result"
	resultSHA := strings.Repeat("4", 64)
	base := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake := &scriptedPollingClient{
		fakeAgentbusClient: base,
		results: []jobResultStep{
			{err: errors.New("result connection reset 1")},
			{err: errors.New("result connection reset 2")},
			{err: errors.New("result connection reset 3")},
			{result: client.JobResult{JobID: jobID, State: engine.StateCompleted, Result: &engine.ResultInfo{SHA256: resultSHA}}},
		},
		statusSteps: []jobStatusStep{
			{err: errors.New("status connection reset 1")},
			{err: errors.New("status connection reset 2")},
			{err: errors.New("status connection reset 3")},
			{status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted}}}},
		},
	}
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	}
	defer func() { jobPollSleep = oldSleep }()

	terminalJob, err := waitForTerminalJobResult(context.Background(), fake, t.TempDir(), jobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminalJob.result.Result == nil || terminalJob.result.Result.SHA256 != resultSHA {
		t.Fatalf("terminal result=%#v, want sha %s", terminalJob.result.Result, resultSHA)
	}
	if sleepCalls != 3 {
		t.Fatalf("sleep calls=%d, want three transient retries", sleepCalls)
	}
}

func TestWaitForTerminalJobResultRunningStatusesResetTransportBudget(t *testing.T) {
	jobID := "job_running_status_resets_transport_budget"
	resultSHA := strings.Repeat("5", 64)
	resultSteps := make([]jobResultStep, 0, maxConsecutiveTransportFailures+2)
	statusSteps := make([]jobStatusStep, 0, maxConsecutiveTransportFailures+2)
	for i := 0; i < maxConsecutiveTransportFailures+1; i++ {
		resultSteps = append(resultSteps, jobResultStep{err: errors.New("result connection reset while running")})
		statusSteps = append(statusSteps, jobStatusStep{status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateRunning}}}})
	}
	resultSteps = append(resultSteps, jobResultStep{result: client.JobResult{JobID: jobID, State: engine.StateCompleted, Result: &engine.ResultInfo{SHA256: resultSHA}}})
	statusSteps = append(statusSteps, jobStatusStep{status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted}}}})
	base := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake := &scriptedPollingClient{
		fakeAgentbusClient: base,
		results:            resultSteps,
		statusSteps:        statusSteps,
	}
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	}
	defer func() { jobPollSleep = oldSleep }()

	terminalJob, err := waitForTerminalJobResult(context.Background(), fake, t.TempDir(), jobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminalJob.result.Result == nil || terminalJob.result.Result.SHA256 != resultSHA {
		t.Fatalf("terminal result=%#v, want sha %s", terminalJob.result.Result, resultSHA)
	}
	if sleepCalls != maxConsecutiveTransportFailures+1 {
		t.Fatalf("sleep calls=%d, want long-running status retries", sleepCalls)
	}
}

func TestWaitForTerminalJobResultRetriesRetryableResultErrorsBeforeStatusFallback(t *testing.T) {
	jobID := "job_retry_before_status_fallback"
	report := testResultText()
	resultSHA := strings.Repeat("9", 64)
	base := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake := &scriptedPollingClient{
		fakeAgentbusClient: base,
		results: []jobResultStep{
			{err: errors.New("temporary result transport failure 1")},
			{err: errors.New("temporary result transport failure 2")},
			{err: errors.New("temporary result transport failure 3")},
			{result: client.JobResult{
				JobID:  jobID,
				State:  engine.StateCompleted,
				Result: &engine.ResultInfo{Text: report, SHA256: resultSHA, Bytes: int64(len(report))},
			}},
		},
		statuses: []client.JobStatusResult{
			{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted}}},
			{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted}}},
			{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted}}},
			{Jobs: []client.JobStatus{{JobID: jobID, State: engine.StateCompleted, CleanupDisposition: cleanupDispositionVerifiedAbsent}}},
		},
	}
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	}
	defer func() { jobPollSleep = oldSleep }()

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	terminalJob, err := waitForTerminalJobResult(context.Background(), fake, stateDir, jobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sleepCalls != 3 {
		t.Fatalf("sleep calls=%d, want three retry delays before real result", sleepCalls)
	}
	if want := []string{"result", "status", "result", "status", "result", "status", "result", "status"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("RPC order=%#v, want %#v", fake.calls, want)
	}
	if terminalJob.result.Result == nil || terminalJob.result.Result.SHA256 != resultSHA {
		t.Fatalf("terminal result info=%#v, want real result sha %s", terminalJob.result.Result, resultSHA)
	}
}

func TestWaitForTerminalJobResultUsesStatusFallbackImmediatelyForOrphaned(t *testing.T) {
	jobID := "job_orphaned_status_only"
	base := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake := &scriptedPollingClient{
		fakeAgentbusClient: base,
		results: []jobResultStep{
			{err: errors.New("orphaned job has no result")},
		},
		statuses: []client.JobStatusResult{
			{Jobs: []client.JobStatus{{
				JobID:              jobID,
				SessionID:          "session_orphaned_status_only",
				State:              engine.StateOrphaned,
				CleanupDisposition: cleanupDispositionUnresolved,
			}}},
		},
	}
	oldSleep := jobPollSleep
	sleepCalls := 0
	jobPollSleep = func(context.Context, time.Duration) error {
		sleepCalls++
		return errors.New("unexpected sleep after orphaned status fallback")
	}
	defer func() { jobPollSleep = oldSleep }()

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	terminalJob, err := waitForTerminalJobResult(context.Background(), fake, stateDir, jobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sleepCalls != 0 {
		t.Fatalf("sleep calls=%d, want immediate orphaned status fallback", sleepCalls)
	}
	if want := []string{"result", "status"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("RPC order=%#v, want %#v", fake.calls, want)
	}
	if terminalJob.result.State != engine.StateOrphaned || terminalJob.result.Result != nil || terminalJob.result.Contract != nil {
		t.Fatalf("terminal status fallback result=%#v, want orphaned without fabricated result", terminalJob.result)
	}
}

func TestSubmittedTerminalJobStatusFallbackRequiresResultlessTerminal(t *testing.T) {
	resultErr := errors.New("terminal result unavailable")
	for _, tc := range []struct {
		name        string
		state       engine.JobState
		wantErr     bool
		wantReason  string
		wantExit    int
		wantSession string
	}{
		{
			name:    "completed returns result error",
			state:   engine.StateCompleted,
			wantErr: true,
		},
		{
			name:        "orphaned returns status-only terminal",
			state:       engine.StateOrphaned,
			wantReason:  "orphaned_without_result",
			wantExit:    engine.ExitCodeForState(engine.StateOrphaned),
			wantSession: "session_orphaned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "job_submitted_" + strings.ReplaceAll(tc.name, " ", "_")
			sessionID := "session_completed"
			if tc.wantSession != "" {
				sessionID = tc.wantSession
			}
			fake := &fakeAgentbusClient{
				hello:     helloWithCapabilities(),
				resultErr: resultErr,
				status: client.JobStatusResult{Jobs: []client.JobStatus{{
					JobID:     jobID,
					SessionID: sessionID,
					State:     tc.state,
				}}},
			}
			terminalJob, err := submittedTerminalJob(context.Background(), fake, t.TempDir(), jobID, nil)
			if tc.wantErr {
				if !errors.Is(err, resultErr) {
					t.Fatalf("submittedTerminalJob err=%v, want result error %v", err, resultErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if terminalJob.result.State != tc.state || terminalJob.result.SessionID != sessionID || terminalJob.result.Result != nil || terminalJob.result.Contract != nil {
				t.Fatalf("terminal job=%#v, want status-only orphaned", terminalJob.result)
			}
			env, err := terminalEnvelopeFromJobResult(t.TempDir(), terminalJob.result)
			if err != nil {
				t.Fatal(err)
			}
			if env.ResultUnavailableReason != tc.wantReason || engine.ExitCodeForState(env.Status) != tc.wantExit {
				t.Fatalf("terminal envelope=%#v, want reason %q exit %d", env, tc.wantReason, tc.wantExit)
			}
		})
	}
}

type jobResultStep struct {
	result client.JobResult
	err    error
}

type jobStatusStep struct {
	status client.JobStatusResult
	err    error
}

type scriptedPollingClient struct {
	*fakeAgentbusClient
	results     []jobResultStep
	statuses    []client.JobStatusResult
	statusSteps []jobStatusStep
	calls       []string
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
	if c.statusSteps != nil {
		if len(c.statusSteps) == 0 {
			return client.JobStatusResult{}, errors.New("unexpected JobStatus")
		}
		step := c.statusSteps[0]
		c.statusSteps = c.statusSteps[1:]
		return step.status, step.err
	}
	if len(c.statuses) == 0 {
		return client.JobStatusResult{}, errors.New("unexpected JobStatus")
	}
	status := c.statuses[0]
	c.statuses = c.statuses[1:]
	return status, nil
}
