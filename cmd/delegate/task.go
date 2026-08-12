package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	Backend            string
	Background         bool
	Wait               bool
	JSON               bool
	CWD                string
	Model              string
	ModelSet           bool
	Effort             string
	EffortSet          bool
	Timeout            time.Duration
	TimeoutSet         bool
	TimeoutResolution  config.DimensionResolution
	Write              bool
	WriteSet           bool
	StrictContract     bool
	StrictContractSet  bool
	NoContract         bool
	NoContractSet      bool
	ReportCorrectionOf string
	Origin             string
	ParentClient       optionalStringFlag
	ParentSession      optionalStringFlag
	AuditOrigin        envelopeOrigin
	Prompt             optionalStringFlag
	PromptFile         string
	PromptStdin        bool
	HandoffPromptFile  string
	OutputSchema       optionalStringFlag
	OutputSchemaFile   optionalStringFlag
	OutputSchemaStdin  bool
	Positional         []string
	RecoverRequest     string
	StateDir           string
	Kind               string
	ReviewWorkspace    string
	ModelEffort        config.ModelEffortResolution
	AgentbusStateRoot  string
	RequestID          string
	WorkspaceKey       string
	LogicalWorkspace   string
	SubmissionState    engine.JobState
	Deduplicated       bool
}

type taskRunResult struct {
	Launch    *LaunchEnvelope
	Terminal  *TerminalEnvelope
	Warnings  []string
	Submitted bool
}

const (
	delegateRequestIDTag = "delegate.request_id"
	agentbusMaxTimeout   = 4 * time.Hour
	readOnlyTaskHint     = "notice: task will run with a read-only backend profile; pass --write for edits or builds."
)

