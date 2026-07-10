package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

const (
	probeStatusActive  = "active"
	probeStatusFlat    = "flat"
	probeStatusUnknown = "unknown"

	probeVerdictActive              = "active"
	probeVerdictStalled             = "stalled"
	probeVerdictStalledExpiredLease = "stalled_expired_lease"
	probeVerdictInconclusive        = "inconclusive"
	probeVerdictTerminal            = "terminal"
)

type statusProbeResult struct {
	Schema          int             `json:"schema"`
	JobID           string          `json:"job_id"`
	State           engine.JobState `json:"state"`
	LastKnownPhase  engine.JobState `json:"last_known_phase"`
	WorkerPID       int             `json:"worker_pid,omitempty"`
	BackendChildPID int             `json:"backend_child_pid,omitempty"`
	ProbedPID       int             `json:"probed_pid,omitempty"`
	LogPaths        engine.LogPaths `json:"log_paths,omitempty"`
	LeaseExpired    bool            `json:"lease_expired"`
	Probes          []livenessProbe `json:"probes"`
	Verdict         string          `json:"verdict"`
	VerdictReason   string          `json:"verdict_reason,omitempty"`
}

type livenessProbe struct {
	Name    string        `json:"name"`
	Status  string        `json:"status"`
	Detail  string        `json:"detail,omitempty"`
	Samples []probeSample `json:"samples,omitempty"`
}

