package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
)

// setupJSONBeforeBackendFilter captures the established JSON layout using raw
// Agentbus JSON so the default-output test can compare the full line without
// changing Agentbus' handshake-only custom marshaling behavior.
type setupJSONBeforeBackendFilter struct {
	Schema                               int                            `json:"schema"`
	Delegate                             string                         `json:"delegate"`
	Agentbus                             json.RawMessage                `json:"agentbus"`
	Config                               setupConfigBeforeBackendFilter `json:"config"`
	Skills                               []setupSkill                   `json:"skills"`
	StateRootWritable                    bool                           `json:"stateRootWritable"`
	StateRootWritability                 setupWritability               `json:"stateRootWritability,omitempty"`
	AgentbusStateRoot                    string                         `json:"agentbusStateRoot"`
	AgentbusStateRootWritable            bool                           `json:"agentbusStateRootWritable"`
	AgentbusStateRootWritability         setupWritability               `json:"agentbusStateRootWritability,omitempty"`
	AgentbusAutostartLockRoot            string                         `json:"agentbusAutostartLockRoot"`
	AgentbusAutostartLockRootWritable    bool                           `json:"agentbusAutostartLockRootWritable"`
	AgentbusAutostartLockRootWritability setupWritability               `json:"agentbusAutostartLockRootWritability,omitempty"`
	AdmissionStrictContainment           bool                           `json:"admissionStrictContainment"`
	PendingSubmissionIntentCount         *int                           `json:"pendingSubmissionIntentCount"`
	PendingSubmissionIntents             []setupPendingSubmissionIntent `json:"pendingSubmissionIntents"`
	UnresolvedCleanupArtifactCount       *int                           `json:"unresolvedCleanupArtifactCount"`
	DaemonReachable                      bool                           `json:"daemonReachable"`
	Ready                                bool                           `json:"ready"`
	Warnings                             []string                       `json:"warnings,omitempty"`
}

type setupConfigBeforeBackendFilter struct {
	Path        string                  `json:"path"`
	Overridable bool                    `json:"overridable"`
	Defaults    delegateconfig.Backends `json:"defaults"`
}

func TestSetupStatePreflightRejectsRelativeAgentbusStateRoot(t *testing.T) {
	setupTestPreflightEnvironment(t)
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

func TestSetupPendingSubmissionIntentsDoesNotCreateMissingStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "delegate")

	count, intents, warnings := setupPendingSubmissionIntents(stateDir)
	if count == nil || *count != 0 || intents == nil || len(intents) != 0 || len(warnings) != 0 {
		t.Fatalf("missing state pending output = count:%v intents:%#v warnings:%#v, want zero, empty array, and no warnings", count, intents, warnings)
	}
	if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent enumeration created state path %q: %v", stateDir, err)
	}
}

func TestSetupExistingDirectoryWritabilityProbesAndCleansUp(t *testing.T) {
	dir := t.TempDir()

	result := setupExistingDirectoryWritability(dir)
	if result.Status != setupWritabilityWritable || result.Reason != "" {
		t.Fatalf("existing directory writability = %#v, want writable", result)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("effective writability probe left files behind: %#v", entries)
	}
}

func TestSetupPreflightEffectivelyProbesExistingRoots(t *testing.T) {
	setupTestPreflightEnvironment(t)
	paths := setupTestPreflightPaths(t)
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	result := setupStatePreflight()
	for _, check := range []struct {
		name   string
		status setupWritability
	}{
		{"delegate state root", result.StateRootWritability},
		{"agentbus state root", result.AgentbusStateRootWritability},
		{"agentbus autostart lock root", result.AgentbusAutostartLockRootWritability},
	} {
		if check.status != setupWritabilityWritable {
			t.Fatalf("%s writability=%q, want writable", check.name, check.status)
		}
	}
	for _, path := range paths {
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("preflight probe left files in %q: %#v", path, entries)
		}
	}
}