func runTask(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	opts, err := parseTaskOptions(args, stdin, stderr)
	if err != nil {
		return 0, err
	}
	if opts.RecoverRequest != "" {
		result, err := recoverTaskSubmission(opts, stderr)
		if err != nil {
			return agentbusCommandErrorResult(opts.JSON, stdout, err)
		}
		return writeTaskRunResult(result, stdout, stderr)
	}
	if err := resolveTaskModelEffort(&opts); err != nil {
		return 0, err
	}
	outputSchema, err := resolveTaskOutputSchema(opts, stdin)
	if err != nil {
		return 0, err
	}
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{
		Write:          opts.Write,
		StrictContract: opts.StrictContract,
		NoContract:     opts.NoContract,
		JSONSchema:     outputSchema,
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
	if !opts.Write && opts.RecoverRequest == "" {
		if _, err := fmt.Fprintln(stderr, readOnlyTaskHint); err != nil {
			return 0, err
		}
	}
	result, err := executeTask(opts, resolved, turnPolicy, stderr)
	if err != nil {
		return agentbusCommandErrorResult(opts.JSON, stdout, err)
	}
	return writeTaskRunResult(result, stdout, stderr)
}

func resolveTaskModelEffort(opts *taskOptions) error {
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

func executeTask(opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy, stderr io.Writer) (taskRunResult, error) {
	ctx := context.Background()
	return runDaemonTask(ctx, opts, resolved, turnPolicy, stderr)
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
	fs.StringVar(&opts.Backend, "backend", "", "backend name")
	fs.BoolVar(&opts.Background, "background", false, "return after launch")
	fs.BoolVar(&opts.Wait, "wait", false, "wait for terminal result")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSON")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute working directory")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout; 0 leaves the deadline to the daemon default; envelope.timeout is authoritative")
	fs.BoolVar(&opts.Write, "write", false, "allow backend writes")
	fs.BoolVar(&opts.StrictContract, "strict-contract", false, "compatibility flag; delegate-report corrective retry is enabled by default")
	fs.BoolVar(&opts.NoContract, "no-contract", false, "disable contract enforcement (cannot be used with --output-schema*)")
	fs.StringVar(&opts.Origin, "origin", "", "originating skill")
	fs.Var(&opts.ParentClient, "parent-client", "explicit parent client for audit linkage")
	fs.Var(&opts.ParentSession, "parent-session", "explicit parent session id for audit linkage")
	fs.Var(&opts.Prompt, "prompt", handoff.PromptFlagUsage)
	fs.StringVar(&opts.PromptFile, "prompt-file", "", "read prompt from file")
	fs.BoolVar(&opts.PromptStdin, "prompt-stdin", false, "read prompt from stdin")
	fs.StringVar(&opts.HandoffPromptFile, "handoff-prompt-file", "", "read prompt from a delegate handoff file")
	fs.Var(&opts.OutputSchema, "output-schema", "inline JSON Schema output contract")
	fs.Var(&opts.OutputSchemaFile, "output-schema-file", "read JSON Schema output contract from file")
	fs.BoolVar(&opts.OutputSchemaStdin, "output-schema-stdin", false, "read JSON Schema output contract from stdin")
	fs.StringVar(&opts.RecoverRequest, "recover-request", "", "recover a durable submission request")
	if err := fs.Parse(args); err != nil {
		return taskOptions{}, err
	}
	opts.Positional = fs.Args()
	visited := map[string]bool{}
	fs.Visit(func(flag *flag.Flag) {
		visited[flag.Name] = true
		switch flag.Name {
		case "model":
			opts.ModelSet = true
		case "effort":
			opts.EffortSet = true
		case "timeout":
			opts.TimeoutSet = true
		case "write":
			opts.WriteSet = true
		case "strict-contract":
			opts.StrictContractSet = true
		case "no-contract":
			opts.NoContractSet = true
		}
	})
	if opts.RecoverRequest != "" {
		if opts.Background && opts.Wait {
			return taskOptions{}, fmt.Errorf("use only one of --background or --wait")
		}
		if err := validateRecoverRequestFlags(visited, opts.Positional); err != nil {
			return taskOptions{}, err
		}
		if err := validateRequestID(opts.RecoverRequest); err != nil {
			return taskOptions{}, err
		}
		stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
		if err != nil {
			return taskOptions{}, err
		}
		opts.StateDir = stateDir
		opts.Kind = taskKind
		return opts, nil
	}
	if opts.Backend == "" {
		return taskOptions{}, fmt.Errorf("delegate task requires --backend")
	}
	if opts.Background && opts.Wait {
		return taskOptions{}, fmt.Errorf("use only one of --background or --wait")
	}
	if opts.StrictContract && opts.NoContract {
		return taskOptions{}, fmt.Errorf("use only one of --strict-contract or --no-contract")
	}
	outputSchemaSources := 0
	if opts.OutputSchema.set {
		outputSchemaSources++
	}
	if opts.OutputSchemaFile.set {
		outputSchemaSources++
	}
	if opts.OutputSchemaStdin {
		outputSchemaSources++
	}
	if outputSchemaSources > 1 {
		return taskOptions{}, fmt.Errorf("use only one of --output-schema, --output-schema-file, or --output-schema-stdin")
	}
	if opts.NoContract && outputSchemaSources > 0 {
		return taskOptions{}, fmt.Errorf("--no-contract cannot be used with --output-schema, --output-schema-file, or --output-schema-stdin")
	}
	if opts.OutputSchemaStdin && opts.PromptStdin {
		return taskOptions{}, fmt.Errorf("--output-schema-stdin cannot be used with --prompt-stdin")
	}
	if err := validateTimeoutOption(opts.Timeout, opts.TimeoutSet); err != nil {
		return taskOptions{}, err
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
	opts.LogicalWorkspace = opts.CWD
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		return taskOptions{}, err
	}
	opts.StateDir = stateDir
	opts.Kind = taskKind
	if stdin == nil {
		stdin = os.Stdin
	}
	opts.AuditOrigin = captureTaskOrigin(opts.Origin, opts.ParentClient, opts.ParentSession, nil)
	return opts, nil
}

func validateRecoverRequestFlags(visited map[string]bool, positional []string) error {
	rejected := []string{
		"backend", "cwd", "model", "effort", "timeout", "write", "strict-contract", "no-contract",
		"origin", "parent-client", "parent-session",
		"prompt", "prompt-file", "prompt-stdin", "handoff-prompt-file",
		"output-schema", "output-schema-file", "output-schema-stdin",
	}
	for _, name := range rejected {
		if visited[name] {
			return fmt.Errorf("delegate task --recover-request cannot be used with --%s", name)
		}
	}
	if len(positional) > 0 {
		return fmt.Errorf("delegate task --recover-request does not accept a prompt or positional arguments")
	}
	return nil
}

func validateTimeoutOption(timeout time.Duration, set bool) error {
	if !set {
		return nil
	}
	if timeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	if timeout > agentbusMaxTimeout {
		return fmt.Errorf("--timeout must be at most %s", agentbusMaxTimeout)
	}
	return nil
}

func resolveTaskOutputSchema(opts taskOptions, stdin io.Reader) (json.RawMessage, error) {
	if opts.OutputSchema.set {
		return append(json.RawMessage{}, []byte(opts.OutputSchema.value)...), nil
	}
	if opts.OutputSchemaFile.set {
		raw, err := os.ReadFile(opts.OutputSchemaFile.value)
		if err != nil {
			return nil, fmt.Errorf("read --output-schema-file %q: %w", opts.OutputSchemaFile.value, err)
		}
		return append(json.RawMessage{}, raw...), nil
	}
	if opts.OutputSchemaStdin {
		if stdin == nil {
			stdin = os.Stdin
		}
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read --output-schema-stdin: %w", err)
		}
		return append(json.RawMessage{}, raw...), nil
	}
	return nil, nil
}

