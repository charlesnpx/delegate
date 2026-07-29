package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

func TestSubmissionIntentPhaseSequenceModeAndPayloadRemoval(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithCapabilities(), result: client.JobResult{JobID: "job_ack_sequence", State: engine.StateQueued}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	handoffResult, err := handoff.Create(handoff.CreateOptions{
		StateDir: stateDir,
		Reader:   strings.NewReader("ack payload"),
	})
	if err != nil {
		t.Fatal(err)
	}

	realSave := saveSubmissionIntent
	var phases []string
	saveSubmissionIntent = func(stateDir string, intent submissionIntent) error {
		phases = append(phases, intent.Phase)
		return realSave(stateDir, intent)
	}
	defer func() { saveSubmissionIntent = realSave }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--handoff-prompt-file", handoffResult.HandoffPath, "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if want := []string{submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseAcknowledged}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("intent phases = %#v, want %#v", phases, want)
	}
	if _, err := os.Stat(handoffResult.HandoffPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff payload removal after ack stat err=%v, want not exist", err)
	}
	requestID := fake.submits[0].RequestID
	intent, found, err := loadSubmissionIntent(stateDir, requestID)
	if err != nil || !found {
		t.Fatalf("load intent found=%v err=%v", found, err)
	}
	if intent.Phase != submissionPhaseAcknowledged || intent.JobID != "job_ack_sequence" {
		t.Fatalf("intent=%#v, want acknowledged job_ack_sequence", intent)
	}
	intentPath := filepath.Join(stateDir, "submissions", encodedStateFilename(requestID))
	info, err := os.Stat(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("intent mode=%o, want 600", got)
	}
}

func TestDeduplicatedTerminalReplayEmitsTerminalEnvelopeWithoutWait(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     engine.JobState
		exitCode  int
		resultSHA string
	}{
		{name: "completed", state: engine.StateCompleted, exitCode: 0, resultSHA: strings.Repeat("a", 64)},
		{name: "failed", state: engine.StateFailed, exitCode: engine.ExitCodeForState(engine.StateFailed), resultSHA: strings.Repeat("b", 64)},
		{name: "canceled", state: engine.StateCanceled, exitCode: engine.ExitCodeForState(engine.StateCanceled), resultSHA: strings.Repeat("c", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "job_dedup_" + tc.name
			report := compliantReport()
			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				submitResult: client.JobSubmitResult{
					JobID:        jobID,
					State:        tc.state,
					Deduplicated: true,
				},
				status: client.JobStatusResult{Jobs: []client.JobStatus{{
					JobID:     jobID,
					SessionID: "session_" + tc.name,
					State:     tc.state,
				}}},
				result: client.JobResult{
					JobID:     jobID,
					SessionID: "session_" + tc.name,
					State:     tc.state,
					Result:    &engine.ResultInfo{Text: report, SHA256: tc.resultSHA, Bytes: int64(len(report))},
					Contract:  ptr(compliantContractStamp(t, report)),
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "terminal replay", "--background", "--json"}, nil, &stdout, &stderr)
			if code != tc.exitCode {
				t.Fatalf("task code=%d stderr=%q stdout=%q, want %d", code, stderr.String(), stdout.String(), tc.exitCode)
			}
			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("terminal JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Schema != envelopeSchema || env.JobID != jobID || env.Status != tc.state || env.Status == engine.StateRunning {
				t.Fatalf("terminal envelope=%#v, want schema %d job %s status %s", env, envelopeSchema, jobID, tc.state)
			}
			if env.RequestID == "" || !strings.HasPrefix(env.RequestID, "delegate-") {
				t.Fatalf("request_id=%q, want delegate request id", env.RequestID)
			}
			if !env.Deduplicated || !env.SubmissionDeduplicated {
				t.Fatalf("dedup fields = %v/%v, want true/true", env.Deduplicated, env.SubmissionDeduplicated)
			}
			if env.ResultSHA256 == nil || *env.ResultSHA256 != tc.resultSHA {
				t.Fatalf("result_sha256=%v, want %s", env.ResultSHA256, tc.resultSHA)
			}
			if len(fake.results) != 1 || fake.results[0].JobID != jobID {
				t.Fatalf("JobResult calls=%#v, want one call for %s", fake.results, jobID)
			}
			if len(fake.statuses) != 1 || fake.statuses[0].JobID != jobID || fake.statuses[0].All {
				t.Fatalf("JobStatus calls=%#v, want one requested status for %s", fake.statuses, jobID)
			}
		})
	}
}

