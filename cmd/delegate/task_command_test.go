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
	wantLaunchHash := sha256.Sum256([]byte(`{"job_id":"job_envelope","result_sha256":null,"schema":1,"status":"queued"}`))
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
	terminal, err := newTerminalEnvelope("job_envelope", engine.StateCompleted, taskKind, contractKindShape, stamp, strings.Repeat("a", 64))
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
					JobID:     fixedJobID,
					SessionID: "session_parity",
					State:     engine.StateCompleted,
					Result:    &engine.ResultInfo{Text: report, SHA256: rawHash, Bytes: int64(len(report))},
					Contract:  tc.contract,
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

func TestTaskMetadataPersistFailureStillLaunchesWithWarning(t *testing.T) {
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
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "launch only"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
		t.Fatalf("launch envelope JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if launch.JobID != "job_meta_warning" || launch.Status != string(engine.StateQueued) {
		t.Fatalf("launch = %#v, want queued job_meta_warning", launch)
	}
	wantWarning := "warning: delegate job metadata for job_meta_warning was not persisted: metadata store read-only"
	if !strings.Contains(stderr.String(), wantWarning) {
		t.Fatalf("stderr = %q, want warning %q", stderr.String(), wantWarning)
	}
}

func stubAgentbusGlobals(t *testing.T, fake *fakeAgentbusClient) func() {
	t.Helper()
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
		},
	}
}

type fakeAgentbusClient struct {
	hello     client.HelloResult
	submits   []client.JobSubmitParams
	submitErr error
	result    client.JobResult
}

func (f *fakeAgentbusClient) Close() error { return nil }

func (f *fakeAgentbusClient) Hello(context.Context) (client.HelloResult, error) {
	return f.hello, nil
}

func (f *fakeAgentbusClient) HelloResult() client.HelloResult { return f.hello }

func (f *fakeAgentbusClient) SessionStart(context.Context, client.SessionStartParams) (client.SessionStartResult, error) {
	return client.SessionStartResult{}, errors.New("unexpected SessionStart")
}

func (f *fakeAgentbusClient) SessionResume(context.Context, client.SessionResumeParams) (client.SessionStartResult, error) {
	return client.SessionStartResult{}, errors.New("unexpected SessionResume")
}

func (f *fakeAgentbusClient) TurnStart(context.Context, client.TurnStartParams) (client.TurnStartResult, <-chan client.TurnNotification, error) {
	return client.TurnStartResult{}, nil, errors.New("unexpected TurnStart")
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

func (f *fakeAgentbusClient) JobStatus(context.Context, client.JobStatusParams) (client.JobStatusResult, error) {
	if f.result.JobID == "" {
		return client.JobStatusResult{}, nil
	}
	return client.JobStatusResult{Jobs: []client.JobStatus{{JobID: f.result.JobID, SessionID: f.result.SessionID, State: f.result.State}}}, nil
}

func (f *fakeAgentbusClient) JobResult(context.Context, client.JobResultParams) (client.JobResult, error) {
	if f.result.JobID == "" {
		return client.JobResult{}, errors.New("result not ready")
	}
	return f.result, nil
}

func (f *fakeAgentbusClient) JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error) {
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
	ch := make(chan engine.Event, 1)
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