func runDaemonTask(ctx context.Context, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy, stderr io.Writer) (taskRunResult, error) {
	requiredCapabilities := requiredCapabilitiesForPolicy(turnPolicy)
	c, hello, agentbusStateRoot, err := connectAgentbusCommand(ctx, requiredCapabilities)
	if err != nil {
		return taskRunResult{}, err
	}
	defer func() { _ = c.Close() }()
	opts.AgentbusStateRoot = agentbusStateRoot
	if err := validateBackend(hello, opts.Backend, opts.Model, opts.Effort, stderr); err != nil {
		return taskRunResult{}, err
	}
	intent, err := prepareNewSubmissionIntent(opts, resolved, turnPolicy)
	if err != nil {
		return taskRunResult{}, err
	}
	if err := saveSubmissionIntent(opts.StateDir, intent); err != nil {
		return taskRunResult{}, fmt.Errorf("persist submission intent before launch: %w", err)
	}
	submitted, currentClient, currentHello, err := submitIntentWithRetry(ctx, c, hello, opts.StateDir, &intent, requiredCapabilities)
	c = currentClient
	hello = currentHello
	if err != nil {
		return taskRunResult{}, err
	}
	var warnings []string
	opts.RequestID = intent.RequestID
	opts.WorkspaceKey = intent.WorkspaceKey
	opts.SubmissionState = submitted.State
	opts.Deduplicated = submitted.Deduplicated
	opts.TimeoutResolution = timeoutResolutionForSubmission(opts.Timeout, opts.TimeoutSet, submitted)
	contractKind := intent.ContractKind
	ackWarnings, acknowledged, err := acknowledgeSubmittedTask(opts, resolved, submitted, contractKind, "after submission")
	warnings = append(warnings, ackWarnings...)
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	if !acknowledged {
		env, envelopeErr := newLaunchEnvelopeForTask(submitted.JobID, submitted.State, opts)
		if envelopeErr != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, envelopeErr
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	}
	if err := transitionSubmissionIntent(opts.StateDir, &intent, submissionPhaseAcknowledged, func(intent *submissionIntent) {
		intent.JobID = submitted.JobID
		intent.Deduplicated = submitted.Deduplicated
		intent.LastError = nil
	}); err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	return submittedTaskRunResult(ctx, c, hello, opts, submitted, warnings, stderr)
}

const maxSubmissionAttempts = 3

type submissionUnresolvedError struct {
	RequestID string
	Err       error
}

func (err submissionUnresolvedError) Error() string {
	if err.Err == nil {
		return submissionRecoveryMessage(err.RequestID)
	}
	return fmt.Sprintf("%s: %v", submissionRecoveryMessage(err.RequestID), err.Err)
}

func (err submissionUnresolvedError) Unwrap() error {
	return err.Err
}

func submissionRecoveryMessage(requestID string) string {
	return fmt.Sprintf("submission request %s is unresolved; recover with: delegate task --recover-request %s", requestID, requestID)
}