func TestSetupPreflightReportsMissingRootsAsUnknownWithoutCreatingThem(t *testing.T) {
	setupTestPreflightEnvironment(t)

	result := setupStatePreflight()
	for _, check := range []struct {
		name   string
		path   string
		status setupWritability
	}{
		{"delegate state root", result.DelegateStateRoot, result.StateRootWritability},
		{"agentbus state root", result.AgentbusStateRoot, result.AgentbusStateRootWritability},
		{"agentbus autostart lock root", result.AgentbusAutostartLockRoot, result.AgentbusAutostartLockRootWritability},
	} {
		if check.status != setupWritabilityUnknown {
			t.Fatalf("%s writability=%q, want unknown for missing path", check.name, check.status)
		}
		if _, err := os.Lstat(check.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight created %s %q: %v", check.name, check.path, err)
		}
	}
}

func TestSetupMissingDirectoryWritabilityIsUnknownAndDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")

	result := setupExistingDirectoryWritability(path)
	if result.Status != setupWritabilityUnknown || !strings.Contains(result.Reason, "may be creatable") {
		t.Fatalf("missing directory writability = %#v, want potentially-creatable unknown", result)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writability probe created missing path %q: %v", path, err)
	}
}

func TestSetupBackendFilterRestrictsDetailsAndPreservesReadiness(t *testing.T) {
	hello := helloWithCapabilities()
	hello.Backends = []string{"codex", "claude", "cursor"}
	hello.BackendMetadata = []client.BackendInfo{
		{Name: "codex", Models: []string{"gpt-5.6"}, Efforts: []string{"high"}},
		{Name: "claude", Models: []string{"claude-opus"}, Efforts: []string{"medium"}},
		{Name: "cursor", Models: []string{"cursor-1", "cursor-2"}, Efforts: []string{"low", "high"}},
	}
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: hello})
	defer restore()
	setupTestPreflightDirectories(t)
	if err := delegateconfig.Save(delegateconfig.Config{
		Overridable: true,
		Backend:     delegateconfig.Backends{Codex: delegateconfig.Defaults{Model: "codex-default", Effort: "high"}},
	}); err != nil {
		t.Fatal(err)
	}

	var unfilteredStdout, unfilteredStderr bytes.Buffer
	if code := run([]string{"setup", "--json"}, nil, &unfilteredStdout, &unfilteredStderr); code != 0 {
		t.Fatalf("unfiltered setup code=%d stderr=%q", code, unfilteredStderr.String())
	}
	var unfiltered setupJSON
	if err := json.Unmarshal(unfilteredStdout.Bytes(), &unfiltered); err != nil {
		t.Fatalf("unfiltered setup JSON invalid: %v; raw=%q", err, unfilteredStdout.String())
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json", "--backend", "codex"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code=%d stderr=%q", code, stderr.String())
	}
	var result setupJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if got, want := result.Agentbus.Backends, []string{"codex"}; !slices.Equal(got, want) {
		t.Fatalf("filtered backends=%#v, want %#v", got, want)
	}
	if got, want := result.Agentbus.BackendMetadata, []client.BackendInfo{hello.BackendMetadata[0]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered backend metadata=%#v, want %#v", got, want)
	}
	var raw struct {
		Config struct {
			Defaults map[string]json.RawMessage `json:"defaults"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := len(raw.Config.Defaults), 1; got != want || raw.Config.Defaults["codex"] == nil {
		t.Fatalf("filtered config defaults=%#v, want only codex", raw.Config.Defaults)
	}
	if result.Schema != unfiltered.Schema || result.Delegate != unfiltered.Delegate || !reflect.DeepEqual(result.Agentbus.Capabilities, unfiltered.Agentbus.Capabilities) || !slices.Equal(result.Agentbus.Required, unfiltered.Agentbus.Required) || !slices.Equal(result.Agentbus.Missing, unfiltered.Agentbus.Missing) || result.Agentbus.CapabilitiesOK != unfiltered.Agentbus.CapabilitiesOK || result.Config.Path != unfiltered.Config.Path || result.Config.Overridable != unfiltered.Config.Overridable || !reflect.DeepEqual(result.Skills, unfiltered.Skills) || result.StateRootWritable != unfiltered.StateRootWritable || result.StateRootWritability != unfiltered.StateRootWritability || result.AgentbusStateRoot != unfiltered.AgentbusStateRoot || result.AgentbusStateRootWritable != unfiltered.AgentbusStateRootWritable || result.AgentbusStateRootWritability != unfiltered.AgentbusStateRootWritability || result.AgentbusAutostartLockRoot != unfiltered.AgentbusAutostartLockRoot || result.AgentbusAutostartLockRootWritable != unfiltered.AgentbusAutostartLockRootWritable || result.AgentbusAutostartLockRootWritability != unfiltered.AgentbusAutostartLockRootWritability || result.AdmissionStrictContainment != unfiltered.AdmissionStrictContainment || !reflect.DeepEqual(result.PendingSubmissionIntentCount, unfiltered.PendingSubmissionIntentCount) || !reflect.DeepEqual(result.PendingSubmissionIntents, unfiltered.PendingSubmissionIntents) || !reflect.DeepEqual(result.UnresolvedCleanupArtifactCount, unfiltered.UnresolvedCleanupArtifactCount) || result.DaemonReachable != unfiltered.DaemonReachable || result.Ready != unfiltered.Ready || !slices.Equal(result.Warnings, unfiltered.Warnings) {
		t.Fatalf("backend filter changed global setup readiness: filtered=%#v unfiltered=%#v", result, unfiltered)
	}
}

func TestSetupBackendFilterRejectsUnavailableBackend(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json", "--backend", "missing"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("setup code=%d, want failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("setup emitted success-looking JSON for unavailable backend: %q", stdout.String())
	}
	if got, want := stderr.String(), `backend "missing" is not available; agentbus reports: codex, claude`; !strings.Contains(got, want) {
		t.Fatalf("stderr=%q, want %q", got, want)
	}
}

func TestSetupWithoutBackendIncludesAllBackendDetails(t *testing.T) {
	hello := helloWithCapabilities()
	hello.Backends = []string{"codex", "claude", "cursor"}
	hello.BackendMetadata = []client.BackendInfo{
		{Name: "codex", Models: []string{"gpt-5.6"}, Efforts: []string{"high"}},
		{Name: "claude", Models: []string{"claude-opus"}, Efforts: []string{"medium"}},
		{Name: "cursor", Models: []string{"cursor-1", "cursor-2"}, Efforts: []string{"low", "high"}},
	}
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: hello})
	defer restore()
	setupTestPreflightDirectories(t)
	defaults := delegateconfig.Backends{
		Claude: delegateconfig.Defaults{Model: "claude-default", Effort: "medium"},
		Codex:  delegateconfig.Defaults{Model: "codex-default", Effort: "high"},
		Cursor: delegateconfig.Defaults{Model: "cursor-default", Effort: "low"},
	}
	if err := delegateconfig.Save(delegateconfig.Config{Overridable: true, Backend: defaults}); err != nil {
		t.Fatal(err)
	}
	gotConfig, err := json.Marshal(setupConfig{Path: "/tmp/config.toml", Overridable: true, Defaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	wantConfig, err := json.Marshal(struct {
		Path        string                  `json:"path"`
		Overridable bool                    `json:"overridable"`
		Defaults    delegateconfig.Backends `json:"defaults"`
	}{Path: "/tmp/config.toml", Overridable: true, Defaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConfig, wantConfig) {
		t.Fatalf("unfiltered config JSON changed: got=%s want=%s", gotConfig, wantConfig)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code=%d stderr=%q", code, stderr.String())
	}
	var result setupJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if !slices.Equal(result.Agentbus.Backends, hello.Backends) || !reflect.DeepEqual(result.Agentbus.BackendMetadata, hello.BackendMetadata) {
		t.Fatalf("unfiltered backend detail changed: backends=%#v metadata=%#v", result.Agentbus.Backends, result.Agentbus.BackendMetadata)
	}
	var raw struct {
		Config struct {
			Defaults json.RawMessage `json:"defaults"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var reportedDefaults delegateconfig.Backends
	if err := json.Unmarshal(raw.Config.Defaults, &reportedDefaults); err != nil {
		t.Fatalf("unfiltered config defaults changed shape: %v; raw=%s", err, raw.Config.Defaults)
	}
	if !reflect.DeepEqual(reportedDefaults, defaults) {
		t.Fatalf("unfiltered config defaults=%#v, want established full backend object %#v", reportedDefaults, defaults)
	}
	var before setupJSONBeforeBackendFilter
	if err := json.Unmarshal(stdout.Bytes(), &before); err != nil {
		t.Fatalf("unfiltered setup JSON invalid for legacy layout: %v", err)
	}
	wantOutput, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.TrimSpace(stdout.Bytes()); !bytes.Equal(got, wantOutput) {
		t.Fatalf("unfiltered setup JSON changed:\n got: %s\nwant: %s", got, wantOutput)
	}
}

func TestSetupJSONReportsPendingSubmissionIntentsAndCounts(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setupTestPreflightDirectories(t)
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for index, phase := range []string{submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseBlocked} {
		requestID := "delegate-" + strings.Repeat(string(phase[0]), 32)
		intent := testSubmissionIntent(testSubmitParams(t, requestID, phase+" prompt", nil), t.TempDir())
		intent.Phase = phase
		intent.CreatedAt = time.Date(2026, time.January, 1, 0, index, 0, 0, time.UTC)
		intent.Params.TaskSpec.Backend = strings.Repeat(string(phase[0]), setupPendingSubmissionIntentLabelLimit+1)
		intent.Origin = &envelopeOrigin{
			Skill:           "delegate:" + strings.Repeat(string(phase[0]), setupPendingSubmissionIntentLabelLimit+1),
			ParentClient:    strings.Repeat("client-", 40),
			ParentSessionID: "private-session-id",
			ParentAgent:     "private-parent-agent",
			Depth:           "private-depth-99",
		}
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
	if got, want := len(result.PendingSubmissionIntents), 3; got != want {
		t.Fatalf("pendingSubmissionIntents length=%d, want %d: %#v", got, want, result.PendingSubmissionIntents)
	}
	for index, want := range []struct {
		requestID string
		phase     string
	}{
		{"delegate-" + strings.Repeat("p", 32), submissionPhasePrepared},
		{"delegate-" + strings.Repeat("i", 32), submissionPhaseInFlight},
		{"delegate-" + strings.Repeat("b", 32), submissionPhaseBlocked},
	} {
		got := result.PendingSubmissionIntents[index]
		wantLabel := strings.Repeat(string(want.phase[0]), setupPendingSubmissionIntentLabelLimit)
		if got.RequestID != want.requestID || got.Phase != want.phase || got.Backend != wantLabel || got.Origin == nil || got.Origin.Skill != "delegate:"+wantLabel[:setupPendingSubmissionIntentLabelLimit-len("delegate:")] {
			t.Fatalf("pendingSubmissionIntents[%d]=%#v, want request %q with durable context", index, got, want.requestID)
		}
		if strings.Contains(stdout.String(), "parent_session_id") || strings.Contains(stdout.String(), "parent_agent") || strings.Contains(stdout.String(), "private-session-id") || strings.Contains(stdout.String(), "private-parent-agent") || strings.Contains(stdout.String(), "client-") || strings.Contains(stdout.String(), "private-depth") {
			t.Fatalf("setup exposed unapproved origin fields: %s", stdout.String())
		}
		if got.CreatedAt.IsZero() {
			t.Fatalf("pendingSubmissionIntents[%d].createdAt is zero", index)
		}
	}
	if result.UnresolvedCleanupArtifactCount == nil || *result.UnresolvedCleanupArtifactCount != 2 {
		t.Fatalf("unresolvedCleanupArtifactCount=%v, want 2", result.UnresolvedCleanupArtifactCount)
	}
}

func TestSetupPendingSubmissionIntentsCap(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setupTestPreflightDirectories(t)
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < setupPendingSubmissionIntentLimit+1; index++ {
		requestID := fmt.Sprintf("delegate-%032d", index)
		intent := testSubmissionIntent(testSubmitParams(t, requestID, "prompt", nil), t.TempDir())
		intent.Phase = submissionPhasePrepared
		intent.CreatedAt = time.Date(2026, time.January, 1, 0, index, 0, 0, time.UTC)
		if err := saveSubmissionIntent(stateDir, intent); err != nil {
			t.Fatal(err)
		}
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
	if result.PendingSubmissionIntentCount == nil || *result.PendingSubmissionIntentCount != setupPendingSubmissionIntentLimit+1 {
		t.Fatalf("pendingSubmissionIntentCount=%v, want authoritative count %d", result.PendingSubmissionIntentCount, setupPendingSubmissionIntentLimit+1)
	}
	if got, want := len(result.PendingSubmissionIntents), setupPendingSubmissionIntentLimit; got != want {
		t.Fatalf("returned intent summaries=%d, want cap %d", got, want)
	}
	if result.PendingSubmissionIntents[0].RequestID != "delegate-00000000000000000000000000000000" || result.PendingSubmissionIntents[len(result.PendingSubmissionIntents)-1].RequestID != "delegate-00000000000000000000000000000019" {
		t.Fatalf("capped intents=%#v, want the %d oldest intents", result.PendingSubmissionIntents, setupPendingSubmissionIntentLimit)
	}
}

func TestSetupAgentbusVersionReadiness(t *testing.T) {
	belowFloorVersion := setupFixtureAgentbusVersionBelowFloor(t)
	atFloorVersion := minimumSupportedAgentbusVersion
	prereleaseOfFloorVersion := atFloorVersion + "-rc.1"
	aboveFloorPrereleaseVersion := strings.TrimPrefix(setupFixtureAgentbusVersionAboveFloor(t), "v") + "-rc.1+build.4"

	for _, tc := range []struct {
		name        string
		output      string
		wantReady   bool
		wantStatus  string
		wantWarning string
		wantRemedy  string
	}{
		{
			name:       "below_floor_with_working_client",
			output:     agentbusVersionFixtureOutput(belowFloorVersion),
			wantStatus: agentbusVersionStatusTooOld,
			wantRemedy: "agentbus " + belowFloorVersion + " is older than the minimum supported version " + minimumSupportedAgentbusVersion + "; run mise-en-place install agentbus to upgrade",
		},
		{
			name:       "same_core_prerelease_is_below_floor",
			output:     agentbusVersionFixtureOutput(prereleaseOfFloorVersion),
			wantStatus: agentbusVersionStatusTooOld,
			wantRemedy: "agentbus " + prereleaseOfFloorVersion + " is older than the minimum supported version " + minimumSupportedAgentbusVersion + "; run mise-en-place install agentbus to upgrade",
		},
		{
			name:       "at_floor",
			output:     agentbusVersionFixtureOutput(atFloorVersion),
			wantReady:  true,
			wantStatus: agentbusVersionStatusSupported,
		},
		{
			name:       "above_floor_multidigit_prerelease",
			output:     agentbusVersionFixtureOutput(aboveFloorPrereleaseVersion),
			wantReady:  true,
			wantStatus: agentbusVersionStatusSupported,
		},
		{
			name:        "unparseable",
			output:      "agentbus development-build\n",
			wantReady:   true,
			wantStatus:  agentbusVersionStatusUnknown,
			wantWarning: "agentbus version \"development-build\" could not be parsed",
		},
		{
			name:        "absent",
			wantReady:   true,
			wantStatus:  agentbusVersionStatusUnknown,
			wantWarning: "agentbus version could not be discovered",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
			defer restore()
			commandOutput = func(string, ...string) ([]byte, error) {
				return []byte(tc.output), nil
			}
			t.Setenv("HOME", t.TempDir())
			setupTestPreflightDirectories(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
			if got := code == 0; got != tc.wantReady {
				t.Fatalf("setup exit success = %t, want %t; stderr=%q", got, tc.wantReady, stderr.String())
			}
			var result setupJSON
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
			}
			if result.Ready != tc.wantReady {
				t.Fatalf("ready = %t, want %t", result.Ready, tc.wantReady)
			}
			if result.Agentbus.MinimumSupportedVersion != minimumSupportedAgentbusVersion || result.Agentbus.VersionStatus != tc.wantStatus {
				t.Fatalf("agentbus version result = %#v, want minimum=%q status=%q", result.Agentbus, minimumSupportedAgentbusVersion, tc.wantStatus)
			}
			if tc.wantWarning == "" {
				if len(result.Warnings) != 0 {
					t.Fatalf("warnings = %#v, want no warning", result.Warnings)
				}
			} else if !strings.Contains(strings.Join(result.Warnings, "\n"), tc.wantWarning) {
				t.Fatalf("warnings = %#v, want %q", result.Warnings, tc.wantWarning)
			}
			if tc.wantRemedy != "" && !strings.Contains(stderr.String(), tc.wantRemedy) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantRemedy)
			}
		})
	}
}