func TestSubmitPreservesNonTerminalReplayStates(t *testing.T) {
	for _, state := range []engine.JobState{
		engine.StateQueued,
		engine.StateStarting,
		engine.StateRunning,
		engine.StateRetrying,
	} {
		t.Run(string(state), func(t *testing.T) {
			jobID := "job_submit_" + string(state)
			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				submitResult: client.JobSubmitResult{
					JobID:        jobID,
					State:        state,
					Deduplicated: true,
				},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "nonterminal replay", "--background", "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("task code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			var env LaunchEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("launch JSON: %v; raw=%q", err, stdout.String())
			}
			if env.Schema != envelopeSchema || env.JobID != jobID || env.Status != string(state) {
				t.Fatalf("launch envelope=%#v, want schema %d job %s status %s", env, envelopeSchema, jobID, state)
			}
			if env.RequestID == "" || !env.Deduplicated || !env.SubmissionDeduplicated {
				t.Fatalf("launch request/dedup fields=%#v, want request id and true dedup", env)
			}
			if len(fake.results) != 0 || len(fake.statuses) != 0 {
				t.Fatalf("nonterminal submit made result/status calls: results=%#v statuses=%#v", fake.results, fake.statuses)
			}
		})
	}
}

func TestAcknowledgementPathIsIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	handoffResult, err := handoff.Create(handoff.CreateOptions{
		StateDir: stateDir,
		Reader:   strings.NewReader("ack payload"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := handoff.ResolvePrompt(handoff.PromptSources{HandoffPromptFile: handoffResult.HandoffPath, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	requestID := "delegate-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	opts := taskOptions{
		Backend:           "codex",
		CWD:               t.TempDir(),
		StateDir:          stateDir,
		Kind:              taskKind,
		AgentbusStateRoot: t.TempDir(),
		RequestID:         requestID,
		WorkspaceKey:      "delegate-v1-" + strings.Repeat("b", 64),
		SubmissionState:   engine.StateRunning,
	}
	submitted := client.JobSubmitResult{JobID: "job_idempotent_ack", State: engine.StateRunning}

	for attempt := 1; attempt <= 2; attempt++ {
		warnings, acknowledged, err := acknowledgeSubmittedTask(opts, resolved, submitted, contractKindShape, "after idempotent test")
		if err != nil || !acknowledged || len(warnings) != 0 {
			t.Fatalf("ack attempt %d warnings=%#v acknowledged=%v err=%v", attempt, warnings, acknowledged, err)
		}
	}
	if _, err := os.Stat(handoffResult.HandoffPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff payload stat err=%v, want removed", err)
	}
	meta, found, err := loadJobMetadata(stateDir, submitted.JobID)
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if meta.RequestID != requestID || meta.JobInputPath == "" {
		t.Fatalf("metadata=%#v, want acknowledged request and retained running input", meta)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("job inputs=%#v metadata path=%q, want one reused input", matches, meta.JobInputPath)
	}
	matchPath, err := filepath.EvalSymlinks(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	metaPath, err := filepath.EvalSymlinks(meta.JobInputPath)
	if err != nil {
		t.Fatal(err)
	}
	if matchPath != metaPath {
		t.Fatalf("job input=%q metadata path=%q, want reused input", matchPath, metaPath)
	}

	params := testSubmitParams(t, requestID, "ack payload", nil)
	intent := testSubmissionIntent(params, opts.AgentbusStateRoot)
	intent.Phase = submissionPhaseAcknowledged
	intent.JobID = submitted.JobID
	if err := saveSubmissionIntent(stateDir, intent); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := transitionSubmissionIntent(stateDir, &intent, submissionPhaseAcknowledged, func(intent *submissionIntent) {
			intent.JobID = submitted.JobID
			intent.Deduplicated = submitted.Deduplicated
			intent.LastError = nil
		}); err != nil {
			t.Fatalf("intent ack attempt %d: %v", attempt, err)
		}
	}
	loaded, found, err := loadSubmissionIntent(stateDir, requestID)
	if err != nil || !found || loaded.Phase != submissionPhaseAcknowledged || loaded.JobID != submitted.JobID {
		t.Fatalf("intent=%#v found=%v err=%v, want acknowledged", loaded, found, err)
	}
}

func TestLostResponseReplayUsesSameRequestAndOneBackendExecution(t *testing.T) {
	bus := newReplayAdmissionBus()
	bus.closeBeforeFirstSubmitResponse = true
	restore := stubReplayAdmissionBus(t, bus)
	defer restore()
	t.Setenv("AGENTBUS_STATE_ROOT", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "lost response", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
		t.Fatalf("launch JSON: %v; raw=%q", err, stdout.String())
	}
	if launch.JobID != "opaque/job:1" {
		t.Fatalf("launch job id=%q, want opaque/job:1", launch.JobID)
	}
	submits := bus.submittedParams()
	if len(submits) != 2 {
		t.Fatalf("submits=%d, want lost-response replay", len(submits))
	}
	if submits[0].RequestID == "" || submits[0].RequestID != submits[1].RequestID {
		t.Fatalf("request IDs = %q / %q, want same nonempty ID", submits[0].RequestID, submits[1].RequestID)
	}
	if !reflect.DeepEqual(submits[0], submits[1]) {
		t.Fatalf("replayed params changed:\nfirst=%#v\nsecond=%#v", submits[0], submits[1])
	}
	if got := bus.backendExecutions(); got != 1 {
		t.Fatalf("backend executions=%d, want 1", got)
	}
}

func TestProtocolV2FakeServerLostResponseReplay(t *testing.T) {
	server := startProtocolV2FakeServer(t)
	server.closeBeforeFirstSubmitResponse = true
	restore := useRealAgentbusClient(t)
	defer restore()
	t.Setenv("AGENTBUS_STATE_ROOT", server.root)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "lost response over protocol", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	submits := server.submittedParams()
	if len(submits) != 2 || submits[0].RequestID == "" || submits[0].RequestID != submits[1].RequestID || !reflect.DeepEqual(submits[0], submits[1]) {
		t.Fatalf("protocol submits=%#v, want exact replay with same request", submits)
	}
	if got := server.backendExecutions(); got != 1 {
		t.Fatalf("backend executions=%d, want 1", got)
	}
}

func TestRecoverRequestRestoresCrashAfterResponseJobMapping(t *testing.T) {
	bus := newReplayAdmissionBus()
	restore := stubReplayAdmissionBus(t, bus)
	defer restore()
	recordedRoot := t.TempDir()
	t.Setenv("AGENTBUS_STATE_ROOT", recordedRoot)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	params := testSubmitParams(t, "delegate-11111111111111111111111111111111", "recover prompt", nil)
	bus.seedAccepted(params, "opaque/recovered:job")
	intent := testSubmissionIntent(params, recordedRoot)
	intent.Phase = submissionPhaseInFlight
	if err := saveSubmissionIntent(stateDir, intent); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--recover-request", params.RequestID, "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	meta, found, err := loadJobMetadata(stateDir, "opaque/recovered:job")
	if err != nil || !found {
		t.Fatalf("metadata found=%v err=%v", found, err)
	}
	if meta.RequestID != params.RequestID || meta.WorkspaceKey != params.WorkspaceKey {
		t.Fatalf("metadata=%#v, want restored request identity and root", meta)
	}
	wantRoot, err := canonicalizeAgentbusStateRoot("recorded root", recordedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if meta.AgentbusStateRoot != wantRoot {
		t.Fatalf("metadata root=%q, want %q", meta.AgentbusStateRoot, wantRoot)
	}
	if got := bus.backendExecutions(); got != 1 {
		t.Fatalf("backend executions=%d, want replay of already accepted request without a new execution", got)
	}
}

func TestRecoverRequestReplayConflictPreservesIntentAndDoesNotMintID(t *testing.T) {
	bus := newReplayAdmissionBus()
	restore := stubReplayAdmissionBus(t, bus)
	defer restore()
	recordedRoot := t.TempDir()
	t.Setenv("AGENTBUS_STATE_ROOT", recordedRoot)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	original := testSubmitParams(t, "delegate-22222222222222222222222222222222", "original prompt", nil)
	bus.seedAccepted(original, "opaque/conflict")
	changed := original
	changed.TaskSpec.Prompt = "changed prompt"
	intent := testSubmissionIntent(changed, recordedRoot)
	intent.Phase = submissionPhaseInFlight
	if err := saveSubmissionIntent(stateDir, intent); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--recover-request", original.RequestID, "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("recover conflict succeeded; stdout=%q", stdout.String())
	}
	loaded, found, err := loadSubmissionIntent(stateDir, original.RequestID)
	if err != nil || !found {
		t.Fatalf("intent found=%v err=%v", found, err)
	}
	if loaded.RequestID != original.RequestID || loaded.Phase != submissionPhaseBlocked || loaded.LastError == nil || loaded.LastError.AdmissionCause != admissionCauseReplayConflict {
		t.Fatalf("intent after conflict=%#v, want same blocked replay_conflict intent", loaded)
	}
	if _, found, err := loadJobMetadata(stateDir, "opaque/conflict"); err != nil || found {
		t.Fatalf("metadata found=%v err=%v, want no new job mapping", found, err)
	}
	intents, err := listSubmissionIntents(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].RequestID != original.RequestID {
		t.Fatalf("intents=%#v, want exactly original request", intents)
	}
}

func TestOpaqueJobMetadataFilenamesRoundTripAndVerifyInFileID(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, jobID := range []string{"abc123", "opaque/job:with?chars", "id with spaces"} {
		meta := jobMetadata{JobID: jobID, Kind: taskKind, ContractKind: contractKindShape}
		if err := saveJobMetadata(stateDir, meta); err != nil {
			t.Fatalf("save %q: %v", jobID, err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, "jobs", encodedStateFilename(jobID))); err != nil {
			t.Fatalf("encoded metadata for %q: %v", jobID, err)
		}
		if strings.Contains(jobID, "/") {
			if _, err := os.Stat(filepath.Join(stateDir, "jobs", jobID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw filename path for %q exists or stat err=%v", jobID, err)
			}
		}
		loaded, found, err := loadJobMetadata(stateDir, jobID)
		if err != nil || !found || loaded.JobID != jobID {
			t.Fatalf("load %q = %#v found=%v err=%v", jobID, loaded, found, err)
		}
		if err := deleteJobMetadata(stateDir, jobID); err != nil {
			t.Fatalf("delete %q: %v", jobID, err)
		}
		if _, found, err := loadJobMetadata(stateDir, jobID); err != nil || found {
			t.Fatalf("after delete %q found=%v err=%v", jobID, found, err)
		}
	}

	jobID := "opaque/mismatch"
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(jobMetadata{JobID: "different", Kind: taskKind, ContractKind: contractKindShape})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadJobMetadata(stateDir, jobID); err == nil || !strings.Contains(err.Error(), `has job_id "different"`) {
		t.Fatalf("load mismatch err=%v, want in-file job_id verification", err)
	}
}

func TestOpaqueJobIDsWorkForStatusAndResultCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	jobID := "opaque/status result:1"
	if err := saveJobMetadata("", jobMetadata{JobID: jobID, Kind: taskKind, ContractKind: contractKindShape}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{
			JobID: jobID,
			State: engine.StateCompleted,
		}}},
		result: client.JobResult{
			JobID:  jobID,
			State:  engine.StateCompleted,
			Result: &engine.ResultInfo{SHA256: strings.Repeat("c", 64)},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var statusOut, statusErr bytes.Buffer
	if code := run([]string{"status", "--job", jobID, "--json"}, nil, &statusOut, &statusErr); code != 0 {
		t.Fatalf("status code=%d stderr=%q", code, statusErr.String())
	}
	var resultOut, resultErr bytes.Buffer
	if code := run([]string{"result", "--job", jobID, "--json"}, nil, &resultOut, &resultErr); code != 0 {
		t.Fatalf("result code=%d stderr=%q", code, resultErr.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(resultOut.Bytes()), &env); err != nil {
		t.Fatalf("result JSON: %v; raw=%q", err, resultOut.String())
	}
	if env.JobID != jobID {
		t.Fatalf("result envelope job=%q, want %q", env.JobID, jobID)
	}
}

func TestRequestAndWorkspaceIdentities(t *testing.T) {
	requestID, err := newRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRequestID(requestID); err != nil {
		t.Fatalf("generated request id invalid: %v", err)
	}
	if len(strings.TrimPrefix(requestID, "delegate-")) < 32 {
		t.Fatalf("request id %q has fewer than 128 random bits", requestID)
	}
	workspace := t.TempDir()
	first, err := workspaceKeyForLogicalWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspaceKeyForLogicalWorkspace(filepath.Join(workspace, "."))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, workspaceKeyPrefix) || len(strings.TrimPrefix(first, workspaceKeyPrefix)) != 64 {
		t.Fatalf("workspace keys first=%q second=%q, want stable domain-separated sha256", first, second)
	}

	repo := newCommandGitFixture(t)
	writeCommandFixture(t, repo, "visible.go", "package visible\n// change\n")
	fake := &fakeAgentbusClient{hello: helloWithCapabilities(), result: client.JobResult{JobID: "opaque-review", State: engine.StateQueued}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	wantReviewKey, err := workspaceKeyForLogicalWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	if fake.submits[0].WorkspaceKey != wantReviewKey {
		t.Fatalf("review workspace key=%q, want original repo key %q", fake.submits[0].WorkspaceKey, wantReviewKey)
	}
	if tempKey, err := workspaceKeyForLogicalWorkspace(fake.submits[0].TaskSpec.CWD); err == nil && fake.submits[0].WorkspaceKey == tempKey {
		t.Fatalf("review workspace key used temp review cwd %q", fake.submits[0].TaskSpec.CWD)
	}
}

func TestSubmissionIntentExactParamsRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	policyValue, err := policy.ResolveTurnPolicy(policy.Flags{StrictContract: true})
	if err != nil {
		t.Fatal(err)
	}
	withPresent := testSubmissionIntent(testSubmitParams(t, "delegate-33333333333333333333333333333333", "present prompt", &zero), "/tmp/agentbus-present")
	withPresent.Params.TaskSpec.Policy = policyValue
	withPresent.Params.TaskSpec.Tags = map[string]string{"delegate.kind": taskKind, delegateRequestIDTag: withPresent.RequestID}
	if err := saveSubmissionIntent(stateDir, withPresent); err != nil {
		t.Fatal(err)
	}
	loadedPresent, found, err := loadSubmissionIntent(stateDir, withPresent.RequestID)
	if err != nil || !found {
		t.Fatalf("load present found=%v err=%v", found, err)
	}
	if loadedPresent.Params.TaskSpec.TimeoutMs == nil || *loadedPresent.Params.TaskSpec.TimeoutMs != 0 || loadedPresent.Params.TaskSpec.Policy == nil || !reflect.DeepEqual(loadedPresent.Params.TaskSpec.Tags, withPresent.Params.TaskSpec.Tags) {
		t.Fatalf("present params after round trip=%#v", loadedPresent.Params.TaskSpec)
	}

	withNil := testSubmissionIntent(testSubmitParams(t, "delegate-44444444444444444444444444444444", "nil prompt", nil), "/tmp/agentbus-nil")
	withNil.Params.TaskSpec.Tags = nil
	if err := saveSubmissionIntent(stateDir, withNil); err != nil {
		t.Fatal(err)
	}
	loadedNil, found, err := loadSubmissionIntent(stateDir, withNil.RequestID)
	if err != nil || !found {
		t.Fatalf("load nil found=%v err=%v", found, err)
	}
	if loadedNil.Params.TaskSpec.TimeoutMs != nil || loadedNil.Params.TaskSpec.Policy != nil || loadedNil.Params.TaskSpec.Tags != nil {
		t.Fatalf("nil params after round trip=%#v", loadedNil.Params.TaskSpec)
	}
}

func TestTimeoutFiveWayAndReviewCommands(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantNil     bool
		wantTimeout int64
		wantCode0   bool
		wantErr     string
	}{
		{name: "omitted", args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout"}, wantNil: true, wantCode0: true},
		{name: "explicit_zero", args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "0"}, wantTimeout: 0, wantCode0: true},
		{name: "positive", args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "1500ms"}, wantTimeout: 1500, wantCode0: true},
		{name: "negative", args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "-1s"}, wantErr: "--timeout must be non-negative"},
		{name: "oversized", args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "4h1ns"}, wantErr: "--timeout must be at most 4h0m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			var stdout, stderr bytes.Buffer
			code := run(tc.args, nil, &stdout, &stderr)
			if tc.wantCode0 {
				if code != 0 {
					t.Fatalf("code=%d stderr=%q", code, stderr.String())
				}
				if len(fake.submits) != 1 {
					t.Fatalf("submits=%d, want 1", len(fake.submits))
				}
				got := fake.submits[0].TaskSpec.TimeoutMs
				if tc.wantNil {
					if got != nil {
						t.Fatalf("timeout=%v, want nil", *got)
					}
				} else if got == nil || *got != tc.wantTimeout {
					t.Fatalf("timeout=%v, want %d", got, tc.wantTimeout)
				}
				return
			}
			if code == 0 || !strings.Contains(stderr.String(), tc.wantErr) || len(fake.submits) != 0 {
				t.Fatalf("code=%d stderr=%q submits=%d, want local usage error %q and no submit", code, stderr.String(), len(fake.submits), tc.wantErr)
			}
		})
	}

	for _, command := range []string{"review", "adversarial-review"} {
		t.Run(command+"_explicit_zero", func(t *testing.T) {
			repo := newCommandGitFixture(t)
			writeCommandFixture(t, repo, "visible.go", "package visible\n// change\n")
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			var stdout, stderr bytes.Buffer
			code := run([]string{command, "--backend", "codex", "--cwd", repo, "--scope", "working-tree", "--timeout", "0", "--background", "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s code=%d stderr=%q", command, code, stderr.String())
			}
			if len(fake.submits) != 1 || fake.submits[0].TaskSpec.TimeoutMs == nil || *fake.submits[0].TaskSpec.TimeoutMs != 0 {
				t.Fatalf("%s timeout submit=%#v", command, fake.submits)
			}
		})
	}
}

