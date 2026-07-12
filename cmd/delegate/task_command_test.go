package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	"github.com/charlesnpx/delegate/internal/policy"
)

func TestEnvelopeSchemasAndHashes(t *testing.T) {
	launch, err := newLaunchEnvelope("job_envelope", engine.StateQueued)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"result_sha256":null`)) {
		t.Fatalf("launch envelope = %s, want null result_sha256", raw)
	}
	wantLaunchHash := sha256.Sum256([]byte(`{"effort":{"source":"default"},"job_id":"job_envelope","model":{"source":"default"},"result_sha256":null,"schema":1,"status":"queued"}`))
	if launch.SHA256 != hex.EncodeToString(wantLaunchHash[:]) {
		t.Fatalf("launch sha256 = %q, want %q", launch.SHA256, hex.EncodeToString(wantLaunchHash[:]))
	}
	launch.SHA256 = "ignored"
	gotHash, err := envelopeSHA256(launch)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != hex.EncodeToString(wantLaunchHash[:]) {
		t.Fatalf("hash with own field changed = %q, want %q", gotHash, hex.EncodeToString(wantLaunchHash[:]))
	}

	stamp := engine.ContractStamp{
		Status:      engine.ContractCompliant,
		Missing:     []string{},
		Attempts:    1,
		ValidatedAt: time.Unix(1, 0).UTC(),
	}
	terminal, err := newTerminalEnvelope("job_envelope", engine.StateCompleted, taskKind, contractKindShape, stamp, strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"kind":"task"`,
		`"contractKind":"shape"`,
		`"result_sha256":"` + strings.Repeat("a", 64) + `"`,
	} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("terminal envelope = %s, want fragment %s", raw, fragment)
		}
	}
	if terminal.SHA256 == "" || terminal.SHA256 == strings.Repeat("a", 64) {
		t.Fatalf("terminal envelope sha256 = %q, want distinct envelope hash", terminal.SHA256)
	}
	canonicalTerminal := `{"contract":{"attempts":1,"missing":[],"reason":"","retryUsed":false,"status":"compliant","validatedAt":"1970-01-01T00:00:01Z"},"contractKind":"shape","effort":{"source":"default"},"job_id":"job_envelope","kind":"task","model":{"source":"default"},"model_reported_unavailable_reason":"agentbus_capability_missing","result_sha256":"` + strings.Repeat("a", 64) + `","schema":1,"status":"completed"}`
	wantTerminalHash := sha256.Sum256([]byte(canonicalTerminal))
	if terminal.SHA256 != hex.EncodeToString(wantTerminalHash[:]) {
		t.Fatalf("terminal sha256 = %q, want independent canonical JSON digest %q", terminal.SHA256, hex.EncodeToString(wantTerminalHash[:]))
	}
}

func TestSetupCapabilityGateReportsStaleAgentbus(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{
		hello: client.HelloResult{
			ProtocolVersion: 1,
			Capabilities:    map[string]bool{"policy.retry": true},
		},
	})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("setup succeeded, want stale capability failure")
	}
	want := "agentbus v0.0.7 lacks capability `policy.shape`; run mise-en-place install agentbus"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestSetupOutputIncludesStopReviewGateLine(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), stopReviewGateLine) {
		t.Fatalf("setup stdout = %q, want %q", stdout.String(), stopReviewGateLine)
	}
	if !strings.Contains(stdout.String(), "agentbus models.reported: true") || !strings.Contains(stdout.String(), "config file:") || !strings.Contains(stdout.String(), "config overridable: true") {
		t.Fatalf("setup stdout = %q, want model-reporting and config lines", stdout.String())
	}
}

func TestSetupJSONReportsAgentbusCapabilitiesAndEverySkill(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code = %d, stderr = %q", code, stderr.String())
	}
	var result setupJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if !result.Agentbus.Found || result.Agentbus.Path != "/tmp/agentbus" {
		t.Fatalf("agentbus discovery = %#v, want found /tmp/agentbus", result.Agentbus)
	}
	if !result.Agentbus.CapabilitiesOK || !result.Agentbus.Capabilities["policy.shape"] || !result.Agentbus.Capabilities["policy.retry"] {
		t.Fatalf("agentbus capabilities = %#v, want required capabilities passing", result.Agentbus)
	}
	if !result.Agentbus.Capabilities["models.reported"] || result.Config.Path == "" || !result.Config.Overridable {
		t.Fatalf("setup config/model capability = %#v / %#v", result.Config, result.Agentbus.Capabilities)
	}
	if result.StopReviewGate != "not available (planned v0.2)" {
		t.Fatalf("stop_review_gate = %q", result.StopReviewGate)
	}
	if len(result.Skills) != 16 {
		t.Fatalf("skill statuses = %d, want 16: %#v", len(result.Skills), result.Skills)
	}
	for _, skill := range result.Skills {
		if skill.Target == "" || skill.Name == "" || skill.Path == "" {
			t.Fatalf("incomplete skill status: %#v", skill)
		}
		if skill.Status != "missing" {
			t.Fatalf("skill status = %#v, want missing in fresh HOME", skill)
		}
	}
}

