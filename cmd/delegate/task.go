package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

type optionalStringFlag struct {
	value string
	set   bool
}

func (f *optionalStringFlag) String() string { return f.value }

func (f *optionalStringFlag) Set(value string) error {
	f.value = value
	f.set = true
	return nil
}

type taskOptions struct {
	Backend           string
	Wait              bool
	JSON              bool
	CWD               string
	Resume            bool
	ResumeSession     string
	Fresh             bool
	Model             string
	ModelSet          bool
	Effort            string
	EffortSet         bool
	Timeout           time.Duration
	Write             bool
	StrictContract    bool
	NoContract        bool
	Origin            string
	ParentClient      optionalStringFlag
	ParentSession     optionalStringFlag
	AuditOrigin       envelopeOrigin
	Embedded          bool
	Prompt            optionalStringFlag
	PromptFile        string
	PromptStdin       bool
	HandoffPromptFile string
	Positional        []string
	StateDir          string
	Kind              string
	ReviewWorkspace   string
	ModelEffort       config.ModelEffortResolution
}

type taskRunResult struct {
	Launch    *LaunchEnvelope
	Terminal  *TerminalEnvelope
	Warnings  []string
	Submitted bool
}

const provisionalJobIDTag = "delegate.provisional_job_id"

func runTask(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	opts, err := parseTaskOptions(args, stdin, stderr)
	if err != nil {
		return 0, err
	}
	if err := resolveTaskModelEffort(&opts); err != nil {
		return 0, err
	}
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{
		Write:          opts.Write,
		StrictContract: opts.StrictContract,
		NoContract:     opts.NoContract,
	})
	if err != nil {
		return 0, err
	}
	resolved, err := handoff.ResolvePrompt(handoff.PromptSources{
		Prompt:            opts.Prompt.value,
		PromptSet:         opts.Prompt.set,
		PromptFile:        opts.PromptFile,
		PromptStdin:       opts.PromptStdin,
		HandoffPromptFile: opts.HandoffPromptFile,
		StateDir:          opts.StateDir,
		Positional:        opts.Positional,
		Stdin:             stdin,
	})
	if err != nil {
		return 0, err
	}
	result, err := executeTask(opts, resolved, turnPolicy)
	if err != nil {
		return 0, err
	}
	return writeTaskRunResult(result, stdout, stderr)
}

func resolveTaskModelEffort(opts *taskOptions) error {
	if opts.ResumeSession != "" {
		if opts.ModelSet || opts.EffortSet {
			return fmt.Errorf("model/effort cannot be changed when resuming a session; the session keeps the values it was started with")
		}
		opts.Model = ""
		opts.Effort = ""
		opts.ModelEffort = sessionModelEffortResolution()
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resolved := config.ResolveModelEffort(opts.Backend, opts.Model, opts.Effort, cfg)
	opts.ModelEffort = resolved
	opts.Model = resolved.Model.Effective
	opts.Effort = resolved.Effort.Effective
	return nil
}

func executeTask(opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (taskRunResult, error) {
	ctx := context.Background()
	if opts.Embedded {
		return runEmbeddedTask(ctx, opts, resolved, turnPolicy)
	}
	return runDaemonTask(ctx, opts, resolved, turnPolicy)
}

func writeTaskRunResult(result taskRunResult, stdout, stderr io.Writer) (int, error) {
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", warning); err != nil {
			return 0, err
		}
	}
	if result.Terminal != nil {
		if err := writeJSONLine(stdout, result.Terminal); err != nil {
			return 0, err
		}
		return engine.ExitCodeForState(result.Terminal.Status), nil
	}
	if result.Launch == nil {
		return 0, fmt.Errorf("task did not produce a launch or terminal envelope")
	}
	if err := writeJSONLine(stdout, result.Launch); err != nil {
		return 0, err
	}
	return 0, nil
}

