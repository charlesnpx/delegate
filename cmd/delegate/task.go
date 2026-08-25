package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
)

// taskOptions contains the small public task surface plus the review-only
// lifecycle fields that review supplies directly. The task command itself
// never persists any of those lifecycle fields.
type taskOptions struct {
	Backend    string
	CWD        string
	Write      bool
	Model      string
	Effort     string
	Timeout    time.Duration
	PromptFile string
	SchemaFile string
	RequestID  string
	Tags       map[string]string

	// review uses the same Agentbus submission primitive, but retains its
	// pre-existing local artifact lifecycle for result/status/cancel.
	Background        bool
	Wait              bool
	StateDir          string
	Kind              string
	ReviewWorkspace   string
	LogicalWorkspace  string
	ModelEffort       config.ModelEffortResolution
	BackendProfile    config.DimensionResolution
	TimeoutResolution config.DimensionResolution
	AgentbusStateRoot string
	WorkspaceKey      string
	SubmissionState   engine.JobState
	Deduplicated      bool
}

type taskRunResult struct {
	Launch    *LaunchEnvelope
	Terminal  *TerminalEnvelope
	Warnings  []string
	Submitted bool
}

const (
	agentbusMaxTimeout           = 4 * time.Hour
	readOnlyTaskHint             = "notice: task will run with a read-only backend profile; pass --write for edits or builds."
	backendProfileReadOnly       = "read-only"
	backendProfileWorkspaceWrite = "workspace-write"
	jsonSchemaRetryTemplate      = "The previous response did not conform to the requested JSON Schema: {{missing}}.\n\nReturn only a corrected response."
)

// taskSubmitReceipt is intentionally a direct projection of the Agentbus
// admission response. In particular, Timeout retains Agentbus's milliseconds
// and source string rather than passing through a Delegate envelope type.
type taskSubmitReceipt struct {
	RequestID    string             `json:"requestId"`
	JobID        string             `json:"jobId"`
	State        engine.JobState    `json:"state"`
	Deduplicated bool               `json:"deduplicated"`
	Model        string             `json:"model,omitempty"`
	Effort       string             `json:"effort,omitempty"`
	Timeout      *taskSubmitTimeout `json:"timeout"`
}

// taskSubmitTimeout keeps Agentbus's resolved millisecond value and source
// unchanged while using the public receipt field name from the task contract.
// It deliberately has no duration-string representation or local resolution.
type taskSubmitTimeout struct {
	EffectiveMS int64  `json:"effectiveMs"`
	Source      string `json:"source"`
}

type taskTagsFlag map[string]string

func (tags taskTagsFlag) String() string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+tags[key])
	}
	return strings.Join(values, ",")
}

func (tags *taskTagsFlag) Set(value string) error {
	key, tagValue, found := strings.Cut(value, "=")
	if !found || key == "" {
		return fmt.Errorf("--tag must be key=value")
	}
	if *tags == nil {
		*tags = taskTagsFlag{}
	}
	(*tags)[key] = tagValue
	return nil
}

func runTask(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	opts, err := parseTaskOptions(args, stderr)
	if err != nil {
		return 0, err
	}
	prompt, err := readTaskPrompt(opts.PromptFile, stdin)
	if err != nil {
		return 0, err
	}
	schema, err := readTaskSchema(opts.SchemaFile)
	if err != nil {
		return 0, err
	}
	if !opts.Write {
		if _, err := fmt.Fprintln(stderr, readOnlyTaskHint); err != nil {
			return 0, err
		}
	}

	submitted, c, _, err := submitTask(context.Background(), &opts, prompt, turnPolicyForSchema(schema))
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()

	if err := writeJSONLine(stdout, taskSubmitReceipt{
		RequestID:    opts.RequestID,
		JobID:        submitted.JobID,
		State:        submitted.State,
		Deduplicated: submitted.Deduplicated,
		Model:        opts.Model,
		Effort:       opts.Effort,
		Timeout:      taskSubmitTimeoutFromAgentbus(submitted.Timeout),
	}); err != nil {
		return 0, submissionError(opts.RequestID, err)
	}
	return 0, nil
}