func TestTaskPolicyTierWiring(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flags     []string
		wantNil   bool
		wantRetry bool
	}{
		{name: "default"},
		{name: "write", flags: []string{"--write"}, wantRetry: true},
		{name: "strict", flags: []string{"--strict-contract"}, wantRetry: true},
		{name: "no_contract", flags: []string{"--no-contract"}, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cwd := t.TempDir()
			args := append([]string{"task", "--backend", "codex", "--cwd", cwd, "--prompt", "do it"}, tc.flags...)
			var stdout, stderr bytes.Buffer
			code := run(args, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
			}
			got := fake.submits[0].TaskSpec.Policy
			if tc.wantNil {
				if got != nil {
					t.Fatalf("policy = %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("policy = nil, want delegate policy")
			}
			if got.Prologue == "" {
				t.Fatal("policy prologue is empty")
			}
			if got.Contract == nil || got.Contract.Shape == nil {
				t.Fatalf("policy contract = %#v, want shape contract", got.Contract)
			}
			if tc.wantRetry {
				if got.Retry == nil || got.Retry.Max != 1 {
					t.Fatalf("retry = %#v, want max 1", got.Retry)
				}
			} else if got.Retry != nil {
				t.Fatalf("retry = %#v, want nil", got.Retry)
			}
		})
	}
}

func TestEmbeddedAndDaemonTaskParity(t *testing.T) {
	for _, tc := range []struct {
		name             string
		flags            []string
		contract         *engine.ContractStamp
		wantContractKind string
		wantStatus       engine.ContractStatus
	}{
		{
			name:             "no_contract",
			flags:            []string{"--no-contract"},
			wantContractKind: contractKindNone,
			wantStatus:       engine.ContractDisabled,
		},
		{
			name:             "default_contract",
			contract:         ptr(compliantContractStamp(t, compliantReport())),
			wantContractKind: contractKindShape,
			wantStatus:       engine.ContractCompliant,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := compliantReport()
			rawHash := rawSHA256(report)
			fixedJobID := "job_parity_" + tc.name

			fakeClient := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				result: client.JobResult{
					JobID:         fixedJobID,
					SessionID:     "session_parity",
					State:         engine.StateCompleted,
					Result:        &engine.ResultInfo{Text: report, SHA256: rawHash, Bytes: int64(len(report)), ModelReported: "parity-model"},
					ModelReported: "parity-model",
					Contract:      tc.contract,
				},
			}
			restoreClient := stubAgentbusGlobals(t, fakeClient)
			defer restoreClient()
			restoreEmbedded := stubEmbeddedGlobals(t, fixedJobID, fakeBackendWithResult("codex", report))
			defer restoreEmbedded()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cwd := t.TempDir()

			baseArgs := []string{"task", "--backend", "codex", "--cwd", cwd, "--prompt", "same task", "--wait"}
			var daemonOut, daemonErr bytes.Buffer
			daemonCode := run(append(append([]string{}, baseArgs...), tc.flags...), nil, &daemonOut, &daemonErr)
			if daemonCode != 0 {
				t.Fatalf("daemon task code = %d, stderr = %q", daemonCode, daemonErr.String())
			}
			var embeddedOut, embeddedErr bytes.Buffer
			embeddedArgs := append(append(append([]string{}, baseArgs...), tc.flags...), "--embedded")
			embeddedCode := run(embeddedArgs, nil, &embeddedOut, &embeddedErr)
			if embeddedCode != 0 {
				t.Fatalf("embedded task code = %d, stderr = %q", embeddedCode, embeddedErr.String())
			}
			if daemonOut.String() != embeddedOut.String() {
				t.Fatalf("daemon envelope:\n%s\nembedded envelope:\n%s", daemonOut.String(), embeddedOut.String())
			}
			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(daemonOut.Bytes()), &env); err != nil {
				t.Fatalf("terminal envelope JSON invalid: %v; raw = %q", err, daemonOut.String())
			}
			if env.ContractKind != tc.wantContractKind {
				t.Fatalf("contractKind = %q, want %q", env.ContractKind, tc.wantContractKind)
			}
			if env.ModelReported != "parity-model" || env.ModelReportedUnavailableReason != "" {
				t.Fatalf("parity model_reported = %q (reason %q), want parity-model", env.ModelReported, env.ModelReportedUnavailableReason)
			}
			if env.Contract.Status != tc.wantStatus {
				t.Fatalf("contract status = %q, want %q", env.Contract.Status, tc.wantStatus)
			}
			if tc.contract != nil {
				if env.Contract.ContractSHA256 == "" || env.Contract.ValidatedAt.IsZero() || env.Contract.Attempts != 1 {
					t.Fatalf("contract stamp = %#v, want populated hash, validatedAt, attempts", env.Contract)
				}
			}
		})
	}
}

