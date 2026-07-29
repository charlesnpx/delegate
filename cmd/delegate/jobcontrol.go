package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

const (
	initialJobPollInterval = 100 * time.Millisecond
	maximumJobPollInterval = 2 * time.Second
)

var jobPollSleep = sleepContext

func runStatus(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jobID := fs.String("job", "", "job id")
	jsonOut := fs.Bool("json", false, "emit JSON")
	wait := fs.Bool("wait", false, "wait for terminal status")
	probe := fs.Bool("probe", false, "run process/socket/log liveness probes for the job (takes ~10-30s by default)")
	probeInterval := fs.Duration("probe-interval", defaultProbeInterval, "duration between liveness samples (minimum 1s)")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate status does not accept positional arguments")
	}
	if *wait && *jobID == "" {
		return 0, fmt.Errorf("delegate status --wait requires --job")
	}
	if *probe && *jobID == "" {
		return 0, fmt.Errorf("delegate status --probe requires --job")
	}
	if *probe && *wait {
		return 0, fmt.Errorf("use only one of --probe or --wait")
	}
	if *probeInterval < minimumProbeInterval {
		return 0, fmt.Errorf("--probe-interval must be at least %s", minimumProbeInterval)
	}
	ctx := context.Background()
	stateRoot, err := agentbusStateRootForJob("", *jobID)
	if err != nil {
		return 0, err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, nil, stateRoot)
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, err)
	}
	defer c.Close()
	cleanupWarnings := newLocalCleanupWarnings(stderr)
	var status client.JobStatusResult
	if *wait {
		status, err = waitForJobStatus(ctx, c, "", *jobID, cleanupWarnings)
	} else {
		status, err = c.JobStatus(ctx, client.JobStatusParams{JobID: *jobID, All: *jobID == ""})
	}
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	if *jobID != "" {
		if job, ok := findJobStatus(status, *jobID); ok {
			if engine.IsTerminal(job.State) && !*probe {
				result, resultErr := c.JobResult(ctx, client.JobResultParams{JobID: *jobID})
				if resultErr != nil {
					result = terminalJobResultFromStatus(job)
					cleanupStatus("", job, cleanupWarnings)
				} else {
					result.CleanupDisposition = cleanupDispositionFromResultAndStatus(result, job, true)
					_ = captureBackendError("", job)
					if err := cleanupJobInput("", result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
						return 0, err
					}
				}
				env, envelopeErr := terminalEnvelopeFromJobResult("", result, hello.Capabilities["models.reported"])
				if envelopeErr != nil {
					return 0, envelopeErr
				}
				if *jsonOut {
					return engine.ExitCodeForState(env.Status), writeJSONLine(stdout, env)
				}
				if env.BackendError != "" {
					_, envelopeErr = fmt.Fprintf(stdout, "%s %s backend_error=%s\n", env.JobID, env.Status, env.BackendError)
				} else {
					_, envelopeErr = fmt.Fprintf(stdout, "%s %s\n", env.JobID, env.Status)
				}
				return engine.ExitCodeForState(env.Status), envelopeErr
			}
		}
	}
	if *probe {
		job, ok := findJobStatus(status, *jobID)
		if !ok {
			return 0, fmt.Errorf("job %q not found", *jobID)
		}
		if _, err := fmt.Fprintln(stderr, probeRuntimeNotice(*probeInterval)); err != nil {
			return 0, err
		}
		probeResult, err := probeJobStatusWithInterval(ctx, job, *probeInterval)
		if err != nil {
			return 0, err
		}
		if *jsonOut {
			return 0, writeJSONLine(stdout, probeResult)
		}
		return 0, writeProbeSummary(stdout, probeResult)
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, status)
	}
	for _, job := range status.Jobs {
		if _, err := fmt.Fprintf(stdout, "%s %s\n", job.JobID, job.State); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func findJobStatus(status client.JobStatusResult, jobID string) (client.JobStatus, bool) {
	for _, job := range status.Jobs {
		if job.JobID == jobID {
			return job, true
		}
	}
	return client.JobStatus{}, false
}

func agentbusStateRootForJob(stateDir, jobID string) (string, error) {
	currentRoot, err := resolveAgentbusStateRoot()
	if err != nil {
		return "", err
	}
	if jobID == "" {
		return currentRoot, nil
	}
	recordedRoot, found, err := recordedAgentbusStateRootForJob(stateDir, jobID)
	if err != nil {
		return "", err
	}
	if found {
		return recordedRoot, nil
	}
	return currentRoot, nil
}

func recordedAgentbusStateRootForJob(stateDir, jobID string) (string, bool, error) {
	if err := validateDelegateJobID(jobID); err != nil {
		return "", false, nil
	}
	meta, found, err := loadJobMetadata(stateDir, jobID)
	if err != nil || !found || meta.AgentbusStateRoot == "" {
		return "", false, err
	}
	root, err := canonicalizeAgentbusStateRoot("job metadata agentbus_state_root", meta.AgentbusStateRoot)
	if err != nil {
		return "", false, err
	}
	return root, true, nil
}

func writeProbeSummary(stdout io.Writer, result statusProbeResult) error {
	if _, err := fmt.Fprintf(stdout, "%s %s verdict=%s\n", result.JobID, result.State, result.Verdict); err != nil {
		return err
	}
	for _, probe := range result.Probes {
		if _, err := fmt.Fprintf(stdout, "%s: %s", probe.Name, probe.Status); err != nil {
			return err
		}
		if probe.Detail != "" {
			if _, err := fmt.Fprintf(stdout, " (%s)", probe.Detail); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			return err
		}
	}
	return nil
}

func runResult(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate result", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jobID := fs.String("job", "", "job id")
	jsonOut := fs.Bool("json", false, "emit JSON")
	wait := fs.Bool("wait", false, "wait for terminal result")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate result does not accept positional arguments")
	}
	if *jobID == "" {
		return 0, fmt.Errorf("delegate result requires --job")
	}
	ctx := context.Background()
	stateRoot, err := agentbusStateRootForJob("", *jobID)
	if err != nil {
		return 0, err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, nil, stateRoot)
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, err)
	}
	defer c.Close()
	cleanupWarnings := newLocalCleanupWarnings(stderr)
	var result client.JobResult
	if *wait {
		result, err = waitForJobResult(ctx, c, "", *jobID, cleanupWarnings)
	} else {
		result, err = c.JobResult(ctx, client.JobResultParams{JobID: *jobID})
		if err != nil {
			result, err = terminalJobResultFallback(ctx, c, "", *jobID, err, cleanupWarnings)
		}
	}
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	statusJob, statusFound := requestedJobStatusForCleanup(ctx, c, "", result.JobID)
	result.CleanupDisposition = cleanupDispositionFromResultAndStatus(result, statusJob, statusFound)
	_ = cleanupJobInput("", result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings)
	env, err := terminalEnvelopeFromJobResult("", result, hello.Capabilities["models.reported"])
	if err != nil {
		return 0, err
	}
	if err := writeJSONLine(stdout, env); err != nil {
		return 0, err
	}
	return engine.ExitCodeForState(env.Status), nil
}