func taskSubmitTimeoutFromAgentbus(timeout *engine.TimeoutResolution) *taskSubmitTimeout {
	if timeout == nil {
		return nil
	}
	return &taskSubmitTimeout{EffectiveMS: timeout.Effective, Source: timeout.Source}
}

func parseTaskOptions(args []string, stderr io.Writer) (taskOptions, error) {
	var opts taskOptions
	var tags taskTagsFlag
	fs := flag.NewFlagSet("delegate task", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Backend, "backend", "", "backend name")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute working directory")
	fs.BoolVar(&opts.Write, "write", false, "allow backend writes")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout; 0 leaves the deadline to the daemon default")
	fs.StringVar(&opts.PromptFile, "prompt-file", "", "read prompt from file, or - for stdin")
	fs.StringVar(&opts.SchemaFile, "schema-file", "", "read optional JSON Schema output contract from file")
	fs.StringVar(&opts.RequestID, "request-id", "", "caller-owned request identity")
	fs.Var(&tags, "tag", "task tag in key=value form (repeatable)")
	if err := fs.Parse(args); err != nil {
		return taskOptions{}, err
	}
	if fs.NArg() != 0 {
		return taskOptions{}, fmt.Errorf("delegate task does not accept positional arguments")
	}
	if opts.Backend == "" {
		return taskOptions{}, fmt.Errorf("delegate task requires --backend")
	}
	if opts.PromptFile == "" {
		return taskOptions{}, fmt.Errorf("delegate task requires --prompt-file")
	}
	if err := validateTimeoutOption(opts.Timeout); err != nil {
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
	if err := ensureTaskRequestID(&opts); err != nil {
		return taskOptions{}, err
	}
	if len(tags) > 0 {
		opts.Tags = cloneTaskTags(tags)
	}
	return opts, nil
}

func validateTimeoutOption(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	if timeout > agentbusMaxTimeout {
		return fmt.Errorf("--timeout must be at most %s", agentbusMaxTimeout)
	}
	return nil
}

func readTaskPrompt(path string, stdin io.Reader) (string, error) {
	if path == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read --prompt-file -: %w", err)
		}
		return string(raw), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read --prompt-file %q: %w", path, err)
	}
	return string(raw), nil
}

func readTaskSchema(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --schema-file %q: %w", path, err)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func turnPolicyForSchema(schema json.RawMessage) *engine.TurnPolicy {
	if schema == nil {
		return nil
	}
	return &engine.TurnPolicy{
		Contract: &engine.ContractSpec{JSONSchema: append(json.RawMessage(nil), schema...)},
		Retry: &engine.RetryPolicy{
			Max:      1,
			Template: jsonSchemaRetryTemplate,
		},
	}
}

func ensureTaskRequestID(opts *taskOptions) error {
	if opts.RequestID == "" {
		requestID, err := newRequestID()
		if err != nil {
			return err
		}
		opts.RequestID = requestID
	}
	return validateRequestID(opts.RequestID)
}

func submissionError(requestID string, err error) error {
	return fmt.Errorf("submission failed for requestId %s: %w", requestID, err)
}

// submissionUnresolvedError marks a JobSubmit failure whose request may have
// been accepted by Agentbus before its acknowledgement was lost.
type submissionUnresolvedError struct {
	Err error
}

func (err submissionUnresolvedError) Error() string {
	return err.Err.Error()
}

func (err submissionUnresolvedError) Unwrap() error {
	return err.Err
}

