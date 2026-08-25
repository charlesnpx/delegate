package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charlesnpx/delegate/internal/handoff"
	reviewpkg "github.com/charlesnpx/delegate/internal/review"
)

const liveRepoReadWarning = "--allow-live-repo-read makes backend filesystem reads easier by using the live repository as cwd; delegate's path/history redaction and final content scan apply only to the context it assembles"

type reviewOptions struct {
	Backend           string
	Wait              bool
	JSON              bool
	CWD               string
	Model             string
	Effort            string
	Timeout           time.Duration
	TimeoutSet        bool
	Origin            string
	ParentClient      optionalStringFlag
	ParentSession     optionalStringFlag
	Base              string
	Scope             string
	AllowLiveRepoRead bool
}

func runReview(kind string, args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseReviewOptions(kind, args, stderr)
	if err != nil {
		return 0, err
	}
	taskDefaults := taskOptions{Backend: opts.Backend, Model: opts.Model, Effort: opts.Effort}
	if err := resolveTaskModelEffort(&taskDefaults); err != nil {
		return 0, err
	}
	opts.Model = taskDefaults.Model
	opts.Effort = taskDefaults.Effort
	assembled, err := reviewpkg.Assemble(context.Background(), reviewpkg.Options{
		CWD:               opts.CWD,
		Base:              opts.Base,
		Scope:             opts.Scope,
		AllowLiveRepoRead: opts.AllowLiveRepoRead,
	})
	if err != nil {
		return 0, err
	}
	ownsWorkspace := true
	defer func() {
		if ownsWorkspace {
			_ = reviewpkg.Cleanup(assembled)
		}
	}()
	prompt, err := reviewpkg.ComposePrompt(kind, assembled)
	if err != nil {
		return 0, err
	}
	taskOpts := taskOptions{
		Backend:          opts.Backend,
		Wait:             opts.Wait,
		JSON:             opts.JSON,
		CWD:              assembled.BackendCWD,
		Model:            opts.Model,
		Effort:           opts.Effort,
		Timeout:          opts.Timeout,
		TimeoutSet:       opts.TimeoutSet,
		Origin:           opts.Origin,
		AuditOrigin:      captureTaskOrigin(opts.Origin, opts.ParentClient, opts.ParentSession, nil),
		StateDir:         assembled.StateDir,
		Kind:             kind,
		ReviewWorkspace:  assembled.Workspace,
		ModelEffort:      taskDefaults.ModelEffort,
		LogicalWorkspace: assembled.RepositoryRoot,
	}
	result, err := executeTask(taskOpts, handoff.ResolvedPrompt{Prompt: prompt, Source: handoff.SourcePrompt}, nil, stderr)
	if result.Submitted {
		// A successful daemon submission owns the workspace even if later local
		// bookkeeping fails; the durable submission intent and launch envelope recover it.
		ownsWorkspace = false
	}
	if err != nil {
		if submissionErrorPreservesReviewWorkspace(err) {
			ownsWorkspace = false
		}
		return agentbusCommandErrorResult(opts.JSON, stdout, err)
	}
	if result.Launch != nil {
		// Job metadata owns the workspace until a terminal state is observed.
		ownsWorkspace = false
	}
	if opts.AllowLiveRepoRead {
		result.Warnings = append([]string{liveRepoReadWarning}, result.Warnings...)
	}
	return writeTaskRunResult(result, stdout, stderr)
}

func parseReviewOptions(kind string, args []string, stderr io.Writer) (reviewOptions, error) {
	command := "delegate review"
	if kind == adversarialReviewKind {
		command = "delegate adversarial-review"
	} else if kind != reviewKind {
		return reviewOptions{}, fmt.Errorf("unsupported review kind %q", kind)
	}
	var opts reviewOptions
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage of %s:\n", command)
		_, _ = fmt.Fprintln(fs.Output(), "  Delegate redacts secret-matched paths and secret-like diff hunks from the review context it assembles.")
		_, _ = fmt.Fprintln(fs.Output(), "  This does not prevent a same-user backend from reading repository or other filesystem files itself.")
		_, _ = fmt.Fprintln(fs.Output(), "  --allow-live-repo-read makes those reads easier by using the repository as cwd.")
		_, _ = fmt.Fprintln(fs.Output(), "  OS-level isolation requires a container/sandbox profile.")
		fs.PrintDefaults()
	}
	var background bool
	fs.StringVar(&opts.Backend, "backend", "", "backend name discovered from agentbus")
	fs.BoolVar(&background, "background", false, "return after launch")
	fs.BoolVar(&opts.Wait, "wait", false, "wait for terminal result")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSON")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute repository working directory")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout; 0 leaves the deadline to the daemon default; envelope.timeout is authoritative")
	fs.StringVar(&opts.Origin, "origin", "", "originating skill")
	fs.Var(&opts.ParentClient, "parent-client", "explicit parent client for audit linkage")
	fs.Var(&opts.ParentSession, "parent-session", "explicit parent session id for audit linkage")
	fs.StringVar(&opts.Base, "base", "", "comparison base ref")
	fs.StringVar(&opts.Scope, "scope", reviewpkg.ScopeAuto, "review scope: auto combines branch and working-tree changes; or working-tree, branch")
	fs.BoolVar(&opts.AllowLiveRepoRead, "allow-live-repo-read", false, "use live repository as backend cwd (makes backend file reads easier; does not prevent backend file reads)")
	if err := fs.Parse(args); err != nil {
		return reviewOptions{}, err
	}
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == "timeout" {
			opts.TimeoutSet = true
		}
	})
	if fs.NArg() != 0 {
		return reviewOptions{}, fmt.Errorf("%s does not accept positional arguments", command)
	}
	if opts.Backend == "" {
		return reviewOptions{}, fmt.Errorf("%s requires --backend", command)
	}
	if background && opts.Wait {
		return reviewOptions{}, fmt.Errorf("use only one of --background or --wait")
	}
	if opts.Scope == reviewpkg.ScopeWorkingTree && opts.Base != "" {
		return reviewOptions{}, fmt.Errorf("--base cannot be used with --scope working-tree")
	}
	if err := validateTimeoutOption(opts.Timeout, opts.TimeoutSet); err != nil {
		return reviewOptions{}, err
	}
	if opts.CWD == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return reviewOptions{}, err
		}
		opts.CWD = cwd
	}
	return opts, nil
}

func submissionErrorPreservesReviewWorkspace(err error) bool {
	var unresolved submissionUnresolvedError
	return errors.As(err, &unresolved)
}
