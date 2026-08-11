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
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
	reviewpkg "github.com/charlesnpx/delegate/internal/review"
)

func TestReviewCommandsUseReadOnlySanitizedTaskPipelineAndEnvelopeKinds(t *testing.T) {
	for _, tc := range []struct {
		command  string
		wantKind string
		framing  string
	}{
		{command: "review", wantKind: reviewKind, framing: "Perform a read-only code review"},
		{command: "adversarial-review", wantKind: adversarialReviewKind, framing: "refute-first"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			repo := newCommandGitFixture(t)
			writeCommandFixture(t, repo, "visible.go", "package visible\n// PUBLIC_CHANGE\n")
			writeCommandFixture(t, repo, ".env.local", "CLI_TRACKED_SECRET_NEVER\n")
			gitCommandFixture(t, repo, "add", ".env.local")
			gitCommandFixture(t, repo, "commit", "-m", "track secret path")
			writeCommandFixture(t, repo, ".env.local", "CLI_CHANGED_SECRET_NEVER\n")

			report := compliantReport()
			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				result: client.JobResult{
					JobID:              "job_" + strings.ReplaceAll(tc.command, "-", "_"),
					SessionID:          "session_review",
					State:              engine.StateCompleted,
					CleanupDisposition: cleanupDispositionVerifiedAbsent,
					Result:             &engine.ResultInfo{Text: report, SHA256: rawSHA256(report), Bytes: int64(len(report))},
					Contract:           ptr(compliantContractStamp(t, report)),
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := delegateconfig.Save(delegateconfig.Config{Overridable: true, Backend: delegateconfig.Backends{Codex: delegateconfig.Defaults{Model: "review-default-model", Effort: "review-default-effort"}}}); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			code := run([]string{tc.command, "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--wait", "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s code=%d stderr=%q", tc.command, code, stderr.String())
			}
			if strings.Contains(stderr.String(), readOnlyTaskHint) {
				t.Fatalf("%s emitted task read-only hint: %q", tc.command, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submits=%d, want 1", len(fake.submits))
			}
			spec := fake.submits[0].TaskSpec
			if spec.Write {
				t.Fatal("review TaskSpec.Write=true, want read-only")
			}
			if spec.CWD == repo || !strings.Contains(filepath.ToSlash(spec.CWD), "/delegate/review-") {
				t.Fatalf("safe review cwd=%q, repo=%q", spec.CWD, repo)
			}
			if spec.Tags["delegate.kind"] != tc.wantKind {
				t.Fatalf("delegate.kind=%q, want %q", spec.Tags["delegate.kind"], tc.wantKind)
			}
			if spec.Model != "review-default-model" || spec.Effort != "review-default-effort" {
				t.Fatalf("review defaults model=%q effort=%q", spec.Model, spec.Effort)
			}
			for _, required := range []string{tc.framing, "PUBLIC_CHANGE", "REDACTED\tM\t\".env.local\""} {
				if !strings.Contains(spec.Prompt, required) {
					t.Fatalf("prompt missing %q: %q", required, spec.Prompt)
				}
			}
			for _, forbidden := range []string{"CLI_TRACKED_SECRET_NEVER", "CLI_CHANGED_SECRET_NEVER", repo} {
				if strings.Contains(spec.Prompt, forbidden) {
					t.Fatalf("prompt leaked %q", forbidden)
				}
			}
			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Kind != tc.wantKind {
				t.Fatalf("envelope kind=%q, want %q", env.Kind, tc.wantKind)
			}
			if env.Model.Effective != "review-default-model" || env.Model.Source != "config" || env.Effort.Effective != "review-default-effort" || env.Effort.Source != "config" {
				t.Fatalf("review model/effort envelope model=%#v effort=%#v", env.Model, env.Effort)
			}
			if _, err := os.Stat(spec.CWD); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("terminal review workspace still exists or stat failed: %v", err)
			}
		})
	}
}

func TestReviewWaitCleanupUsesStatusDispositionWhenResultOmitsIt(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.txt", "change\n")
	report := compliantReport()
	jobID := "job_review_wait_status_cleanup"
	sessionID := "session_review_wait_status_cleanup"
	fake := &reviewWaitStatusCleanupClient{fakeAgentbusClient: fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:     jobID,
			SessionID: sessionID,
			State:     engine.StateCompleted,
			Result:    &engine.ResultInfo{Text: report, SHA256: rawSHA256(report), Bytes: int64(len(report))},
			Contract:  ptr(compliantContractStamp(t, report)),
		},
	}}
	restore := stubAgentbusClientGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--wait", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review code=%d stderr=%q stdout=%q, want success", code, stderr.String(), stdout.String())
	}
	if fake.captureErr != nil {
		t.Fatalf("capture artifacts before status cleanup: %v", fake.captureErr)
	}
	if fake.inputPath == "" || fake.workspace == "" {
		t.Fatalf("captured input=%q workspace=%q, want both paths before cleanup", fake.inputPath, fake.workspace)
	}
	assertPathMissing(t, fake.inputPath)
	assertPathMissing(t, fake.workspace)
	meta, found, err := loadJobMetadata("", jobID)
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if meta.JobInputPath != "" || meta.ReviewWorkspace != "" || meta.CleanupDisposition != cleanupDispositionVerifiedAbsent {
		t.Fatalf("metadata after cleanup=%#v, want removed artifacts and status fallback disposition", meta)
	}
	if strings.Contains(stderr.String(), "retained local job artifacts") {
		t.Fatalf("stderr=%q, want no retention warning", stderr.String())
	}
}