func setupFixtureAgentbusVersionBelowFloor(t *testing.T) string {
	t.Helper()
	return setupFixtureAgentbusVersionAdjacentToFloor(t, -1)
}

func setupFixtureAgentbusVersionAboveFloor(t *testing.T) string {
	t.Helper()
	return setupFixtureAgentbusVersionAdjacentToFloor(t, 1)
}

func setupFixtureAgentbusVersionAdjacentToFloor(t *testing.T, direction int) string {
	t.Helper()
	floor, err := parseAgentbusSemver(minimumSupportedAgentbusVersion)
	if err != nil {
		t.Fatalf("parse minimum supported agentbus version %q: %v", minimumSupportedAgentbusVersion, err)
	}
	major, err := strconv.ParseUint(floor.major, 10, 64)
	if err != nil {
		t.Fatalf("parse minimum supported agentbus major version %q: %v", floor.major, err)
	}
	minor, err := strconv.ParseUint(floor.minor, 10, 64)
	if err != nil {
		t.Fatalf("parse minimum supported agentbus minor version %q: %v", floor.minor, err)
	}
	patch, err := strconv.ParseUint(floor.patch, 10, 64)
	if err != nil {
		t.Fatalf("parse minimum supported agentbus patch version %q: %v", floor.patch, err)
	}

	switch direction {
	case -1:
		switch {
		case patch > 0:
			patch--
		case minor > 0:
			minor--
		case major > 0:
			major--
		default:
			t.Fatal("minimum supported agentbus version has no lower released semantic version")
		}
	case 1:
		patch++
	default:
		t.Fatalf("unsupported adjacent agentbus version direction %d", direction)
	}
	return "v" + strconv.FormatUint(major, 10) + "." + strconv.FormatUint(minor, 10) + "." + strconv.FormatUint(patch, 10)
}