func runCancel(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jobID := fs.String("job", "", "job id")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate cancel does not accept positional arguments")
	}
	if *jobID == "" {
		return 0, fmt.Errorf("delegate cancel requires --job")
	}
	ctx := context.Background()
	stateRoot, err := agentbusStateRootForJob("", *jobID)
	if err != nil {
		return 0, err
	}
	c, _, err := connectAgentbusCommandAtRoot(ctx, nil, stateRoot)
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, err)
	}
	defer c.Close()
	result, err := c.JobCancel(ctx, client.JobCancelParams{JobID: *jobID})
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	cleanupWarnings := newLocalCleanupWarnings(stderr)
	// TODO(D7): fetch status/result after cancel; JobCancelResult carries no cleanupDisposition.
	if err := cleanupJobInput("", result.JobID, "", result.State, "", cleanupWarnings); err != nil {
		return 0, err
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, result)
	}
	_, err = fmt.Fprintf(stdout, "%s %s\n", result.JobID, result.State)
	return 0, err
}

func waitForJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	interval := initialJobPollInterval
	for {
		result, err := c.JobResult(ctx, client.JobResultParams{JobID: jobID})
		if err == nil && engine.IsTerminal(result.State) {
			statusJob, statusFound := requestedJobStatusForCleanup(ctx, c, stateDir, jobID)
			cleanupDisposition := cleanupDispositionFromResultAndStatus(result, statusJob, statusFound)
			if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State, cleanupDisposition, cleanupWarnings); err != nil {
				return client.JobResult{}, err
			}
			return result, nil
		}
		if err != nil {
			result, fallbackErr := terminalJobResultFallback(ctx, c, stateDir, jobID, err, cleanupWarnings)
			if fallbackErr == nil {
				return result, nil
			}
		}
		if err := jobPollSleep(ctx, interval); err != nil {
			return client.JobResult{}, err
		}
		interval = nextJobPollInterval(interval)
	}
}

func submittedTerminalJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	result, resultErr := c.JobResult(ctx, client.JobResultParams{JobID: jobID})
	status, statusErr := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
	var statusJob client.JobStatus
	statusFound := false
	if statusErr == nil {
		if job, found := findJobStatus(status, jobID); found {
			statusJob = job
			statusFound = true
			_ = captureBackendError(stateDir, job)
		}
	}
	if resultErr != nil {
		if statusFound && engine.IsTerminal(statusJob.State) {
			if err := cleanupJobInput(stateDir, statusJob.JobID, statusJob.SessionID, statusJob.State, statusJob.CleanupDisposition, cleanupWarnings); err != nil {
				return client.JobResult{}, err
			}
			return terminalJobResultFromStatus(statusJob), nil
		}
		return client.JobResult{}, resultErr
	}
	if statusFound && result.ModelReported == "" {
		result.ModelReported = statusJob.ModelReported
	}
	result.CleanupDisposition = cleanupDispositionFromResultAndStatus(result, statusJob, statusFound)
	if !engine.IsTerminal(result.State) {
		if statusFound && engine.IsTerminal(statusJob.State) {
			if err := cleanupJobInput(stateDir, statusJob.JobID, statusJob.SessionID, statusJob.State, statusJob.CleanupDisposition, cleanupWarnings); err != nil {
				return client.JobResult{}, err
			}
			return terminalJobResultFromStatus(statusJob), nil
		}
		return client.JobResult{}, fmt.Errorf("submitted job %s reported terminal state but job.result returned %q", jobID, result.State)
	}
	if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
		return client.JobResult{}, err
	}
	return result, nil
}

func nextJobPollInterval(interval time.Duration) time.Duration {
	next := time.Duration(float64(interval) * 1.5)
	if next > maximumJobPollInterval {
		return maximumJobPollInterval
	}
	return next
}

func terminalJobResultFallback(ctx context.Context, c agentbusClient, stateDir, jobID string, resultErr error, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	status, statusErr := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
	if statusErr != nil {
		return client.JobResult{}, resultErr
	}
	cleanupStatuses(stateDir, status, cleanupWarnings)
	job, found := findJobStatus(status, jobID)
	if !found || !engine.IsTerminal(job.State) {
		return client.JobResult{}, resultErr
	}
	return terminalJobResultFromStatus(job), nil
}

func terminalJobResultFromStatus(job client.JobStatus) client.JobResult {
	return client.JobResult{JobID: job.JobID, SessionID: job.SessionID, State: job.State, CleanupDisposition: job.CleanupDisposition, ModelReported: job.ModelReported}
}

func cleanupDispositionFromResultAndStatus(result client.JobResult, statusJob client.JobStatus, statusFound bool) string {
	if result.CleanupDisposition != "" {
		return result.CleanupDisposition
	}
	if statusFound {
		return statusJob.CleanupDisposition
	}
	return ""
}

