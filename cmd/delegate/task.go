package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

// taskOptions contains the public task surface. Review supplies CWD and
// LogicalWorkspace directly so it can share the same one-shot admission path.
type taskOptions struct {
	Backend       string
	CWD           string
	Intent        engine.Intent
	InputMode     client.InputMode
	Model         string
	Effort        string
	Timeout       time.Duration
	TimeoutSet    bool
	PromptFile    string
	SchemaFile    string
	RequestID     string
	ResumeJobID   string
	RetainSession bool
	Tags          map[string]string

	LogicalWorkspace string
	WorkspaceKey     string
	Review           *reviewSubmissionEvidence
}

const (
	agentbusMaxTimeout                      = 4 * time.Hour
	taskInputModeFixed     client.InputMode = "fixed"
	taskInputModeSteerable client.InputMode = "steerable"
)

// taskSubmitReceipt projects the Agentbus admission response together with the
// workspace key sent to Agentbus. In particular, Timeout retains Agentbus's
// milliseconds and source string without local reinterpretation.
type taskSubmitReceipt struct {
	RequestID       string                    `json:"requestId"`
	WorkspaceKey    string                    `json:"workspaceKey"`
	JobID           string                    `json:"jobId"`
	State           client.PublicState        `json:"state"`
	Backend         string                    `json:"backend"`
	Intent          engine.Intent             `json:"intent"`
	Deduplicated    bool                      `json:"deduplicated"`
	InputMode       client.InputMode          `json:"inputMode"`
	ModelRequested  string                    `json:"modelRequested,omitempty"`
	EffortRequested string                    `json:"effortRequested,omitempty"`
	Timeout         *taskSubmitTimeout        `json:"timeout"`
	Queue           *taskSubmitQueue          `json:"queue,omitempty"`
	Review          *reviewSubmissionEvidence `json:"review,omitempty"`
}

type taskSubmitQueue struct {
	Position       int `json:"position"`
	BackendRunning int `json:"backendRunning"`
	BackendLimit   int `json:"backendLimit"`
	Running        int `json:"running"`
	Limit          int `json:"limit"`
}

// reviewSubmissionEvidence records prepared inputs, not provider compliance.
type reviewSubmissionEvidence struct {
	Mode           string                 `json:"mode"`
	Scope          string                 `json:"scope,omitempty"`
	InputDigest    string                 `json:"inputDigest"`
	BaseTip        string                 `json:"baseTip,omitempty"`
	ComparisonBase string                 `json:"comparisonBase,omitempty"`
	Head           string                 `json:"head,omitempty"`
	RequestDigest  string                 `json:"requestDigest,omitempty"`
	CharterHash    string                 `json:"charterHash,omitempty"`
	RecipeDigest   string                 `json:"recipeDigest,omitempty"`
	Reviewer       string                 `json:"reviewer,omitempty"`
	Subject        *reviewSubjectEvidence `json:"subject,omitempty"`
}

