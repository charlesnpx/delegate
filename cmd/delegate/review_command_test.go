package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/witness/contract/charter"
	reviewcontract "github.com/charlesnpx/witness/contract/review"
)

func TestContractReviewReusesDeterministicRequestAndWorkspace(t *testing.T) {
	timeout := &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}
	fake := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_contract_review",
			State:   publicStateQueued,
			Timeout: timeout,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewFixture(t)
	callerCWD := t.TempDir() // Deliberately not a repository.
	args := []string{
		"review", "--backend", "codex", "--cwd", callerCWD,
		"--request-file", fixture.requestPath,
		"--artifact-file", fixture.artifactPath,
		"--charter-file", fixture.charterPath,
		"--model", "review-model", "--effort", "review-effort",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("contract review code=%d stderr=%q", code, stderr.String())
	}
	assertTaskReceiptShape(t, stdout.Bytes(), "job_contract_review")
	var replayOut, replayErr bytes.Buffer
	if code := run(args, nil, &replayOut, &replayErr); code != 0 {
		t.Fatalf("replayed contract review code=%d stderr=%q", code, replayErr.String())
	}
	assertTaskReceiptShape(t, replayOut.Bytes(), "job_contract_review")
	if len(fake.submits) != 2 {
		t.Fatalf("submits=%d, want 2", len(fake.submits))
	}
	first, replayed := fake.submits[0], fake.submits[1]
	wantRequestID := "delegate-review-" + strings.TrimPrefix(fixture.requestDigest, "sha256:")[:32]
	if first.RequestID != wantRequestID || replayed.RequestID != wantRequestID {
		t.Fatalf("request IDs=%q/%q, want %q", first.RequestID, replayed.RequestID, wantRequestID)
	}
	if first.TaskSpec.CWD != replayed.TaskSpec.CWD {
		t.Fatalf("backend cwds=%q/%q, want the same deterministic workspace", first.TaskSpec.CWD, replayed.TaskSpec.CWD)
	}
	spec := first.TaskSpec
	if len(spec.OutputSchema) == 0 {
		t.Fatal("contract review output schema is empty")
	}
	for _, want := range []string{fixture.frozen.CharterHash, fixture.reviewInputDigest, "EXACTLY ONE review-report-v1 JSON object"} {
		if !strings.Contains(string(spec.OutputSchema), want) && !strings.Contains(spec.Prompt, want) {
			t.Fatalf("schema/prompt missing %q\nschema=%s\nprompt=%s", want, spec.OutputSchema, spec.Prompt)
		}
	}
	if strings.Contains(spec.Prompt, "Present findings first, ordered by severity") {
		t.Fatalf("contract prompt includes plain-mode prose: %q", spec.Prompt)
	}

	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	resolvedStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace := filepath.Join(resolvedStateDir, "review-contract", strings.TrimPrefix(fixture.requestDigest, "sha256:"))
	if spec.CWD != wantWorkspace {
		t.Fatalf("submitted cwd=%q, want deterministic private workspace %q", spec.CWD, wantWorkspace)
	}
	metadata, metadataErrs := loadReviewWorkspaceMetadata(stateDir)
	if len(metadataErrs) != 0 || len(metadata) != 0 {
		t.Fatalf("contract workspace metadata=%#v errors=%v, want no cleanup record", metadata, metadataErrs)
	}
	workspace := spec.CWD
	artifact, err := os.ReadFile(filepath.Join(workspace, "review.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifact, fixture.artifact) {
		t.Fatalf("private artifact changed:\n got %q\nwant %q", artifact, fixture.artifact)
	}
	charterData, err := os.ReadFile(filepath.Join(workspace, "charter.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frozen charter.FrozenCharter
	if err := json.Unmarshal(charterData, &frozen); err != nil {
		t.Fatalf("private charter is not frozen charter JSON: %v", err)
	}
	if frozen.CharterHash != fixture.frozen.CharterHash {
		t.Fatalf("private charter hash=%q, want %q", frozen.CharterHash, fixture.frozen.CharterHash)
	}
}

func TestContractReviewRejectsTamperedArtifactBeforeSubmission(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewFixture(t)
	if err := os.WriteFile(fixture.artifactPath, append(fixture.artifact, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(contractReviewArgs(fixture, t.TempDir()), nil, &stdout, &stderr); code == 0 {
		t.Fatal("tampered contract review unexpectedly succeeded")
	}
	if !strings.Contains(stderr.String(), "digest mismatch") {
		t.Fatalf("stderr=%q, want artifact digest mismatch", stderr.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("submits=%d, want 0 before preflight rejection", len(fake.submits))
	}
}

func TestContractReviewPreAdmissionFailureKeepsPublishedWorkspace(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	fake.hello.BackendMetadata = []client.BackendInfo{{Name: "claude"}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run(contractReviewArgs(fixture, t.TempDir()), nil, &stdout, &stderr); code == 0 {
		t.Fatal("contract review unexpectedly succeeded with an unavailable backend")
	}
	if got := len(fake.submits); got != 0 {
		t.Fatalf("submits=%d, want 0 before backend validation failure", got)
	}

	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	resolvedStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(resolvedStateDir, "review-contract", strings.TrimPrefix(fixture.requestDigest, "sha256:"))
	if info, err := os.Stat(workspace); err != nil {
		t.Fatalf("published workspace: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("published workspace mode=%v, want directory", info.Mode())
	}
	wantCharter, err := json.Marshal(fixture.frozen)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		path string
		want []byte
	}{
		{name: "charter", path: filepath.Join(workspace, "charter.json"), want: wantCharter},
		{name: "artifact", path: filepath.Join(workspace, "review.patch"), want: fixture.artifact},
	} {
		got, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("published %s: %v", file.name, err)
		}
		if !bytes.Equal(got, file.want) {
			t.Fatalf("published %s changed:\n got %q\nwant %q", file.name, got, file.want)
		}
	}
}

func TestContractReviewRejectsLiveDiscoveryFlags(t *testing.T) {
	for _, flagArgs := range [][]string{
		{"--base", "origin/main"},
		{"--scope", "working-tree"},
		{"--allow-live-repo-read"},
	} {
		t.Run(strings.Join(flagArgs, " "), func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithBackends()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			fixture := newContractReviewFixture(t)
			args := append(contractReviewArgs(fixture, t.TempDir()), flagArgs...)
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code == 0 {
				t.Fatalf("contract review with %q unexpectedly succeeded", flagArgs)
			}
			if !strings.Contains(stderr.String(), "contract mode") || !strings.Contains(stderr.String(), flagArgs[0]) {
				t.Fatalf("stderr=%q, want contract-mode rejection for %q", stderr.String(), flagArgs[0])
			}
			if len(fake.submits) != 0 {
				t.Fatalf("submits=%d, want 0", len(fake.submits))
			}
		})
	}
}

func TestContractReviewRequiresAllInputFiles(t *testing.T) {
	fixture := newContractReviewFixture(t)
	for _, args := range [][]string{
		{"--request-file", fixture.requestPath},
		{"--request-file", fixture.requestPath, "--artifact-file", fixture.artifactPath},
		{"--request-file", fixture.requestPath, "--charter-file", fixture.charterPath},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			args = append([]string{"--backend", "codex"}, args...)
			if _, err := parseReviewOptions(reviewKind, args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--request-file, --artifact-file, and --charter-file together") {
				t.Fatalf("parseReviewOptions(%q) error=%v, want all-files error", args, err)
			}
		})
	}
}

func TestReviewCommandsSubmitTaskReceiptAndSweepTerminalWorkspace(t *testing.T) {
	timeout := &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}
	fake := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_review",
			State:   publicStateQueued,
			Timeout: timeout,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	repo := newReviewCommandRepository(t)
	var firstOut, firstErr bytes.Buffer
	if code := run([]string{
		"review", "--backend", "claude", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, nil, &firstOut, &firstErr); code != 0 {
		t.Fatalf("review code=%d stderr=%q", code, firstErr.String())
	}
	assertTaskReceiptShape(t, firstOut.Bytes(), "job_review")
	if got := len(fake.gets); got != 0 {
		t.Fatalf("review job reads=%d, want none before a workspace is retained", got)
	}

	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	metadata, errs := loadReviewWorkspaceMetadata(stateDir)
	if len(errs) != 0 || len(metadata) != 1 {
		t.Fatalf("first review workspace metadata=%#v errors=%v", metadata, errs)
	}
	firstWorkspace := metadata[0].Workspace
	artifactPath := filepath.Join(firstWorkspace, "review.patch")
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read retained review artifact: %v", err)
	}
	if !bytes.Contains(artifact, []byte("large review context")) {
		t.Fatalf("review artifact did not contain the assembled repository content")
	}

	fake.get = client.JobGetResult{JobID: "job_review", State: publicStateCompleted}
	fake.submitResult = client.JobSubmitResult{JobID: "job_adversarial", State: publicStateQueued, Timeout: timeout}
	var secondOut, secondErr bytes.Buffer
	if code := run([]string{
		"adversarial-review", "--backend", "claude", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, nil, &secondOut, &secondErr); code != 0 {
		t.Fatalf("adversarial-review code=%d stderr=%q", code, secondErr.String())
	}
	assertTaskReceiptShape(t, secondOut.Bytes(), "job_adversarial")
	if got := len(fake.gets); got != 1 {
		t.Fatalf("later review job reads=%d, want one one-shot cleanup check", got)
	}
	if got := fake.gets[0].JobID; got != "job_review" {
		t.Fatalf("cleanup job=%q, want job_review", got)
	}
	if _, err := os.Stat(firstWorkspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal review workspace still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(artifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal review artifact still exists or stat failed: %v", err)
	}
	metadata, errs = loadReviewWorkspaceMetadata(stateDir)
	if len(errs) != 0 || len(metadata) != 1 || metadata[0].JobID != "job_adversarial" {
		t.Fatalf("metadata after later review=%#v errors=%v", metadata, errs)
	}

	fake.get = client.JobGetResult{JobID: "job_adversarial", State: publicStateCompleted}
	if errs := sweepReviewWorkspaces(context.Background(), stateDir); len(errs) != 0 {
		t.Fatalf("final workspace sweep errors=%v", errs)
	}
	metadata, errs = loadReviewWorkspaceMetadata(stateDir)
	if len(errs) != 0 || len(metadata) != 0 {
		t.Fatalf("metadata after terminal cleanup=%#v errors=%v", metadata, errs)
	}
}

func TestSweepReviewWorkspacesLeavesWorkspaceWhenJobGetFails(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(stateDir, "review-retained")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	meta := reviewWorkspaceMetadata{
		JobID:             "job_unavailable",
		Workspace:         workspace,
		AgentbusStateRoot: stateRoot,
	}
	if err := saveReviewWorkspaceMetadata(stateDir, meta); err != nil {
		t.Fatal(err)
	}

	fake := &fakeAgentbusClient{hello: helloWithBackends(), getErr: errors.New("job lookup unavailable")}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	errs := sweepReviewWorkspaces(context.Background(), stateDir)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "job lookup unavailable") {
		t.Fatalf("sweep errors=%v, want lookup error", errs)
	}
	if len(fake.gets) != 1 || fake.gets[0].JobID != meta.JobID {
		t.Fatalf("job gets=%#v, want %q", fake.gets, meta.JobID)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace was removed after failed lookup: %v", err)
	}
	metadata, metadataErrs := loadReviewWorkspaceMetadata(stateDir)
	if len(metadataErrs) != 0 || len(metadata) != 1 || metadata[0] != meta {
		t.Fatalf("metadata after failed lookup=%#v errors=%v, want %#v", metadata, metadataErrs, meta)
	}
}

func TestReviewCommandsPassModelEffortAndResumeThrough(t *testing.T) {
	for _, tc := range []struct {
		name, command, model, effort, resume string
	}{
		{name: "review supplied", command: "review", model: "review-model", effort: "review-effort", resume: "job_killed"},
		{name: "adversarial review omitted", command: "adversarial-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello: helloWithBackends(),
				submitResult: client.JobSubmitResult{
					JobID:   "job_passthrough",
					State:   publicStateQueued,
					Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			args := []string{tc.command, "--backend", "claude", "--cwd", newReviewCommandRepository(t), "--scope", "working-tree"}
			if tc.model != "" {
				args = append(args, "--model", tc.model, "--effort", tc.effort)
			}
			if tc.resume != "" {
				args = append(args, "--resume", tc.resume)
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("%s code=%d stderr=%q", tc.command, code, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submits=%d, want 1", len(fake.submits))
			}
			if got := fake.submits[0].TaskSpec; taskSpecString(got.Model) != tc.model || taskSpecString(got.Effort) != tc.effort || got.ResumeJobID != tc.resume {
				t.Fatalf("submitted model/effort/resume=%#v/%#v/%q, want %q/%q/%q", got.Model, got.Effort, got.ResumeJobID, tc.model, tc.effort, tc.resume)
			}
			if tc.resume != "" && len(fake.gets) != 0 {
				t.Fatalf("review locally inspected resume target: gets=%#v", fake.gets)
			}
		})
	}
}

func TestReviewAmbiguousSubmissionFailureKeepsWorkspace(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello:     helloWithBackends(),
		submitErr: errors.New("lost acknowledgement"),
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	repo := newReviewCommandRepository(t)
	var stdout, stderr bytes.Buffer
	_, err := runReview(reviewKind, []string{
		"--backend", "codex", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runReview() error = nil, want ambiguous submission failure")
	}
	if got := len(fake.submits); got != 1 {
		t.Fatalf("submits=%d, want 1", got)
	}

	workspaces := reviewWorkspacePaths(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate"))
	if len(workspaces) != 1 {
		t.Fatalf("retained workspaces=%q, want one", workspaces)
	}
	if _, err := os.Stat(filepath.Join(workspaces[0], "review.patch")); err != nil {
		t.Fatalf("retained workspace artifact: %v", err)
	}
}

func TestReviewMetadataWriteFailureAfterSubmissionKeepsWorkspace(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello:        helloWithBackends(),
		submitResult: client.JobSubmitResult{JobID: "job_metadata_failure", State: publicStateQueued},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, reviewWorkspaceMetadataDirectoryName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := newReviewCommandRepository(t)
	var stdout, stderr bytes.Buffer
	_, err := runReview(reviewKind, []string{
		"--backend", "codex", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runReview() error = nil, want review workspace metadata write failure")
	}
	if !strings.Contains(err.Error(), "record review workspace") {
		t.Fatalf("error=%q, want review workspace metadata context", err)
	}
	if got := len(fake.submits); got != 1 {
		t.Fatalf("submits=%d, want 1", got)
	}
	if workspaces := reviewWorkspacePaths(t, stateDir); len(workspaces) != 1 {
		t.Fatalf("retained workspaces=%q, want one", workspaces)
	}
}

func TestReviewPreSubmissionFailureRemovesWorkspace(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	fake.hello.BackendMetadata = []client.BackendInfo{{Name: "claude"}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	repo := newReviewCommandRepository(t)
	var stdout, stderr bytes.Buffer
	_, err := runReview(reviewKind, []string{
		"--backend", "codex", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runReview() error = nil, want pre-submission backend validation failure")
	}
	if got := len(fake.submits); got != 0 {
		t.Fatalf("submits=%d, want no submission", got)
	}
	if workspaces := reviewWorkspacePaths(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")); len(workspaces) != 0 {
		t.Fatalf("workspaces=%q, want none after pre-submission failure", workspaces)
	}
}

func TestReviewAcceptedRPCErrorReportsIdentityAndKeepsWorkspaceMetadata(t *testing.T) {
	var rpcErr client.RPCError
	if err := json.Unmarshal([]byte(`{"Object":{"code":-32000,"message":"admission closing","data":{"code":"backend_unavailable","jobId":"job_accepted_after_error"}}}`), &rpcErr); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: &rpcErr}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	repo := newReviewCommandRepository(t)
	var stdout, stderr bytes.Buffer
	_, err := runReview(reviewKind, []string{
		"--backend", "codex", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runReview() error = nil, want post-accept RPC error")
	}
	if got := len(fake.submits); got != 1 {
		t.Fatalf("submits=%d, want 1", got)
	}
	if !strings.Contains(err.Error(), fake.submits[0].RequestID) || !strings.Contains(err.Error(), "job_accepted_after_error") {
		t.Fatalf("error=%q, want request ID %q and accepted job ID", err, fake.submits[0].RequestID)
	}

	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	metadata, metadataErrs := loadReviewWorkspaceMetadata(stateDir)
	if len(metadataErrs) != 0 || len(metadata) != 1 {
		t.Fatalf("review workspace metadata=%#v errors=%v", metadata, metadataErrs)
	}
	if metadata[0].JobID != "job_accepted_after_error" {
		t.Fatalf("metadata job id=%q, want accepted job id", metadata[0].JobID)
	}
	if _, err := os.Stat(metadata[0].Workspace); err != nil {
		t.Fatalf("retained workspace: %v", err)
	}
}

func reviewWorkspacePaths(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != reviewWorkspaceMetadataDirectoryName && strings.HasPrefix(entry.Name(), "review-") {
			paths = append(paths, filepath.Join(stateDir, entry.Name()))
		}
	}
	return paths
}

func assertTaskReceiptShape(t *testing.T, raw []byte, jobID string) {
	t.Helper()
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("receipt JSON invalid: %v; raw=%q", err, string(raw))
	}
	if receipt.JobID != jobID || receipt.State != publicStateQueued || receipt.Timeout == nil || receipt.WorkspaceKey == "" {
		t.Fatalf("receipt=%#v", receipt)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if got, want := len(fields), 8; got != want {
		t.Fatalf("receipt fields=%#v, want exactly %d", fields, want)
	}
	for _, key := range []string{"requestId", "workspaceKey", "jobId", "state", "deduplicated", "model", "effort", "timeout"} {
		if _, found := fields[key]; !found {
			t.Fatalf("receipt fields=%#v, missing %q", fields, key)
		}
	}
	for _, forbidden := range []string{"schema", "kind", "status", "cleanup_disposition"} {
		if _, found := fields[forbidden]; found {
			t.Fatalf("receipt fields=%#v, unexpectedly includes %q", fields, forbidden)
		}
	}
}

func taskSpecString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func newReviewCommandRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runReviewGit(t, repo, "init")
	runReviewGit(t, repo, "config", "user.email", "delegate-test@example.invalid")
	runReviewGit(t, repo, "config", "user.name", "Delegate Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runReviewGit(t, repo, "add", "tracked.txt")
	runReviewGit(t, repo, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "large.txt"), []byte(strings.Repeat("large review context\n", 16*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func runReviewGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

type contractReviewFixture struct {
	requestPath       string
	artifactPath      string
	charterPath       string
	artifact          []byte
	frozen            charter.FrozenCharter
	requestDigest     string
	reviewInputDigest string
}

func newContractReviewFixture(t *testing.T) contractReviewFixture {
	t.Helper()
	inputCharter, ok := charter.InitTemplate(charter.TemplateDeltaReview, "delegate-test", "event-test", "Delegate contract test fixture.")
	if !ok {
		t.Fatal("delta-review charter template is unavailable")
	}
	frozen, err := charter.Freeze(inputCharter, nil)
	if err != nil {
		t.Fatalf("freeze fixture charter: %v", err)
	}
	charterData, err := json.Marshal(inputCharter)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("diff --git a/subject.go b/subject.go\n+CALLER_FROZEN_SECRET=delegate-must-not-scan\n")
	sum := sha256.Sum256(artifact)
	reviewInputDigest := "sha256:" + hex.EncodeToString(sum[:])
	request := reviewcontract.ReviewRequestDocument{
		SchemaVersion: reviewcontract.ReviewRequestV1,
		ConsumerIdentity: reviewcontract.Identity{
			Kind: "delegate",
			ID:   "delegate-contract-test",
		},
		Subject:           reviewcontract.RequestSubject{Head: "frozen-subject-head"},
		CharterHash:       frozen.CharterHash,
		ReviewInputDigest: reviewInputDigest,
	}
	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest, err := reviewcontract.ReviewRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := contractReviewFixture{
		requestPath:       filepath.Join(dir, "request.json"),
		artifactPath:      filepath.Join(dir, "artifact.patch"),
		charterPath:       filepath.Join(dir, "charter.json"),
		artifact:          artifact,
		frozen:            frozen,
		requestDigest:     requestDigest,
		reviewInputDigest: reviewInputDigest,
	}
	for _, file := range []struct {
		path string
		data []byte
	}{
		{path: fixture.requestPath, data: requestData},
		{path: fixture.artifactPath, data: artifact},
		{path: fixture.charterPath, data: charterData},
	} {
		if err := os.WriteFile(file.path, file.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func contractReviewArgs(fixture contractReviewFixture, cwd string) []string {
	return []string{
		"review", "--backend", "codex", "--cwd", cwd,
		"--request-file", fixture.requestPath,
		"--artifact-file", fixture.artifactPath,
		"--charter-file", fixture.charterPath,
	}
}