func parseTaskOptions(args []string, stdin io.Reader, stderr io.Writer) (taskOptions, error) {
	var opts taskOptions
	fs := flag.NewFlagSet("delegate task", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var background bool
	fs.StringVar(&opts.Backend, "backend", "", "backend name")
	fs.BoolVar(&background, "background", false, "return after launch")
	fs.BoolVar(&opts.Wait, "wait", false, "wait for terminal result (required with resume flags in v0.1.0)")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSON")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute working directory")
	fs.BoolVar(&opts.Resume, "resume", false, "resume the last session (requires --wait in v0.1.0)")
	fs.StringVar(&opts.ResumeSession, "resume-session", "", "resume a session id (requires --wait in v0.1.0)")
	fs.BoolVar(&opts.Fresh, "fresh", false, "start a fresh session")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout")
	fs.BoolVar(&opts.Write, "write", false, "allow backend writes")
	fs.BoolVar(&opts.StrictContract, "strict-contract", false, "enable corrective retry")
	fs.BoolVar(&opts.NoContract, "no-contract", false, "disable contract enforcement")
	fs.StringVar(&opts.Origin, "origin", "", "originating skill")
	fs.Var(&opts.ParentClient, "parent-client", "explicit parent client for audit linkage")
	fs.Var(&opts.ParentSession, "parent-session", "explicit parent session id for audit linkage")
	fs.BoolVar(&opts.Embedded, "embedded", false, "run through the embedded engine path")
	fs.Var(&opts.Prompt, "prompt", handoff.PromptFlagUsage)
	fs.StringVar(&opts.PromptFile, "prompt-file", "", "read prompt from file")
	fs.BoolVar(&opts.PromptStdin, "prompt-stdin", false, "read prompt from stdin")
	fs.StringVar(&opts.HandoffPromptFile, "handoff-prompt-file", "", "read prompt from a delegate handoff file")
	if err := fs.Parse(args); err != nil {
		return taskOptions{}, err
	}
	opts.Positional = fs.Args()
	fs.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "model":
			opts.ModelSet = true
		case "effort":
			opts.EffortSet = true
		}
	})
	if opts.Backend == "" {
		return taskOptions{}, fmt.Errorf("delegate task requires --backend")
	}
	if background && opts.Wait {
		return taskOptions{}, fmt.Errorf("use only one of --background or --wait")
	}
	if opts.StrictContract && opts.NoContract {
		return taskOptions{}, fmt.Errorf("use only one of --strict-contract or --no-contract")
	}
	if opts.Resume && opts.ResumeSession != "" {
		return taskOptions{}, fmt.Errorf("use only one of --resume or --resume-session")
	}
	if opts.Resume && opts.Fresh || opts.ResumeSession != "" && opts.Fresh {
		return taskOptions{}, fmt.Errorf("use only one of resume flags or --fresh")
	}
	if (opts.Resume || opts.ResumeSession != "") && !opts.Wait {
		return taskOptions{}, fmt.Errorf("--resume and --resume-session require --wait in v0.1.0; background resume lands post-v0.1.0")
	}
	if (opts.Resume || opts.ResumeSession != "") && (opts.ModelSet || opts.EffortSet) {
		return taskOptions{}, fmt.Errorf("model/effort cannot be changed when resuming a session; the session keeps the values it was started with")
	}
	if opts.Embedded && !opts.Wait {
		return taskOptions{}, fmt.Errorf("--embedded requires --wait; background supervision is daemon-only")
	}
	cwd := opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return taskOptions{}, err
		}
	}
	if !filepath.IsAbs(cwd) {
		return taskOptions{}, fmt.Errorf("--cwd must be absolute")
	}
	opts.CWD = filepath.Clean(cwd)
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		return taskOptions{}, err
	}
	opts.StateDir = stateDir
	opts.Kind = taskKind
	if opts.Resume {
		sessionID, found, err := mostRecentDelegateSession(opts.StateDir, opts.Backend, opts.CWD)
		if err != nil {
			return taskOptions{}, err
		}
		if !found {
			return taskOptions{}, fmt.Errorf("no resumable delegate session for backend %q in cwd %q; run a fresh task first or pass --resume-session <id>", opts.Backend, opts.CWD)
		}
		opts.ResumeSession = sessionID
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	opts.AuditOrigin = captureTaskOrigin(opts.Origin, opts.ParentClient, opts.ParentSession, nil)
	return opts, nil
}

