package main

import (
	"context"
	"fmt"
	"os"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

const (
	reportCorrectionTag   = "delegate.report_correction"
	reportCorrectionOfTag = "delegate.report_correction_of"
)

func maybeCorrectDelegateReport(ctx context.Context, c agentbusClient, hello client.HelloResult, stateDir string, terminalJob terminalJobResult, cleanupWarnings *localCleanupWarnings) (terminalJobResult, agentbusClient, client.HelloResult, []string, error) {
	result := terminalJob.result
	if !reportResultCanBeCorrected(result.State) || result.Result == nil {
		return terminalJob, c, hello, nil, nil
	}
	meta, found, err := loadJobMetadata(stateDir, result.JobID)
	if err != nil || !found || !managedDelegateReportMetadata(meta) {
		return terminalJob, c, hello, nil, nil
	}
	body, ok := resultBodyText(result.Result)
	if !ok {
		return terminalJob, c, hello, nil, nil
	}
	validation, err := policy.ValidateDelegateReportShape(body)
	if err != nil {
		return terminalJob, c, hello, nil, err
	}
	if validation.Compliant || meta.ReportCorrectionOf != "" {
		return terminalJob, c, hello, nil, nil
	}
	if meta.ReportCorrectionJobID != "" {
		corrected, err := waitForTerminalJobResult(ctx, c, stateDir, meta.ReportCorrectionJobID, cleanupWarnings)
		if err != nil {
			return terminalJob, c, hello, []string{reportCorrectionFallbackWarning(meta.JobID, meta.ReportCorrectionJobID, err)}, nil
		}
		if !reportCorrectionResultUsable(corrected.result) {
			return terminalJob, c, hello, []string{reportCorrectionFallbackWarning(meta.JobID, corrected.result.JobID, nil)}, nil
		}
		return corrected, c, hello, nil, nil
	}
	corrected, nextClient, nextHello, warnings, err := submitDelegateReportCorrection(ctx, c, hello, stateDir, meta, body, validation.Violations, cleanupWarnings)
	if err != nil {
		warnings = append(warnings, reportCorrectionFallbackWarning(meta.JobID, corrected.result.JobID, err))
		return terminalJob, nextClient, nextHello, warnings, nil
	}
	if !reportCorrectionResultUsable(corrected.result) {
		warnings = append(warnings, reportCorrectionFallbackWarning(meta.JobID, corrected.result.JobID, nil))
		return terminalJob, nextClient, nextHello, warnings, nil
	}
	return corrected, nextClient, nextHello, warnings, nil
}

func reportResultCanBeCorrected(state engine.JobState) bool {
	return state == engine.StateCompleted || state == engine.StateCompletedNoncompliant
}

func managedDelegateReportMetadata(meta jobMetadata) bool {
	return meta.JobID != "" && meta.Backend != "" && meta.ContractKind == contractKindShape && !meta.NoContract
}

func reportCorrectionResultUsable(result client.JobResult) bool {
	if !reportResultCanBeCorrected(result.State) || result.Result == nil {
		return false
	}
	// Match the terminal envelope's authoritative-result criterion: a present
	// result digest (see newTerminalEnvelope/resultUnavailableReason). Without it
	// the envelope emits completed_without_result, so selecting such a correction
	// would suppress the original body SHA and cleanup disposition.
	if result.Result.SHA256 == "" {
		return false
	}
	_, ok := resultBodyText(result.Result)
	return ok
}

func reportCorrectionFallbackWarning(originalJobID, correctionJobID string, err error) string {
	correction := "delegate-report correction"
	if correctionJobID != "" {
		correction = fmt.Sprintf("delegate-report correction %s", correctionJobID)
	}
	if err != nil {
		return fmt.Sprintf("%s for %s could not complete: %v; using original terminal result", correction, originalJobID, err)
	}
	return fmt.Sprintf("%s for %s did not produce an authoritative result body; using original terminal result", correction, originalJobID)
}

func submitDelegateReportCorrection(ctx context.Context, c agentbusClient, hello client.HelloResult, stateDir string, original jobMetadata, priorBody string, violations []string, cleanupWarnings *localCleanupWarnings) (terminalJobResult, agentbusClient, client.HelloResult, []string, error) {
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{})
	if err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	if err := requireCapabilities(hello, "", required); err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	prompt, err := policy.DelegateReportCorrectionPrompt(priorBody, violations)
	if err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	opts, err := reportCorrectionTaskOptions(stateDir, original)
	if err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	resolved := handoff.ResolvedPrompt{Prompt: prompt, Source: handoff.SourcePrompt}
	intent, err := prepareNewSubmissionIntent(opts, resolved, turnPolicy)
	if err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	if err := saveSubmissionIntent(opts.StateDir, intent); err != nil {
		return terminalJobResult{}, c, hello, nil, fmt.Errorf("persist report-correction submission intent before launch: %w", err)
	}
	submitted, nextClient, nextHello, err := submitIntentWithRetry(ctx, c, hello, opts.StateDir, &intent, required)
	c = nextClient
	hello = nextHello
	if err != nil {
		return terminalJobResult{}, c, hello, nil, err
	}
	opts.RequestID = intent.RequestID
	opts.WorkspaceKey = intent.WorkspaceKey
	opts.SubmissionState = submitted.State
	opts.Deduplicated = submitted.Deduplicated
	opts.TimeoutResolution = timeoutResolutionForSubmission(opts.Timeout, opts.TimeoutSet, submitted, c)
	warnings, acknowledged, err := acknowledgeSubmittedTask(opts, resolved, submitted, intent.ContractKind, "after report-correction submission")
	if err != nil {
		return terminalJobResult{}, c, hello, warnings, err
	}
	if !acknowledged {
		return terminalJobResult{}, c, hello, warnings, fmt.Errorf("report-correction job %s could not be acknowledged", submitted.JobID)
	}
	if err := markReportCorrectionSubmitted(stateDir, original.JobID, submitted.JobID); err != nil {
		return terminalJobResult{}, c, hello, warnings, err
	}
	if engine.IsTerminal(submitted.State) {
		terminalJob, err := submittedTerminalJob(ctx, c, opts.StateDir, submitted.JobID, cleanupWarnings)
		return terminalJob, c, hello, warnings, err
	}
	terminalJob, err := waitForTerminalJobResult(ctx, c, opts.StateDir, submitted.JobID, cleanupWarnings)
	return terminalJob, c, hello, warnings, err
}