func TestTaskSubmitFailureUnlinksHandoffAndDeletesJobInput(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithCapabilities(), submitErr: errors.New("submit failed")}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	xdgState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdgState)
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	handoffResult, err := handoff.Create(handoff.CreateOptions{
		StateDir: stateDir,
		Reader:   strings.NewReader("durable prompt"),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--handoff-prompt-file", handoffResult.HandoffPath}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task code = 0, want submit failure; stdout = %q stderr = %q", stdout.String(), stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
	}
	if _, err := os.Stat(handoffResult.HandoffPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff file still exists or unexpected stat error: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("job input cleanup left files: %#v", matches)
	}
}

func TestTaskMetadataPersistFailureBeforeLaunchAborts(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID: "job_meta_warning",
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveDelegateJobMetadata
	saveDelegateJobMetadata = func(string, jobMetadata) error {
		return errors.New("metadata store read-only")
	}
	defer func() { saveDelegateJobMetadata = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do not launch"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task code = 0, want metadata failure; stdout = %q", stdout.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
	}
	if !strings.Contains(stderr.String(), "persist metadata before launch: metadata store read-only") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("job inputs after aborted launch = %#v, %v", matches, err)
	}
}

func TestBackgroundJobInputIsReassociatedAndSweptByNextStatus(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID: "job_background_reap",
			State: engine.StateCompleted,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveDelegateJobMetadata
	saveCalls := 0
	saveDelegateJobMetadata = func(string, jobMetadata) error {
		saveCalls++
		if saveCalls == 2 {
			return errors.New("metadata unavailable after launch")
		}
		return nil
	}
	defer func() { saveDelegateJobMetadata = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}

	var taskOut, taskErr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "background prompt", "--background"}, nil, &taskOut, &taskErr); code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, taskErr.String())
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("job inputs after CLI exit = %#v, want one", matches)
	}
	input, ok := handoff.ParseJobInputPath(matches[0])
	if !ok || input.JobID != "job_background_reap" {
		t.Fatalf("reassociated job input = %#v, %v; want job_background_reap", input, ok)
	}

	var statusOut, statusErr bytes.Buffer
	if code := run([]string{"status", "--job", "job_background_reap"}, nil, &statusOut, &statusErr); code != 0 {
		t.Fatalf("status code = %d, stderr = %q", code, statusErr.String())
	}
	if _, err := os.Stat(matches[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job input remains after terminal status sweep: %v", err)
	}
	if len(fake.statuses) < 2 {
		t.Fatalf("JobStatus calls = %d, want primary lookup plus sweep lookup", len(fake.statuses))
	}
}

func TestBackgroundJobInputIsSweptByNextResult(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID: "job_result_reap",
			State: engine.StateCompleted,
			Result: &engine.ResultInfo{
				SHA256: strings.Repeat("b", 64),
			},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveDelegateJobMetadata
	saveCalls := 0
	saveDelegateJobMetadata = func(string, jobMetadata) error {
		saveCalls++
		if saveCalls == 2 {
			return errors.New("metadata unavailable after launch")
		}
		return nil
	}
	defer func() { saveDelegateJobMetadata = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}

	var taskOut, taskErr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "background prompt", "--background"}, nil, &taskOut, &taskErr); code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, taskErr.String())
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("job input glob = %#v, %v; want one", matches, err)
	}

	var resultOut, resultErr bytes.Buffer
	if code := run([]string{"result", "--job", "job_result_reap"}, nil, &resultOut, &resultErr); code != 0 {
		t.Fatalf("result code = %d, stderr = %q", code, resultErr.String())
	}
	if _, err := os.Stat(matches[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job input remains after terminal result sweep: %v", err)
	}
	if len(fake.statuses) == 0 {
		t.Fatal("result did not perform terminal job-input status lookup")
	}
}

func TestSweepAdoptsOldProvisionalMetadataBySubmittedJobTag(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "delegate")
	provisionalID := "job_provisional_adoption"
	actualID := "job_actual_adoption"
	input, err := handoff.PersistJobInput(handoff.JobInputOptions{
		StateDir: stateDir,
		JobID:    provisionalID,
		Prompt:   handoff.ResolvedPrompt{Prompt: "review prompt", Source: handoff.SourcePrompt},
	})
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err = handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(stateDir, "review-")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{
		Schema:          envelopeSchema,
		JobID:           provisionalID,
		Kind:            reviewKind,
		JobInputPath:    input.Path,
		ReviewWorkspace: workspace,
		Provisional:     true,
		CreatedAt:       time.Now().Add(-2 * provisionalMetadataAdoptionThreshold),
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{status: client.JobStatusResult{Jobs: []client.JobStatus{{
		JobID: actualID,
		State: engine.StateCompleted,
		Tags:  map[string]string{provisionalJobIDTag: provisionalID},
	}}}}

	if err := sweepTerminalJobInputs(context.Background(), fake, stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adopted terminal workspace remains: %v", err)
	}
	if _, found, err := loadJobMetadata(stateDir, provisionalID); err != nil || found {
		t.Fatalf("provisional metadata found=%v err=%v", found, err)
	}
	meta, found, err := loadJobMetadata(stateDir, actualID)
	if err != nil || !found || meta.Provisional || meta.JobInputPath != "" || meta.ReviewWorkspace != "" {
		t.Fatalf("adopted metadata=%#v found=%v err=%v", meta, found, err)
	}
}