func TestRecoverRequestRejectsShapingFlagsAndMissingIntent(t *testing.T) {
	for _, args := range [][]string{
		{"task", "--recover-request", "delegate-55555555555555555555555555555555", "--backend", "codex"},
		{"task", "--recover-request", "delegate-55555555555555555555555555555555", "--prompt", "no"},
		{"task", "--recover-request", "delegate-55555555555555555555555555555555", "--timeout", "0"},
		{"task", "--recover-request", "delegate-55555555555555555555555555555555", "--write"},
		{"task", "--recover-request", "delegate-55555555555555555555555555555555", "positional"},
	} {
		t.Run(strings.Join(args[2:], "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, nil, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), "--recover-request") {
				t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
			}
		})
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	requestID := "delegate-66666666666666666666666666666666"
	code := run([]string{"task", "--recover-request", requestID, "--json"}, nil, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "delegate task --recover-request "+requestID) {
		t.Fatalf("missing intent code=%d stderr=%q", code, stderr.String())
	}
}

func TestRecoverRequestUsesRecordedRootAndPersistedParams(t *testing.T) {
	tmp := t.TempDir()
	currentRoot := filepath.Join(tmp, "current-agentbus")
	recordedRoot := filepath.Join(tmp, "recorded-agentbus")
	for _, root := range []string{currentRoot, recordedRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENTBUS_STATE_ROOT", currentRoot)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	timeout := int64(0)
	params := testSubmitParams(t, "delegate-77777777777777777777777777777777", "persisted params", &timeout)
	intent := testSubmissionIntent(params, recordedRoot)
	intent.Phase = submissionPhaseInFlight
	if err := saveSubmissionIntent(stateDir, intent); err != nil {
		t.Fatal(err)
	}

	fake := &fakeAgentbusClient{hello: helloWithCapabilities(), result: client.JobResult{JobID: "opaque/rooted", State: engine.StateQueued}}
	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	var roots []string
	connectAgentbus = func(_ context.Context, opts client.Options) (agentbusClient, error) {
		roots = append(roots, opts.StateRoot)
		return fake, nil
	}
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	commandOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("unexpected agentbus version call") }
	defer func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--recover-request", params.RequestID, "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover code=%d stderr=%q", code, stderr.String())
	}
	wantRoot, err := canonicalizeAgentbusStateRoot("recorded root", recordedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != wantRoot {
		t.Fatalf("connection roots=%#v, want recorded root %q", roots, wantRoot)
	}
	if len(fake.submits) != 1 || !reflect.DeepEqual(fake.submits[0], params) {
		t.Fatalf("submitted params=%#v, want persisted %#v", fake.submits, params)
	}
}

