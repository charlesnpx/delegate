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
	"github.com/charlesnpx/delegate/internal/policy"
)

const (
	initialJobPollInterval                          = 100 * time.Millisecond
	maximumJobPollInterval                          = 2 * time.Second
	maxRetryableJobResultErrorsBeforeStatusFallback = 5
	// Bounds permanent agentbus fail-stop polling while leaving room for ordinary daemon restarts.
	maxConsecutiveTransportFailures = 25
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
	stateRoot, err := agentbusStateRootForJob("", *jobID, stderr, true)
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
				terminalJob := terminalJobResultFromResultAndStatus(result, job, true)
				if resultErr != nil {
					cleanupStatus("", job, cleanupWarnings)
					if !terminalStatusDoesNotExpectJobResult(job) {
						return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(resultErr))
					}
					terminalJob = terminalJobResultFromStatus(job)
				} else {
					result = terminalJob.result
					_ = captureBackendError("", job)
					if err := cleanupJobInput("", result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
						return 0, err
					}
					terminalJob.result = result
				}
				env, envelopeErr := terminalEnvelopeFromJobResultWithOptions("", terminalJob.result, terminalJob.envelopeOptions(terminalEnvelopeOptions{ModelsReportedCapable: hello.Capabilities["models.reported"]}))
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

func agentbusStateRootForJob(stateDir, jobID string, stderr io.Writer, allowCorruptRootFallback bool) (string, error) {
	if jobID != "" {
		recordedRoot, found, err := recordedAgentbusStateRootForJob(stateDir, jobID)
		if err != nil {
			if !allowCorruptRootFallback {
				return "", fmt.Errorf("use recorded AgentBus state root for job %q: %w", jobID, err)
			}
			stateRoot, resolveErr := resolveAgentbusStateRoot()
			if resolveErr != nil {
				return "", resolveErr
			}
			if stderr != nil {
				if _, warnErr := fmt.Fprintf(stderr, "warning: delegate could not use recorded AgentBus state root for job %q; using default resolved AgentBus state root instead, so status/result may report a different same-ID job or not find the intended job: %v\n", jobID, err); warnErr != nil {
					return "", warnErr
				}
			}
			return stateRoot, nil
		}
		if found {
			return recordedRoot, nil
		}
	}
	return resolveAgentbusStateRoot()
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
	if _, err := fmt.Fprintf(stdout, "%s %s authority_state=%s verdict=%s", result.JobID, result.State, result.AuthorityState, result.Verdict); err != nil {
		return err
	}
	if result.CleanupDisposition != "" {
		if _, err := fmt.Fprintf(stdout, " cleanup_disposition=%s", result.CleanupDisposition); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return err
	}
	for _, warning := range result.AuthorityWarnings {
		if _, err := fmt.Fprintf(stdout, "authority_warning: %s\n", warning); err != nil {
			return err
		}
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
	stateRoot, err := agentbusStateRootForJob("", *jobID, stderr, true)
	if err != nil {
		return 0, err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, nil, stateRoot)
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, err)
	}
	defer c.Close()
	cleanupWarnings := newLocalCleanupWarnings(stderr)
	var terminalJob terminalJobResult
	if *wait {
		terminalJob, err = waitForTerminalJobResult(ctx, c, "", *jobID, cleanupWarnings)
	} else {
		result, resultErr := c.JobResult(ctx, client.JobResultParams{JobID: *jobID})
		err = resultErr
		if err != nil {
			terminalJob, err = terminalJobResultFallbackWithStatus(ctx, c, "", *jobID, err, cleanupWarnings)
		} else {
			statusJob, statusFound := requestedJobStatusForCleanup(ctx, c, "", result.JobID)
			terminalJob = terminalJobResultFromResultAndStatus(result, statusJob, statusFound)
			result = terminalJob.result
			_ = cleanupJobInput("", result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings)
			terminalJob.result = result
		}
	}
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	env, err := terminalEnvelopeFromJobResultWithOptions("", terminalJob.result, terminalJob.envelopeOptions(terminalEnvelopeOptions{ModelsReportedCapable: hello.Capabilities["models.reported"]}))
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
	// SAFETY INVARIANT: cancel is a MUTATING op and must pass allowCorruptRootFallback=false.
	// AgentBus job IDs are sequential per state root (job-%020d), so identical IDs recur
	// across roots; falling back to the default root on corrupt recorded metadata could
	// cancel an UNRELATED same-ID job. Keep this false (status/result may use true because
	// they are read-only). Do not flip without cross-root identity verification.
	stateRoot, err := agentbusStateRootForJob("", *jobID, stderr, false)
	if err != nil {
		return 0, err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, nil, stateRoot)
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, err)
	}
	defer c.Close()
	result, err := cancelJobWithObservedStatus(ctx, c, "", *jobID, newLocalCleanupWarnings(stderr))
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	if result.terminal != nil {
		env, err := terminalEnvelopeFromJobResultWithOptions("", result.terminal.result, result.terminal.envelopeOptions(terminalEnvelopeOptions{ModelsReportedCapable: hello.Capabilities["models.reported"]}))
		if err != nil {
			return 0, err
		}
		if *jsonOut {
			return engine.ExitCodeForState(env.Status), writeJSONLine(stdout, env)
		}
		return engine.ExitCodeForState(env.Status), writeTerminalSummary(stdout, env)
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, result.status)
	}
	return 0, writeJobStatusLines(stdout, result.status)
}