func TestTaskResumeSessionUsesResumeAndTurnStart(t *testing.T) {
	cwd := t.TempDir()
	fake := &fakeAgentbusClient{
		hello:        helloWithCapabilities(),
		sessions:     []client.SessionInfo{{SessionID: "session_explicit", Backend: "codex", CWD: cwd}},
		resumeResult: client.SessionStartResult{SessionID: "session_explicit", Backend: "codex"},
		turnResult:   client.TurnStartResult{TurnID: "turn_explicit", JobID: "job_explicit_resume", SessionID: "session_explicit"},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--resume-session", "session_explicit", "--prompt", "continue", "--wait"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(fake.resumes) != 1 || fake.resumes[0].SessionID != "session_explicit" {
		t.Fatalf("SessionResume calls = %#v", fake.resumes)
	}
	if len(fake.turns) != 1 || fake.turns[0].SessionID != "session_explicit" || fake.turns[0].Prompt != "continue" {
		t.Fatalf("TurnStart calls = %#v", fake.turns)
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
	}
	meta, found, err := loadJobMetadata("", "job_explicit_resume")
	if err != nil || !found {
		t.Fatalf("loadJobMetadata() = %#v, %v, %v", meta, found, err)
	}
	if meta.Backend != "codex" || meta.CWD != cwd || meta.SessionID != "session_explicit" {
		t.Fatalf("resume metadata = %#v", meta)
	}
}

func TestTaskResumeUsesSessionModelEffortAndSkipsConfigDefaults(t *testing.T) {
	cwd := t.TempDir()
	fake := &fakeAgentbusClient{
		hello:        helloWithCapabilities(),
		sessions:     []client.SessionInfo{{SessionID: "session_configured_resume", Backend: "codex", CWD: cwd}},
		resumeResult: client.SessionStartResult{SessionID: "session_configured_resume", Backend: "codex"},
		turnResult:   client.TurnStartResult{TurnID: "turn_configured_resume", JobID: "job_configured_resume", SessionID: "session_configured_resume"},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	if err := config.Save(config.Config{Overridable: false, Backend: config.Backends{
		Codex: config.Defaults{Model: "configured-model", Effort: "configured-effort"},
	}}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--resume-session", "session_configured_resume", "--prompt", "continue", "--wait"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(fake.submits) != 0 || len(fake.resumes) != 1 || len(fake.turns) != 1 {
		t.Fatalf("resume calls: submits=%d resumes=%d turns=%d", len(fake.submits), len(fake.resumes), len(fake.turns))
	}

	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("resume envelope JSON invalid: %v; raw=%q", err, stdout.String())
	}
	assertSessionModelEffort(t, env.Model, env.Effort)
	meta, found, err := loadJobMetadata("", "job_configured_resume")
	if err != nil || !found {
		t.Fatalf("loadJobMetadata() = %#v, %v, %v", meta, found, err)
	}
	assertSessionModelEffort(t, meta.Model, meta.Effort)
}

