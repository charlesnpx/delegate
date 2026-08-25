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
