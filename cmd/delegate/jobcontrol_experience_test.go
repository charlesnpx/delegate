package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestRunStatusTerminalJobEmitsJobsShapeWithExitCode(t *testing.T) {
	finalAttemptStartedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	finalAttemptEndedAt := finalAttemptStartedAt.Add(time.Minute)
	for _, tc := range []struct {
		name                 string
		job                  client.JobStatus
		wantExitCode         int
		wantTerminalMetadata bool
	}{
		{
			name: "terminal job preserves failure metadata and timing",
			job: client.JobStatus{
				JobID:                 "job_term",
				State:                 engine.StateFailed,
				FailureReason:         "backend exited before producing a result",
				FailureClass:          engine.FailureClassBackendError,
				FinalAttemptStartedAt: &finalAttemptStartedAt,
				FinalAttemptEndedAt:   &finalAttemptEndedAt,
			},
			wantExitCode:         engine.ExitCodeForState(engine.StateFailed),
			wantTerminalMetadata: true,
		},
		{
			name:         "running job remains free of terminal metadata",
			job:          client.JobStatus{JobID: "job_running", State: engine.StateRunning},
			wantExitCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello:  helloWithCapabilities(),
				status: client.JobStatusResult{Jobs: []client.JobStatus{tc.job}},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run([]string{"status", "--job", tc.job.JobID, "--json"}, nil, &stdout, &stderr)
			if code != tc.wantExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, tc.wantExitCode, stderr.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil {
				t.Fatalf("status JSON invalid: %v; raw=%q", err, stdout.String())
			}
			jobs, ok := envelope["jobs"].([]any)
			if !ok || len(jobs) != 1 {
				t.Fatalf("status JSON = %q, want one JobStatusResult job", stdout.String())
			}
			job, ok := jobs[0].(map[string]any)
			if !ok {
				t.Fatalf("status job = %#v, want object", jobs[0])
			}
			if _, flipped := envelope["contract"]; flipped {
				t.Fatalf("status JSON leaked a terminal-envelope contract field: %q", stdout.String())
			}
			if tc.wantTerminalMetadata {
				if job["failureReason"] != tc.job.FailureReason || job["failureClass"] != string(tc.job.FailureClass) || job["finalAttemptStartedAt"] != finalAttemptStartedAt.Format(time.RFC3339) || job["finalAttemptEndedAt"] != finalAttemptEndedAt.Format(time.RFC3339) {
					t.Fatalf("status job=%#v, want failure metadata and final-attempt timing", job)
				}
				return
			}
			for _, field := range []string{"failureReason", "failureClass", "finalAttemptStartedAt", "finalAttemptEndedAt"} {
				if _, found := job[field]; found {
					t.Fatalf("running status job=%#v, unexpectedly contains %q", job, field)
				}
			}
		})
	}
}