func prepareNewSubmissionIntent(opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (submissionIntent, error) {
	requestID, err := newRequestID()
	if err != nil {
		return submissionIntent{}, err
	}
	prompt, err := policy.AppendReportFormatBlock(resolved.Prompt, turnPolicy)
	if err != nil {
		return submissionIntent{}, err
	}
	logicalWorkspace := opts.LogicalWorkspace
	if logicalWorkspace == "" {
		logicalWorkspace = opts.CWD
	}
	workspaceKey, err := workspaceKeyForLogicalWorkspace(logicalWorkspace)
	if err != nil {
		return submissionIntent{}, err
	}
	tags := taskTags(opts)
	tags[delegateRequestIDTag] = requestID
	params := client.JobSubmitParams{
		WorkspaceKey: workspaceKey,
		RequestID:    requestID,
		TaskSpec: client.TaskSpec{
			Backend:   opts.Backend,
			CWD:       opts.CWD,
			Write:     opts.Write,
			Model:     opts.Model,
			Effort:    opts.Effort,
			Prompt:    prompt,
			Policy:    turnPolicy,
			Tags:      tags,
			TimeoutMs: timeoutMillis(opts.Timeout, opts.TimeoutSet),
		},
	}
	modelEffort := normalizedModelEffort(taskModelEffort(opts))
	now := time.Now().UTC()
	return submissionIntent{
		Schema:             submissionIntentSchema,
		RequestID:          requestID,
		WorkspaceKey:       workspaceKey,
		AgentbusStateRoot:  opts.AgentbusStateRoot,
		Params:             params,
		Kind:               effectiveTaskKind(opts),
		ContractKind:       contractKindForPolicy(turnPolicy, opts.NoContract),
		NoContract:         opts.NoContract,
		HandoffSource:      resolved.Source == handoff.SourceHandoffPromptFile,
		HandoffPayloadPath: handoffPayloadPathForIntent(resolved),
		Model:              modelEffort.Model,
		Effort:             modelEffort.Effort,
		Origin:             envelopeOriginPointer(taskEnvelopeOrigin(opts)),
		ReviewWorkspace:    opts.ReviewWorkspace,
		Phase:              submissionPhasePrepared,
		CreatedAt:          now,
		UpdatedAt:          now,
	}, nil
}

func handoffPayloadPathForIntent(resolved handoff.ResolvedPrompt) string {
	if resolved.Source != handoff.SourceHandoffPromptFile {
		return ""
	}
	return resolved.HandoffPath
}

func submitIntentWithRetry(ctx context.Context, c agentbusClient, hello client.HelloResult, stateDir string, intent *submissionIntent, requiredCapabilities []string) (client.JobSubmitResult, agentbusClient, client.HelloResult, error) {
	if intent == nil {
		return client.JobSubmitResult{}, c, hello, errors.New("submission intent is nil")
	}
	if err := transitionSubmissionIntent(stateDir, intent, submissionPhaseInFlight, nil); err != nil {
		return client.JobSubmitResult{}, c, hello, err
	}
	for attempt := 1; attempt <= maxSubmissionAttempts; attempt++ {
		submitted, err := c.JobSubmit(ctx, intent.Params)
		if err == nil {
			return submitted, c, hello, nil
		}
		opErr := agentbusOperationError(err)
		classification, ok := classifyAgentbusError(opErr)
		if !ok {
			return client.JobSubmitResult{}, c, hello, opErr
		}
		lastError := agentbusErrorFromClassification(classification)
		if classification.Retryable && attempt < maxSubmissionAttempts {
			if err := transitionSubmissionIntent(stateDir, intent, submissionPhaseInFlight, func(intent *submissionIntent) {
				intent.LastError = lastError
			}); err != nil {
				return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: opErr}
			}
			_ = c.Close()
			next, nextHello, connectErr := connectAgentbusCommandAtRoot(ctx, requiredCapabilities, intent.AgentbusStateRoot)
			if connectErr != nil {
				opErr = agentbusOperationError(connectErr)
				classification, _ = classifyAgentbusError(opErr)
				lastError = agentbusErrorFromClassification(classification)
				if err := transitionSubmissionIntent(stateDir, intent, submissionPhaseBlocked, func(intent *submissionIntent) {
					intent.LastError = lastError
				}); err != nil {
					return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: opErr}
				}
				return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: opErr}
			}
			c = next
			hello = nextHello
			continue
		}
		phase := submissionPhaseForError(classification)
		if err := transitionSubmissionIntent(stateDir, intent, phase, func(intent *submissionIntent) {
			intent.LastError = lastError
		}); err != nil {
			if classification.Retryable || classification.PreserveIntent {
				return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: opErr}
			}
			return client.JobSubmitResult{}, c, hello, opErr
		}
		if classification.Retryable || classification.PreserveIntent {
			return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: opErr}
		}
		return client.JobSubmitResult{}, c, hello, opErr
	}
	return client.JobSubmitResult{}, c, hello, submissionUnresolvedError{RequestID: intent.RequestID, Err: errors.New("submission retry attempts exhausted")}
}