type reviewWaitStatusCleanupClient struct {
	fakeAgentbusClient
	inputPath  string
	workspace  string
	captureErr error
}

func (f *reviewWaitStatusCleanupClient) JobStatus(_ context.Context, params client.JobStatusParams) (client.JobStatusResult, error) {
	f.statuses = append(f.statuses, params)
	meta, found, err := loadJobMetadata("", params.JobID)
	if err != nil {
		f.captureErr = err
	} else if !found {
		f.captureErr = errors.New("metadata missing before wait cleanup")
	} else {
		f.inputPath = meta.JobInputPath
		f.workspace = meta.ReviewWorkspace
		if f.inputPath == "" || f.workspace == "" {
			f.captureErr = errors.New("metadata missing artifact paths before wait cleanup")
		} else if _, err := os.Stat(f.inputPath); err != nil {
			f.captureErr = err
		} else if _, err := os.Stat(f.workspace); err != nil {
			f.captureErr = err
		}
	}
	return client.JobStatusResult{Jobs: []client.JobStatus{{
		JobID:              params.JobID,
		SessionID:          "session_review_wait_status_cleanup",
		State:              engine.StateCompleted,
		CleanupDisposition: cleanupDispositionVerifiedAbsent,
	}}}, nil
}

func TestReviewBackgroundArtifactPersistsUntilTerminalResultCleanup(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "large.txt", strings.Repeat("x", reviewpkg.MaxInlineBytes+1))
	report := compliantReport()
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              "job_review_artifact",
			SessionID:          "session_review_artifact",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
			Result:             &engine.ResultInfo{Text: report, SHA256: rawSHA256(report), Bytes: int64(len(report))},
			Contract:           ptr(compliantContractStamp(t, report)),
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var launchOut, launchErr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--background", "--json"}, nil, &launchOut, &launchErr)
	if code != 0 {
		t.Fatalf("review launch code=%d stderr=%q", code, launchErr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	workspace := fake.submits[0].TaskSpec.CWD
	meta, found, err := loadJobMetadata("", "job_review_artifact")
	if err != nil || !found {
		t.Fatalf("load metadata found=%v err=%v", found, err)
	}
	if meta.ReviewWorkspace != workspace {
		t.Fatalf("metadata workspace=%q, want %q", meta.ReviewWorkspace, workspace)
	}
	artifact := filepath.Join(workspace, "review.patch")
	if info, err := os.Stat(artifact); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact before result info=%v err=%v", info, err)
	}
	if !strings.Contains(fake.submits[0].TaskSpec.Prompt, `"review.patch"`) || strings.Contains(fake.submits[0].TaskSpec.Prompt, strings.Repeat("x", 100)) {
		t.Fatalf("spilled prompt did not reference artifact cleanly: %q", fake.submits[0].TaskSpec.Prompt)
	}

	var resultOut, resultErr bytes.Buffer
	code = run([]string{"result", "--job", "job_review_artifact", "--json"}, nil, &resultOut, &resultErr)
	if code != 0 {
		t.Fatalf("result code=%d stderr=%q", code, resultErr.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(resultOut.Bytes()), &env); err != nil {
		t.Fatal(err)
	}
	if env.Kind != reviewKind {
		t.Fatalf("result envelope kind=%q, want review", env.Kind)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal result left workspace: %v", err)
	}
}

func TestReviewSubmissionIntentFailureBeforeLaunchAbortsAndCleansWorkspace(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.txt", "change\n")
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveSubmissionIntent
	saveSubmissionIntent = func(string, submissionIntent) error {
		return errors.New("intent store unavailable before launch")
	}
	defer func() { saveSubmissionIntent = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--background", "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("review code=0, want metadata failure; stdout=%q", stdout.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls=%d, want 0", len(fake.submits))
	}
	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "delegate")
	workspaces, err := filepath.Glob(filepath.Join(stateDir, "review-*"))
	if err != nil || len(workspaces) != 0 {
		t.Fatalf("review workspaces after aborted launch=%#v, %v", workspaces, err)
	}
	if !strings.Contains(stderr.String(), "persist submission intent before launch") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestReviewMetadataFailureAfterLaunchUsesDurableFallbackAndPreservesKind(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.txt", "change\n")
	report := compliantReport()
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:              "job_review_metadata_fallback",
			SessionID:          "session_review_metadata_fallback",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
			Result:             &engine.ResultInfo{Text: report, SHA256: rawSHA256(report), Bytes: int64(len(report))},
			Contract:           ptr(compliantContractStamp(t, report)),
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveDelegateJobMetadata
	saveDelegateJobMetadata = func(string, jobMetadata) error {
		return errors.New("primary metadata write failed after launch")
	}
	defer func() { saveDelegateJobMetadata = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--wait", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls=%d, want 1", len(fake.submits))
	}
	workspace := fake.submits[0].TaskSpec.CWD
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatal(err)
	}
	if env.Kind != reviewKind {
		t.Fatalf("terminal envelope kind=%q, want %q", env.Kind, reviewKind)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback metadata did not reap review workspace: %v", err)
	}
	if !strings.Contains(stderr.String(), "durable state-directory fallback") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestReviewMetadataFailureAfterSubmitReturnsRealJobEnvelopeAndKeepsWorkspace(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.txt", "change\n")
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		result: client.JobResult{JobID: "job_review_metadata_orphan", State: engine.StateRunning},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldPrimary := saveDelegateJobMetadata
	oldFallback := saveLaunchedJobMetadataFallback
	saveDelegateJobMetadata = func(stateDir string, meta jobMetadata) error {
		return errors.New("primary metadata unavailable after submit")
	}
	saveLaunchedJobMetadataFallback = func(string, jobMetadata) error {
		return errors.New("fallback metadata unavailable after submit")
	}
	defer func() {
		saveDelegateJobMetadata = oldPrimary
		saveLaunchedJobMetadataFallback = oldFallback
	}()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--wait", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls=%d, want 1", len(fake.submits))
	}
	var env LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("launch envelope invalid: %v; raw=%q", err, stdout.String())
	}
	if env.JobID != "job_review_metadata_orphan" || env.Status != string(engine.StateQueued) {
		t.Fatalf("launch envelope=%#v, want real submitted job", env)
	}
	workspace := fake.submits[0].TaskSpec.CWD
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("review workspace removed after successful submit: info=%v err=%v", info, err)
	}
	for _, warning := range []string{"primary metadata unavailable after submit", "fallback metadata unavailable after submit"} {
		if !strings.Contains(stderr.String(), warning) {
			t.Fatalf("stderr=%q, want warning %q", stderr.String(), warning)
		}
	}
	requestID := fake.submits[0].RequestID
	intent, found, err := loadSubmissionIntent("", requestID)
	if err != nil || !found {
		t.Fatalf("submission intent found=%v err=%v", found, err)
	}
	if intent.Phase != submissionPhaseInFlight || intent.ReviewWorkspace != workspace || intent.JobID != "" {
		t.Fatalf("submission intent=%#v, want in-flight intent retaining review workspace and no ack job", intent)
	}
	if _, found, err := loadJobMetadata("", "job_review_metadata_orphan"); err != nil || found {
		t.Fatalf("metadata found=%v err=%v, want absent after primary+fallback failure", found, err)
	}
}