func testSubmitParams(t *testing.T, requestID, prompt string, timeout *int64) client.JobSubmitParams {
	t.Helper()
	workspace := t.TempDir()
	workspaceKey, err := workspaceKeyForLogicalWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return client.JobSubmitParams{
		WorkspaceKey: workspaceKey,
		RequestID:    requestID,
		TaskSpec: client.TaskSpec{
			Backend:   "codex",
			CWD:       workspace,
			Prompt:    prompt,
			Tags:      map[string]string{"delegate.kind": taskKind, delegateRequestIDTag: requestID},
			TimeoutMs: timeout,
		},
	}
}

func testSubmissionIntent(params client.JobSubmitParams, root string) submissionIntent {
	return submissionIntent{
		Schema:            submissionIntentSchema,
		RequestID:         params.RequestID,
		WorkspaceKey:      params.WorkspaceKey,
		AgentbusStateRoot: root,
		Params:            params,
		Kind:              taskKind,
		ContractKind:      contractKindShape,
		Phase:             submissionPhasePrepared,
		CreatedAt:         time.Unix(1, 0).UTC(),
		UpdatedAt:         time.Unix(1, 0).UTC(),
	}
}

func useRealAgentbusClient(t *testing.T) func() {
	t.Helper()
	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
		opts.DisableAutoStart = true
		return client.Connect(ctx, opts)
	}
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	commandOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("unexpected agentbus version call") }
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}
}