func TestSetupTooOldAgentbusSkipsConnectionAndReportsKnownJSON(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	belowFloorVersion := setupFixtureAgentbusVersionBelowFloor(t)
	connected := false
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		connected = true
		return nil, errors.New("connect should not be called for a too-old agentbus")
	}
	commandOutput = func(string, ...string) ([]byte, error) {
		return []byte(agentbusVersionFixtureOutput(belowFloorVersion)), nil
	}
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("setup code=%d, want readiness failure 1; stderr=%q", code, stderr.String())
	}
	if connected {
		t.Fatal("connectAgentbus was called for a too-old agentbus")
	}
	var result setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if result.Schema != commandJSONSchema || result.Delegate != versionLine() || result.Ready {
		t.Fatalf("setup result = %#v, want schema/delegate and ready=false", result)
	}
	if !result.Agentbus.Found || result.Agentbus.Path != "/tmp/agentbus" || result.Agentbus.Version != belowFloorVersion || result.Agentbus.MinimumSupportedVersion != minimumSupportedAgentbusVersion || result.Agentbus.VersionStatus != agentbusVersionStatusTooOld {
		t.Fatalf("agentbus result = %#v, want known too-old discovery facts", result.Agentbus)
	}
	var raw struct {
		Agentbus map[string]json.RawMessage `json:"agentbus"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &raw); err != nil {
		t.Fatalf("raw setup JSON invalid: %v", err)
	}
	for _, field := range []string{"protocolVersion", "backends", "backendMetadata", "capabilities", "requiredCapabilities", "missingCapabilities", "capabilitiesOK"} {
		if _, found := raw.Agentbus[field]; found {
			t.Fatalf("agentbus JSON contains unobserved handshake field %q: %s", field, stdout.String())
		}
	}
	wantRemedy := "agentbus " + belowFloorVersion + " is older than the minimum supported version " + minimumSupportedAgentbusVersion + "; run mise-en-place install agentbus to upgrade"
	if !strings.Contains(stderr.String(), wantRemedy) {
		t.Fatalf("stderr=%q, want %q", stderr.String(), wantRemedy)
	}
}

func TestSetupTooOldAgentbusPlaintextReportsVersionRemedy(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	belowFloorVersion := setupFixtureAgentbusVersionBelowFloor(t)
	commandOutput = func(string, ...string) ([]byte, error) {
		return []byte(agentbusVersionFixtureOutput(belowFloorVersion)), nil
	}
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("setup code=%d, want readiness failure 1; stderr=%q", code, stderr.String())
	}
	for _, line := range []string{"agentbus version: " + belowFloorVersion, "agentbus version status: " + agentbusVersionStatusTooOld, "ready: false"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("stdout=%q, want %q", stdout.String(), line)
		}
	}
	wantRemedy := "agentbus " + belowFloorVersion + " is older than the minimum supported version " + minimumSupportedAgentbusVersion + "; run mise-en-place install agentbus to upgrade"
	if !strings.Contains(stderr.String(), wantRemedy) {
		t.Fatalf("stderr=%q, want %q", stderr.String(), wantRemedy)
	}
}

func TestSetupUnknownAgentbusVersionKeepsConnectErrorPath(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		return nil, errors.New("agentbus unavailable")
	}
	commandOutput = func(string, ...string) ([]byte, error) {
		return []byte("agentbus development-build\n"), nil
	}
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code != agentbusExitDaemonRuntime {
		t.Fatalf("setup code=%d, want ordinary connect-error code %d; stderr=%q", code, agentbusExitDaemonRuntime, stderr.String())
	}
	var envelope agentbusErrorEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil {
		t.Fatalf("connect-error JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if envelope.Code != agentbusErrorTransport || !strings.Contains(envelope.Message, "agentbus unavailable") {
		t.Fatalf("connect-error envelope=%#v, want transport error", envelope)
	}
	if strings.Contains(stdout.String()+stderr.String(), "minimum supported version") || strings.Contains(stdout.String()+stderr.String(), "mise-en-place install agentbus to upgrade") {
		t.Fatalf("unknown-version connect error reported version remediation: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestMinimumSupportedAgentbusVersionDoesNotExceedGoModPin(t *testing.T) {
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	var pinned string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "github.com/charlesnpx/agentbus" {
			pinned = fields[1]
			break
		}
	}
	if pinned == "" {
		t.Fatal("agentbus module pin not found in go.mod")
	}
	comparison, err := compareAgentbusSemver(minimumSupportedAgentbusVersion, pinned)
	if err != nil {
		t.Fatalf("compare declared floor %q with go.mod pin %q: %v", minimumSupportedAgentbusVersion, pinned, err)
	}
	if comparison > 0 {
		t.Fatalf("minimum supported agentbus version %q exceeds go.mod pin %q", minimumSupportedAgentbusVersion, pinned)
	}
}

func TestSetupReadyRequiresWritableAgentbusStateRoot(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	stateRoot := filepath.Join(t.TempDir(), "agentbus-state-file")
	if err := os.WriteFile(stateRoot, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", stateRoot)
	setupTestPreflightDirectories(t)

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
	setupTestPreflightDirectories(t)
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