func waitForJobStatus(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (client.JobStatusResult, error) {
	interval := initialJobPollInterval
	for {
		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if err != nil {
			return client.JobStatusResult{}, err
		}
		cleanupStatuses(stateDir, status, cleanupWarnings)
		for _, job := range status.Jobs {
			if job.JobID == jobID && engine.IsTerminal(job.State) {
				return status, nil
			}
		}
		if err := jobPollSleep(ctx, interval); err != nil {
			return client.JobStatusResult{}, err
		}
		interval = nextJobPollInterval(interval)
	}
}

func cleanupStatuses(stateDir string, status client.JobStatusResult, cleanupWarnings *localCleanupWarnings) {
	for _, job := range status.Jobs {
		cleanupStatus(stateDir, job, cleanupWarnings)
	}
}

func cleanupStatus(stateDir string, job client.JobStatus, cleanupWarnings *localCleanupWarnings) {
	_ = captureBackendError(stateDir, job)
	_ = cleanupJobInput(stateDir, job.JobID, job.SessionID, job.State, job.CleanupDisposition, cleanupWarnings)
}

func requestedJobStatusForCleanup(ctx context.Context, c agentbusClient, stateDir, jobID string) (client.JobStatus, bool) {
	status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
	if err != nil {
		return client.JobStatus{}, false
	}
	if job, found := findJobStatus(status, jobID); found {
		_ = captureBackendError(stateDir, job)
		return job, true
	}
	return client.JobStatus{}, false
}

func sweepTerminalJobInputs(ctx context.Context, c agentbusClient, stateDir string) error {
	removed, terminalSweepErr := handoff.SweepTerminalJobInputs(stateDir, func(jobID string) (engine.JobState, string, bool, error) {
		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if err != nil {
			return "", "", false, err
		}
		job, found := findJobStatus(status, jobID)
		if !found {
			return "", "", false, nil
		}
		return job.State, job.CleanupDisposition, true, nil
	}, handoff.Hooks{})
	sweepErr := terminalSweepErr
	for _, input := range removed {
		sweepErr = errors.Join(sweepErr, cleanupJobInput(stateDir, input.JobID, "", input.State, input.CleanupDisposition, nil))
	}
	return sweepErr
}

func terminalEnvelopeFromJobResult(stateDir string, result client.JobResult, modelsReportedCapable ...bool) (TerminalEnvelope, error) {
	capable := false
	if len(modelsReportedCapable) > 0 {
		capable = modelsReportedCapable[0]
	}
	return terminalEnvelopeFromJobResultWithOptions(stateDir, result, terminalEnvelopeOptions{ModelsReportedCapable: capable})
}

func terminalEnvelopeFromJobResultWithOptions(stateDir string, result client.JobResult, option terminalEnvelopeOptions) (TerminalEnvelope, error) {
	meta, found, err := loadJobMetadata(stateDir, result.JobID)
	if err != nil {
		return TerminalEnvelope{}, err
	}
	kind := taskKind
	contractKind := contractKindShape
	if found {
		if meta.Kind != "" {
			kind = meta.Kind
		}
		if meta.ContractKind != "" {
			contractKind = meta.ContractKind
		}
	}
	stamp := skippedDelegateContractStamp(engine.SkipResultUnavailable)
	if found && meta.NoContract {
		stamp = policy.DisabledStamp()
		contractKind = contractKindNone
	} else if result.Contract != nil {
		stamp = *result.Contract
	} else if contractKind == contractKindNone {
		stamp = policy.DisabledStamp()
	}
	resultSHA256 := ""
	if result.Result != nil {
		resultSHA256 = result.Result.SHA256
	}
	backendError := ""
	modelEffort := config.ModelEffortResolution{}
	origin := envelopeOrigin{}
	if found {
		backendError = meta.BackendError
		modelEffort.Model = meta.Model
		modelEffort.Effort = meta.Effort
		if meta.Origin != nil {
			origin = *meta.Origin
		}
		if option.RequestID == "" {
			option.RequestID = meta.RequestID
		}
		if !option.DeduplicatedSet {
			option.Deduplicated = meta.Deduplicated
			option.DeduplicatedSet = true
		}
	}
	if result.Result == nil && backendError != "" && !(found && meta.NoContract) {
		stamp = skippedDelegateContractStamp(engine.SkipBackendError)
	}
	option.ModelEffort = modelEffort
	option.ModelReported = result.ModelReported
	option.Origin = origin
	return newTerminalEnvelope(result.JobID, result.State, kind, contractKind, stamp, resultSHA256, backendError, option)
}

func skippedDelegateContractStamp(reason engine.SkippedReason) engine.ContractStamp {
	spec, err := policy.DelegateReportSpec()
	if err != nil {
		return engine.SkippedContractStamp(reason, 0, false, "", "")
	}
	hash, err := engine.ContractSHA256(spec)
	if err != nil {
		return engine.SkippedContractStamp(reason, 0, false, "", "")
	}
	return engine.SkippedContractStamp(reason, 0, false, "", hash)
}