type replayAdmissionBus struct {
	mu                             sync.Mutex
	closeBeforeFirstSubmitResponse bool
	closedFirstSubmitResponse      bool
	submits                        []client.JobSubmitParams
	accepted                       map[string]storedProtocolSubmit
	executions                     int
	nextJob                        int
}

func newReplayAdmissionBus() *replayAdmissionBus {
	return &replayAdmissionBus{accepted: map[string]storedProtocolSubmit{}}
}

func stubReplayAdmissionBus(t *testing.T, bus *replayAdmissionBus) func() {
	t.Helper()
	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		return &replayAdmissionClient{bus: bus, hello: helloWithCapabilities()}, nil
	}
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	commandOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("unexpected agentbus version call") }
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}
}

func (b *replayAdmissionBus) seedAccepted(params client.JobSubmitParams, jobID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accepted[params.RequestID] = storedProtocolSubmit{jobID: jobID, fingerprint: mustSubmitFingerprint(params)}
	b.executions++
}

func (b *replayAdmissionBus) submit(params client.JobSubmitParams) (client.JobSubmitResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submits = append(b.submits, params)
	fingerprint := mustSubmitFingerprint(params)
	var jobID string
	if existing, ok := b.accepted[params.RequestID]; ok {
		if existing.fingerprint != fingerprint {
			return client.JobSubmitResult{}, rpcAdmissionError(agentbusErrorBackendUnavailable, "replay conflict", admissionCauseReplayConflict, existing.jobID)
		}
		jobID = existing.jobID
	} else {
		b.nextJob++
		jobID = fmt.Sprintf("opaque/job:%d", b.nextJob)
		b.accepted[params.RequestID] = storedProtocolSubmit{jobID: jobID, fingerprint: fingerprint}
		b.executions++
	}
	if b.closeBeforeFirstSubmitResponse && !b.closedFirstSubmitResponse {
		b.closedFirstSubmitResponse = true
		return client.JobSubmitResult{}, errors.New("lost response after accept")
	}
	return client.JobSubmitResult{JobID: jobID, State: engine.StateQueued}, nil
}

