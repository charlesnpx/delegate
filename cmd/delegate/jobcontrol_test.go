package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
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
