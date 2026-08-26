package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	reviewpkg "github.com/charlesnpx/delegate/internal/review"
)

const (
	liveRepoReadWarning   = "--allow-live-repo-read makes backend filesystem reads easier by using the live repository as cwd; delegate's path/history redaction and final content scan apply only to the context it assembles"
	reviewKind            = reviewpkg.KindReview
	adversarialReviewKind = reviewpkg.KindAdversarialReview
)

type reviewOptions struct {
	Backend           string
	CWD               string
	Model             string
	Effort            string
	Timeout           time.Duration
	Base              string
	Scope             string
	AllowLiveRepoRead bool
}

func runReview(kind string, args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseReviewOptions(kind, args, stderr)
	if err != nil {
		return 0, err
	}
	taskOpts := taskOptions{Backend: opts.Backend, Model: opts.Model, Effort: opts.Effort}
	assembled, err := reviewpkg.Assemble(context.Background(), reviewpkg.Options{
		CWD:               opts.CWD,
		Base:              opts.Base,
		Scope:             opts.Scope,
		AllowLiveRepoRead: opts.AllowLiveRepoRead,
	})
	if err != nil {
		return 0, err
	}

	for _, sweepErr := range sweepReviewWorkspaces(context.Background(), assembled.StateDir) {
		if _, err := fmt.Fprintf(stderr, "warning: review workspace sweep: %v\n", sweepErr); err != nil {
			_ = reviewpkg.Cleanup(assembled)
			return 0, err
		}
	}
	prompt, err := reviewpkg.ComposePrompt(kind, assembled)
	if err != nil {
		_ = reviewpkg.Cleanup(assembled)
		return 0, err
	}
	taskOpts.CWD = assembled.BackendCWD
	taskOpts.Timeout = opts.Timeout
	taskOpts.LogicalWorkspace = assembled.RepositoryRoot
	submitted, c, agentbusStateRoot, err := submitTask(context.Background(), &taskOpts, prompt, nil)
	var unresolved *submissionUnresolvedError
	cleanupWorkspace := err != nil && !errors.As(err, &unresolved)
	if cleanupWorkspace {
		_ = reviewpkg.Cleanup(assembled)
	}
	if err != nil {
		if !cleanupWorkspace && assembled.Workspace != "" {
			if jobID := submissionJobID(err); jobID != "" {
				if recordErr := saveReviewWorkspaceMetadata(assembled.StateDir, reviewWorkspaceMetadata{
					JobID:             jobID,
					Workspace:         assembled.Workspace,
					AgentbusStateRoot: agentbusStateRoot,
				}); recordErr != nil {
					return 0, fmt.Errorf("%w; record review workspace for job %s: %v", err, jobID, recordErr)
				}
			}
		}
		return 0, err
	}
	defer func() { _ = c.Close() }()

	if assembled.Workspace != "" {
		if err := saveReviewWorkspaceMetadata(assembled.StateDir, reviewWorkspaceMetadata{
			JobID:             submitted.JobID,
			Workspace:         assembled.Workspace,
			AgentbusStateRoot: agentbusStateRoot,
		}); err != nil {
			return 0, fmt.Errorf("record review workspace for job %s: %w", submitted.JobID, err)
		}
	}
	if opts.AllowLiveRepoRead {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", liveRepoReadWarning); err != nil {
			return 0, err
		}
	}
	if err := writeTaskSubmitReceipt(stdout, taskOpts, submitted); err != nil {
		return 0, submissionError(taskOpts.RequestID, err)
	}
	return 0, nil
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
	fs.StringVar(&opts.Backend, "backend", "", "backend name discovered from agentbus")
	fs.StringVar(&opts.CWD, "cwd", "", "absolute repository working directory")
	fs.StringVar(&opts.Model, "model", "", "backend model")
	fs.StringVar(&opts.Effort, "effort", "", "backend effort")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "backend timeout; 0 leaves the deadline to the daemon default")
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
	if opts.Scope == reviewpkg.ScopeWorkingTree && opts.Base != "" {
		return reviewOptions{}, fmt.Errorf("--base cannot be used with --scope working-tree")
	}
	if err := validateTimeoutOption(opts.Timeout); err != nil {
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