func (b *replayAdmissionBus) submittedParams() []client.JobSubmitParams {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]client.JobSubmitParams(nil), b.submits...)
}

func (b *replayAdmissionBus) backendExecutions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.executions
}

type replayAdmissionClient struct {
	bus   *replayAdmissionBus
	hello client.HelloResult
}

func (c *replayAdmissionClient) Close() error { return nil }

func (c *replayAdmissionClient) HelloResult() client.HelloResult { return c.hello }

func (c *replayAdmissionClient) JobSubmit(_ context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	return c.bus.submit(params)
}

func (c *replayAdmissionClient) JobStatus(context.Context, client.JobStatusParams) (client.JobStatusResult, error) {
	return client.JobStatusResult{}, errors.New("unexpected JobStatus")
}

func (c *replayAdmissionClient) JobResult(context.Context, client.JobResultParams) (client.JobResult, error) {
	return client.JobResult{}, errors.New("unexpected JobResult")
}

func (c *replayAdmissionClient) JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error) {
	return client.JobCancelResult{}, errors.New("unexpected JobCancel")
}

func rpcAdmissionError(stableCode, message, admissionCause, jobID string) error {
	data := map[string]any{"code": stableCode}
	if admissionCause != "" {
		data["admissionCause"] = admissionCause
	}
	if jobID != "" {
		data["jobId"] = jobID
	}
	raw, err := json.Marshal(map[string]any{
		"Object": map[string]any{
			"code":    -32000,
			"message": message,
			"data":    data,
		},
	})
	if err != nil {
		return err
	}
	var rpcErr client.RPCError
	if err := json.Unmarshal(raw, &rpcErr); err != nil {
		return err
	}
	return &rpcErr
}