func submissionPhaseForError(classification agentbusErrorClassification) string {
	if classification.Retryable || classification.PreserveIntent {
		return submissionPhaseBlocked
	}
	return submissionPhaseRejected
}

func recoverTaskSubmission(opts taskOptions, stderr io.Writer) (taskRunResult, error) {
	ctx := context.Background()
	intent, found, err := loadSubmissionIntent(opts.StateDir, opts.RecoverRequest)
	if err != nil {
		return taskRunResult{}, err
	}
	if !found {
		return taskRunResult{}, fmt.Errorf("no local submission intent for request %s; recover with: delegate task --recover-request %s", opts.RecoverRequest, opts.RecoverRequest)
	}
	recordedRoot, err := canonicalizeAgentbusStateRoot("submission intent agentbus_state_root", intent.AgentbusStateRoot)
	if err != nil {
		return taskRunResult{}, err
	}
	intent.AgentbusStateRoot = recordedRoot
	requiredCapabilities := requiredCapabilitiesForPolicy(intent.Params.TaskSpec.Policy)
	c, hello, err := connectAgentbusCommandAtRoot(ctx, requiredCapabilities, intent.AgentbusStateRoot)
	if err != nil {
		return taskRunResult{}, agentbusOperationError(err)
	}
	defer func() { _ = c.Close() }()
	submitted, currentClient, currentHello, err := submitIntentWithRetry(ctx, c, hello, opts.StateDir, &intent, requiredCapabilities)
	c = currentClient
	hello = currentHello
	if err != nil {
		return taskRunResult{}, err
	}
	taskOpts := taskOptionsFromIntent(opts.StateDir, intent, submitted)
	taskOpts.Background = opts.Background
	taskOpts.Wait = opts.Wait
	var warnings []string
	resolved := resolvedPromptFromIntent(intent)
	ackWarnings, acknowledged, err := acknowledgeSubmittedTask(taskOpts, resolved, submitted, intent.ContractKind, "after recovery")
	warnings = append(warnings, ackWarnings...)
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	if !acknowledged {
		env, envelopeErr := newLaunchEnvelopeForTask(submitted.JobID, submitted.State, taskOpts)
		if envelopeErr != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, envelopeErr
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	}
	if err := transitionSubmissionIntent(opts.StateDir, &intent, submissionPhaseAcknowledged, func(intent *submissionIntent) {
		intent.JobID = submitted.JobID
		intent.Deduplicated = submitted.Deduplicated
		intent.LastError = nil
	}); err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	return submittedTaskRunResult(ctx, c, hello, taskOpts, submitted, warnings, stderr)
}

func taskOptionsFromIntent(stateDir string, intent submissionIntent, submitted client.JobSubmitResult) taskOptions {
	modelEffort := config.ModelEffortResolution{Model: intent.Model, Effort: intent.Effort}
	origin := envelopeOrigin{}
	if intent.Origin != nil {
		origin = *intent.Origin
	}
	spec := intent.Params.TaskSpec
	timeout, timeoutSet := timeoutFromMillis(spec.TimeoutMs)
	return taskOptions{
		Backend:            spec.Backend,
		CWD:                spec.CWD,
		Model:              spec.Model,
		Effort:             spec.Effort,
		Timeout:            timeout,
		TimeoutSet:         timeoutSet,
		Write:              spec.Write,
		NoContract:         intent.NoContract,
		ReportCorrectionOf: recoveredReportCorrectionOf(intent),
		StateDir:           stateDir,
		Kind:               intent.Kind,
		ReviewWorkspace:    intent.ReviewWorkspace,
		ModelEffort:        modelEffort,
		AuditOrigin:        origin,
		AgentbusStateRoot:  intent.AgentbusStateRoot,
		RequestID:          intent.RequestID,
		WorkspaceKey:       intent.WorkspaceKey,
		SubmissionState:    submitted.State,
		Deduplicated:       submitted.Deduplicated,
		TimeoutResolution:  timeoutResolutionForSubmission(timeout, timeoutSet, submitted),
	}
}

func recoveredReportCorrectionOf(intent submissionIntent) string {
	if intent.Params.TaskSpec.Tags == nil {
		return ""
	}
	return intent.Params.TaskSpec.Tags[reportCorrectionOfTag]
}