func runDaemonTask(ctx context.Context, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (taskRunResult, error) {
	c, hello, err := connectAgentbusCommand(ctx, requiredCapabilitiesForPolicy(turnPolicy))
	if err != nil {
		return taskRunResult{}, err
	}
	defer c.Close()
	if err := validateBackend(hello, opts.Backend, opts.Model, opts.Effort); err != nil {
		return taskRunResult{}, err
	}
	spec := client.TaskSpec{
		Backend:   opts.Backend,
		CWD:       opts.CWD,
		Write:     opts.Write,
		Model:     opts.Model,
		Effort:    opts.Effort,
		Prompt:    resolved.Prompt,
		Policy:    turnPolicy,
		Tags:      taskTags(opts),
		TimeoutMs: timeoutMillis(opts.Timeout),
	}
	if opts.ResumeSession != "" {
		return runDaemonSessionTask(ctx, c, opts, resolved, turnPolicy, hello.Capabilities["models.reported"])
	}
	input, err := persistPreLaunchJobInput(opts, resolved)
	if err != nil {
		return taskRunResult{}, err
	}
	contractKind := contractKindForPolicy(turnPolicy, opts.NoContract)
	if _, err := persistProvisionalJobMetadata(opts, input, input.JobID, contractKind); err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		return taskRunResult{}, fmt.Errorf("persist metadata before launch: %w", err)
	}
	pendingJobID := input.JobID
	spec.Tags[provisionalJobIDTag] = pendingJobID
	submitted, err := c.JobSubmit(ctx, client.JobSubmitParams{TaskSpec: spec})
	if err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		_ = deleteJobMetadata(opts.StateDir, pendingJobID)
		return taskRunResult{}, err
	}
	var warnings []string
	input, warnings = reassociateSubmittedJobInput(input, submitted.JobID, warnings)
	if warning, err := persistLaunchedJobMetadata(opts, input, submitted.JobID, contractKind); err != nil {
		warnings = append(warnings, err.Error())
		env, envelopeErr := newLaunchEnvelopeWithOrigin(submitted.JobID, submitted.State, taskEnvelopeOrigin(opts), taskModelEffort(opts))
		if envelopeErr != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, envelopeErr
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	if pendingJobID != submitted.JobID && input.JobID == submitted.JobID {
		if err := deleteJobMetadata(opts.StateDir, pendingJobID); err != nil {
			warnings = append(warnings, fmt.Sprintf("pending metadata for %s could not be removed: %v", pendingJobID, err))
		}
	}
	err = cleanupJobInput(opts.StateDir, submitted.JobID, "", submitted.State)
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	if opts.Wait {
		jobResult, err := waitForJobResult(ctx, c, opts.StateDir, submitted.JobID)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		env, err := terminalEnvelopeFromJobResult(opts.StateDir, jobResult, hello.Capabilities["models.reported"])
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Terminal: &env, Warnings: warnings, Submitted: true}, nil
	}
	env, err := newLaunchEnvelopeWithOrigin(submitted.JobID, submitted.State, taskEnvelopeOrigin(opts), taskModelEffort(opts))
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
}

func runDaemonSessionTask(ctx context.Context, c agentbusClient, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy, modelsReportedCapabilities ...bool) (taskRunResult, error) {
	modelsReportedCapable := false
	if len(modelsReportedCapabilities) > 0 {
		modelsReportedCapable = modelsReportedCapabilities[0]
	}
	target, err := resumableSessionInfo(ctx, c, opts.StateDir, opts.ResumeSession)
	if err != nil {
		return taskRunResult{}, err
	}
	if err := validateResumeTarget(opts, target); err != nil {
		return taskRunResult{}, err
	}
	session, err := c.SessionResume(ctx, client.SessionResumeParams{SessionID: opts.ResumeSession})
	if err != nil {
		return taskRunResult{}, err
	}
	input, err := persistPreLaunchJobInput(opts, resolved)
	if err != nil {
		return taskRunResult{}, err
	}
	contractKind := contractKindForPolicy(turnPolicy, opts.NoContract)
	if _, err := persistProvisionalJobMetadata(opts, input, input.JobID, contractKind); err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		return taskRunResult{}, fmt.Errorf("persist metadata before launch: %w", err)
	}
	pendingJobID := input.JobID
	write := opts.Write
	started, notifications, err := c.TurnStart(ctx, client.TurnStartParams{
		SessionID: session.SessionID,
		Prompt:    resolved.Prompt,
		Write:     &write,
		Policy:    turnPolicy,
		TimeoutMs: timeoutMillis(opts.Timeout),
	})
	if err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		_ = deleteJobMetadata(opts.StateDir, pendingJobID)
		return taskRunResult{}, err
	}
	var warnings []string
	if err := persistProvisionalJobAdoption(opts.StateDir, pendingJobID, started.JobID); err != nil {
		warnings = append(warnings, fmt.Sprintf("provisional metadata for %s could not record adopted job %s: %v", pendingJobID, started.JobID, err))
	}
	input, warnings = reassociateSubmittedJobInput(input, started.JobID, warnings)
	if warning, err := persistLaunchedJobMetadata(opts, input, started.JobID, contractKind); err != nil {
		warnings = append(warnings, err.Error())
		env, envelopeErr := newLaunchEnvelopeWithOrigin(started.JobID, engine.StateRunning, taskEnvelopeOrigin(opts), taskModelEffort(opts))
		if envelopeErr != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, envelopeErr
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	if pendingJobID != started.JobID && input.JobID == started.JobID {
		if err := deleteJobMetadata(opts.StateDir, pendingJobID); err != nil {
			warnings = append(warnings, fmt.Sprintf("pending metadata for %s could not be removed: %v", pendingJobID, err))
		}
	}
	err = cleanupJobInput(opts.StateDir, started.JobID, started.SessionID, engine.StateRunning)
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	if opts.Wait {
		jobResult, err := waitForTurnResult(ctx, c, opts.StateDir, started.JobID, notifications)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		env, err := terminalEnvelopeFromJobResult(opts.StateDir, jobResult, modelsReportedCapable)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Terminal: &env, Warnings: warnings, Submitted: true}, nil
	}
	env, err := newLaunchEnvelopeWithOrigin(started.JobID, engine.StateRunning, taskEnvelopeOrigin(opts), taskModelEffort(opts))
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
}

