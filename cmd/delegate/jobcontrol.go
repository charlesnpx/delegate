package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/policy"
)

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
		if err == nil {
			cleanupStatuses("", status)
		}
	}
	if err != nil {
		return 0, err
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
		if err == nil {
			_ = cleanupJobInput("", result.JobID, result.SessionID, result.State)
		}
	}
	if err != nil {
		return 0, err
	}
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
		_ = cleanupJobInput(stateDir, job.JobID, job.SessionID, job.State)
	}
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
	return newTerminalEnvelope(result.JobID, result.State, kind, contractKind, stamp, resultSHA256)
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