func mustSubmitFingerprint(params client.JobSubmitParams) string {
	raw, err := json.Marshal(params.TaskSpec)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

type protocolV2FakeServer struct {
	t                              *testing.T
	root                           string
	ln                             net.Listener
	closeBeforeFirstSubmitResponse bool
	mu                             sync.Mutex
	closedFirstSubmitResponse      bool
	submits                        []client.JobSubmitParams
	accepted                       map[string]storedProtocolSubmit
	executions                     int
	nextJob                        int
}

type storedProtocolSubmit struct {
	jobID       string
	fingerprint string
}

func startProtocolV2FakeServer(t *testing.T) *protocolV2FakeServer {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ab-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
	})
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "agentbus.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("sandbox disallows unix socket bind for protocol-v2 fake server: %v", err)
		}
		t.Fatal(err)
	}
	server := &protocolV2FakeServer{t: t, root: root, ln: ln, accepted: map[string]storedProtocolSubmit{}}
	go server.accept()
	t.Cleanup(func() {
		_ = ln.Close()
	})
	return server
}

func (s *protocolV2FakeServer) seedAccepted(params client.JobSubmitParams, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted[params.RequestID] = storedProtocolSubmit{jobID: jobID, fingerprint: submitFingerprint(s.t, params)}
	s.executions++
}

