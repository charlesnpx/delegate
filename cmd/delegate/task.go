package main

import (
	"context"
	"encoding/json"
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
	Prompt            optionalStringFlag
	PromptFile        string
	PromptStdin       bool
	HandoffPromptFile string
	OutputSchema      optionalStringFlag
	OutputSchemaFile  optionalStringFlag
	OutputSchemaStdin bool
	Positional        []string
	StateDir          string
	Kind              string
	ReviewWorkspace   string
	ModelEffort       config.ModelEffortResolution
	AgentbusStateRoot string
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
	var background bool
	fs.StringVar(&opts.Backend, "backend", "", "backend name")
	fs.BoolVar(&background, "background", false, "return after launch")
	fs.BoolVar(&opts.Wait, "wait", false, "wait for terminal result")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSON")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute working directory")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout")
	fs.BoolVar(&opts.Write, "write", false, "allow backend writes")
	fs.BoolVar(&opts.StrictContract, "strict-contract", false, "enable corrective retry (JSON Schema contracts retry by default)")
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
	if stdin == nil {
		stdin = os.Stdin
	}
	opts.AuditOrigin = captureTaskOrigin(opts.Origin, opts.ParentClient, opts.ParentSession, nil)
	return opts, nil
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
	c, hello, agentbusStateRoot, err := connectAgentbusCommand(ctx, requiredCapabilitiesForPolicy(turnPolicy))
	if err != nil {
		return taskRunResult{}, err
	}
	defer c.Close()
	opts.AgentbusStateRoot = agentbusStateRoot
	if err := validateBackend(hello, opts.Backend, opts.Model, opts.Effort, stderr); err != nil {
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
	submitted, err := c.JobSubmit(ctx, client.JobSubmitParams{
		// TODO(D3): persist real durable WorkspaceKey/RequestID before submit.
		WorkspaceKey: "",
		RequestID:    "",
		TaskSpec:     spec,
	})
	if err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		_ = deleteJobMetadata(opts.StateDir, pendingJobID)
		return taskRunResult{}, agentbusOperationError(err)
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
		Schema:            envelopeSchema,
		JobID:             jobID,
		Kind:              effectiveTaskKind(opts),
		Backend:           opts.Backend,
		CWD:               opts.CWD,
		ContractKind:      contractKind,
		NoContract:        opts.NoContract,
		JobInputPath:      input.Path,
		ReviewWorkspace:   opts.ReviewWorkspace,
		AgentbusStateRoot: opts.AgentbusStateRoot,
		Model:             modelEffort.Model,
		Effort:            modelEffort.Effort,
		Origin:            envelopeOriginPointer(taskEnvelopeOrigin(opts)),
	}
}

func taskModelEffort(opts taskOptions) config.ModelEffortResolution {
	return opts.ModelEffort
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