type reviewSubjectEvidence struct {
	Head   string `json:"head"`
	Tree   string `json:"tree,omitempty"`
	Branch string `json:"branch,omitempty"`
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
	submitted, c, _, err := submitTask(withAgentbusSelection(context.Background()), &opts, prompt, schema, stderr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if err := writeTaskSubmitReceipt(stdout, opts, submitted); err != nil {
		return 0, fmt.Errorf("submission admitted for requestId %s jobId %s; write receipt: %w", opts.RequestID, submitted.JobID, err)
	}
	if submitted.Queue != nil {
		queue := submitted.Queue
		_, _ = fmt.Fprintf(stderr, "notice: job is queued at position %d for backend %s (%d of %d backend slots in use, %d of %d daemon slots); it starts when a slot frees.\n",
			queue.Position, opts.Backend, queue.BackendRunning, queue.BackendLimit, queue.Running, queue.Limit)
	}
	return 0, nil
}

func writeTaskSubmitReceipt(stdout io.Writer, opts taskOptions, submitted client.JobSubmitResult) error {
	var queue *taskSubmitQueue
	if submitted.Queue != nil {
		q := submitted.Queue
		queue = &taskSubmitQueue{Position: q.Position, BackendRunning: q.BackendRunning, BackendLimit: q.BackendLimit, Running: q.Running, Limit: q.Limit}
	}
	return writeJSONLine(stdout, taskSubmitReceipt{
		RequestID:       opts.RequestID,
		WorkspaceKey:    opts.WorkspaceKey,
		JobID:           submitted.JobID,
		State:           submitted.State,
		Backend:         opts.Backend,
		Intent:          opts.Intent,
		Deduplicated:    submitted.Deduplicated,
		InputMode:       opts.InputMode,
		ModelRequested:  opts.Model,
		EffortRequested: opts.Effort,
		Timeout:         taskSubmitTimeoutFromAgentbus(submitted.Timeout),
		Queue:           queue,
		Review:          opts.Review,
	})
}

func taskSubmitTimeoutFromAgentbus(timeout *engine.TimeoutResolution) *taskSubmitTimeout {
	if timeout == nil {
		return nil
	}
	return &taskSubmitTimeout{EffectiveMS: timeout.Effective, Source: timeout.Source}
}

func parseTaskOptions(args []string, stderr io.Writer) (taskOptions, error) {
	opts := taskOptions{InputMode: taskInputModeSteerable, Intent: engine.IntentInspect}
	var tags taskTagsFlag
	var noSteer bool
	var intent string
	fs := flag.NewFlagSet("delegate task", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "Usage of delegate task:")
		_, _ = fmt.Fprintln(fs.Output(), "  Tasks run with host access. Inspect is an instruction, not an enforced permission boundary;")
		_, _ = fmt.Fprintln(fs.Output(), "  ordinary build/test artifacts, caches, temporary files and local services are allowed.")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.Backend, "backend", "", "backend name")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute working directory")
	fs.StringVar(&intent, "intent", string(engine.IntentInspect), "task intent: inspect or edit")
	fs.BoolVar(&noSteer, "no-steer", false, "submit a fixed-input task that cannot be steered")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.Func("timeout", "execution timeout (default 2h; 0 or 0s means unlimited; maximum 4h)", func(raw string) error {
		parsed, err := parseTimeoutDuration(raw)
		if err == nil {
			opts.Timeout = parsed
			opts.TimeoutSet = true
		}
		return err
	})
	fs.StringVar(&opts.PromptFile, "prompt-file", "", "read prompt from file, or - for stdin")
	fs.StringVar(&opts.SchemaFile, "schema-file", "", "read optional JSON Schema output contract from file")
	fs.StringVar(&opts.RequestID, "request-id", "", "caller-owned request identity")
	fs.StringVar(&opts.ResumeJobID, "resume", "", "resume a prior job; creates a new job with a fresh deadline")
	fs.BoolVar(&opts.RetainSession, "retain-session", false, "request session retention for later continuation; dispose with agentbus session discard")
	fs.Var(&tags, "tag", "task tag in key=value form (repeatable)")
	if err := fs.Parse(args); err != nil {
		return taskOptions{}, err
	}
	if fs.NArg() != 0 {
		return taskOptions{}, fmt.Errorf("delegate task does not accept positional arguments")
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["cwd"] && opts.CWD == "" {
		return taskOptions{}, fmt.Errorf("--cwd must be absolute")
	}
	for _, name := range []string{"model", "effort"} {
		if provided[name] && fs.Lookup(name).Value.String() == "" {
			return taskOptions{}, fmt.Errorf("--%s must be nonempty", name)
		}
	}
	if noSteer {
		opts.InputMode = taskInputModeFixed
	}
	opts.Intent = engine.Intent(intent)
	if !opts.Intent.Valid() {
		return taskOptions{}, fmt.Errorf("--intent must be inspect or edit")
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
	if timeout%time.Millisecond != 0 {
		return fmt.Errorf("--timeout must be a whole number of milliseconds")
	}
	return nil
}

var timeoutComponent = regexp.MustCompile(`([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)`)

func parseTimeoutDuration(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "0" {
		return 0, nil
	}
	if strings.HasPrefix(value, "-") {
		return 0, fmt.Errorf("--timeout must be non-negative")
	}
	numeric := value != ""
	for index, character := range value {
		if index == 0 && (character == '+' || character == '-') {
			continue
		}
		if character < '0' || character > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err == nil && seconds >= 0 {
			secondsSuggestion := strconv.FormatInt(seconds, 10) + "s"
			message := fmt.Sprintf("--timeout %s is missing a duration unit; use %s", value, secondsSuggestion)
			if seconds > 0 && seconds%60 == 0 {
				message += fmt.Sprintf(" or %dm", seconds/60)
			}
			return 0, fmt.Errorf("%s", message)
		}
		return 0, fmt.Errorf("--timeout %s is missing a duration unit; use an explicit duration such as 40m or 2400s", value)
	}
	_, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout %q: %w", raw, err)
	}
	// ParseDuration truncates fractions below a nanosecond. Check the exact
	// submitted value so such fractions cannot silently become zero/unlimited
	// or round down to an otherwise valid whole millisecond.
	units := map[string]*big.Rat{
		"ns": big.NewRat(1, 1000000), "us": big.NewRat(1, 1000),
		"µs": big.NewRat(1, 1000), "μs": big.NewRat(1, 1000),
		"ms": big.NewRat(1, 1), "s": big.NewRat(1000, 1),
		"m": big.NewRat(60000, 1), "h": big.NewRat(3600000, 1),
	}
	milliseconds := new(big.Rat)
	for _, part := range timeoutComponent.FindAllStringSubmatch(value, -1) {
		amount, ok := new(big.Rat).SetString(part[1])
		if !ok {
			return 0, fmt.Errorf("invalid --timeout %q", raw)
		}
		milliseconds.Add(milliseconds, amount.Mul(amount, units[part[3]]))
	}
	if !milliseconds.IsInt() {
		return 0, fmt.Errorf("--timeout must be a whole number of milliseconds")
	}
	if milliseconds.Cmp(big.NewRat(agentbusMaxTimeout.Milliseconds(), 1)) > 0 {
		return 0, fmt.Errorf("--timeout must be at most %s", agentbusMaxTimeout)
	}
	return time.Duration(milliseconds.Num().Int64()) * time.Millisecond, nil
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
func submitTask(ctx context.Context, opts *taskOptions, prompt string, schema json.RawMessage, stderr io.Writer) (client.JobSubmitResult, agentbusClient, string, error) {
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
	if err := writeAgentbusNotice(stderr, hello); err != nil {
		_ = c.Close()
		return client.JobSubmitResult{}, nil, stateRoot, submissionError(opts.RequestID, err)
	}
	if err := validateBackend(hello, opts.Backend); err != nil {
		_ = c.Close()
		return client.JobSubmitResult{}, nil, "", submissionError(opts.RequestID, err)
	}

	params := client.JobSubmitParams{
		WorkspaceKey: workspaceKey,
		RequestID:    opts.RequestID,
		TaskSpec: client.TaskSpec{
			Backend:       opts.Backend,
			CWD:           opts.CWD,
			Intent:        opts.Intent,
			InputMode:     opts.InputMode,
			ResumeJobID:   opts.ResumeJobID,
			RetainSession: opts.RetainSession,
			Model:         optionalTaskSpecString(opts.Model),
			Effort:        optionalTaskSpecString(opts.Effort),
			Prompt:        prompt,
			OutputSchema:  append(json.RawMessage(nil), schema...),
			Tags:          optionalTaskSpecTags(opts.Tags),
			TimeoutMS:     timeoutMillis(opts.Timeout, opts.TimeoutSet),
		},
	}
	submitted, err := c.JobSubmit(ctx, params)
	if err != nil {
		_ = c.Close()
		if opts.ResumeJobID != "" && submissionJobID(err) == opts.ResumeJobID {
			return client.JobSubmitResult{}, nil, stateRoot, submissionError(opts.RequestID, err)
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

func timeoutMillis(timeout time.Duration, set bool) *int64 {
	if !set {
		return nil
	}
	ms := timeout.Milliseconds()
	return &ms
}