func TestEmbeddedTaskResumeUsesSessionModelEffortAndClearsSessionOpts(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := config.Save(config.Config{Overridable: false, Backend: config.Backends{
		Codex: config.Defaults{Model: "configured-model", Effort: "configured-effort"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata("", jobMetadata{
		JobID:     "job_embedded_configured_resume_source",
		Kind:      taskKind,
		Backend:   "codex",
		CWD:       cwd,
		SessionID: "session_embedded_configured_resume",
		CreatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{name: "codex", sessionID: "session_embedded_configured_resume", reports: []string{compliantReport()}}
	restore := stubEmbeddedGlobals(t, "job_embedded_configured_resume", backend)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--resume-session", "session_embedded_configured_resume", "--prompt", "continue", "--embedded", "--wait"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(backend.starts) != 1 {
		t.Fatalf("embedded starts = %#v, want one resume", backend.starts)
	}
	if backend.starts[0].Model != "" || backend.starts[0].Effort != "" {
		t.Fatalf("embedded resume SessionOpts = %#v, want empty model and effort", backend.starts[0])
	}

	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("embedded resume envelope JSON invalid: %v; raw=%q", err, stdout.String())
	}
	assertSessionModelEffort(t, env.Model, env.Effort)
	meta, found, err := loadJobMetadata("", "job_embedded_configured_resume")
	if err != nil || !found {
		t.Fatalf("loadJobMetadata() = %#v, %v, %v", meta, found, err)
	}
	assertSessionModelEffort(t, meta.Model, meta.Effort)
}

func TestTaskResumeRejectsExplicitModelOrEffort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		embedded   bool
		resumeArgs []string
		override   []string
	}{
		{name: "daemon_resume_model", resumeArgs: []string{"--resume"}, override: []string{"--model", "different-model"}},
		{name: "daemon_resume_session_effort", resumeArgs: []string{"--resume-session", "session_override"}, override: []string{"--effort", "different-effort"}},
		{name: "embedded_resume_model", embedded: true, resumeArgs: []string{"--resume"}, override: []string{"--model", "different-model"}},
		{name: "embedded_resume_session_effort", embedded: true, resumeArgs: []string{"--resume-session", "session_override"}, override: []string{"--effort", "different-effort"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "continue", "--wait"}
			args = append(args, tc.resumeArgs...)
			args = append(args, tc.override...)

			var fake *fakeAgentbusClient
			var backend *recordingBackend
			if tc.embedded {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				backend = &recordingBackend{name: "codex", sessionID: "session_override", reports: []string{compliantReport()}}
				restore := stubEmbeddedGlobals(t, "job_resume_override", backend)
				defer restore()
				args = append(args, "--embedded")
			} else {
				fake = &fakeAgentbusClient{hello: helloWithCapabilities()}
				restore := stubAgentbusGlobals(t, fake)
				defer restore()
			}

			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code == 0 {
				t.Fatalf("task succeeded, stdout = %q", stdout.String())
			}
			if want := "model/effort cannot be changed when resuming a session; the session keeps the values it was started with"; !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if fake != nil && (len(fake.submits) != 0 || len(fake.resumes) != 0 || len(fake.turns) != 0) {
				t.Fatalf("daemon calls after rejected resume override: submits=%d resumes=%d turns=%d", len(fake.submits), len(fake.resumes), len(fake.turns))
			}
			if backend != nil && len(backend.starts) != 0 {
				t.Fatalf("embedded starts after rejected resume override: %#v", backend.starts)
			}
		})
	}
}

func assertSessionModelEffort(t *testing.T, model, effort config.DimensionResolution) {
	t.Helper()
	for name, dimension := range map[string]config.DimensionResolution{
		"model":  model,
		"effort": effort,
	} {
		if dimension.Source != "session" || dimension.Requested != "" || dimension.Effective != "" {
			t.Fatalf("%s resolution = %#v, want only source session", name, dimension)
		}
	}
}

func TestTaskResumeMetadataFailureAfterTurnStartEmitsLaunchAndPreservesInput(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "delegate")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	stateDir, err = handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(stateDir, "review-")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	fake := &fakeAgentbusClient{
		sessions:     []client.SessionInfo{{SessionID: "session_durable_resume", Backend: "codex", CWD: cwd}},
		resumeResult: client.SessionStartResult{SessionID: "session_durable_resume", Backend: "codex"},
		turnResult:   client.TurnStartResult{TurnID: "turn_durable_resume", JobID: "job_durable_resume", SessionID: "session_durable_resume"},
	}

	oldPrimary := saveDelegateJobMetadata
	oldFallback := saveLaunchedJobMetadataFallback
	primaryCalls := 0
	saveDelegateJobMetadata = func(dir string, meta jobMetadata) error {
		primaryCalls++
		if primaryCalls == 1 {
			return saveJobMetadata(dir, meta)
		}
		return errors.New("primary metadata unavailable after turn start")
	}
	saveLaunchedJobMetadataFallback = func(string, jobMetadata) error {
		return errors.New("fallback metadata unavailable after turn start")
	}
	defer func() {
		saveDelegateJobMetadata = oldPrimary
		saveLaunchedJobMetadataFallback = oldFallback
	}()

	result, err := runDaemonSessionTask(context.Background(), fake, taskOptions{
		Backend:         "codex",
		Wait:            true,
		CWD:             cwd,
		ResumeSession:   "session_durable_resume",
		StateDir:        stateDir,
		Kind:            reviewKind,
		ReviewWorkspace: workspace,
	}, handoff.ResolvedPrompt{Prompt: "durable resumed prompt", Source: handoff.SourcePrompt}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Submitted || result.Launch == nil || result.Launch.JobID != "job_durable_resume" || result.Terminal != nil {
		t.Fatalf("result = %#v, want real post-TurnStart launch", result)
	}
	assertSessionModelEffort(t, result.Launch.Model, result.Launch.Effort)
	if len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, "\n"), "persist launched job metadata for job_durable_resume") {
		t.Fatalf("warnings = %#v, want metadata durability warning", result.Warnings)
	}

	var stdout, stderr bytes.Buffer
	if code, err := writeTaskRunResult(result, &stdout, &stderr); err != nil || code != 0 {
		t.Fatalf("writeTaskRunResult() = %d, %v", code, err)
	}
	var envelope LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil || envelope.JobID != "job_durable_resume" {
		t.Fatalf("launch envelope = %#v, err=%v, raw=%q", envelope, err, stdout.String())
	}
	assertSessionModelEffort(t, envelope.Model, envelope.Effort)

	inputs, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil || len(inputs) != 1 {
		t.Fatalf("job inputs = %#v, err=%v, want one", inputs, err)
	}
	input, ok := handoff.ParseJobInputPath(inputs[0])
	if !ok || input.JobID != "job_durable_resume" {
		t.Fatalf("reassociated input = %#v, ok=%v", input, ok)
	}
	if raw, err := os.ReadFile(inputs[0]); err != nil || string(raw) != "durable resumed prompt" {
		t.Fatalf("durable input = %q, err=%v", raw, err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("review workspace was not preserved: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	var provisional jobMetadata
	for _, entry := range entries {
		jobID := strings.TrimSuffix(entry.Name(), ".json")
		meta, found, loadErr := loadJobMetadata(stateDir, jobID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if found && meta.Provisional {
			provisional = meta
			break
		}
	}
	if provisional.JobID == "" || provisional.AdoptedJobID != "job_durable_resume" {
		t.Fatalf("provisional metadata = %#v, want adopted job mapping", provisional)
	}
	assertSessionModelEffort(t, provisional.Model, provisional.Effort)
	provisional.CreatedAt = time.Now().Add(-2 * provisionalMetadataAdoptionThreshold)
	if err := saveJobMetadata(stateDir, provisional); err != nil {
		t.Fatal(err)
	}
	fake.status = client.JobStatusResult{Jobs: []client.JobStatus{{
		JobID:     "job_durable_resume",
		SessionID: "session_durable_resume",
		State:     engine.StateCompleted,
	}}}
	if err := sweepTerminalJobInputs(context.Background(), fake, stateDir); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadJobMetadata(stateDir, provisional.JobID); err != nil || found {
		t.Fatalf("provisional metadata found=%v err=%v after sweep", found, err)
	}
	meta, found, err := loadJobMetadata(stateDir, "job_durable_resume")
	if err != nil || !found || meta.Provisional || meta.SessionID != "session_durable_resume" {
		t.Fatalf("adopted metadata=%#v found=%v err=%v", meta, found, err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adopted terminal workspace remains: %v", err)
	}
}

func TestTaskResumeSelectsMostRecentSessionForBackendAndCWD(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello:        helloWithCapabilities(),
		resumeResult: client.SessionStartResult{SessionID: "session_new", Backend: "codex"},
		turnResult:   client.TurnStartResult{TurnID: "turn_latest", JobID: "job_latest_resume", SessionID: "session_new"},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	for _, meta := range []jobMetadata{
		{JobID: "job_old_session", Kind: taskKind, Backend: "codex", CWD: cwd, SessionID: "session_old", CreatedAt: time.Unix(1, 0).UTC()},
		{JobID: "job_new_session", Kind: taskKind, Backend: "codex", CWD: cwd, SessionID: "session_new", CreatedAt: time.Unix(2, 0).UTC()},
		{JobID: "job_other_backend", Kind: taskKind, Backend: "claude", CWD: cwd, SessionID: "session_other", CreatedAt: time.Unix(3, 0).UTC()},
		{JobID: "job_other_cwd", Kind: taskKind, Backend: "codex", CWD: t.TempDir(), SessionID: "session_elsewhere", CreatedAt: time.Unix(4, 0).UTC()},
	} {
		if err := saveJobMetadata("", meta); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--resume", "--prompt", "continue latest", "--wait"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(fake.resumes) != 1 || fake.resumes[0].SessionID != "session_new" {
		t.Fatalf("SessionResume calls = %#v, want session_new", fake.resumes)
	}
	if len(fake.turns) != 1 || fake.turns[0].SessionID != "session_new" {
		t.Fatalf("TurnStart calls = %#v, want session_new", fake.turns)
	}
}

func TestTaskResumeWithoutRecordedSessionHasGuidance(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--resume", "--prompt", "continue", "--wait"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task succeeded, stdout = %q", stdout.String())
	}
	for _, guidance := range []string{"no resumable delegate session", "run a fresh task first", "--resume-session <id>"} {
		if !strings.Contains(stderr.String(), guidance) {
			t.Fatalf("stderr = %q, want guidance %q", stderr.String(), guidance)
		}
	}
	if len(fake.resumes) != 0 || len(fake.submits) != 0 {
		t.Fatalf("daemon calls on missing resume metadata: resumes=%#v submits=%#v", fake.resumes, fake.submits)
	}
}

func TestTaskResumeRequiresWaitInV010(t *testing.T) {
	for _, resumeArgs := range [][]string{{"--resume"}, {"--resume-session", "session_background"}} {
		t.Run(strings.Join(resumeArgs, "_"), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			args := []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "continue"}
			args = append(args, resumeArgs...)
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code == 0 {
				t.Fatalf("task succeeded, stdout = %q", stdout.String())
			}
			if want := "background resume lands post-v0.1.0"; !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		})
	}
}

