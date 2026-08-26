package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestReviewCommandsSubmitTaskReceiptAndSweepTerminalWorkspace(t *testing.T) {
	timeout := &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_review",
			State:   engine.StateQueued,
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
	if got := len(fake.statuses); got != 0 {
		t.Fatalf("review status reads=%d, want none before a workspace is retained", got)
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

	fake.status = client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_review", State: engine.StateCompleted}}}
	fake.submitResult = client.JobSubmitResult{JobID: "job_adversarial", State: engine.StateQueued, Timeout: timeout}
	var secondOut, secondErr bytes.Buffer
	if code := run([]string{
		"adversarial-review", "--backend", "claude", "--cwd", repo, "--scope", "working-tree",
		"--model", "review-model", "--effort", "review-effort",
	}, nil, &secondOut, &secondErr); code != 0 {
		t.Fatalf("adversarial-review code=%d stderr=%q", code, secondErr.String())
	}
	assertTaskReceiptShape(t, secondOut.Bytes(), "job_adversarial")
	if got := len(fake.statuses); got != 1 {
		t.Fatalf("later review status reads=%d, want one one-shot cleanup check", got)
	}
	if got := fake.statuses[0].JobID; got != "job_review" {
		t.Fatalf("cleanup status job=%q, want job_review", got)
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

	fake.status = client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_adversarial", State: engine.StateCompleted}}}
	if errs := sweepReviewWorkspaces(context.Background(), stateDir); len(errs) != 0 {
		t.Fatalf("final workspace sweep errors=%v", errs)
	}
	metadata, errs = loadReviewWorkspaceMetadata(stateDir)
	if len(errs) != 0 || len(metadata) != 0 {
		t.Fatalf("metadata after terminal cleanup=%#v errors=%v", metadata, errs)
	}
}

func TestReviewCommandsPassModelAndEffortThrough(t *testing.T) {
	for _, tc := range []struct {
		name, command, model, effort string
	}{
		{name: "review supplied", command: "review", model: "review-model", effort: "review-effort"},
		{name: "adversarial review omitted", command: "adversarial-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				submitResult: client.JobSubmitResult{
					JobID:   "job_passthrough",
					State:   engine.StateQueued,
					Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			args := []string{tc.command, "--backend", "claude", "--cwd", newReviewCommandRepository(t), "--scope", "working-tree"}
			if tc.model != "" {
				args = append(args, "--model", tc.model, "--effort", tc.effort)
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("%s code=%d stderr=%q", tc.command, code, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submits=%d, want 1", len(fake.submits))
			}
			if got := fake.submits[0].TaskSpec; got.Model != tc.model || got.Effort != tc.effort {
				t.Fatalf("submitted model/effort=%q/%q, want %q/%q", got.Model, got.Effort, tc.model, tc.effort)
			}
		})
	}
}

func TestReviewAmbiguousSubmissionFailureKeepsWorkspace(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
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
		hello:        helloWithCapabilities(),
		submitResult: client.JobSubmitResult{JobID: "job_metadata_failure", State: engine.StateQueued},
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
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	fake.hello.Backends = []string{"claude"}
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
	fake := &fakeAgentbusClient{hello: helloWithCapabilities(), submitErr: &rpcErr}
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
	if receipt.JobID != jobID || receipt.State != engine.StateQueued || receipt.Timeout == nil {
		t.Fatalf("receipt=%#v", receipt)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if got, want := len(fields), 7; got != want {
		t.Fatalf("receipt fields=%#v, want exactly %d", fields, want)
	}
	for _, key := range []string{"requestId", "jobId", "state", "deduplicated", "model", "effort", "timeout"} {
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
