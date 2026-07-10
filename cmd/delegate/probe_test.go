package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestRunStatusProbeFlatWithInjectedProcessSocketAndFS(t *testing.T) {
	restoreProbe := stubProbeOps(t, probeOps{
		Process: sequenceProcess(
			processObservation{Alive: true, CPU: 0, Etime: "00:10", Stat: "S", Raw: "0.0 00:10 S"},
			processObservation{Alive: true, CPU: 0, Etime: "00:11", Stat: "S", Raw: "0.0 00:11 S"},
		),
		Socket: sequenceSocket(
			socketObservation{},
			socketObservation{},
		),
		LogSize: sequenceLog(
			logObservation{Available: true, SizeBytes: 4096},
			logObservation{Available: true, SizeBytes: 4096},
		),
		Sleep: noProbeSleep,
	})
	defer restoreProbe()
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID:           "job_probe",
			State:           engine.StateRunning,
			WorkerPID:       100,
			BackendChildPID: 200,
			LogPaths:        engine.LogPaths{Stdout: "/tmp/out.log"},
		}}},
	}
	restoreAgentbus := stubAgentbusGlobals(t, fake)
	defer restoreAgentbus()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--job", "job_probe", "--probe", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status probe code = %d, stderr = %q", code, stderr.String())
	}
	var result statusProbeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("probe JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if result.Verdict != probeVerdictStalled {
		t.Fatalf("verdict = %q, want %q", result.Verdict, probeVerdictStalled)
	}
	if result.ProbedPID != 200 {
		t.Fatalf("probed pid = %d, want backend child pid 200", result.ProbedPID)
	}
	for _, probe := range result.Probes {
		if probe.Status != probeStatusFlat {
			t.Fatalf("probe %#v status = %q, want flat", probe.Name, probe.Status)
		}
	}
}

func TestRunStatusProbeExpiredLeaseIsImmediateStallSignal(t *testing.T) {
	restoreProbe := stubProbeOps(t, probeOps{
		Process: func(context.Context, int) processObservation {
			t.Fatal("process probe should not run for expired lease")
			return processObservation{}
		},
		Socket: func(context.Context, int) socketObservation {
			t.Fatal("socket probe should not run for expired lease")
			return socketObservation{}
		},
		LogSize: func(engine.LogPaths) logObservation {
			t.Fatal("log probe should not run for expired lease")
			return logObservation{}
		},
		Sleep: noProbeSleep,
	})
	defer restoreProbe()
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID: "job_expired",
			State: engine.StateRunning,
			Lease: &engine.Lease{Expired: true},
		}}},
	}
	restoreAgentbus := stubAgentbusGlobals(t, fake)
	defer restoreAgentbus()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--job", "job_expired", "--probe", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status probe code = %d, stderr = %q", code, stderr.String())
	}
	var result statusProbeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("probe JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if result.Verdict != probeVerdictStalledExpiredLease || !result.LeaseExpired {
		t.Fatalf("probe result = %#v, want expired lease stall", result)
	}
	if len(result.Probes) != 3 {
		t.Fatalf("probes = %d, want skipped triple", len(result.Probes))
	}
}

func TestRunStatusProbeRequiresJob(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--probe"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("status --probe without --job succeeded")
	}
	if !strings.Contains(stderr.String(), "requires --job") {
		t.Fatalf("stderr = %q, want requires --job", stderr.String())
	}
}

func TestLivenessVerdictActiveWhenAnyProbeMoves(t *testing.T) {
	probes, err := runLivenessProbes(context.Background(), client.JobStatus{
		JobID:           "job_active",
		State:           engine.StateRunning,
		BackendChildPID: 42,
		LogPaths:        engine.LogPaths{Stdout: "/tmp/out.log"},
	}, probeOps{
		Process: sequenceProcess(
			processObservation{Alive: true, CPU: 0},
			processObservation{Alive: true, CPU: 0.7},
		),
		Socket: sequenceSocket(socketObservation{}, socketObservation{}),
		LogSize: sequenceLog(
			logObservation{Available: true, SizeBytes: 10},
			logObservation{Available: true, SizeBytes: 10},
		),
		Sleep: noProbeSleep,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := livenessVerdict(probes); got != probeVerdictActive {
		t.Fatalf("verdict = %q, want active", got)
	}
}

func stubProbeOps(t *testing.T, ops probeOps) func() {
	t.Helper()
	oldOps := livenessOps
	oldInterval := probeInterval
	livenessOps = ops
	probeInterval = 0
	return func() {
		livenessOps = oldOps
		probeInterval = oldInterval
	}
}

func sequenceProcess(samples ...processObservation) func(context.Context, int) processObservation {
	index := 0
	return func(context.Context, int) processObservation {
		if index >= len(samples) {
			return samples[len(samples)-1]
		}
		sample := samples[index]
		index++
		return sample
	}
}

func sequenceSocket(samples ...socketObservation) func(context.Context, int) socketObservation {
	index := 0
	return func(context.Context, int) socketObservation {
		if index >= len(samples) {
			return samples[len(samples)-1]
		}
		sample := samples[index]
		index++
		return sample
	}
}

func sequenceLog(samples ...logObservation) func(engine.LogPaths) logObservation {
	index := 0
	return func(engine.LogPaths) logObservation {
		if index >= len(samples) {
			return samples[len(samples)-1]
		}
		sample := samples[index]
		index++
		return sample
	}
}

func noProbeSleep(context.Context, time.Duration) error { return nil }