func TestTaskResumeRejectsSessionBackendOrCWDMismatch(t *testing.T) {
	requestedCWD := t.TempDir()
	otherCWD := t.TempDir()
	for _, tc := range []struct {
		name          string
		actualBackend string
		actualCWD     string
	}{
		{name: "backend", actualBackend: "claude", actualCWD: requestedCWD},
		{name: "cwd", actualBackend: "codex", actualCWD: otherCWD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello:    helloWithCapabilities(),
				sessions: []client.SessionInfo{{SessionID: "session_mismatch", Backend: tc.actualBackend, CWD: tc.actualCWD}},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", requestedCWD, "--resume-session", "session_mismatch", "--prompt", "continue", "--wait"}, nil, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("task succeeded, stdout = %q", stdout.String())
			}
			for _, want := range []string{tc.actualBackend, filepath.Clean(tc.actualCWD), "--fresh"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want actual session detail %q", stderr.String(), want)
				}
			}
			if len(fake.resumes) != 0 || len(fake.turns) != 0 {
				t.Fatalf("mismatched session was resumed: resumes=%#v turns=%#v", fake.resumes, fake.turns)
			}
		})
	}
}

func TestEmbeddedTaskResumeRejectsSessionBackendOrCWDMismatch(t *testing.T) {
	requestedCWD := t.TempDir()
	otherCWD := t.TempDir()
	for _, tc := range []struct {
		name          string
		actualBackend string
		actualCWD     string
	}{
		{name: "backend", actualBackend: "claude", actualCWD: requestedCWD},
		{name: "cwd", actualBackend: "codex", actualCWD: otherCWD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			if err := saveJobMetadata("", jobMetadata{
				JobID:     "job_embedded_resume_source",
				Kind:      taskKind,
				Backend:   tc.actualBackend,
				CWD:       tc.actualCWD,
				SessionID: "session_mismatch",
				CreatedAt: time.Unix(1, 0).UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			backend := &recordingBackend{name: "codex", sessionID: "session_mismatch", reports: []string{compliantReport()}}
			restore := stubEmbeddedGlobals(t, "job_embedded_resume_attempt", backend)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", requestedCWD, "--resume-session", "session_mismatch", "--prompt", "continue", "--embedded", "--wait"}, nil, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("task succeeded, stdout = %q", stdout.String())
			}
			for _, want := range []string{tc.actualBackend, filepath.Clean(tc.actualCWD), "--fresh"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want actual session detail %q", stderr.String(), want)
				}
			}
			if len(backend.starts) != 0 {
				t.Fatalf("mismatched embedded session was resumed: starts=%#v", backend.starts)
			}
		})
	}
}

