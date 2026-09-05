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
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

// taskOptions contains the public task surface. Review supplies CWD and
// LogicalWorkspace directly so it can share the same one-shot admission path.
type taskOptions struct {
	Backend     string
	CWD         string
	Write       bool
	Model       string
	Effort      string
	Timeout     time.Duration
	PromptFile  string
	SchemaFile  string
	RequestID   string
	ResumeJobID string
	Tags        map[string]string

	LogicalWorkspace string
	WorkspaceKey     string
}

const (
	agentbusMaxTimeout = 4 * time.Hour
	readOnlyTaskHint   = "notice: task will run with a read-only backend profile; pass --write for edits or builds."
)

// taskSubmitReceipt projects the Agentbus admission response together with the
// workspace key sent to Agentbus. In particular, Timeout retains Agentbus's
// milliseconds and source string without local reinterpretation.
type taskSubmitReceipt struct {
	RequestID    string             `json:"requestId"`
	WorkspaceKey string             `json:"workspaceKey"`
	JobID        string             `json:"jobId"`
	State        client.PublicState `json:"state"`
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

	submitted, c, _, err := submitTask(context.Background(), &opts, prompt, schema)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if err := writeTaskSubmitReceipt(stdout, opts, submitted); err != nil {
		return 0, submissionError(opts.RequestID, err)
	}
	return 0, nil
}

func writeTaskSubmitReceipt(stdout io.Writer, opts taskOptions, submitted client.JobSubmitResult) error {
	return writeJSONLine(stdout, taskSubmitReceipt{
		RequestID:    opts.RequestID,
		WorkspaceKey: opts.WorkspaceKey,
		JobID:        submitted.JobID,
		State:        submitted.State,
		Deduplicated: submitted.Deduplicated,
		Model:        opts.Model,
		Effort:       opts.Effort,
		Timeout:      taskSubmitTimeoutFromAgentbus(submitted.Timeout),
	})
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
	fs.StringVar(&opts.ResumeJobID, "resume", "", "resume a prior job; creates a new job with a fresh deadline")
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

type submissionUnresolvedError struct {
	err error
}

func (err *submissionUnresolvedError) Error() string {
	return err.err.Error()
}

func (err *submissionUnresolvedError) Unwrap() error {
	return err.err
}

func submissionJobID(err error) string {
	var rpcErr *client.RPCError
	if errors.As(err, &rpcErr) && rpcErr != nil {
		return rpcErr.Object.Data.JobID
	}
	return ""
}

func submissionError(requestID string, err error) error {
	if jobID := submissionJobID(err); jobID != "" {
		return fmt.Errorf("submission failed for requestId %s jobId %s: %w", requestID, jobID, err)
	}
	return fmt.Errorf("submission failed for requestId %s: %w", requestID, err)
}

// submitTask is the one-shot Agentbus admission primitive shared by task and
// review. It deliberately does no retry, polling, persistence, or cleanup.
func submitTask(ctx context.Context, opts *taskOptions, prompt string, schema json.RawMessage) (client.JobSubmitResult, agentbusClient, string, error) {
	if err := ensureTaskRequestID(opts); err != nil {
		return client.JobSubmitResult{}, nil, "", err
	}
	logicalWorkspace := opts.LogicalWorkspace
	if logicalWorkspace == "" {
		logicalWorkspace = opts.CWD
	}
	workspaceKey, err := workspaceKeyForLogicalWorkspace(logicalWorkspace)
	if err != nil {
		return client.JobSubmitResult{}, nil, "", submissionError(opts.RequestID, err)
	}
	opts.WorkspaceKey = workspaceKey
	c, hello, stateRoot, err := connectAgentbusCommand(ctx)
	if err != nil {
		return client.JobSubmitResult{}, nil, "", submissionError(opts.RequestID, err)
	}
	if err := validateBackend(hello, opts.Backend); err != nil {
		_ = c.Close()
		return client.JobSubmitResult{}, nil, "", submissionError(opts.RequestID, err)
	}

	params := client.JobSubmitParams{
		WorkspaceKey: workspaceKey,
		RequestID:    opts.RequestID,
		TaskSpec: client.TaskSpec{
			Backend:      opts.Backend,
			CWD:          opts.CWD,
			Write:        opts.Write,
			ResumeJobID:  opts.ResumeJobID,
			Model:        optionalTaskSpecString(opts.Model),
			Effort:       optionalTaskSpecString(opts.Effort),
			Prompt:       prompt,
			OutputSchema: append(json.RawMessage(nil), schema...),
			Tags:         optionalTaskSpecTags(opts.Tags),
			TimeoutMS:    timeoutMillis(opts.Timeout),
		},
	}
	submitted, err := c.JobSubmit(ctx, params)
	if err != nil {
		_ = c.Close()
		if opts.ResumeJobID != "" && submissionJobID(err) == opts.ResumeJobID {
			return client.JobSubmitResult{}, nil, stateRoot, err
		}
		return client.JobSubmitResult{}, nil, stateRoot, &submissionUnresolvedError{err: submissionError(opts.RequestID, err)}
	}
	return submitted, c, stateRoot, nil
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

func optionalTaskSpecString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalTaskSpecTags does not clone. The caller's map was already copied into
// taskOptions at parse time, the submit that follows is synchronous, and nothing
// between the two mutates it.
func optionalTaskSpecTags(tags map[string]string) *map[string]string {
	if len(tags) == 0 {
		return nil
	}
	return &tags
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