type canceledJobObservation struct {
	status   client.JobStatusResult
	terminal *terminalJobResult
}

func cancelJobWithObservedStatus(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (canceledJobObservation, error) {
	canceled, err := c.JobCancel(ctx, client.JobCancelParams{JobID: jobID})
	if err != nil {
		return canceledJobObservation{}, err
	}
	effectiveJobID := canceled.JobID
	if effectiveJobID == "" {
		effectiveJobID = jobID
	}
	status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: effectiveJobID})
	if err != nil {
		return canceledJobObservation{}, err
	}
	job, found := findJobStatus(status, effectiveJobID)
	if !found || !engine.IsTerminal(job.State) {
		cleanupStatuses(stateDir, status, cleanupWarnings)
		return canceledJobObservation{status: status}, nil
	}
	terminalJob, err := terminalJobFromTerminalStatus(ctx, c, stateDir, job, cleanupWarnings)
	if err != nil {
		return canceledJobObservation{}, err
	}
	return canceledJobObservation{status: status, terminal: &terminalJob}, nil
}

type terminalJobResult struct {
	result      client.JobResult
	statusJob   client.JobStatus
	statusFound bool
}

func terminalJobResultFromResultAndStatus(result client.JobResult, statusJob client.JobStatus, statusFound bool) terminalJobResult {
	result.CleanupDisposition = cleanupDispositionFromResultAndStatus(result, statusJob, statusFound)
	if statusFound {
		if result.ModelReported == "" {
			result.ModelReported = statusJob.ModelReported
		}
		result.LateFinalization = result.LateFinalization || statusJob.LateFinalization
	}
	return terminalJobResult{result: result, statusJob: statusJob, statusFound: statusFound}
}

func terminalJobResultFromStatus(job client.JobStatus) terminalJobResult {
	return terminalJobResultFromResultAndStatus(terminalJobResultEnvelopeInputFromStatus(job), job, true)
}

func (result terminalJobResult) envelopeOptions(option terminalEnvelopeOptions) terminalEnvelopeOptions {
	if option.CleanupDisposition == "" {
		option.CleanupDisposition = result.result.CleanupDisposition
	}
	option.LateFinalization = option.LateFinalization || result.result.LateFinalization
	if result.statusFound {
		if option.CleanupDisposition == "" {
			option.CleanupDisposition = result.statusJob.CleanupDisposition
		}
		option.LateFinalization = option.LateFinalization || result.statusJob.LateFinalization
		option.AgentbusWarnings = append([]string(nil), result.statusJob.Warnings...)
	}
	return option
}

func waitForTerminalJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (terminalJobResult, error) {
	interval := initialJobPollInterval
	consecutiveRetryableResultErrors := 0
	consecutiveTransportFailures := 0
	for {
		result, err := c.JobResult(ctx, client.JobResultParams{JobID: jobID})
		if err == nil {
			consecutiveRetryableResultErrors = 0
			consecutiveTransportFailures = 0
		}
		if err == nil && engine.IsTerminal(result.State) {
			statusJob, statusFound := requestedJobStatusForCleanup(ctx, c, stateDir, jobID)
			terminalJob := terminalJobResultFromResultAndStatus(result, statusJob, statusFound)
			result = terminalJob.result
			if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
				return terminalJobResult{}, err
			}
			terminalJob.result = result
			return terminalJob, nil
		}
		if err != nil {
			retry, pollErr := retryableJobPollError(err)
			if !retry {
				return terminalJobResult{}, pollErr
			}
			consecutiveRetryableResultErrors++
			status, statusErr := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
			if statusErr != nil {
				statusRetry, statusPollErr := retryableJobPollError(statusErr)
				if !statusRetry {
					return terminalJobResult{}, statusPollErr
				}
				consecutiveTransportFailures++
				if consecutiveTransportFailures >= maxConsecutiveTransportFailures {
					return terminalJobResult{}, agentbusOperationError(statusErr)
				}
			} else {
				consecutiveTransportFailures = 0
				cleanupStatuses(stateDir, status, cleanupWarnings)
				if job, found := findJobStatus(status, jobID); found && engine.IsTerminal(job.State) {
					if terminalStatusDoesNotExpectJobResult(job) {
						return terminalJobResultFromStatus(job), nil
					}
					if consecutiveRetryableResultErrors >= maxRetryableJobResultErrorsBeforeStatusFallback {
						return terminalJobResult{}, err
					}
				}
			}
		}
		if err := jobPollSleep(ctx, interval); err != nil {
			return terminalJobResult{}, err
		}
		interval = nextJobPollInterval(interval)
	}
}

func terminalStatusDoesNotExpectJobResult(job client.JobStatus) bool {
	return engine.IsTerminal(job.State) && job.State == engine.StateOrphaned
}

func retryableJobPollError(err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	opErr := agentbusOperationError(err)
	classification, ok := classifyAgentbusError(opErr)
	if !ok {
		return false, opErr
	}
	if classification.Code == agentbusErrorTransport {
		return true, nil
	}
	if classification.Code == agentbusErrorBackendUnavailable && classification.AdmissionCause == "" {
		return true, nil
	}
	return false, opErr
}

func waitForJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	result, err := waitForTerminalJobResult(ctx, c, stateDir, jobID, cleanupWarnings)
	return result.result, err
}

func submittedTerminalJob(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (terminalJobResult, error) {
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
				return terminalJobResult{}, err
			}
			if !terminalStatusDoesNotExpectJobResult(statusJob) {
				return terminalJobResult{}, resultErr
			}
			return terminalJobResultFromStatus(statusJob), nil
		}
		return terminalJobResult{}, resultErr
	}
	terminalJob := terminalJobResultFromResultAndStatus(result, statusJob, statusFound)
	result = terminalJob.result
	if !engine.IsTerminal(result.State) {
		if statusFound && engine.IsTerminal(statusJob.State) {
			if err := cleanupJobInput(stateDir, statusJob.JobID, statusJob.SessionID, statusJob.State, statusJob.CleanupDisposition, cleanupWarnings); err != nil {
				return terminalJobResult{}, err
			}
			if terminalStatusDoesNotExpectJobResult(statusJob) {
				return terminalJobResultFromStatus(statusJob), nil
			}
		}
		return terminalJobResult{}, fmt.Errorf("submitted job %s reported terminal state but job.result returned %q", jobID, result.State)
	}
	if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
		return terminalJobResult{}, err
	}
	terminalJob.result = result
	return terminalJob, nil
}

func submittedTerminalJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	result, err := submittedTerminalJob(ctx, c, stateDir, jobID, cleanupWarnings)
	return result.result, err
}

func nextJobPollInterval(interval time.Duration) time.Duration {
	next := time.Duration(float64(interval) * 1.5)
	if next > maximumJobPollInterval {
		return maximumJobPollInterval
	}
	return next
}

func terminalJobResultFallbackWithStatus(ctx context.Context, c agentbusClient, stateDir, jobID string, resultErr error, cleanupWarnings *localCleanupWarnings) (terminalJobResult, error) {
	status, statusErr := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
	if statusErr != nil {
		return terminalJobResult{}, resultErr
	}
	cleanupStatuses(stateDir, status, cleanupWarnings)
	job, found := findJobStatus(status, jobID)
	if !found || !engine.IsTerminal(job.State) {
		return terminalJobResult{}, resultErr
	}
	if !terminalStatusDoesNotExpectJobResult(job) {
		return terminalJobResult{}, resultErr
	}
	return terminalJobResultFromStatus(job), nil
}