func (s *protocolV2FakeServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *protocolV2FakeServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
			return
		}
		switch req.Method {
		case "protocol.hello":
			s.writeResult(conn, req.ID, map[string]any{
				"protocolVersion": 2,
				"backends":        []string{"codex"},
				"capabilities":    helloWithCapabilities().Capabilities,
			})
		case "job.submit":
			var params client.JobSubmitParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				s.writeError(conn, req.ID, "invalid_task_spec", "invalid submit params", "")
				continue
			}
			jobID, conflict, closeWithoutResponse := s.acceptSubmit(params)
			if closeWithoutResponse {
				return
			}
			if conflict {
				s.writeError(conn, req.ID, agentbusErrorBackendUnavailable, "replay conflict", admissionCauseReplayConflict)
				continue
			}
			s.writeResult(conn, req.ID, map[string]any{"jobId": jobID, "state": string(engine.StateQueued)})
		default:
			s.writeError(conn, req.ID, "method_not_found", "method not found", "")
		}
	}
}

func (s *protocolV2FakeServer) acceptSubmit(params client.JobSubmitParams) (jobID string, conflict bool, closeWithoutResponse bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits = append(s.submits, params)
	fingerprint := submitFingerprint(s.t, params)
	if existing, ok := s.accepted[params.RequestID]; ok {
		if existing.fingerprint != fingerprint {
			return existing.jobID, true, false
		}
		jobID = existing.jobID
	} else {
		s.nextJob++
		jobID = fmt.Sprintf("opaque/job:%d", s.nextJob)
		s.accepted[params.RequestID] = storedProtocolSubmit{jobID: jobID, fingerprint: fingerprint}
		s.executions++
	}
	if s.closeBeforeFirstSubmitResponse && !s.closedFirstSubmitResponse {
		s.closedFirstSubmitResponse = true
		return jobID, false, true
	}
	return jobID, false, false
}

func (s *protocolV2FakeServer) writeResult(conn net.Conn, id json.RawMessage, result any) {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	if err != nil {
		s.t.Errorf("marshal result: %v", err)
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

func (s *protocolV2FakeServer) writeError(conn net.Conn, id json.RawMessage, code, message, admissionCause string) {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    -32000,
			"message": message,
			"data": map[string]any{
				"code":           code,
				"admissionCause": admissionCause,
			},
		},
	})
	if err != nil {
		s.t.Errorf("marshal error: %v", err)
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

func (s *protocolV2FakeServer) submittedParams() []client.JobSubmitParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.JobSubmitParams(nil), s.submits...)
}

func (s *protocolV2FakeServer) backendExecutions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executions
}

func submitFingerprint(t *testing.T, params client.JobSubmitParams) string {
	t.Helper()
	raw, err := json.Marshal(params.TaskSpec)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