func TestReviewAllowLiveRepoReadIsExplicitAndWarned(t *testing.T) {
	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.txt", "change\n")
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "claude", "--cwd", repo, "--scope", "working-tree", "--allow-live-repo-read", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	spec := fake.submits[0].TaskSpec
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if spec.CWD != canonicalRepo || spec.Write {
		t.Fatalf("live review spec cwd=%q write=%v", spec.CWD, spec.Write)
	}
	if !strings.Contains(spec.Prompt, "LIVE-REPOSITORY MODE") || !strings.Contains(stderr.String(), liveRepoReadWarning) {
		t.Fatalf("prompt=%q stderr=%q", spec.Prompt, stderr.String())
	}
}

func TestReviewRejectsWriteAndWorkingTreeBaseFlags(t *testing.T) {
	for _, args := range [][]string{
		{"review", "--backend", "codex", "--write"},
		{"review", "--backend", "codex", "--scope", "working-tree", "--base", "main"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code == 0 {
			t.Fatalf("run(%v) code=0", args)
		}
	}
}

func TestReviewHelpStatesContextBoundaryAndFilesystemLimits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_ = run([]string{"review", "--help"}, nil, &stdout, &stderr)
	for _, want := range []string{
		"redacts secret-matched paths and secret-like diff hunks",
		"does not prevent a same-user backend from reading",
		"--allow-live-repo-read makes those reads easier",
		"OS-level isolation requires a container/sandbox profile",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("review help missing %q: %q", want, stderr.String())
		}
	}
}

func newCommandGitFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitCommandFixture(t, repo, "init", "-b", "main")
	gitCommandFixture(t, repo, "config", "user.name", "Delegate Test")
	gitCommandFixture(t, repo, "config", "user.email", "delegate@example.invalid")
	writeCommandFixture(t, repo, "README.md", "fixture\n")
	gitCommandFixture(t, repo, "add", "README.md")
	gitCommandFixture(t, repo, "commit", "-m", "initial")
	return repo
}

func writeCommandFixture(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitCommandFixture(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
}