func validateResumeTarget(opts taskOptions, target client.SessionInfo) error {
	actualCWD := filepath.Clean(target.CWD)
	requestedCWD := filepath.Clean(opts.CWD)
	if target.Backend != opts.Backend || actualCWD != requestedCWD {
		return fmt.Errorf("session %q has backend %q and cwd %q, which do not match requested --backend %q and effective --cwd %q; use --fresh to start a new session", opts.ResumeSession, target.Backend, actualCWD, opts.Backend, requestedCWD)
	}
	return nil
}

func resumableSessionInfo(ctx context.Context, c agentbusClient, stateDir, sessionID string) (client.SessionInfo, error) {
	listed, listErr := c.SessionList(ctx, client.SessionListParams{})
	if listErr == nil {
		for _, session := range listed.Sessions {
			if session.SessionID == sessionID && session.Backend != "" && session.CWD != "" {
				return session, nil
			}
		}
	}
	meta, found, err := delegateSessionMetadata(stateDir, sessionID)
	if err != nil {
		return client.SessionInfo{}, err
	}
	if found && meta.Backend != "" && meta.CWD != "" {
		return client.SessionInfo{SessionID: sessionID, Backend: meta.Backend, CWD: meta.CWD}, nil
	}
	if listErr != nil {
		return client.SessionInfo{}, fmt.Errorf("inspect session %q before resume: %w; use --fresh to start a new session", sessionID, listErr)
	}
	return client.SessionInfo{}, fmt.Errorf("cannot verify backend and cwd for session %q before resume; use --fresh to start a new session", sessionID)
}

var saveDelegateJobMetadata = saveJobMetadata
var saveLaunchedJobMetadataFallback = saveJobMetadata

func persistDelegateJob(opts taskOptions, resolved handoff.ResolvedPrompt, jobID, contractKind string) (jobMetadata, error) {
	input, err := persistDelegateJobInput(opts, resolved, jobID)
	if err != nil {
		return jobMetadata{}, err
	}
	meta, err := persistDelegateJobMetadata(opts, input, jobID, contractKind)
	if err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		return jobMetadata{}, err
	}
	return meta, nil
}

func persistPreLaunchJobInput(opts taskOptions, resolved handoff.ResolvedPrompt) (handoff.JobInput, error) {
	jobID, err := newJobID()
	if err != nil {
		return handoff.JobInput{}, err
	}
	return persistDelegateJobInput(opts, resolved, jobID)
}

func persistDelegateJobInput(opts taskOptions, resolved handoff.ResolvedPrompt, jobID string) (handoff.JobInput, error) {
	return handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: opts.StateDir,
		JobID:    jobID,
		Prompt:   resolved,
	})
}

func persistDelegateJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) (jobMetadata, error) {
	meta := delegateJobMetadata(opts, input, jobID, contractKind)
	if err := saveDelegateJobMetadata(opts.StateDir, meta); err != nil {
		return jobMetadata{}, err
	}
	return meta, nil
}

func persistProvisionalJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) (jobMetadata, error) {
	meta := delegateJobMetadata(opts, input, jobID, contractKind)
	meta.Provisional = true
	if err := saveDelegateJobMetadata(opts.StateDir, meta); err != nil {
		return jobMetadata{}, err
	}
	return meta, nil
}

func persistProvisionalJobAdoption(stateDir, provisionalID, jobID string) error {
	meta, found, err := loadJobMetadata(stateDir, provisionalID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("provisional metadata not found")
	}
	meta.AdoptedJobID = jobID
	return saveJobMetadata(stateDir, meta)
}

func delegateJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) jobMetadata {
	modelEffort := normalizedModelEffort(taskModelEffort(opts))
	return jobMetadata{
		Schema:          envelopeSchema,
		JobID:           jobID,
		Kind:            effectiveTaskKind(opts),
		Backend:         opts.Backend,
		CWD:             opts.CWD,
		ContractKind:    contractKind,
		NoContract:      opts.NoContract,
		JobInputPath:    input.Path,
		ReviewWorkspace: opts.ReviewWorkspace,
		Model:           modelEffort.Model,
		Effort:          modelEffort.Effort,
		Origin:          envelopeOriginPointer(taskEnvelopeOrigin(opts)),
	}
}

func taskModelEffort(opts taskOptions) config.ModelEffortResolution {
	if opts.ResumeSession != "" {
		return sessionModelEffortResolution()
	}
	return opts.ModelEffort
}

func sessionModelEffortResolution() config.ModelEffortResolution {
	return config.ModelEffortResolution{
		Model:  config.DimensionResolution{Source: "session"},
		Effort: config.DimensionResolution{Source: "session"},
	}
}

func persistLaunchedJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) (string, error) {
	meta := delegateJobMetadata(opts, input, jobID, contractKind)
	if err := saveDelegateJobMetadata(opts.StateDir, meta); err != nil {
		if fallbackErr := saveLaunchedJobMetadataFallback(opts.StateDir, meta); fallbackErr != nil {
			return "", fmt.Errorf("persist launched job metadata for %s: primary: %v; state-dir fallback: %w", jobID, err, fallbackErr)
		}
		return fmt.Sprintf("delegate job metadata for %s required the durable state-directory fallback after the primary write failed: %v", jobID, err), nil
	}
	return "", nil
}

func reassociateSubmittedJobInput(input handoff.JobInput, jobID string, warnings []string) (handoff.JobInput, []string) {
	reassociated, err := handoff.ReassociateJobInput(input, jobID, handoff.Hooks{})
	if reassociated.Path != "" {
		input = reassociated
	}
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("job input for %s could not be re-associated: %v", jobID, err))
	}
	return input, warnings
}

func taskTags(opts taskOptions) map[string]string {
	tags := map[string]string{"delegate.kind": effectiveTaskKind(opts)}
	origin := taskEnvelopeOrigin(opts)
	if origin.Skill != "" {
		tags[delegateOriginTag] = origin.Skill
	}
	if origin.ParentClient != "" {
		tags[parentClientTag] = origin.ParentClient
	}
	if origin.ParentSessionID != "" {
		tags[parentSessionTag] = origin.ParentSessionID
	}
	if origin.ParentAgent != "" {
		tags[parentAgentTag] = origin.ParentAgent
	}
	if origin.Depth != "" {
		tags[delegateDepthTag] = origin.Depth
	}
	return tags
}

func effectiveTaskKind(opts taskOptions) string {
	if opts.Kind == "" {
		return taskKind
	}
	return opts.Kind
}

func timeoutMillis(timeout time.Duration) *int64 {
	if timeout <= 0 {
		return nil
	}
	ms := timeout.Milliseconds()
	if ms == 0 {
		ms = 1
	}
	return &ms
}

func contractKindForPolicy(turnPolicy *engine.TurnPolicy, noContract bool) string {
	if noContract || turnPolicy == nil || turnPolicy.Contract == nil {
		return contractKindNone
	}
	if turnPolicy.Contract.Shape != nil {
		return contractKindShape
	}
	if turnPolicy.Contract.JSONSchema != nil {
		return contractKindJSONSchema
	}
	return contractKindNone
}
