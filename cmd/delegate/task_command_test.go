package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if bytes.Contains(raw, []byte(`"origin"`)) {
		t.Fatalf("launch envelope = %s, want no origin without captured linkage", raw)
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

	origin := envelopeOrigin{Skill: "delegate:rescue:claude", ParentClient: "claude-code", ParentSessionID: "parent-session", ParentAgent: "agent", Depth: "1"}
	withOrigin, err := newLaunchEnvelopeWithOrigin("job_envelope", engine.StateQueued, origin)
	if err != nil {
		t.Fatal(err)
	}
	if withOrigin.Origin == nil || *withOrigin.Origin != origin || withOrigin.SHA256 == launch.SHA256 {
		t.Fatalf("launch with origin = %#v, want origin-covered distinct hash", withOrigin)
	}
	canonicalOrigin := `{"effort":{"source":"default"},"job_id":"job_envelope","model":{"source":"default"},"origin":{"depth":"1","parent_agent":"agent","parent_client":"claude-code","parent_session_id":"parent-session","skill":"delegate:rescue:claude"},"result_sha256":null,"schema":1,"status":"queued"}`
	wantOriginHash := sha256.Sum256([]byte(canonicalOrigin))
	if withOrigin.SHA256 != hex.EncodeToString(wantOriginHash[:]) {
		t.Fatalf("launch origin sha256 = %q, want %q", withOrigin.SHA256, hex.EncodeToString(wantOriginHash[:]))
	}
}

