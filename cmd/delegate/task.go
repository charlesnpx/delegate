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
	Effort            string
	Timeout           time.Duration
	Write             bool
	StrictContract    bool
	NoContract        bool
	Origin            string
	Embedded          bool
	Prompt            optionalStringFlag
	PromptFile        string
	PromptStdin       bool
	HandoffPromptFile string
	Positional        []string
	StateDir          string
}

type taskRunResult struct {
	Launch   *LaunchEnvelope
	Terminal *TerminalEnvelope
}

func runTask(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	opts, err := parseTaskOptions(args, stdin, stderr)
	if err != nil {
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
	ctx := context.Background()
	var result taskRunResult
	if opts.Embedded {
		result, err = runEmbeddedTask(ctx, opts, resolved, turnPolicy)
	} else {
		result, err = runDaemonTask(ctx, opts, resolved, turnPolicy)
	}
	if err != nil {
		return 0, err
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
	fs.BoolVar(&opts.Resume, "resume", false, "resume the last session")
	fs.StringVar(&opts.ResumeSession, "resume-session", "", "resume a session id")
	fs.BoolVar(&opts.Fresh, "fresh", false, "start a fresh session")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout")
	fs.BoolVar(&opts.Write, "write", false, "allow backend writes")
	fs.BoolVar(&opts.StrictContract, "strict-contract", false, "enable corrective retry")
	fs.BoolVar(&opts.NoContract, "no-contract", false, "disable contract enforcement")
	fs.StringVar(&opts.Origin, "origin", "", "originating skill")
	fs.BoolVar(&opts.Embedded, "embedded", false, "run through the embedded engine path")
	fs.Var(&opts.Prompt, "prompt", handoff.PromptFlagUsage)
	fs.StringVar(&opts.PromptFile, "prompt-file", "", "read prompt from file")
	fs.BoolVar(&opts.PromptStdin, "prompt-stdin", false, "read prompt from stdin")
	fs.StringVar(&opts.HandoffPromptFile, "handoff-prompt-file", "", "read prompt from a delegate handoff file")
	if err := fs.Parse(args); err != nil {
		return taskOptions{}, err
	}
	opts.Positional = fs.Args()
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
	if opts.Resume {
		return taskOptions{}, fmt.Errorf("--resume without --resume-session is not supported by agentbus v0.1.0")
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
	if stdin == nil {
		stdin = os.Stdin
	}
	return opts, nil
}

func runDaemonTask(ctx context.Context, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (taskRunResult, error) {
	c, _, err := connectAgentbusCommand(ctx, requiredCapabilitiesForPolicy(turnPolicy))
	if err != nil {
		return taskRunResult{}, err
	}
	defer c.Close()
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
		return runDaemonSessionTask(ctx, c, opts, resolved, turnPolicy)
	}
	submitted, err := c.JobSubmit(ctx, client.JobSubmitParams{TaskSpec: spec})
	if err != nil {
		return taskRunResult{}, err
	}
	if _, err := persistDelegateJob(opts, resolved, submitted.JobID, contractKindForPolicy(turnPolicy, opts.NoContract)); err != nil {
		return taskRunResult{}, err
	}
	if err := cleanupJobInput(opts.StateDir, submitted.JobID, "", submitted.State); err != nil {
		return taskRunResult{}, err
	}
	if opts.Wait {
		jobResult, err := waitForJobResult(ctx, c, opts.StateDir, submitted.JobID)
		if err != nil {
			return taskRunResult{}, err
		}
		env, err := terminalEnvelopeFromJobResult(opts.StateDir, jobResult)
		if err != nil {
			return taskRunResult{}, err
		}
		return taskRunResult{Terminal: &env}, nil
	}
	env, err := newLaunchEnvelope(submitted.JobID, submitted.State)
	if err != nil {
		return taskRunResult{}, err
	}
	return taskRunResult{Launch: &env}, nil
}

func runDaemonSessionTask(ctx context.Context, c agentbusClient, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (taskRunResult, error) {
	session, err := c.SessionResume(ctx, client.SessionResumeParams{SessionID: opts.ResumeSession})
	if err != nil {
		return taskRunResult{}, err
	}
	write := opts.Write
	started, notifications, err := c.TurnStart(ctx, client.TurnStartParams{
		SessionID: session.SessionID,
		Prompt:    resolved.Prompt,
		Write:     &write,
		Policy:    turnPolicy,
		TimeoutMs: timeoutMillis(opts.Timeout),
	})
	if err != nil {
		return taskRunResult{}, err
	}
	if _, err := persistDelegateJob(opts, resolved, started.JobID, contractKindForPolicy(turnPolicy, opts.NoContract)); err != nil {
		return taskRunResult{}, err
	}
	if err := cleanupJobInput(opts.StateDir, started.JobID, started.SessionID, engine.StateRunning); err != nil {
		return taskRunResult{}, err
	}
	if opts.Wait {
		jobResult, err := waitForTurnResult(ctx, c, opts.StateDir, started.JobID, notifications)
		if err != nil {
			return taskRunResult{}, err
		}
		env, err := terminalEnvelopeFromJobResult(opts.StateDir, jobResult)
		if err != nil {
			return taskRunResult{}, err
		}
		return taskRunResult{Terminal: &env}, nil
	}
	env, err := newLaunchEnvelope(started.JobID, engine.StateRunning)
	if err != nil {
		return taskRunResult{}, err
	}
	return taskRunResult{Launch: &env}, nil
}

func persistDelegateJob(opts taskOptions, resolved handoff.ResolvedPrompt, jobID, contractKind string) (jobMetadata, error) {
	input, err := handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: opts.StateDir,
		JobID:    jobID,
		Prompt:   resolved,
	})
	if err != nil {
		return jobMetadata{}, err
	}
	meta := jobMetadata{
		Schema:       envelopeSchema,
		JobID:        jobID,
		Kind:         taskKind,
		ContractKind: contractKind,
		NoContract:   opts.NoContract,
		JobInputPath: input.Path,
	}
	if err := saveJobMetadata(opts.StateDir, meta); err != nil {
		_, _ = handoff.DeleteJobInputOnPreLaunchTerminal(input, engine.StateFailed, handoff.Hooks{})
		return jobMetadata{}, err
	}
	return meta, nil
}

func taskTags(opts taskOptions) map[string]string {
	tags := map[string]string{"delegate.kind": taskKind}
	if opts.Origin != "" {
		tags["delegate.origin"] = opts.Origin
	}
	return tags
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