func terminalJobFromTerminalStatus(ctx context.Context, c agentbusClient, stateDir string, job client.JobStatus, cleanupWarnings *localCleanupWarnings) (terminalJobResult, error) {
	result, err := c.JobResult(ctx, client.JobResultParams{JobID: job.JobID})
	if err != nil || !engine.IsTerminal(result.State) {
		if err := cleanupJobInput(stateDir, job.JobID, job.SessionID, job.State, job.CleanupDisposition, cleanupWarnings); err != nil {
			return terminalJobResult{}, err
		}
		if terminalStatusDoesNotExpectJobResult(job) {
			return terminalJobResultFromStatus(job), nil
		}
		if err != nil {
			return terminalJobResult{}, err
		}
		return terminalJobResult{}, fmt.Errorf("terminal status for job %s was %q but job.result returned %q", job.JobID, job.State, result.State)
	}
	terminalJob := terminalJobResultFromResultAndStatus(result, job, true)
	result = terminalJob.result
	_ = captureBackendError(stateDir, job)
	if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State, result.CleanupDisposition, cleanupWarnings); err != nil {
		return terminalJobResult{}, err
	}
	terminalJob.result = result
	return terminalJob, nil
}

func terminalJobResultFallback(ctx context.Context, c agentbusClient, stateDir, jobID string, resultErr error, cleanupWarnings *localCleanupWarnings) (client.JobResult, error) {
	result, err := terminalJobResultFallbackWithStatus(ctx, c, stateDir, jobID, resultErr, cleanupWarnings)
	return result.result, err
}

func terminalJobResultEnvelopeInputFromStatus(job client.JobStatus) client.JobResult {
	return client.JobResult{JobID: job.JobID, SessionID: job.SessionID, State: job.State, CleanupDisposition: job.CleanupDisposition, LateFinalization: job.LateFinalization, ModelReported: job.ModelReported}
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
	consecutiveTransportFailures := 0
	for {
		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if err != nil {
			retry, pollErr := retryableJobPollError(err)
			if !retry {
				return client.JobStatusResult{}, pollErr
			}
			consecutiveTransportFailures++
			if consecutiveTransportFailures >= maxConsecutiveTransportFailures {
				return client.JobStatusResult{}, agentbusOperationError(err)
			}
		} else {
			consecutiveTransportFailures = 0
			cleanupStatuses(stateDir, status, cleanupWarnings)
			for _, job := range status.Jobs {
				if job.JobID == jobID && engine.IsTerminal(job.State) {
					return status, nil
				}
			}
		}
		if err := jobPollSleep(ctx, interval); err != nil {
			return client.JobStatusResult{}, err
		}
		interval = nextJobPollInterval(interval)
	}
}

func writeTerminalSummary(stdout io.Writer, env TerminalEnvelope) error {
	if env.BackendError != "" {
		_, err := fmt.Fprintf(stdout, "%s %s backend_error=%s\n", env.JobID, env.Status, env.BackendError)
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s %s\n", env.JobID, env.Status)
	return err
}

func writeJobStatusLines(stdout io.Writer, status client.JobStatusResult) error {
	for _, job := range status.Jobs {
		if _, err := fmt.Fprintf(stdout, "%s %s\n", job.JobID, job.State); err != nil {
			return err
		}
	}
	return nil
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
		// Local metadata only enriches the envelope; the agentbus result is authoritative.
		found = false
	}
	cleanupDisposition := option.CleanupDisposition
	if cleanupDisposition == "" {
		cleanupDisposition = result.CleanupDisposition
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
		if cleanupDisposition == "" {
			cleanupDisposition = meta.CleanupDisposition
		}
		if localArtifactsRetainedFromMetadata(meta, result.State, cleanupDisposition) {
			option.LocalArtifactsRetained = true
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
	option.CleanupDisposition = cleanupDisposition
	option.LateFinalization = option.LateFinalization || result.LateFinalization
	return newTerminalEnvelope(result.JobID, result.State, kind, contractKind, stamp, resultSHA256, backendError, option)
}

func localArtifactsRetainedFromMetadata(meta jobMetadata, state engine.JobState, _ string) bool {
	return engine.IsTerminal(state) && (meta.JobInputPath != "" || meta.ReviewWorkspace != "")
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