func TestJSONSchemaTerminalEnvelopePreservesContractStamp(t *testing.T) {
	spec := engine.ContractSpec{JSONSchema: json.RawMessage(`{"type":"object","required":["schema_version"],"properties":{"schema_version":{"const":"1"}}}`)}
	validation, err := engine.ValidateContract(`{"schema_version":"2"}`, spec)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid || len(validation.Missing) == 0 {
		t.Fatalf("validation = %#v, want schema violation", validation)
	}
	if !strings.HasPrefix(validation.Missing[0], "/") || !strings.Contains(validation.Missing[0], ": ") {
		t.Fatalf("violation = %q, want <json-pointer>: <message>", validation.Missing[0])
	}
	stamp := engine.StampValidation(2, true, "", validation, time.Unix(2, 0).UTC())
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{JobID: "job_schema", Kind: taskKind, ContractKind: contractKindJSONSchema}); err != nil {
		t.Fatal(err)
	}
	env, err := terminalEnvelopeFromJobResult(stateDir, client.JobResult{
		JobID:    "job_schema",
		State:    engine.StateCompletedNoncompliant,
		Result:   &engine.ResultInfo{SHA256: strings.Repeat("b", 64)},
		Contract: &stamp,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if env.ContractKind != contractKindJSONSchema || env.Contract.ContractSHA256 != validation.ContractSHA256 {
		t.Fatalf("terminal envelope = %#v, want JSON Schema kind and contract hash %q", env, validation.ContractSHA256)
	}
	if len(env.Contract.Missing) != len(validation.Missing) || env.Contract.Missing[0] != validation.Missing[0] {
		t.Fatalf("terminal missing = %#v, want schema violations %#v", env.Contract.Missing, validation.Missing)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"contractKind":"jsonSchema"`)) || !bytes.Contains(raw, []byte(`"contractSha256":"`+validation.ContractSHA256+`"`)) {
		t.Fatalf("terminal envelope JSON = %s, want JSON Schema kind and contract hash", raw)
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())

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
	for _, line := range []string{"stateRootWritable: true", "agentbusStateRootWritable: true", "daemonReachable: true"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("setup stdout = %q, want %q", stdout.String(), line)
		}
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
	if !result.StateRootWritable || !result.AgentbusStateRootWritable || !result.DaemonReachable {
		t.Fatalf("setup preflight fields = stateRootWritable=%t agentbusStateRootWritable=%t daemonReachable=%t, want all true", result.StateRootWritable, result.AgentbusStateRootWritable, result.DaemonReachable)
	}
	if len(result.Skills) != 22 {
		t.Fatalf("skill statuses = %d, want 22: %#v", len(result.Skills), result.Skills)
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
	schema := `{"type":"object","required":["schema_version"],"properties":{"schema_version":{"const":"1"}}}`
	for _, tc := range []struct {
		name           string
		flags          []string
		wantNil        bool
		wantRetry      bool
		wantJSONSchema bool
	}{
		{name: "default"},
		{name: "write", flags: []string{"--write"}, wantRetry: true},
		{name: "strict", flags: []string{"--strict-contract"}, wantRetry: true},
		{name: "no_contract", flags: []string{"--no-contract"}, wantNil: true},
		{name: "json_schema", flags: []string{"--output-schema", schema}, wantRetry: true, wantJSONSchema: true},
		{name: "json_schema_strict", flags: []string{"--output-schema", schema, "--strict-contract"}, wantRetry: true, wantJSONSchema: true},
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
			if got.Contract == nil {
				t.Fatal("policy contract = nil")
			}
			if tc.wantJSONSchema {
				if got.Contract.Shape != nil || string(got.Contract.JSONSchema) != schema {
					t.Fatalf("policy contract = %#v, want JSON Schema only", got.Contract)
				}
				if !strings.Contains(got.Prologue, schema) {
					t.Fatalf("policy prologue = %q, want embedded JSON Schema", got.Prologue)
				}
			} else if got.Prologue == "" || got.Contract.Shape == nil {
				t.Fatalf("policy = %#v, want delegate-report shape contract", got)
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

func TestTaskOutputSchemaInputErrorsDoNotSubmit(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "inline_and_file",
			args: []string{"--output-schema", `{"type":"object"}`, "--output-schema-file", "schema.json"},
			want: "use only one of --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "inline_and_stdin",
			args: []string{"--output-schema", `{"type":"object"}`, "--output-schema-stdin"},
			want: "use only one of --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "file_and_stdin",
			args: []string{"--output-schema-file", "schema.json", "--output-schema-stdin"},
			want: "use only one of --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "no_contract_inline",
			args: []string{"--no-contract", "--output-schema", `{"type":"object"}`},
			want: "--no-contract cannot be used with --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "no_contract_file",
			args: []string{"--no-contract", "--output-schema-file", "schema.json"},
			want: "--no-contract cannot be used with --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "no_contract_stdin",
			args: []string{"--no-contract", "--output-schema-stdin"},
			want: "--no-contract cannot be used with --output-schema, --output-schema-file, or --output-schema-stdin",
		},
		{
			name: "schema_stdin_and_prompt_stdin",
			args: []string{"--output-schema-stdin", "--prompt-stdin"},
			want: "--output-schema-stdin cannot be used with --prompt-stdin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			args := append([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it"}, tc.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(`{"type":"object"}`), &stdout, &stderr); code == 0 {
				t.Fatalf("task code = 0, want error; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
			if len(fake.submits) != 0 {
				t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
			}
		})
	}
}

func TestTaskOutputSchemaFastFailsBeforeSubmit(t *testing.T) {
	for _, schema := range []string{`{"type":`, ""} {
		t.Run(fmt.Sprintf("schema_%q", schema), func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--output-schema", schema}, nil, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("task code = 0, want invalid schema error; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "jsonSchema must be valid JSON") {
				t.Fatalf("stderr = %q, want engine schema error", stderr.String())
			}
			if len(fake.submits) != 0 {
				t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
			}
		})
	}
}

func TestTaskOutputSchemaFileNotFoundDoesNotSubmit(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	missing := filepath.Join(t.TempDir(), "missing-schema.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--output-schema-file", missing}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task code = 0, want missing file error; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "read --output-schema-file") || !strings.Contains(stderr.String(), missing) {
		t.Fatalf("stderr = %q, want missing output schema file error", stderr.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
	}
}

func TestTaskOutputSchemaFileAndStdinReachPolicyAndMetadata(t *testing.T) {
	schema := `{"type":"object","required":["schema_version"],"properties":{"schema_version":{"const":"1"}}}`
	for _, tc := range []struct {
		name  string
		args  func(t *testing.T) []string
		stdin io.Reader
	}{
		{
			name: "file",
			args: func(t *testing.T) []string {
				path := filepath.Join(t.TempDir(), "schema.json")
				if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"--output-schema-file", path}
			},
		},
		{
			name:  "stdin",
			args:  func(*testing.T) []string { return []string{"--output-schema-stdin"} },
			stdin: strings.NewReader(schema),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			xdgState := t.TempDir()
			t.Setenv("XDG_STATE_HOME", xdgState)
			args := append([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it"}, tc.args(t)...)
			var stdout, stderr bytes.Buffer
			if code := run(args, tc.stdin, &stdout, &stderr); code != 0 {
				t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
			}
			turnPolicy := fake.submits[0].TaskSpec.Policy
			if turnPolicy == nil || turnPolicy.Contract == nil || string(turnPolicy.Contract.JSONSchema) != schema || turnPolicy.Contract.Shape != nil || turnPolicy.Retry == nil || turnPolicy.Retry.Max != 1 {
				t.Fatalf("submitted policy = %#v, want JSON Schema contract with retry", turnPolicy)
			}
			stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			meta, found, err := loadJobMetadata(stateDir, "job_fake")
			if err != nil || !found {
				t.Fatalf("load job metadata = %#v / %t / %v, want schema task metadata", meta, found, err)
			}
			if meta.ContractKind != contractKindJSONSchema {
				t.Fatalf("metadata contractKind = %q, want %q", meta.ContractKind, contractKindJSONSchema)
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
	assertOnlyRequestedStatusCall(t, fake.statuses, "job_background_reap")
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

func TestLaunchRequiresStrictContainmentCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(*testing.T) []string
	}{
		{
			name: "task",
			args: func(t *testing.T) []string {
				return []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it"}
			},
		},
		{
			name: "review",
			args: func(t *testing.T) []string {
				repo := newCommandGitFixture(t)
				writeCommandFixture(t, repo, "visible.go", "package visible\n// change\n")
				return []string{"review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree"}
			},
		},
		{
			name: "adversarial-review",
			args: func(t *testing.T) []string {
				repo := newCommandGitFixture(t)
				writeCommandFixture(t, repo, "visible.go", "package visible\n// change\n")
				return []string{"adversarial-review", "--backend", "codex", "--cwd", repo, "--scope", "working-tree"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hello := helloWithCapabilities()
			delete(hello.Capabilities, "admission.strictContainment")
			fake := &fakeAgentbusClient{hello: hello}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run(tc.args(t), nil, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s succeeded; stdout=%q", tc.name, stdout.String())
			}
			if want := "lacks capability `admission.strictContainment`"; !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if len(fake.submits) != 0 {
				t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
			}
		})
	}
}

func TestReadCommandsDoNotRequirePolicyCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		fake fakeAgentbusClient
	}{
		{
			name: "status",
			args: []string{"status", "--job", "job_status", "--json"},
			fake: fakeAgentbusClient{status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_status", State: engine.StateRunning}}}},
		},
		{
			name: "result",
			args: []string{"result", "--job", "job_result", "--json"},
			fake: fakeAgentbusClient{
				result: client.JobResult{
					JobID: "job_result",
					State: engine.StateCompleted,
					Result: &engine.ResultInfo{
						SHA256: strings.Repeat("a", 64),
					},
				},
				status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_result", State: engine.StateCompleted}}},
			},
		},
		{
			name: "cancel",
			args: []string{"cancel", "--job", "job_cancel", "--json"},
			fake: fakeAgentbusClient{cancel: client.JobCancelResult{JobID: "job_cancel", State: engine.StateCanceled}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			fake.hello = client.HelloResult{
				ProtocolVersion: 2,
				Capabilities:    map[string]bool{},
			}
			restore := stubAgentbusGlobals(t, &fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("%s code = %d, stderr = %q", tc.name, code, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("%s wrote no output", tc.name)
			}
		})
	}
}

func TestCommandHelpExcludesRemovedResumeAndEmbeddedFlags(t *testing.T) {
	for _, command := range []string{"task", "review", "adversarial-review"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_ = run([]string{command, "--help"}, nil, &stdout, &stderr)
			help := stdout.String() + stderr.String()
			for _, removed := range []string{"--resume", "--resume-session", "--fresh", "--embedded"} {
				if strings.Contains(help, removed) {
					t.Fatalf("%s help contains removed flag %q:\n%s", command, removed, help)
				}
			}
		})
	}
}

func TestTaskPassesThroughUnadvertisedCatalogModelAndEffort(t *testing.T) {
	fake := &fakeAgentbusClient{hello: client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"claude"},
		Capabilities:    helloWithCapabilities().Capabilities,
		BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"sonnet"}, Efforts: []string{"medium"}}},
	}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "claude", "--cwd", t.TempDir(),
		"--model", "claude-sonnet-4-6", "--effort", "high",
		"--prompt", "pass these values through", "--background", "--json",
	}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("task code = %d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
	}
	// Warnings must never contaminate the JSON stdout channel: stdout must be
	// exactly one decodable launch envelope with no warning text.
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
		t.Fatalf("stdout is not a single launch envelope: %v; raw=%q", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "warning:") {
		t.Fatalf("stdout contains warning text: %q", stdout.String())
	}
	spec := fake.submits[0].TaskSpec
	if spec.Model != "claude-sonnet-4-6" || spec.Effort != "high" {
		t.Fatalf("TaskSpec model/effort = %q/%q, want claude-sonnet-4-6/high", spec.Model, spec.Effort)
	}
	for _, want := range []string{
		`warning: model "claude-sonnet-4-6" is not advertised by agentbus for backend "claude" (advertised: sonnet); passing through — the backend is authoritative`,
		`warning: effort "high" is not advertised by agentbus for backend "claude" (advertised: medium); passing through — the backend is authoritative`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
		}
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

func helloWithCapabilities() client.HelloResult {
	return client.HelloResult{
		ProtocolVersion: 2,
		Backends:        []string{"codex", "claude"},
		Capabilities: map[string]bool{
			"admission.strictContainment": true,
			"policy.shape":                true,
			"policy.jsonSchema":           true,
			"policy.named":                true,
			"policy.retry":                true,
			"models.reported":             true,
		},
	}
}

type fakeAgentbusClient struct {
	hello     client.HelloResult
	submits   []client.JobSubmitParams
	submitErr error
	statuses  []client.JobStatusParams
	statusErr error
	status    client.JobStatusResult
	result    client.JobResult
	resultErr error
	cancel    client.JobCancelResult
	cancelErr error
}

func (f *fakeAgentbusClient) Close() error { return nil }

func (f *fakeAgentbusClient) HelloResult() client.HelloResult { return f.hello }

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
	if f.statusErr != nil {
		return client.JobStatusResult{}, f.statusErr
	}
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
