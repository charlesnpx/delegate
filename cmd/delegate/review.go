package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
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
	ResumeJobID       string
	Base              string
	Scope             string
	AllowLiveRepoRead bool
	RequestFile       string
	ArtifactFile      string
	CharterFile       string
	Reviewer          string
}

type contractReviewInput struct {
	FrozenCharter     charter.FrozenCharter
	FrozenCharterJSON []byte
	Artifact          []byte
	RequestDigest     string
	ReviewInputDigest string
	ConsumerIdentity  reviewcontract.Identity
	SchemaVersion     string
	Recipe            reviewcontract.ReviewRecipe
	FrozenRecipe      json.RawMessage
	RecipeDigest      string
	Reviewer          string
}

func runReview(kind string, args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseReviewOptions(kind, args, stderr)
	if err != nil {
		return 0, err
	}
	taskOpts := taskOptions{Backend: opts.Backend, Model: opts.Model, Effort: opts.Effort, ResumeJobID: opts.ResumeJobID}
	contractMode := opts.RequestFile != ""
	var schema json.RawMessage
	logicalWorkspace := ""
	var assembled reviewpkg.Context
	var prompt string
	contractCharterHash := ""
	contractReviewInputDigest := ""
	var contractInput contractReviewInput
	if contractMode {
		contractInput, err = loadContractReviewInput(opts)
		if err != nil {
			return 0, err
		}
		taskOpts.RequestID, err = contractReviewRequestID(contractInput.RequestDigest, contractInput.Reviewer, opts.ResumeJobID)
		if err != nil {
			return 0, err
		}
		logicalWorkspace, err = reviewpkg.CanonicalizeCWD(opts.CWD)
		if err != nil {
			return 0, err
		}
		if contractInput.SchemaVersion == reviewcontract.ReviewRequestV2 {
			schema, err = reviewReportV2Schema(contractInput.FrozenCharter, contractInput.RequestDigest, contractInput.RecipeDigest, contractInput.Reviewer, contractInput.ReviewInputDigest, contractInput.ConsumerIdentity)
			if err != nil {
				return 0, fmt.Errorf("build review-report-v2 schema: %w", err)
			}
		} else {
			schema, err = reviewcontract.DefaultReviewerSchema(contractInput.FrozenCharter, contractInput.ReviewInputDigest, contractInput.ConsumerIdentity)
			if err != nil {
				return 0, fmt.Errorf("build review-report-v1 schema: %w", err)
			}
		}
		assembled, err = reviewpkg.PrepareContractWorkspace(reviewpkg.ContractWorkspaceOptions{
			RequestDigest: contractInput.RequestDigest,
			Charter:       contractInput.FrozenCharterJSON,
			Artifact:      contractInput.Artifact,
		})
		if err != nil {
			return 0, err
		}
		contractCharterHash = contractInput.FrozenCharter.CharterHash
		contractReviewInputDigest = contractInput.ReviewInputDigest
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
	if contractMode {
		if contractInput.SchemaVersion == reviewcontract.ReviewRequestV2 {
			prompt, err = reviewpkg.ComposeContractPromptV2(kind, contractInput.Recipe.Instructions, contractInput.FrozenRecipe, contractInput.RequestDigest, contractInput.RecipeDigest, contractInput.Reviewer, contractCharterHash, contractReviewInputDigest)
		} else {
			prompt, err = reviewpkg.ComposeContractPrompt(kind, contractCharterHash, contractReviewInputDigest)
		}
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
	submitted, c, agentbusStateRoot, err := submitTask(context.Background(), &taskOpts, prompt, schema)
	var unresolved *submissionUnresolvedError
	cleanupWorkspace := err != nil && !errors.As(err, &unresolved)
	if cleanupWorkspace {
		_ = reviewpkg.Cleanup(assembled)
	}
	if err != nil {
		if !cleanupWorkspace && !contractMode && assembled.Workspace != "" {
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

	if !contractMode && assembled.Workspace != "" {
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
	fs.StringVar(&opts.ResumeJobID, "resume", "", "resume a prior job; creates a new job with a fresh deadline")
	fs.StringVar(&opts.Base, "base", "", "comparison base ref")
	fs.StringVar(&opts.Scope, "scope", reviewpkg.ScopeAuto, "review scope: auto combines branch and working-tree changes; or working-tree, branch")
	fs.BoolVar(&opts.AllowLiveRepoRead, "allow-live-repo-read", false, "use live repository as backend cwd (makes backend file reads easier; does not prevent backend file reads)")
	fs.StringVar(&opts.RequestFile, "request-file", "", "review-request-v1 or review-request-v2 file for exact-input contract mode")
	fs.StringVar(&opts.ArtifactFile, "artifact-file", "", "exact review artifact file for contract mode")
	fs.StringVar(&opts.CharterFile, "charter-file", "", "review charter file for contract mode")
	fs.StringVar(&opts.Reviewer, "reviewer", "", "reviewer identifier selected from a review-request-v2 recipe")
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
	if opts.RequestFile != "" {
		for _, name := range []string{"base", "scope", "allow-live-repo-read"} {
			if provided[name] {
				return reviewOptions{}, fmt.Errorf("--%s cannot be used with --request-file/--artifact-file/--charter-file contract mode", name)
			}
		}
	} else if provided["reviewer"] {
		return reviewOptions{}, fmt.Errorf("--reviewer requires --request-file contract mode")
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
	schemaVersion, err := reviewRequestSchemaVersion(requestData)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("detect schema_version in --request-file %q: %w", opts.RequestFile, err)
	}

	input := contractReviewInput{SchemaVersion: schemaVersion}
	requestCharterHash := ""
	requestReviewInputDigest := ""
	switch schemaVersion {
	case reviewcontract.ReviewRequestV1:
		if opts.Reviewer != "" {
			return contractReviewInput{}, fmt.Errorf("--reviewer is only valid for review-request-v2; review-request-v1 has no reviewers")
		}
		request, decodeErr := reviewcontract.DecodeAndValidateReviewRequest(requestData)
		if decodeErr != nil {
			return contractReviewInput{}, fmt.Errorf("validate --request-file %q: %w", opts.RequestFile, decodeErr)
		}
		input.RequestDigest, err = reviewcontract.ReviewRequestDigest(request)
		if err != nil {
			return contractReviewInput{}, fmt.Errorf("digest --request-file %q: %w", opts.RequestFile, err)
		}
		input.ReviewInputDigest = request.ReviewInputDigest
		input.ConsumerIdentity = request.ConsumerIdentity
		requestCharterHash = request.CharterHash
		requestReviewInputDigest = request.ReviewInputDigest
	case reviewcontract.ReviewRequestV2:
		request, decodeErr := reviewcontract.DecodeAndValidateReviewRequestV2(requestData)
		if decodeErr != nil {
			return contractReviewInput{}, fmt.Errorf("validate --request-file %q: %w", opts.RequestFile, decodeErr)
		}
		if opts.Reviewer == "" {
			return contractReviewInput{}, fmt.Errorf("review-request-v2 requires --reviewer; declared reviewers: %s", declaredReviewers(request.RequiredOutputs))
		}
		if !containsReviewer(request.RequiredOutputs, opts.Reviewer) {
			return contractReviewInput{}, fmt.Errorf("reviewer %q is not declared by review-request-v2; declared reviewers: %s", opts.Reviewer, declaredReviewers(request.RequiredOutputs))
		}
		recipe, recipeErr := reviewcontract.ReviewRequestV2Recipe(request)
		if recipeErr != nil {
			return contractReviewInput{}, fmt.Errorf("decode frozen recipe in --request-file %q: %w", opts.RequestFile, recipeErr)
		}
		input.RequestDigest, err = reviewcontract.ReviewRequestV2Digest(request)
		if err != nil {
			return contractReviewInput{}, fmt.Errorf("digest --request-file %q: %w", opts.RequestFile, err)
		}
		input.ReviewInputDigest = request.ReviewInputDigest
		input.ConsumerIdentity = request.ConsumerIdentity
		input.Recipe = recipe
		input.FrozenRecipe = append(json.RawMessage(nil), request.FrozenRecipe...)
		input.RecipeDigest = request.RecipeDigest
		input.Reviewer = opts.Reviewer
		requestCharterHash = request.CharterHash
		requestReviewInputDigest = request.ReviewInputDigest
	default:
		return contractReviewInput{}, fmt.Errorf("unsupported review request schema_version %q; expected %q or %q", schemaVersion, reviewcontract.ReviewRequestV1, reviewcontract.ReviewRequestV2)
	}

	if requestCharterHash != frozen.CharterHash {
		return contractReviewInput{}, fmt.Errorf("review request charter hash mismatch: request %s does not match frozen charter %s", requestCharterHash, frozen.CharterHash)
	}

	artifact, err := readContractReviewArtifact(opts.ArtifactFile)
	if err != nil {
		return contractReviewInput{}, err
	}
	sum := sha256.Sum256(artifact)
	artifactDigest := "sha256:" + hex.EncodeToString(sum[:])
	if artifactDigest != requestReviewInputDigest {
		return contractReviewInput{}, fmt.Errorf("review artifact digest mismatch: got %s, request review_input_digest is %s", artifactDigest, requestReviewInputDigest)
	}

	frozenJSON, err := json.Marshal(frozen)
	if err != nil {
		return contractReviewInput{}, fmt.Errorf("marshal frozen charter: %w", err)
	}
	return contractReviewInput{
		FrozenCharter:     frozen,
		FrozenCharterJSON: frozenJSON,
		Artifact:          artifact,
		RequestDigest:     input.RequestDigest,
		ReviewInputDigest: input.ReviewInputDigest,
		ConsumerIdentity:  input.ConsumerIdentity,
		SchemaVersion:     input.SchemaVersion,
		Recipe:            input.Recipe,
		FrozenRecipe:      input.FrozenRecipe,
		RecipeDigest:      input.RecipeDigest,
		Reviewer:          input.Reviewer,
	}, nil
}

func reviewRequestSchemaVersion(data []byte) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", err
	}
	raw, ok := envelope["schema_version"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("request schema_version is required")
	}
	var schemaVersion string
	if err := json.Unmarshal(raw, &schemaVersion); err != nil {
		return "", fmt.Errorf("request schema_version must be a string: %w", err)
	}
	if schemaVersion == "" {
		return "", fmt.Errorf("request schema_version must be non-empty")
	}
	return schemaVersion, nil
}

func containsReviewer(declared []string, reviewer string) bool {
	for _, candidate := range declared {
		if candidate == reviewer {
			return true
		}
	}
	return false
}

func declaredReviewers(declared []string) string {
	return strings.Join(declared, ", ")
}

// contractReviewRequestID derives a contract review's replay identity. A resume
// is a distinct identity, because agentbus hashes the whole task spec and would
// otherwise reject the resumed submission as a replay conflict before it ever
// validated the resume target. It stays replay-stable: the same digest, reviewer,
// and target always derive the same id. An empty reviewer is the v1 path and
// deliberately retains its previous identity format.
func contractReviewRequestID(requestDigest, reviewer, resumeJobID string) (string, error) {
	const digestPrefix = "sha256:"
	const digestHexLength = 64
	const requestIDPrefix = "delegate-review-"

	digestHex, found := strings.CutPrefix(requestDigest, digestPrefix)
	if !found || len(digestHex) != digestHexLength {
		return "", fmt.Errorf("invalid contract review request digest %q", requestDigest)
	}
	if _, err := hex.DecodeString(digestHex); err != nil {
		return "", fmt.Errorf("invalid contract review request digest %q: %w", requestDigest, err)
	}
	if reviewer == "" && resumeJobID == "" {
		return requestIDPrefix + digestHex[:32], nil
	}
	identity := requestDigest
	if reviewer != "" {
		identity += "\x00" + reviewer
	}
	if resumeJobID != "" {
		identity += "\x00" + resumeJobID
	}
	requestIdentity := sha256.Sum256([]byte(identity))
	return requestIDPrefix + hex.EncodeToString(requestIdentity[:])[:32], nil
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
