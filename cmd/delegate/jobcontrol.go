package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

const provisionalMetadataAdoptionThreshold = time.Minute

func runStatus(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jobID := fs.String("job", "", "job id")
	jsonOut := fs.Bool("json", false, "emit JSON")
	wait := fs.Bool("wait", false, "wait for terminal status")
	probe := fs.Bool("probe", false, "run process/socket/log liveness probes for the job")
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
	ctx := context.Background()
	c, _, err := connectAgentbusCommand(ctx, setupRequiredCapabilities())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var status client.JobStatusResult
	if *wait {
		status, err = waitForJobStatus(ctx, c, "", *jobID)
	} else {
		status, err = c.JobStatus(ctx, client.JobStatusParams{JobID: *jobID, All: *jobID == ""})
	}
	if err != nil {
		return 0, err
	}
	_ = sweepTerminalJobInputs(ctx, c, "")
	cleanupStatuses("", status)
	if *jobID != "" {
		if job, ok := findJobStatus(status, *jobID); ok && engine.IsTerminal(job.State) && !*probe {
			result, resultErr := c.JobResult(ctx, client.JobResultParams{JobID: *jobID})
			if resultErr != nil {
				result = client.JobResult{JobID: job.JobID, SessionID: job.SessionID, State: job.State}
			}
			env, envelopeErr := terminalEnvelopeFromJobResult("", result)
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
	if *probe {
		job, ok := findJobStatus(status, *jobID)
		if !ok {
			return 0, fmt.Errorf("job %q not found", *jobID)
		}
		probeResult, err := probeJobStatus(ctx, job)
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
	_ = fs.Bool("json", false, "emit JSON")
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
	c, _, err := connectAgentbusCommand(ctx, setupRequiredCapabilities())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var result client.JobResult
	if *wait {
		result, err = waitForJobResult(ctx, c, "", *jobID)
	} else {
		result, err = c.JobResult(ctx, client.JobResultParams{JobID: *jobID})
	}
	if err != nil {
		return 0, err
	}
	_ = sweepTerminalJobInputs(ctx, c, "")
	_ = cleanupJobInput("", result.JobID, result.SessionID, result.State)
	env, err := terminalEnvelopeFromJobResult("", result)
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
	c, _, err := connectAgentbusCommand(ctx, setupRequiredCapabilities())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	result, err := c.JobCancel(ctx, client.JobCancelParams{JobID: *jobID})
	if err != nil {
		return 0, err
	}
	if err := cleanupJobInput("", result.JobID, "", result.State); err != nil {
		return 0, err
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, result)
	}
	_, err = fmt.Fprintf(stdout, "%s %s\n", result.JobID, result.State)
	return 0, err
}

func waitForTurnResult(ctx context.Context, c agentbusClient, stateDir, jobID string, notifications <-chan client.TurnNotification) (client.JobResult, error) {
	for notification := range notifications {
		if notification.Result == nil {
			continue
		}
		result := client.JobResult{
			JobID:     notification.Result.JobID,
			SessionID: notification.Result.SessionID,
			State:     notification.Result.State,
			Result:    notification.Result.Result,
			Contract:  notification.Result.Contract,
		}
		if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State); err != nil {
			return client.JobResult{}, err
		}
		return result, nil
	}
	return waitForJobResult(ctx, c, stateDir, jobID)
}

func waitForJobResult(ctx context.Context, c agentbusClient, stateDir, jobID string) (client.JobResult, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := c.JobResult(ctx, client.JobResultParams{JobID: jobID})
		if err == nil && engine.IsTerminal(result.State) {
			if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State); err != nil {
				return client.JobResult{}, err
			}
			return result, nil
		}
		status, statusErr := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if statusErr == nil {
			cleanupStatuses(stateDir, status)
			for _, job := range status.Jobs {
				if job.JobID == jobID && engine.IsTerminal(job.State) {
					result, err := c.JobResult(ctx, client.JobResultParams{JobID: jobID})
					if err != nil {
						return client.JobResult{}, err
					}
					if err := cleanupJobInput(stateDir, result.JobID, result.SessionID, result.State); err != nil {
						return client.JobResult{}, err
					}
					return result, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return client.JobResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForJobStatus(ctx context.Context, c agentbusClient, stateDir, jobID string) (client.JobStatusResult, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if err != nil {
			return client.JobStatusResult{}, err
		}
		cleanupStatuses(stateDir, status)
		for _, job := range status.Jobs {
			if job.JobID == jobID && engine.IsTerminal(job.State) {
				return status, nil
			}
		}
		select {
		case <-ctx.Done():
			return client.JobStatusResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func cleanupStatuses(stateDir string, status client.JobStatusResult) {
	for _, job := range status.Jobs {
		_ = captureBackendError(stateDir, job)
		_ = cleanupJobInput(stateDir, job.JobID, job.SessionID, job.State)
	}
}

func adoptProvisionalJobMetadata(ctx context.Context, c agentbusClient, stateDir string, cutoff time.Time) error {
	provisional, err := provisionalJobMetadataOlderThan(stateDir, cutoff)
	if err != nil || len(provisional) == 0 {
		return err
	}
	status, err := c.JobStatus(ctx, client.JobStatusParams{All: true})
	if err != nil {
		return fmt.Errorf("list jobs for provisional metadata adoption: %w", err)
	}
	jobsByProvisionalID := make(map[string]client.JobStatus, len(status.Jobs))
	jobsByID := make(map[string]client.JobStatus, len(status.Jobs))
	for _, job := range status.Jobs {
		jobsByID[job.JobID] = job
		if provisionalID := job.Tags[provisionalJobIDTag]; provisionalID != "" {
			jobsByProvisionalID[provisionalID] = job
		}
	}
	var joined error
	for _, meta := range provisional {
		job, ok := jobsByProvisionalID[meta.JobID]
		if !ok && meta.AdoptedJobID != "" {
			job, ok = jobsByID[meta.AdoptedJobID]
		}
		if !ok {
			continue
		}
		if err := adoptOneProvisionalJobMetadata(stateDir, meta, job); err != nil {
			joined = errors.Join(joined, fmt.Errorf("adopt provisional metadata %s as %s: %w", meta.JobID, job.JobID, err))
		}
	}
	return joined
}

func adoptOneProvisionalJobMetadata(stateDir string, provisional jobMetadata, job client.JobStatus) error {
	inputPath, err := reassociateProvisionalJobInput(stateDir, provisional, job.JobID)
	if err != nil {
		return err
	}
	meta, found, err := loadJobMetadata(stateDir, job.JobID)
	if err != nil {
		return err
	}
	if !found {
		meta = provisional
		meta.JobID = job.JobID
		meta.Provisional = false
	}
	meta.AdoptedJobID = ""
	if inputPath != "" {
		meta.JobInputPath = inputPath
	}
	if job.SessionID != "" {
		meta.SessionID = job.SessionID
	}
	if err := saveJobMetadata(stateDir, meta); err != nil {
		return err
	}
	if provisional.JobID != job.JobID {
		if err := deleteJobMetadata(stateDir, provisional.JobID); err != nil {
			return err
		}
	}
	if inputPath == "" && engine.IsTerminal(job.State) {
		return cleanupJobInput(stateDir, job.JobID, job.SessionID, job.State)
	}
	return nil
}

func reassociateProvisionalJobInput(stateDir string, meta jobMetadata, jobID string) (string, error) {
	resolvedState, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	if meta.JobInputPath != "" && filepath.Clean(filepath.Dir(meta.JobInputPath)) != resolvedState {
		return "", fmt.Errorf("provisional job-input path is outside delegate state: %q", meta.JobInputPath)
	}
	if meta.JobInputPath != "" {
		if _, err := os.Lstat(meta.JobInputPath); err == nil {
			input := handoff.JobInput{JobID: meta.JobID, Path: meta.JobInputPath}
			reassociated, err := handoff.ReassociateJobInput(input, jobID, handoff.Hooks{})
			return reassociated.Path, err
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	entries, err := os.ReadDir(resolvedState)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		input, ok := handoff.ParseJobInputPath(filepath.Join(resolvedState, entry.Name()))
		if ok && input.JobID == jobID {
			return input.Path, nil
		}
	}
	return "", nil
}

func sweepTerminalJobInputs(ctx context.Context, c agentbusClient, stateDir string) error {
	sweepErr := adoptProvisionalJobMetadata(ctx, c, stateDir, time.Now().Add(-provisionalMetadataAdoptionThreshold))
	removed, terminalSweepErr := handoff.SweepTerminalJobInputs(stateDir, func(jobID string) (engine.JobState, bool, error) {
		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: jobID})
		if err != nil {
			return "", false, err
		}
		job, found := findJobStatus(status, jobID)
		if !found {
			return "", false, nil
		}
		return job.State, true, nil
	}, handoff.Hooks{})
	sweepErr = errors.Join(sweepErr, terminalSweepErr)
	for _, input := range removed {
		sweepErr = errors.Join(sweepErr, cleanupJobInput(stateDir, input.JobID, "", engine.StateCompleted))
	}
	return sweepErr
}

func terminalEnvelopeFromJobResult(stateDir string, result client.JobResult) (TerminalEnvelope, error) {
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
	if found {
		backendError = meta.BackendError
	}
	if result.Result == nil && backendError != "" && !(found && meta.NoContract) {
		stamp = skippedDelegateContractStamp(engine.SkipBackendError)
	}
	return newTerminalEnvelope(result.JobID, result.State, kind, contractKind, stamp, resultSHA256, backendError)
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
