package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestRunStatusProbeFlatWithInjectedProcessSocketAndFS(t *testing.T) {
	restoreProbe := stubProbeOps(t, probeOps{
		ProcessIdentity: matchingProcessIdentity(200, "backend-start"),
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
			Worker:          engine.ProcessRef{PID: 100, StartTime: "worker-start"},
			BackendChild:    engine.ProcessRef{PID: 200, StartTime: "backend-start"},
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

func TestRunStatusProbeMissingLsofIsInconclusive(t *testing.T) {
	restoreCommand := stubProbeCommandOutput(t, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec: lsof: executable file not found")
	})
	defer restoreCommand()
	restoreProbe := stubProbeOps(t, probeOps{
		ProcessIdentity: matchingProcessIdentity(200, "backend-start"),
		Process: sequenceProcess(
			processObservation{Alive: true, CPU: 0},
			processObservation{Alive: true, CPU: 0},
		),
		LogSize: sequenceLog(
			logObservation{Available: true, SizeBytes: 4096},
			logObservation{Available: true, SizeBytes: 4096},
		),
		Sleep: noProbeSleep,
	})
	defer restoreProbe()

	result, err := probeJobStatus(context.Background(), verifiedProbeJob())
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != probeVerdictInconclusive {
		t.Fatalf("verdict = %q, want %q; result = %#v", result.Verdict, probeVerdictInconclusive, result)
	}
	if !strings.Contains(result.VerdictReason, "network") {
		t.Fatalf("verdict reason = %q, want network unknown", result.VerdictReason)
	}
	assertProbeStatus(t, result.Probes, "network", probeStatusUnknown)
}

func TestRunStatusProbeMissingProcessStartTimeIsInconclusive(t *testing.T) {
	restoreProbe := stubProbeOps(t, probeOps{
		ProcessIdentity: func(context.Context, int) processIdentityObservation {
			t.Fatal("process identity should not be read without a recorded start-time")
			return processIdentityObservation{}
		},
		Process: func(context.Context, int) processObservation {
			t.Fatal("process probe should not sample without a recorded start-time")
			return processObservation{}
		},
		Socket: func(context.Context, int) socketObservation {
			t.Fatal("socket probe should not sample without a recorded start-time")
			return socketObservation{}
		},
		LogSize: sequenceLog(
			logObservation{Available: true, SizeBytes: 4096},
			logObservation{Available: true, SizeBytes: 4096},
		),
		Sleep: noProbeSleep,
	})
	defer restoreProbe()

	job := verifiedProbeJob()
	job.BackendChild = engine.ProcessRef{}
	result, err := probeJobStatus(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != probeVerdictInconclusive {
		t.Fatalf("verdict = %q, want %q; result = %#v", result.Verdict, probeVerdictInconclusive, result)
	}
	if !strings.Contains(result.VerdictReason, "process") || !strings.Contains(result.VerdictReason, "network") {
		t.Fatalf("verdict reason = %q, want process and network unknown", result.VerdictReason)
	}
	assertProbeStatus(t, result.Probes, "process", probeStatusUnknown)
	assertProbeStatus(t, result.Probes, "network", probeStatusUnknown)
	assertProbeStatus(t, result.Probes, "log_size", probeStatusFlat)
}

func TestRunStatusProbePIDReuseIsInconclusive(t *testing.T) {
	restoreProbe := stubProbeOps(t, probeOps{
		ProcessIdentity: func(context.Context, int) processIdentityObservation {
			return processIdentityObservation{Alive: true, StartTime: "different-start"}
		},
		Process: func(context.Context, int) processObservation {
			t.Fatal("process probe should not sample a reused pid")
			return processObservation{}
		},
		Socket: func(context.Context, int) socketObservation {
			t.Fatal("socket probe should not sample a reused pid")
			return socketObservation{}
		},
		LogSize: sequenceLog(
			logObservation{Available: true, SizeBytes: 4096},
			logObservation{Available: true, SizeBytes: 4096},
		),
		Sleep: noProbeSleep,
	})
	defer restoreProbe()

	result, err := probeJobStatus(context.Background(), verifiedProbeJob())
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != probeVerdictInconclusive {
		t.Fatalf("verdict = %q, want %q; result = %#v", result.Verdict, probeVerdictInconclusive, result)
	}
	assertProbeStatus(t, result.Probes, "process", probeStatusUnknown)
	assertProbeStatus(t, result.Probes, "network", probeStatusUnknown)
	assertProbeStatus(t, result.Probes, "log_size", probeStatusFlat)
}

func TestLivenessVerdictActiveWhenAnyProbeMoves(t *testing.T) {
	probes, err := runLivenessProbes(context.Background(), client.JobStatus{
		JobID:           "job_active",
		State:           engine.StateRunning,
		BackendChildPID: 42,
		BackendChild:    engine.ProcessRef{PID: 42, StartTime: "backend-start"},
		LogPaths:        engine.LogPaths{Stdout: "/tmp/out.log"},
	}, probeOps{
		ProcessIdentity: matchingProcessIdentity(42, "backend-start"),
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

func TestLivenessVerdictUnknownNeverStalled(t *testing.T) {
	verdict, reason := livenessVerdictReason([]livenessProbe{
		{Name: "process", Status: probeStatusFlat},
		{Name: "network", Status: probeStatusUnknown},
		{Name: "log_size", Status: probeStatusFlat},
	})
	if verdict != probeVerdictInconclusive {
		t.Fatalf("verdict = %q, want %q", verdict, probeVerdictInconclusive)
	}
	if !strings.Contains(reason, "network") {
		t.Fatalf("reason = %q, want network unknown", reason)
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

func matchingProcessIdentity(wantPID int, startTime string) func(context.Context, int) processIdentityObservation {
	return func(_ context.Context, pid int) processIdentityObservation {
		if pid != wantPID {
			return processIdentityObservation{Err: "unexpected pid"}
		}
		return processIdentityObservation{Alive: true, StartTime: startTime}
	}
}

func verifiedProbeJob() client.JobStatus {
	return client.JobStatus{
		JobID:           "job_probe",
		State:           engine.StateRunning,
		WorkerPID:       100,
		BackendChildPID: 200,
		Worker:          engine.ProcessRef{PID: 100, StartTime: "worker-start"},
		BackendChild:    engine.ProcessRef{PID: 200, StartTime: "backend-start"},
		LogPaths:        engine.LogPaths{Stdout: "/tmp/out.log"},
	}
}

func assertProbeStatus(t *testing.T, probes []livenessProbe, name, want string) {
	t.Helper()
	for _, probe := range probes {
		if probe.Name != name {
			continue
		}
		if probe.Status != want {
			t.Fatalf("%s status = %q, want %q; probe = %#v", name, probe.Status, want, probe)
		}
		return
	}
	t.Fatalf("probe %q missing from %#v", name, probes)
}

func stubProbeCommandOutput(t *testing.T, command func(context.Context, string, ...string) ([]byte, error)) func() {
	t.Helper()
	oldCommand := probeCommandOutput
	probeCommandOutput = command
	return func() {
		probeCommandOutput = oldCommand
	}
}

func noProbeSleep(context.Context, time.Duration) error { return nil }