func TestTaskHelpDocumentsResumeWaitRequirement(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--help"}, nil, &stdout, &stderr); code == 0 {
		t.Fatalf("task --help code = 0, want flag package help exit")
	}
	if want := "required with resume flags in v0.1.0"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("task --help stderr = %q, want %q", stderr.String(), want)
	}
}

func TestTaskFreshIsDefaultAndExplicit(t *testing.T) {
	for _, flags := range [][]string{nil, {"--fresh"}} {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			args := []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "fresh task"}
			args = append(args, flags...)
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
			}
			if len(fake.submits) != 1 || len(fake.resumes) != 0 || len(fake.turns) != 0 {
				t.Fatalf("fresh calls: submits=%d resumes=%d turns=%d", len(fake.submits), len(fake.resumes), len(fake.turns))
			}
		})
	}
}

func stubAgentbusGlobals(t *testing.T, fake *fakeAgentbusClient) func() {
	return stubAgentbusClientGlobals(t, fake)
}

func stubAgentbusClientGlobals(t *testing.T, fake agentbusClient) func() {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		return fake, nil
	}
	lookPath = func(string) (string, error) {
		return "/tmp/agentbus", nil
	}
	commandOutput = func(string, ...string) ([]byte, error) {
		return []byte("agentbus v0.0.7\n"), nil
	}
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}
}

func stubEmbeddedGlobals(t *testing.T, jobID string, backend engine.Backend) func() {
	t.Helper()
	oldBackend := embeddedBackend
	oldJobID := newJobID
	oldNow := nowUTC
	embeddedBackend = func(name string) (engine.Backend, bool) {
		if backend != nil && name == backend.Name() {
			return backend, true
		}
		return nil, false
	}
	newJobID = func() (string, error) { return jobID, nil }
	nowUTC = func() time.Time { return time.Unix(42, 0).UTC() }
	return func() {
		embeddedBackend = oldBackend
		newJobID = oldJobID
		nowUTC = oldNow
	}
}

func helloWithCapabilities() client.HelloResult {
	return client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"codex", "claude"},
		Capabilities: map[string]bool{
			"policy.shape":      true,
			"policy.jsonSchema": true,
			"policy.named":      true,
			"policy.retry":      true,
			"models.reported":   true,
		},
	}
}

type fakeAgentbusClient struct {
	hello        client.HelloResult
	sessions     []client.SessionInfo
	sessionLists []client.SessionListParams
	resumes      []client.SessionResumeParams
	resumeResult client.SessionStartResult
	resumeErr    error
	turns        []client.TurnStartParams
	turnResult   client.TurnStartResult
	turnErr      error
	submits      []client.JobSubmitParams
	submitErr    error
	statuses     []client.JobStatusParams
	status       client.JobStatusResult
	result       client.JobResult
	resultErr    error
	cancel       client.JobCancelResult
	cancelErr    error
}

func (f *fakeAgentbusClient) Close() error { return nil }

func (f *fakeAgentbusClient) Hello(context.Context) (client.HelloResult, error) {
	return f.hello, nil
}

func (f *fakeAgentbusClient) HelloResult() client.HelloResult { return f.hello }

func (f *fakeAgentbusClient) SessionStart(context.Context, client.SessionStartParams) (client.SessionStartResult, error) {
	return client.SessionStartResult{}, errors.New("unexpected SessionStart")
}

