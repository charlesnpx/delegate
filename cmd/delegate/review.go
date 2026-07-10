package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
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
	StrictContract    bool
	Origin            string
	Embedded          bool
	Base              string
	Scope             string
	AllowLiveRepoRead bool
}

func runReview(kind string, args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseReviewOptions(kind, args, stderr)
	if err != nil {
		return 0, err
	}
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
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{StrictContract: opts.StrictContract})
	if err != nil {
		return 0, err
	}
	taskOpts := taskOptions{
		Backend:         opts.Backend,
		Wait:            opts.Wait,
		JSON:            opts.JSON,
		CWD:             assembled.BackendCWD,
		Model:           opts.Model,
		Effort:          opts.Effort,
		Timeout:         opts.Timeout,
		StrictContract:  opts.StrictContract,
		Origin:          opts.Origin,
		Embedded:        opts.Embedded,
		StateDir:        assembled.StateDir,
		Kind:            kind,
		ReviewWorkspace: assembled.Workspace,
	}
	result, err := executeTask(taskOpts, handoff.ResolvedPrompt{Prompt: prompt, Source: handoff.SourcePrompt}, turnPolicy)
	if result.Submitted {
		// A successful daemon submission owns the workspace even if later local
		// bookkeeping fails; provisional metadata and the launch envelope recover it.
		ownsWorkspace = false
	}
	if err != nil {
		return 0, err
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
		_, _ = fmt.Fprintln(fs.Output(), "  OS-level isolation requires a container/sandbox profile and is a named v0.2 roadmap item.")
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
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout")
	fs.BoolVar(&opts.StrictContract, "strict-contract", false, "enable one read-only corrective retry")
	fs.StringVar(&opts.Origin, "origin", "", "originating skill")
	fs.BoolVar(&opts.Embedded, "embedded", false, "run through the embedded engine path")
	fs.StringVar(&opts.Base, "base", "", "comparison base ref")
	fs.StringVar(&opts.Scope, "scope", reviewpkg.ScopeAuto, "review scope: auto combines branch and working-tree changes; or working-tree, branch")
	fs.BoolVar(&opts.AllowLiveRepoRead, "allow-live-repo-read", false, "use live repository as backend cwd (makes backend file reads easier; does not prevent backend file reads)")
	if err := fs.Parse(args); err != nil {
		return reviewOptions{}, err
	}
	if fs.NArg() != 0 {
		return reviewOptions{}, fmt.Errorf("%s does not accept positional arguments", command)
	}
	if opts.Backend == "" {
		return reviewOptions{}, fmt.Errorf("%s requires --backend", command)
	}
	if background && opts.Wait {
		return reviewOptions{}, fmt.Errorf("use only one of --background or --wait")
	}
	if opts.Embedded && !opts.Wait {
		return reviewOptions{}, fmt.Errorf("--embedded requires --wait; background supervision is daemon-only")
	}
	if opts.Scope == reviewpkg.ScopeWorkingTree && opts.Base != "" {
		return reviewOptions{}, fmt.Errorf("--base cannot be used with --scope working-tree")
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