// submitTask is the one-shot Agentbus admission primitive shared by task and
// review. It deliberately does no retry, polling, persistence, or cleanup.
func submitTask(ctx context.Context, opts *taskOptions, prompt string, turnPolicy *engine.TurnPolicy) (client.JobSubmitResult, agentbusClient, client.HelloResult, error) {
	if err := ensureTaskRequestID(opts); err != nil {
		return client.JobSubmitResult{}, nil, client.HelloResult{}, err
	}
	logicalWorkspace := opts.LogicalWorkspace
	if logicalWorkspace == "" {
		logicalWorkspace = opts.CWD
	}
	workspaceKey, err := workspaceKeyForLogicalWorkspace(logicalWorkspace)
	if err != nil {
		return client.JobSubmitResult{}, nil, client.HelloResult{}, submissionError(opts.RequestID, err)
	}
	requiredCapabilities := requiredCapabilitiesForPolicy(turnPolicy)
	c, hello, stateRoot, err := connectAgentbusCommand(ctx, requiredCapabilities)
	if err != nil {
		return client.JobSubmitResult{}, nil, client.HelloResult{}, submissionError(opts.RequestID, agentbusOperationError(err))
	}
	if err := validateBackend(hello, opts.Backend); err != nil {
		_ = c.Close()
		return client.JobSubmitResult{}, nil, client.HelloResult{}, submissionError(opts.RequestID, err)
	}

	params := client.JobSubmitParams{
		WorkspaceKey: workspaceKey,
		RequestID:    opts.RequestID,
		TaskSpec: client.TaskSpec{
			Backend:   opts.Backend,
			CWD:       opts.CWD,
			Write:     opts.Write,
			Model:     opts.Model,
			Effort:    opts.Effort,
			Prompt:    prompt,
			Policy:    turnPolicy,
			Tags:      cloneTaskTags(opts.Tags),
			TimeoutMs: timeoutMillis(opts.Timeout),
		},
	}
	submitted, err := c.JobSubmit(ctx, params)
	if err != nil {
		_ = c.Close()
		return client.JobSubmitResult{}, nil, client.HelloResult{}, submissionError(opts.RequestID, submissionUnresolvedError{Err: agentbusOperationError(err)})
	}
	opts.AgentbusStateRoot = stateRoot
	opts.WorkspaceKey = workspaceKey
	opts.SubmissionState = submitted.State
	opts.Deduplicated = submitted.Deduplicated
	return submitted, c, hello, nil
}

func cloneTaskTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(tags))
	for key, value := range tags {
		cloned[key] = value
	}
	return cloned
}

// executeTask retains the review command's existing lifecycle after the shared
// one-shot submission. The public task command intentionally does not call it.
func executeTask(opts taskOptions, prompt string, turnPolicy *engine.TurnPolicy, stderr io.Writer) (taskRunResult, error) {
	ctx := context.Background()
	submitted, c, hello, err := submitTask(ctx, &opts, prompt, turnPolicy)
	if err != nil {
		return taskRunResult{}, err
	}
	defer func() { _ = c.Close() }()
	opts.TimeoutResolution = timeoutResolutionForSubmission(opts.Timeout, submitted)

	warnings, acknowledged, err := acknowledgeSubmittedTask(opts, prompt, submitted, "after submission")
	if err != nil {
		return taskRunResult{Submitted: true, Warnings: warnings}, err
	}
	if !acknowledged {
		env, err := newLaunchEnvelopeForTask(submitted.JobID, submitted.State, opts)
		if err != nil {
			return taskRunResult{Submitted: true, Warnings: warnings}, err
		}
		return taskRunResult{Launch: &env, Warnings: warnings, Submitted: true}, nil
	}
	return submittedTaskRunResult(ctx, c, hello, opts, submitted, warnings, stderr)
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

func persistDelegateJobInput(opts taskOptions, prompt, jobID string) (handoff.JobInput, error) {
	return handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: opts.StateDir,
		JobID:    jobID,
		Prompt:   prompt,
	})
}

func delegateJobMetadata(opts taskOptions, input handoff.JobInput, jobID string) jobMetadata {
	modelEffort := normalizedModelEffort(taskModelEffort(opts))
	return jobMetadata{
		Schema:            jobMetadataSchema,
		JobID:             jobID,
		RequestID:         opts.RequestID,
		WorkspaceKey:      opts.WorkspaceKey,
		Kind:              effectiveTaskKind(opts),
		Backend:           opts.Backend,
		CWD:               opts.CWD,
		JobInputPath:      input.Path,
		ReviewWorkspace:   opts.ReviewWorkspace,
		AgentbusStateRoot: opts.AgentbusStateRoot,
		SubmissionState:   opts.SubmissionState,
		State:             opts.SubmissionState,
		Deduplicated:      opts.Deduplicated,
		Model:             modelEffort.Model,
		Effort:            modelEffort.Effort,
		BackendProfile:    taskBackendProfile(opts),
		Timeout:           normalizedTimeout(opts.TimeoutResolution),
	}
}