func (f *fakeAgentbusClient) SessionResume(_ context.Context, params client.SessionResumeParams) (client.SessionStartResult, error) {
	f.resumes = append(f.resumes, params)
	if f.resumeErr != nil {
		return client.SessionStartResult{}, f.resumeErr
	}
	if f.resumeResult.SessionID == "" {
		return client.SessionStartResult{}, errors.New("unexpected SessionResume")
	}
	return f.resumeResult, nil
}

func (f *fakeAgentbusClient) SessionList(_ context.Context, params client.SessionListParams) (client.SessionListResult, error) {
	f.sessionLists = append(f.sessionLists, params)
	return client.SessionListResult{Sessions: f.sessions}, nil
}

func (f *fakeAgentbusClient) TurnStart(_ context.Context, params client.TurnStartParams) (client.TurnStartResult, <-chan client.TurnNotification, error) {
	f.turns = append(f.turns, params)
	if f.turnErr != nil {
		return client.TurnStartResult{}, nil, f.turnErr
	}
	if f.turnResult.JobID == "" {
		return client.TurnStartResult{}, nil, errors.New("unexpected TurnStart")
	}
	notifications := make(chan client.TurnNotification, 1)
	notifications <- client.TurnNotification{Result: &client.TurnResultParams{
		SessionID: f.turnResult.SessionID,
		TurnID:    f.turnResult.TurnID,
		JobID:     f.turnResult.JobID,
		State:     engine.StateCompleted,
		Result:    &engine.ResultInfo{SHA256: strings.Repeat("d", 64)},
	}}
	close(notifications)
	return f.turnResult, notifications, nil
}

func (f *fakeAgentbusClient) JobSubmit(_ context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	f.submits = append(f.submits, params)
	if f.submitErr != nil {
		return client.JobSubmitResult{}, f.submitErr
	}
	jobID := "job_fake"
	if f.result.JobID != "" {
		jobID = f.result.JobID
	}
	return client.JobSubmitResult{JobID: jobID, State: engine.StateQueued}, nil
}

func (f *fakeAgentbusClient) JobStatus(_ context.Context, params client.JobStatusParams) (client.JobStatusResult, error) {
	f.statuses = append(f.statuses, params)
	if len(f.status.Jobs) > 0 {
		return f.status, nil
	}
	if f.result.JobID == "" {
		return client.JobStatusResult{}, nil
	}
	return client.JobStatusResult{Jobs: []client.JobStatus{{JobID: f.result.JobID, SessionID: f.result.SessionID, State: f.result.State}}}, nil
}

func (f *fakeAgentbusClient) JobResult(context.Context, client.JobResultParams) (client.JobResult, error) {
	if f.resultErr != nil {
		return client.JobResult{}, f.resultErr
	}
	if f.result.JobID == "" {
		return client.JobResult{}, errors.New("result not ready")
	}
	return f.result, nil
}

func (f *fakeAgentbusClient) JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error) {
	if f.cancelErr != nil {
		return client.JobCancelResult{}, f.cancelErr
	}
	if f.cancel.JobID != "" {
		return f.cancel, nil
	}
	return client.JobCancelResult{}, errors.New("unexpected JobCancel")
}

type fakeBackend struct {
	name   string
	result string
}

func fakeBackendWithResult(name, result string) fakeBackend {
	return fakeBackend{name: name, result: result}
}

func (b fakeBackend) Name() string { return b.name }

func (b fakeBackend) Preflight(context.Context) (engine.Health, error) {
	return engine.Health{Backend: b.name}, nil
}

func (b fakeBackend) Start(context.Context, engine.SessionOpts) (engine.Session, error) {
	return fakeSession{id: "session_parity", result: b.result}, nil
}

func (b fakeBackend) Resume(context.Context, string, engine.SessionOpts) (engine.Session, error) {
	return fakeSession{id: "session_parity", result: b.result}, nil
}

type fakeSession struct {
	id     string
	result string
}

func (s fakeSession) ID() string { return s.id }

func (s fakeSession) Turn(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
	ch := make(chan engine.Event, 2)
	ch <- engine.Event{Type: engine.EventModelReported, ModelReported: "parity-model"}
	ch <- engine.Event{Type: engine.EventResultMessage, Text: s.result, RawText: s.result}
	close(ch)
	return ch, nil
}

func (s fakeSession) Interrupt(context.Context) error { return nil }

func compliantReport() string {
	return strings.Join([]string{
		"complete",
		"Criteria scored:",
		"- Done",
		"",
		"Receipts:",
		"- cmd/delegate/main.go:1",
		"",
		"Verification:",
		"- go test ./cmd/delegate",
		"",
		"Scope boundary:",
		"- task-command tests",
		"",
	}, "\n")
}

func compliantContractStamp(t *testing.T, text string) engine.ContractStamp {
	t.Helper()
	spec, err := policy.DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.ValidateContract(text, spec)
	if err != nil {
		t.Fatal(err)
	}
	return engine.StampValidation(1, false, "", result, time.Unix(42, 0).UTC())
}

func ptr[T any](v T) *T {
	return &v
}

func rawSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
