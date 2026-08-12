package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
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
	t.Setenv("AGENTBUS_STATE_ROOT", "")

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
	stateHome := t.TempDir()
	cacheHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("AGENTBUS_STATE_ROOT", "")
	paths := []string{
		filepath.Join(stateHome, "delegate"),
		filepath.Join(stateHome, "agentbus"),
		filepath.Join(cacheHome, "agentbus", "start-locks"),
	}
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
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTBUS_STATE_ROOT", "")

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
			Depth:           "99",
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
		if strings.Contains(stdout.String(), "parent_session_id") || strings.Contains(stdout.String(), "parent_agent") || strings.Contains(stdout.String(), "private-session-id") || strings.Contains(stdout.String(), "private-parent-agent") || strings.Contains(stdout.String(), "client-") || strings.Contains(stdout.String(), "99") {
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
	t.Setenv("HOME", t.TempDir())
	stateRoot := filepath.Join(t.TempDir(), "agentbus-state-file")
	if err := os.WriteFile(stateRoot, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", stateRoot)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "Library", "Caches", "agentbus", "start-locks"), 0o700); err != nil {
		t.Fatal(err)
	}

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
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "Library", "Caches", "agentbus", "start-locks"), 0o700); err != nil {
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
