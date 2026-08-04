package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
)

func TestSetupStatePreflightRejectsRelativeAgentbusStateRoot(t *testing.T) {
	workspace := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	t.Setenv("XDG_STATE_HOME", "relative-state")

	result := setupStatePreflight()
	if result.AgentbusStateRootWritable {
		t.Fatal("agentbusStateRootWritable = true, want false for relative XDG_STATE_HOME")
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], `XDG_STATE_HOME "relative-state" must be absolute`) {
		t.Fatalf("warnings = %#v, want relative XDG_STATE_HOME warning", result.Warnings)
	}
	if _, err := os.Stat(filepath.Join(workspace, "relative-state")); !os.IsNotExist(err) {
		t.Fatalf("relative Agentbus state path was created: %v", err)
	}
}

func TestSetupReportsPendingSubmissionAndUnresolvedCleanupCounts(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseBlocked} {
		requestID := "delegate-" + strings.Repeat(string(phase[0]), 32)
		intent := testSubmissionIntent(testSubmitParams(t, requestID, phase+" prompt", nil), t.TempDir())
		intent.Phase = phase
		if err := saveSubmissionIntent(stateDir, intent); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{submissionPhaseAcknowledged, submissionPhaseRejected} {
		requestID := "delegate-" + strings.Repeat(string(phase[0]), 32)
		intent := testSubmissionIntent(testSubmitParams(t, requestID, phase+" prompt", nil), t.TempDir())
		intent.Phase = phase
		if err := saveSubmissionIntent(stateDir, intent); err != nil {
			t.Fatal(err)
		}
	}

	jobID := "job_setup_unresolved"
	input, err := handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: stateDir,
		JobID:    jobID,
		Prompt:   handoff.ResolvedPrompt{Prompt: "retained", Source: handoff.SourcePrompt},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(stateDir, "review-")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:              jobID,
		Kind:               reviewKind,
		ContractKind:       contractKindShape,
		JobInputPath:       input.Path,
		ReviewWorkspace:    workspace,
		State:              engine.StateCompleted,
		CleanupDisposition: cleanupDispositionUnresolved,
		CreatedAt:          time.Unix(1, 0).UTC(),
		UpdatedAt:          time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:              "job_setup_safe",
		Kind:               taskKind,
		ContractKind:       contractKindShape,
		State:              engine.StateCompleted,
		CleanupDisposition: cleanupDispositionVerifiedAbsent,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code=%d stderr=%q", code, stderr.String())
	}
	var result setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if result.PendingSubmissionIntentCount == nil || *result.PendingSubmissionIntentCount != 3 {
		t.Fatalf("pendingSubmissionIntentCount=%v, want 3", result.PendingSubmissionIntentCount)
	}
	if result.UnresolvedCleanupArtifactCount == nil || *result.UnresolvedCleanupArtifactCount != 2 {
		t.Fatalf("unresolvedCleanupArtifactCount=%v, want 2", result.UnresolvedCleanupArtifactCount)
	}
}

func TestSetupReadyRequiresWritableAgentbusStateRoot(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("HOME", t.TempDir())
	stateRoot := filepath.Join(t.TempDir(), "agentbus-state-file")
	if err := os.WriteFile(stateRoot, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", stateRoot)

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("setup succeeded, want nonzero exit for non-writable Agentbus state root")
	}
	var result setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if !result.Agentbus.CapabilitiesOK || !result.AdmissionStrictContainment {
		t.Fatalf("setup capabilities=%#v strict=%t, want compliant daemon", result.Agentbus, result.AdmissionStrictContainment)
	}
	if result.Ready || result.AgentbusStateRootWritable {
		t.Fatalf("setup ready=%t agentbusStateRootWritable=%t, want both false", result.Ready, result.AgentbusStateRootWritable)
	}
	if !result.AgentbusAutostartLockRootWritable {
		t.Fatalf("agentbusAutostartLockRootWritable=false, want only state root check to fail: %#v", result)
	}
	if !strings.Contains(stderr.String(), "agentbus state root is not writable") || !strings.Contains(stderr.String(), stateRoot) {
		t.Fatalf("stderr=%q, want failed Agentbus state root check and path %q", stderr.String(), stateRoot)
	}
}

func TestSetupReadyRequiresWritableDelegateStateRoot(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("HOME", t.TempDir())
	agentbusRoot := filepath.Join(t.TempDir(), "agentbus")
	if err := os.MkdirAll(agentbusRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", agentbusRoot)
	xdgState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdgState)
	delegateRoot := filepath.Join(xdgState, "delegate")
	if err := os.MkdirAll(delegateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(delegateRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(delegateRoot, 0o700)
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("setup succeeded, want nonzero exit for non-writable Delegate state root")
	}
	var result setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if !result.Agentbus.CapabilitiesOK || !result.AdmissionStrictContainment {
		t.Fatalf("setup capabilities=%#v strict=%t, want compliant daemon", result.Agentbus, result.AdmissionStrictContainment)
	}
	if result.Ready || result.StateRootWritable {
		t.Fatalf("setup ready=%t stateRootWritable=%t, want both false", result.Ready, result.StateRootWritable)
	}
	if !result.AgentbusStateRootWritable || !result.AgentbusAutostartLockRootWritable {
		t.Fatalf("agentbus writable fields = state:%t lock:%t, want only delegate state check to fail", result.AgentbusStateRootWritable, result.AgentbusAutostartLockRootWritable)
	}
	if !strings.Contains(stderr.String(), "delegate state root is not usable") || !strings.Contains(stderr.String(), delegateRoot) {
		t.Fatalf("stderr=%q, want failed Delegate state root check and path %q", stderr.String(), delegateRoot)
	}
	// The precise cause (mode mismatch) and its remedy must surface instead of a
	// generic "not writable" — a mode fix is chmod, not a permissions grant.
	if !strings.Contains(stderr.String(), "want 700") || !strings.Contains(stderr.String(), "chmod 700 ") {
		t.Fatalf("stderr=%q, want precise mode reason and chmod remedy", stderr.String())
	}
}