type probeSample struct {
	Index       int      `json:"index"`
	Alive       *bool    `json:"alive,omitempty"`
	CPU         *float64 `json:"cpu,omitempty"`
	Etime       string   `json:"etime,omitempty"`
	Stat        string   `json:"stat,omitempty"`
	Established *bool    `json:"established,omitempty"`
	SizeBytes   *int64   `json:"size_bytes,omitempty"`
	Raw         string   `json:"raw,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type processObservation struct {
	Alive bool
	CPU   float64
	Etime string
	Stat  string
	Raw   string
	Err   string
}

type processIdentityObservation struct {
	Alive     bool
	StartTime string
	Err       string
}

type socketObservation struct {
	Established bool
	Raw         string
	Err         string
}

type logObservation struct {
	Available bool
	SizeBytes int64
	Err       string
}

type probeOps struct {
	ProcessIdentity func(context.Context, int) processIdentityObservation
	Process         func(context.Context, int) processObservation
	Socket          func(context.Context, int) socketObservation
	LogSize         func(engine.LogPaths) logObservation
	Sleep           func(context.Context, time.Duration) error
}

var (
	probeInterval = 60 * time.Second
	livenessOps   = probeOps{
		ProcessIdentity: realProcessIdentityObservation,
		Process:         realProcessObservation,
		Socket:          realSocketObservation,
		LogSize:         realLogObservation,
		Sleep:           sleepContext,
	}
	probeCommandOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

func probeJobStatus(ctx context.Context, job client.JobStatus) (statusProbeResult, error) {
	pid := job.BackendChildPID
	if pid == 0 {
		pid = job.WorkerPID
	}
	result := statusProbeResult{
		Schema:          envelopeSchema,
		JobID:           job.JobID,
		State:           job.State,
		LastKnownPhase:  job.State,
		WorkerPID:       job.WorkerPID,
		BackendChildPID: job.BackendChildPID,
		ProbedPID:       pid,
		LogPaths:        job.LogPaths,
	}
	if job.Lease != nil {
		result.LeaseExpired = job.Lease.Expired
	}
	if engine.IsTerminal(job.State) {
		result.Verdict = probeVerdictTerminal
		return result, nil
	}
	if result.LeaseExpired {
		result.Probes = skippedLeaseProbes()
		result.Verdict = probeVerdictStalledExpiredLease
		return result, nil
	}
	probes, err := runLivenessProbes(ctx, job, livenessOps, probeInterval)
	if err != nil {
		return statusProbeResult{}, err
	}
	result.Probes = probes
	result.Verdict, result.VerdictReason = livenessVerdictReason(probes)
	return result, nil
}

func skippedLeaseProbes() []livenessProbe {
	return []livenessProbe{
		{Name: "process", Status: probeStatusUnknown, Detail: "skipped because the heartbeat lease is already expired"},
		{Name: "network", Status: probeStatusUnknown, Detail: "skipped because the heartbeat lease is already expired"},
		{Name: "log_size", Status: probeStatusUnknown, Detail: "skipped because the heartbeat lease is already expired"},
	}
}

func runLivenessProbes(ctx context.Context, job client.JobStatus, ops probeOps, interval time.Duration) ([]livenessProbe, error) {
	if ops.ProcessIdentity == nil {
		ops.ProcessIdentity = realProcessIdentityObservation
	}
	if ops.Process == nil {
		ops.Process = realProcessObservation
	}
	if ops.Socket == nil {
		ops.Socket = realSocketObservation
	}
	if ops.LogSize == nil {
		ops.LogSize = realLogObservation
	}
	if ops.Sleep == nil {
		ops.Sleep = sleepContext
	}
	pid := job.BackendChildPID
	if pid == 0 {
		pid = job.WorkerPID
	}
	identity, ok, detail := verifyProcessIdentity(ctx, ops, job, pid)
	var firstProcess processObservation
	var firstSocket socketObservation
	if ok && identity.Alive {
		firstProcess = observeProcess(ctx, ops, pid)
		firstSocket = observeSocket(ctx, ops, pid)
	}
	firstLog := observeLog(ops, job.LogPaths)
	if err := ops.Sleep(ctx, interval); err != nil {
		return nil, err
	}
	secondLog := observeLog(ops, job.LogPaths)
	if !ok {
		return []livenessProbe{
			unknownPIDProbe("process", detail),
			unknownPIDProbe("network", detail),
			logProbe(firstLog, secondLog),
		}, nil
	}
	if !identity.Alive {
		missingProcess := processObservation{Alive: false}
		missingSocket := socketObservation{}
		return []livenessProbe{
			processProbe(missingProcess, missingProcess, pid),
			socketProbe(missingSocket, missingSocket, pid),
			logProbe(firstLog, secondLog),
		}, nil
	}
	secondIdentity, secondOK, secondDetail := verifyProcessIdentity(ctx, ops, job, pid)
	if !secondOK {
		return []livenessProbe{
			unknownPIDProbe("process", secondDetail),
			unknownPIDProbe("network", secondDetail),
			logProbe(firstLog, secondLog),
		}, nil
	}
	if !secondIdentity.Alive {
		missingProcess := processObservation{Alive: false}
		missingSocket := socketObservation{}
		return []livenessProbe{
			processProbe(firstProcess, missingProcess, pid),
			socketProbe(firstSocket, missingSocket, pid),
			logProbe(firstLog, secondLog),
		}, nil
	}
	secondProcess := observeProcess(ctx, ops, pid)
	secondSocket := observeSocket(ctx, ops, pid)
	return []livenessProbe{
		processProbe(firstProcess, secondProcess, pid),
		socketProbe(firstSocket, secondSocket, pid),
		logProbe(firstLog, secondLog),
	}, nil
}

func verifyProcessIdentity(ctx context.Context, ops probeOps, job client.JobStatus, pid int) (processIdentityObservation, bool, string) {
	if pid <= 0 {
		return processIdentityObservation{}, false, "no child pid recorded"
	}
	expectedStartTime, ok := expectedProcessStartTime(job, pid)
	if !ok {
		return processIdentityObservation{}, false, "process start-time is missing from job status"
	}
	identity := ops.ProcessIdentity(ctx, pid)
	if identity.Err != "" {
		return identity, false, "process start-time could not be verified: " + identity.Err
	}
	if !identity.Alive {
		return identity, true, ""
	}
	if identity.StartTime == "" {
		return identity, false, "process start-time is unavailable from the host"
	}
	if identity.StartTime != expectedStartTime {
		return identity, false, "process start-time does not match job status"
	}
	return identity, true, ""
}

func expectedProcessStartTime(job client.JobStatus, pid int) (string, bool) {
	if job.BackendChildPID == pid {
		return job.BackendChildStartTime, job.BackendChildStartTime != ""
	}
	if job.WorkerPID == pid {
		return job.WorkerStartTime, job.WorkerStartTime != ""
	}
	return "", false
}

func observeProcess(ctx context.Context, ops probeOps, pid int) processObservation {
	if pid <= 0 {
		return processObservation{Err: "no child pid recorded"}
	}
	return ops.Process(ctx, pid)
}

func observeSocket(ctx context.Context, ops probeOps, pid int) socketObservation {
	if pid <= 0 {
		return socketObservation{Err: "no child pid recorded"}
	}
	return ops.Socket(ctx, pid)
}

func observeLog(ops probeOps, paths engine.LogPaths) logObservation {
	if paths.Stdout == "" && paths.Stderr == "" {
		return logObservation{Err: "no captured log paths recorded"}
	}
	return ops.LogSize(paths)
}

func unknownPIDProbe(name, detail string) livenessProbe {
	return livenessProbe{Name: name, Status: probeStatusUnknown, Detail: detail}
}

func processProbe(first, second processObservation, pid int) livenessProbe {
	samples := []probeSample{processSample(1, first), processSample(2, second)}
	if pid <= 0 {
		return livenessProbe{Name: "process", Status: probeStatusUnknown, Detail: "no child pid recorded", Samples: samples}
	}
	if !first.Alive && !second.Alive {
		return livenessProbe{Name: "process", Status: probeStatusFlat, Detail: "process is not present", Samples: samples}
	}
	if first.CPU > 0 || second.CPU > 0 {
		return livenessProbe{Name: "process", Status: probeStatusActive, Detail: "CPU activity observed across process samples", Samples: samples}
	}
	if first.Alive || second.Alive {
		return livenessProbe{Name: "process", Status: probeStatusFlat, Detail: "process stayed alive but CPU remained flat", Samples: samples}
	}
	return livenessProbe{Name: "process", Status: probeStatusUnknown, Detail: "process activity could not be determined", Samples: samples}
}

func socketProbe(first, second socketObservation, pid int) livenessProbe {
	samples := []probeSample{socketSample(1, first), socketSample(2, second)}
	if pid <= 0 {
		return livenessProbe{Name: "network", Status: probeStatusUnknown, Detail: "no child pid recorded", Samples: samples}
	}
	if first.Established || second.Established {
		return livenessProbe{Name: "network", Status: probeStatusActive, Detail: "established TCP socket observed", Samples: samples}
	}
	if first.Err != "" || second.Err != "" {
		return livenessProbe{Name: "network", Status: probeStatusUnknown, Detail: "socket probe returned errors", Samples: samples}
	}
	return livenessProbe{Name: "network", Status: probeStatusFlat, Detail: "no established TCP socket observed", Samples: samples}
}

func logProbe(first, second logObservation) livenessProbe {
	samples := []probeSample{logSample(1, first), logSample(2, second)}
	if !first.Available && !second.Available {
		return livenessProbe{Name: "log_size", Status: probeStatusUnknown, Detail: "captured log paths are unavailable", Samples: samples}
	}
	if second.SizeBytes > first.SizeBytes {
		return livenessProbe{Name: "log_size", Status: probeStatusActive, Detail: "captured log size increased over the probe interval", Samples: samples}
	}
	if second.SizeBytes < first.SizeBytes {
		return livenessProbe{Name: "log_size", Status: probeStatusActive, Detail: "captured log size changed over the probe interval", Samples: samples}
	}
	return livenessProbe{Name: "log_size", Status: probeStatusFlat, Detail: "captured log size was flat over the probe interval", Samples: samples}
}

func processSample(index int, obs processObservation) probeSample {
	alive := obs.Alive
	cpu := obs.CPU
	return probeSample{
		Index: index,
		Alive: &alive,
		CPU:   &cpu,
		Etime: obs.Etime,
		Stat:  obs.Stat,
		Raw:   obs.Raw,
		Error: obs.Err,
	}
}

func socketSample(index int, obs socketObservation) probeSample {
	established := obs.Established
	return probeSample{
		Index:       index,
		Established: &established,
		Raw:         obs.Raw,
		Error:       obs.Err,
	}
}

func logSample(index int, obs logObservation) probeSample {
	size := obs.SizeBytes
	return probeSample{Index: index, SizeBytes: &size, Error: obs.Err}
}

func livenessVerdict(probes []livenessProbe) string {
	verdict, _ := livenessVerdictReason(probes)
	return verdict
}

func livenessVerdictReason(probes []livenessProbe) (string, string) {
	if len(probes) == 0 {
		return probeVerdictInconclusive, "no liveness probes ran"
	}
	allFlat := true
	var unknown []string
	for _, probe := range probes {
		if probe.Status == probeStatusActive {
			return probeVerdictActive, ""
		}
		if probe.Status == probeStatusUnknown {
			unknown = append(unknown, probe.Name)
		}
		if probe.Status != probeStatusFlat {
			allFlat = false
		}
	}
	if len(unknown) > 0 {
		return probeVerdictInconclusive, "unknown probes: " + strings.Join(unknown, ", ")
	}
	if allFlat && hasRequiredFlatProbes(probes) {
		return probeVerdictStalled, ""
	}
	return probeVerdictInconclusive, "stalled verdict requires process, network, and log_size probes to be flat"
}

func hasRequiredFlatProbes(probes []livenessProbe) bool {
	required := map[string]bool{
		"process":  false,
		"network":  false,
		"log_size": false,
	}
	for _, probe := range probes {
		if _, ok := required[probe.Name]; ok && probe.Status == probeStatusFlat {
			required[probe.Name] = true
		}
	}
	for _, ok := range required {
		if !ok {
			return false
		}
	}
	return true
}

func realProcessIdentityObservation(ctx context.Context, pid int) processIdentityObservation {
	select {
	case <-ctx.Done():
		return processIdentityObservation{Err: ctx.Err().Error()}
	default:
	}
	info, alive, err := (engine.NativeProcessTable{}).Lookup(pid)
	if err != nil {
		return processIdentityObservation{Err: err.Error()}
	}
	return processIdentityObservation{Alive: alive, StartTime: info.StartTime}
}

func realProcessObservation(ctx context.Context, pid int) processObservation {
	out, err := probeCommandOutput(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "%cpu=,etime=,stat=")
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		obs := processObservation{Alive: false}
		if err != nil {
			obs.Err = err.Error()
		}
		return obs
	}
	fields := strings.Fields(raw)
	obs := processObservation{Alive: true, Raw: raw}
	if len(fields) > 0 {
		if cpu, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil {
			obs.CPU = cpu
		} else {
			obs.Err = parseErr.Error()
		}
	}
	if len(fields) > 1 {
		obs.Etime = fields[1]
	}
	if len(fields) > 2 {
		obs.Stat = fields[2]
	}
	if err != nil && obs.Err == "" {
		obs.Err = err.Error()
	}
	return obs
}

func realSocketObservation(ctx context.Context, pid int) socketObservation {
	out, err := probeCommandOutput(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:ESTABLISHED")
	raw := strings.TrimSpace(string(out))
	obs := socketObservation{Raw: raw}
	if err != nil {
		var exitErr *exec.ExitError
		if raw == "" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return obs
		}
		obs.Err = err.Error()
		return obs
	}
	if raw != "" && strings.Contains(raw, "TCP") {
		obs.Established = true
	}
	return obs
}

func realLogObservation(paths engine.LogPaths) logObservation {
	var total int64
	var available bool
	var errs []string
	for _, path := range []string{paths.Stdout, paths.Stderr} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if info.IsDir() {
			errs = append(errs, fmt.Sprintf("%s: is a directory", path))
			continue
		}
		available = true
		total += info.Size()
	}
	return logObservation{Available: available, SizeBytes: total, Err: strings.Join(errs, "; ")}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