func reportCorrectionTaskOptions(stateDir string, original jobMetadata) (taskOptions, error) {
	if original.Backend == "" {
		return taskOptions{}, fmt.Errorf("cannot submit report correction for %s: missing backend metadata", original.JobID)
	}
	cwd, err := reportCorrectionCWD(stateDir, original.CWD)
	if err != nil {
		return taskOptions{}, err
	}
	agentbusStateRoot := original.AgentbusStateRoot
	if agentbusStateRoot == "" {
		agentbusStateRoot, err = resolveAgentbusStateRoot()
		if err != nil {
			return taskOptions{}, err
		}
	}
	kind := original.Kind
	if kind == "" {
		kind = taskKind
	}
	tagsOrigin := envelopeOrigin{}
	if original.Origin != nil {
		tagsOrigin = *original.Origin
	}
	modelEffort := config.ModelEffortResolution{Model: original.Model, Effort: original.Effort}
	return taskOptions{
		Backend:            original.Backend,
		CWD:                cwd,
		Model:              original.Model.Effective,
		Effort:             original.Effort.Effective,
		StateDir:           stateDir,
		Kind:               kind,
		ModelEffort:        modelEffort,
		AuditOrigin:        tagsOrigin,
		AgentbusStateRoot:  agentbusStateRoot,
		LogicalWorkspace:   cwd,
		ReportCorrectionOf: original.JobID,
	}, nil
}

func reportCorrectionCWD(stateDir, originalCWD string) (string, error) {
	if originalCWD != "" {
		if info, err := os.Stat(originalCWD); err == nil && info.IsDir() {
			return originalCWD, nil
		}
	}
	resolved, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", fmt.Errorf("resolve report-correction cwd: %w", err)
	}
	if err := handoff.EnsureStateDir(resolved); err != nil {
		return "", fmt.Errorf("prepare report-correction cwd: %w", err)
	}
	return resolved, nil
}

func markReportCorrectionSubmitted(stateDir, originalJobID, correctionJobID string) error {
	meta, found, err := loadJobMetadata(stateDir, originalJobID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cannot mark report correction for %s: metadata not found", originalJobID)
	}
	if meta.ReportCorrectionJobID == correctionJobID {
		return nil
	}
	if meta.ReportCorrectionJobID != "" {
		return fmt.Errorf("report correction for %s already submitted as %s", originalJobID, meta.ReportCorrectionJobID)
	}
	meta.ReportCorrectionJobID = correctionJobID
	return saveJobMetadata(stateDir, meta)
}