func taskModelEffort(opts taskOptions) config.ModelEffortResolution {
	return opts.ModelEffort
}

func taskBackendProfile(opts taskOptions) config.DimensionResolution {
	if opts.BackendProfile.Source != "" {
		return opts.BackendProfile
	}
	if opts.Write {
		return config.DimensionResolution{Effective: backendProfileWorkspaceWrite, Source: "flag"}
	}
	return config.DimensionResolution{Effective: backendProfileReadOnly, Source: "default"}
}

var saveDelegateJobMetadata = saveJobMetadata
var saveLaunchedJobMetadataFallback = saveJobMetadata

func persistLaunchedJobMetadata(opts taskOptions, input handoff.JobInput, jobID string) (string, error) {
	meta := delegateJobMetadata(opts, input, jobID)
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

func acknowledgeSubmittedTask(opts taskOptions, prompt string, submitted client.JobSubmitResult, inputContext string) ([]string, bool, error) {
	var warnings []string
	input, inputErr := persistDelegateJobInput(opts, prompt, submitted.JobID)
	if inputErr != nil {
		warnings = append(warnings, fmt.Sprintf("job input for %s could not be persisted %s: %v", submitted.JobID, inputContext, inputErr))
	}
	if warning, err := persistLaunchedJobMetadata(opts, input, submitted.JobID); err != nil {
		warnings = append(warnings, err.Error())
		return warnings, false, nil
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	return warnings, true, nil
}

func submittedTaskRunResult(ctx context.Context, c agentbusClient, hello client.HelloResult, opts taskOptions, submitted client.JobSubmitResult, warnings []string, stderr io.Writer) (taskRunResult, error) {
	terminalOptions := terminalEnvelopeOptions{
		ModelsReportedCapable: hello.Capabilities["models.reported"],
		BackendProfile:        taskBackendProfile(opts),
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
		env, err := terminalEnvelopeFromJobResultWithOptions(opts.StateDir, jobResult.result, jobResult.envelopeOptions(terminalOptions))
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
		env, err := terminalEnvelopeFromJobResultWithOptions(opts.StateDir, jobResult.result, jobResult.envelopeOptions(terminalOptions))
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

func newLaunchEnvelopeForTask(jobID string, state engine.JobState, opts taskOptions) (LaunchEnvelope, error) {
	return newLaunchEnvelopeWithOptions(jobID, state, launchEnvelopeOptions{
		ModelEffort:    taskModelEffort(opts),
		BackendProfile: taskBackendProfile(opts),
		Timeout:        opts.TimeoutResolution,
		RequestID:      opts.RequestID,
		Deduplicated:   opts.Deduplicated,
	})
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
	legacyTimeout := merged.Schema < jobMetadataSchema
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
	if dimensionResolutionEmpty(merged.BackendProfile) {
		merged.BackendProfile = next.BackendProfile
	}
	if legacyTimeout || dimensionResolutionEmpty(merged.Timeout) || merged.Timeout.Source == "unknown" {
		merged.Timeout = next.Timeout
	}
	return merged
}

func dimensionResolutionEmpty(value config.DimensionResolution) bool {
	return value.Requested == "" && value.Effective == "" && value.Source == ""
}

func effectiveTaskKind(opts taskOptions) string {
	if opts.Kind == "" {
		return taskKind
	}
	return opts.Kind
}

func timeoutMillis(timeout time.Duration) *int64 {
	if timeout == 0 {
		return nil
	}
	ms := timeout.Milliseconds()
	if ms == 0 {
		ms = 1
	}
	return &ms
}