func resolvedPromptFromIntent(intent submissionIntent) handoff.ResolvedPrompt {
	resolved := handoff.ResolvedPrompt{Prompt: intent.Params.TaskSpec.Prompt, Source: handoff.SourcePrompt}
	if intent.HandoffSource {
		resolved.Source = handoff.SourceHandoffPromptFile
		resolved.HandoffPath = intent.HandoffPayloadPath
	}
	return resolved
}

func persistDelegateJobInputWithoutPayloadCleanup(opts taskOptions, resolved handoff.ResolvedPrompt, jobID string) (handoff.JobInput, error) {
	if input, found, err := acknowledgedJobInputFromMetadata(opts, jobID); err != nil || found {
		return input, err
	}
	if resolved.Source == handoff.SourceHandoffPromptFile {
		resolved.Source = handoff.SourcePrompt
		resolved.HandoffPath = ""
	}
	return persistDelegateJobInput(opts, resolved, jobID)
}

func removeResolvedPromptPayload(resolved handoff.ResolvedPrompt) error {
	if resolved.Source != handoff.SourceHandoffPromptFile || resolved.HandoffPath == "" {
		return nil
	}
	if err := os.Remove(resolved.HandoffPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("handoff prompt payload %q could not be removed after acknowledgement: %w", resolved.HandoffPath, err)
	}
	dir, err := os.Open(filepath.Dir(resolved.HandoffPath))
	if err != nil {
		return fmt.Errorf("sync handoff prompt directory after acknowledgement: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync handoff prompt directory after acknowledgement: %w", err)
	}
	return nil
}

var saveDelegateJobMetadata = saveJobMetadata
var saveLaunchedJobMetadataFallback = saveJobMetadata

func persistDelegateJobInput(opts taskOptions, resolved handoff.ResolvedPrompt, jobID string) (handoff.JobInput, error) {
	return handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: opts.StateDir,
		JobID:    jobID,
		Prompt:   resolved,
	})
}

func delegateJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) jobMetadata {
	modelEffort := normalizedModelEffort(taskModelEffort(opts))
	return jobMetadata{
		Schema:             jobMetadataSchema,
		JobID:              jobID,
		RequestID:          opts.RequestID,
		WorkspaceKey:       opts.WorkspaceKey,
		Kind:               effectiveTaskKind(opts),
		Backend:            opts.Backend,
		CWD:                opts.CWD,
		ContractKind:       contractKind,
		NoContract:         opts.NoContract,
		ReportCorrectionOf: opts.ReportCorrectionOf,
		JobInputPath:       input.Path,
		ReviewWorkspace:    opts.ReviewWorkspace,
		AgentbusStateRoot:  opts.AgentbusStateRoot,
		SubmissionState:    opts.SubmissionState,
		State:              opts.SubmissionState,
		Deduplicated:       opts.Deduplicated,
		Model:              modelEffort.Model,
		Effort:             modelEffort.Effort,
		Timeout:            normalizedTimeout(opts.TimeoutResolution),
		Origin:             envelopeOriginPointer(taskEnvelopeOrigin(opts)),
	}
}

func taskModelEffort(opts taskOptions) config.ModelEffortResolution {
	return opts.ModelEffort
}

func persistLaunchedJobMetadata(opts taskOptions, input handoff.JobInput, jobID, contractKind string) (string, error) {
	meta := delegateJobMetadata(opts, input, jobID, contractKind)
	if existing, found, err := acknowledgedJobMetadata(opts, jobID); err != nil {
		return "", err
	} else if found {
		meta = mergeAcknowledgedJobMetadata(existing, meta)
		if reflect.DeepEqual(existing, meta) {
			return "", nil
		}
	}
	if err := saveDelegateJobMetadata(opts.StateDir, meta); err != nil {
		if fallbackErr := saveLaunchedJobMetadataFallback(opts.StateDir, meta); fallbackErr != nil {
			return "", fmt.Errorf("persist launched job metadata for %s: primary: %v; state-dir fallback: %w", jobID, err, fallbackErr)
		}
		return fmt.Sprintf("delegate job metadata for %s required the durable state-directory fallback after the primary write failed: %v", jobID, err), nil
	}
	return "", nil
}

