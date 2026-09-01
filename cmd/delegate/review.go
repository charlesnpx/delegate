package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	reviewpkg "github.com/charlesnpx/delegate/internal/review"
	"github.com/charlesnpx/witness/contract/charter"
	reviewcontract "github.com/charlesnpx/witness/contract/review"
)

const (
	liveRepoReadWarning    = "--allow-live-repo-read makes backend filesystem reads easier by using the live repository as cwd; delegate's path/history redaction and final content scan apply only to the context it assembles"
	reviewKind             = reviewpkg.KindReview
	adversarialReviewKind  = reviewpkg.KindAdversarialReview
	maxReviewArtifactBytes = 8 * 1024 * 1024
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
	RequestFile       string
	ArtifactFile      string
	CharterFile       string
	ContractMode      bool
}

type contractReviewInput struct {
	FrozenCharter     charter.FrozenCharter
	FrozenCharterJSON []byte
	Artifact          []byte
	ReviewInputDigest string
}

func runReview(kind string, args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseReviewOptions(kind, args, stderr)
	if err != nil {
		return 0, err
	}
	taskOpts := taskOptions{Backend: opts.Backend, Model: opts.Model, Effort: opts.Effort}
	policy := turnPolicyForSchema(nil, "")
	logicalWorkspace := ""
	var assembled reviewpkg.Context
	var prompt string
	contractCharterHash := ""
	contractReviewInputDigest := ""
	if opts.ContractMode {
		input, err := loadContractReviewInput(opts)
		if err != nil {
			return 0, err
		}
		logicalWorkspace, err = reviewpkg.CanonicalizeCWD(opts.CWD)
		if err != nil {
			return 0, err
		}
		schema, err := reviewcontract.DefaultReviewerSchema(input.FrozenCharter, input.ReviewInputDigest)
		if err != nil {
			return 0, fmt.Errorf("build review-report-v1 schema: %w", err)
		}
		assembled, err = reviewpkg.PrepareContractWorkspace(reviewpkg.ContractWorkspaceOptions{
			Charter:  input.FrozenCharterJSON,
			Artifact: input.Artifact,
		})
		if err != nil {
			return 0, err
		}
		policy = turnPolicyForSchema(schema, reviewRetryTemplate)
		contractCharterHash = input.FrozenCharter.CharterHash
		contractReviewInputDigest = input.ReviewInputDigest
	} else {
		assembled, err = reviewpkg.Assemble(context.Background(), reviewpkg.Options{
			CWD:               opts.CWD,
			Base:              opts.Base,
			Scope:             opts.Scope,
			AllowLiveRepoRead: opts.AllowLiveRepoRead,
		})
		if err != nil {
			return 0, err
		}
		logicalWorkspace = assembled.RepositoryRoot
	}

	for _, sweepErr := range sweepReviewWorkspaces(context.Background(), assembled.StateDir) {
		if _, err := fmt.Fprintf(stderr, "warning: review workspace sweep: %v\n", sweepErr); err != nil {
			_ = reviewpkg.Cleanup(assembled)
			return 0, err
		}
	}
	if opts.ContractMode {
		prompt, err = reviewpkg.ComposeContractPrompt(kind, contractCharterHash, contractReviewInputDigest)
	} else {
		prompt, err = reviewpkg.ComposePrompt(kind, assembled)
	}
	if err != nil {
		_ = reviewpkg.Cleanup(assembled)
		return 0, err
	}
	taskOpts.CWD = assembled.BackendCWD
	taskOpts.Timeout = opts.Timeout
	taskOpts.LogicalWorkspace = logicalWorkspace
	submitted, c, agentbusStateRoot, err := submitTask(context.Background(), &taskOpts, prompt, policy)
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
	fs.StringVar(&opts.RequestFile, "request-file", "", "review-request-v1 file for exact-input contract mode")
	fs.StringVar(&opts.ArtifactFile, "artifact-file", "", "exact review artifact file for contract mode")
	fs.StringVar(&opts.CharterFile, "charter-file", "", "review charter file for contract mode")
	if err := fs.Parse(args); err != nil {
		return reviewOptions{}, err
	}
	if fs.NArg() != 0 {
		return reviewOptions{}, fmt.Errorf("%s does not accept positional arguments", command)
	}
	if opts.Backend == "" {
		return reviewOptions{}, fmt.Errorf("%s requires --backend", command)
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	contractFileCount := 0
	for _, path := range []string{opts.RequestFile, opts.ArtifactFile, opts.CharterFile} {
		if path != "" {
			contractFileCount++
		}
	}
	if contractFileCount != 0 && contractFileCount != 3 {
		return reviewOptions{}, fmt.Errorf("contract mode requires --request-file, --artifact-file, and --charter-file together")
	}
	if contractFileCount == 0 && (provided["request-file"] || provided["artifact-file"] || provided["charter-file"]) {
		return reviewOptions{}, fmt.Errorf("contract mode requires non-empty --request-file, --artifact-file, and --charter-file together")
	}
	opts.ContractMode = contractFileCount == 3
	if opts.ContractMode {
		for _, name := range []string{"base", "scope", "allow-live-repo-read"} {
			if provided[name] {
				return reviewOptions{}, fmt.Errorf("--%s cannot be used with --request-file/--artifact-file/--charter-file contract mode", name)
			}
		}
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

func loadContractReviewInput(opts reviewOptions) (contractReviewInput, error) {
	inputCharter, err := charter.ReadFile(opts.CharterFile)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("read --charter-file %q: %w", opts.CharterFile, err)
	}
	frozen, err := charter.Freeze(inputCharter, nil)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("freeze --charter-file %q: %w", opts.CharterFile, err)
	}

	requestData, err := os.ReadFile(opts.RequestFile)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("read --request-file %q: %w", opts.RequestFile, err)
	}
	request, err := reviewcontract.DecodeAndValidateReviewRequest(requestData)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("validate --request-file %q: %w", opts.RequestFile, err)
	}
	if request.CharterHash != frozen.CharterHash {
		return contractReviewInput{}, fmt.Errorf("review request charter hash mismatch: request %s does not match frozen charter %s", request.CharterHash, frozen.CharterHash)
	}

	artifact, err := readContractReviewArtifact(opts.ArtifactFile)
	if err != nil {
		return contractReviewInput{}, err
	}
	sum := sha256.Sum256(artifact)
	artifactDigest := "sha256:" + hex.EncodeToString(sum[:])
	if artifactDigest != request.ReviewInputDigest {
		return contractReviewInput{}, fmt.Errorf("review artifact digest mismatch: got %s, request review_input_digest is %s", artifactDigest, request.ReviewInputDigest)
	}

	frozenJSON, err := json.Marshal(frozen)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("marshal frozen charter: %w", err)
	}
	return contractReviewInput{
		FrozenCharter:     frozen,
		FrozenCharterJSON: frozenJSON,
		Artifact:          artifact,
		ReviewInputDigest: request.ReviewInputDigest,
	}, nil
}

func readContractReviewArtifact(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read --artifact-file %q: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReviewArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read --artifact-file %q: %w", path, err)
	}
	if len(data) > maxReviewArtifactBytes {
		return nil, fmt.Errorf("review artifact exceeds the 8 MiB limit")
	}
	return data, nil
}