func acknowledgeSubmittedTask(opts taskOptions, resolved handoff.ResolvedPrompt, submitted client.JobSubmitResult, contractKind, inputContext string) ([]string, bool, error) {
	var warnings []string
	input, inputErr := persistDelegateJobInputWithoutPayloadCleanup(opts, resolved, submitted.JobID)
	if inputErr != nil {
		warnings = append(warnings, fmt.Sprintf("job input for %s could not be persisted %s: %v", submitted.JobID, inputContext, inputErr))
	}
	if warning, err := persistLaunchedJobMetadata(opts, input, submitted.JobID, contractKind); err != nil {
		warnings = append(warnings, err.Error())
		return warnings, false, nil
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	if err := removeResolvedPromptPayload(resolved); err != nil {
		warnings = append(warnings, err.Error())
	}
	return warnings, true, nil
}

func submittedTaskRunResult(ctx context.Context, c agentbusClient, hello client.HelloResult, opts taskOptions, submitted client.JobSubmitResult, warnings []string, stderr io.Writer) (taskRunResult, error) {
	terminalOptions := terminalEnvelopeOptions{
		ModelsReportedCapable: hello.Capabilities["models.reported"],
		Timeout:               opts.TimeoutResolution,
		RequestID:             opts.RequestID,
		Deduplicated:          opts.Deduplicated,
		DeduplicatedSet:       true,
	}
	cleanupWarnings := newLocalCleanupWarnings(stderr)
	if opts.Background {
		env, err := newLaunchEnvelopeForTask(submitted.JobID, submitted.State, opts)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	}
	if engine.IsTerminal(submitted.State) {
		jobResult, err := submittedTerminalJob(ctx, c, opts.StateDir, submitted.JobID, cleanupWarnings)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		corrected, nextClient, nextHello, correctionWarnings, err := maybeCorrectDelegateReport(ctx, c, hello, opts.StateDir, jobResult, cleanupWarnings)
		c = nextClient
		hello = nextHello
		warnings = append(warnings, correctionWarnings...)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		envelopeOptions := correctionEnvelopeOptions(submitted.JobID, corrected.result.JobID, terminalOptions)
		envelopeOptions.ModelsReportedCapable = hello.Capabilities["models.reported"]
		env, err := terminalEnvelopeFromJobResultWithOptions(opts.StateDir, corrected.result, corrected.envelopeOptions(envelopeOptions))
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Terminal: &env, Warnings: warnings, Submitted: true}, nil
	}
	if opts.Wait {
		jobResult, err := waitForTerminalJobResult(ctx, c, opts.StateDir, submitted.JobID, cleanupWarnings)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		corrected, nextClient, nextHello, correctionWarnings, err := maybeCorrectDelegateReport(ctx, c, hello, opts.StateDir, jobResult, cleanupWarnings)
		c = nextClient
		hello = nextHello
		warnings = append(warnings, correctionWarnings...)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		envelopeOptions := correctionEnvelopeOptions(submitted.JobID, corrected.result.JobID, terminalOptions)
		envelopeOptions.ModelsReportedCapable = hello.Capabilities["models.reported"]
		env, err := terminalEnvelopeFromJobResultWithOptions(opts.StateDir, corrected.result, corrected.envelopeOptions(envelopeOptions))
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Terminal: &env, Warnings: warnings, Submitted: true}, nil
	}
	env, err := newLaunchEnvelopeForTask(submitted.JobID, submitted.State, opts)
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
}

func correctionEnvelopeOptions(originalJobID, finalJobID string, options terminalEnvelopeOptions) terminalEnvelopeOptions {
	if finalJobID == "" || finalJobID == originalJobID {
		return options
	}
	options.RequestID = ""
	options.Deduplicated = false
	options.DeduplicatedSet = false
	return options
}

func writeWarnings(stderr io.Writer, warnings []string) error {
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func newLaunchEnvelopeForTask(jobID string, state engine.JobState, opts taskOptions) (LaunchEnvelope, error) {
	return newLaunchEnvelopeWithOptions(jobID, state, launchEnvelopeOptions{
		ModelEffort:  taskModelEffort(opts),
		Timeout:      opts.TimeoutResolution,
		Origin:       taskEnvelopeOrigin(opts),
		RequestID:    opts.RequestID,
		Deduplicated: opts.Deduplicated,
	})
}

func acknowledgedJobInputFromMetadata(opts taskOptions, jobID string) (handoff.JobInput, bool, error) {
	meta, found, err := acknowledgedJobMetadata(opts, jobID)
	if err != nil || !found {
		return handoff.JobInput{}, false, err
	}
	return handoff.JobInput{JobID: jobID, Path: meta.JobInputPath}, true, nil
}

func acknowledgedJobMetadata(opts taskOptions, jobID string) (jobMetadata, bool, error) {
	meta, found, err := loadJobMetadata(opts.StateDir, jobID)
	if err != nil || !found {
		return jobMetadata{}, false, err
	}
	if err := validateAcknowledgedJobMetadata(meta, opts, jobID); err != nil {
		return jobMetadata{}, false, err
	}
	return meta, true, nil
}

func validateAcknowledgedJobMetadata(meta jobMetadata, opts taskOptions, jobID string) error {
	if meta.JobID != jobID {
		return fmt.Errorf("delegate job metadata %q has job_id %q", jobID, meta.JobID)
	}
	if opts.RequestID != "" && meta.RequestID != "" && meta.RequestID != opts.RequestID {
		return fmt.Errorf("delegate job metadata for %s belongs to request %s, not %s", jobID, meta.RequestID, opts.RequestID)
	}
	if opts.WorkspaceKey != "" && meta.WorkspaceKey != "" && meta.WorkspaceKey != opts.WorkspaceKey {
		return fmt.Errorf("delegate job metadata for %s belongs to workspace %s, not %s", jobID, meta.WorkspaceKey, opts.WorkspaceKey)
	}
	return nil
}

func mergeAcknowledgedJobMetadata(existing, next jobMetadata) jobMetadata {
	merged := existing
	if merged.Schema == 0 || merged.Schema < next.Schema {
		merged.Schema = next.Schema
	}
	if merged.RequestID == "" {
		merged.RequestID = next.RequestID
	}
	if merged.WorkspaceKey == "" {
		merged.WorkspaceKey = next.WorkspaceKey
	}
	if merged.Kind == "" {
		merged.Kind = next.Kind
	}
	if merged.Backend == "" {
		merged.Backend = next.Backend
	}
	if merged.CWD == "" {
		merged.CWD = next.CWD
	}
	if merged.ContractKind == "" {
		merged.ContractKind = next.ContractKind
	}
	if next.NoContract {
		merged.NoContract = true
	}
	if merged.ReportCorrectionOf == "" {
		merged.ReportCorrectionOf = next.ReportCorrectionOf
	}
	if merged.ReportCorrectionJobID == "" {
		merged.ReportCorrectionJobID = next.ReportCorrectionJobID
	}
	if merged.AgentbusStateRoot == "" {
		merged.AgentbusStateRoot = next.AgentbusStateRoot
	}
	if next.SubmissionState != "" {
		merged.SubmissionState = next.SubmissionState
	}
	if next.Deduplicated {
		merged.Deduplicated = true
	}
	if dimensionResolutionEmpty(merged.Model) {
		merged.Model = next.Model
	}
	if dimensionResolutionEmpty(merged.Effort) {
		merged.Effort = next.Effort
	}
	if dimensionResolutionEmpty(merged.Timeout) || merged.Timeout.Source == "unknown" {
		merged.Timeout = next.Timeout
	}
	if merged.Origin == nil {
		merged.Origin = next.Origin
	}
	return merged
}

func dimensionResolutionEmpty(value config.DimensionResolution) bool {
	return value.Requested == "" && value.Effective == "" && value.Source == ""
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
	if opts.ReportCorrectionOf != "" {
		tags[reportCorrectionTag] = "true"
		tags[reportCorrectionOfTag] = opts.ReportCorrectionOf
	}
	return tags
}

func effectiveTaskKind(opts taskOptions) string {
	if opts.Kind == "" {
		return taskKind
	}
	return opts.Kind
}

func timeoutMillis(timeout time.Duration, set bool) *int64 {
	if !set || timeout == 0 {
		return nil
	}
	ms := timeout.Milliseconds()
	if ms == 0 {
		ms = 1
	}
	return &ms
}

func timeoutFromMillis(timeout *int64) (time.Duration, bool) {
	if timeout == nil {
		return 0, false
	}
	return time.Duration(*timeout) * time.Millisecond, true
}

func requestedTimeoutValue(timeout time.Duration, set bool) string {
	if !set {
		return ""
	}
	return timeout.String()
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
