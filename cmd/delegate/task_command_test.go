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

func TestEnvelopeSchemas(t *testing.T) {
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
	if launch.Schema != envelopeSchema {
		t.Fatalf("launch schema = %d, want %d", launch.Schema, envelopeSchema)
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
	origin := envelopeOrigin{Skill: "delegate:rescue:claude", ParentClient: "claude-code", ParentSessionID: "parent-session", ParentAgent: "agent", Depth: "1"}
	withOrigin, err := newLaunchEnvelopeWithOrigin("job_envelope", engine.StateQueued, origin)
	if err != nil {
		t.Fatal(err)
	}
	if withOrigin.Origin == nil || *withOrigin.Origin != origin {
		t.Fatalf("launch with origin = %#v, want captured origin", withOrigin)
	}
}

func TestEnvelopeSchema2SubmissionFieldsRoundTrip(t *testing.T) {
	requestID := "delegate-11111111111111111111111111111111"
	launch, err := newLaunchEnvelopeWithOptions("job_schema2", engine.StateRetrying, launchEnvelopeOptions{
		RequestID:    requestID,
		Deduplicated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	var decodedLaunch LaunchEnvelope
	if err := json.Unmarshal(raw, &decodedLaunch); err != nil {
		t.Fatalf("launch round trip: %v; raw=%s", err, raw)
	}
	if decodedLaunch.Schema != 2 || decodedLaunch.RequestID != requestID || !decodedLaunch.Deduplicated || decodedLaunch.Status != string(engine.StateRetrying) {
		t.Fatalf("launch round trip = %#v", decodedLaunch)
	}

	terminal, err := newTerminalEnvelope("job_schema2", engine.StateCompleted, taskKind, contractKindShape, engine.ContractStamp{}, "", "", terminalEnvelopeOptions{
		RequestID:       requestID,
		Deduplicated:    true,
		DeduplicatedSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	var decodedTerminal TerminalEnvelope
	if err := json.Unmarshal(raw, &decodedTerminal); err != nil {
		t.Fatalf("terminal round trip: %v; raw=%s", err, raw)
	}
	if decodedTerminal.Schema != 2 || decodedTerminal.RequestID != requestID || !decodedTerminal.Deduplicated {
		t.Fatalf("terminal round trip = %#v", decodedTerminal)
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

func TestSetupCapabilityGateReportsMissingStrictContainment(t *testing.T) {
	hello := helloWithCapabilities()
	delete(hello.Capabilities, "admission.strictContainment")
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: hello})
	defer restore()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--json"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("setup succeeded, want strict-containment capability failure")
	}
	var result setupJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("setup JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if result.Ready || result.Agentbus.CapabilitiesOK || result.AdmissionStrictContainment {
		t.Fatalf("setup result=%#v, want not-ready strict containment failure", result)
	}
	if len(result.Agentbus.Missing) == 0 || result.Agentbus.Missing[0] != "admission.strictContainment" {
		t.Fatalf("missing capabilities=%#v, want strict containment first", result.Agentbus.Missing)
	}
	want := "agentbus " + minimumSupportedAgentbusVersion + " lacks capability `admission.strictContainment`; run mise-en-place install agentbus"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestSetupOutputIncludesReadinessFields(t *testing.T) {
	restore := stubAgentbusGlobals(t, &fakeAgentbusClient{hello: helloWithCapabilities()})
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "agentbus models.reported: true") || !strings.Contains(stdout.String(), "config file:") || !strings.Contains(stdout.String(), "config overridable: true") {
		t.Fatalf("setup stdout = %q, want model-reporting and config lines", stdout.String())
	}
	for _, line := range []string{"stateRootWritable: true", "agentbusStateRootWritable: true", "daemonReachable: true"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("setup stdout = %q, want %q", stdout.String(), line)
		}
	}
	for _, line := range []string{"agentbus minimum supported version: " + minimumSupportedAgentbusVersion, "agentbus version status: supported", "agentbusStateRoot:", "agentbusAutostartLockRoot:", "agentbusAutostartLockRootWritable: true", "admission.strictContainment: true", "pendingSubmissionIntentCount: 0", "unresolvedCleanupArtifactCount: 0", "ready: true"} {
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
	// Setup must be ready against a post-relocation agentbus that advertises
	// policy.shape=false: delegate owns shape validation, so that capability is not
	// required and must not appear in the required/missing sets.
	if !result.Agentbus.CapabilitiesOK || result.Agentbus.Capabilities["policy.shape"] {
		t.Fatalf("agentbus capabilities = %#v, want passing with policy.shape not required", result.Agentbus)
	}
	for _, capName := range append(result.Agentbus.Required, result.Agentbus.Missing...) {
		if capName == "policy.shape" {
			t.Fatalf("policy.shape must not be required/missing; required=%#v missing=%#v", result.Agentbus.Required, result.Agentbus.Missing)
		}
	}
	if !result.Agentbus.Capabilities["models.reported"] || result.Config.Path == "" || !result.Config.Overridable {
		t.Fatalf("setup config/model capability = %#v / %#v", result.Config, result.Agentbus.Capabilities)
	}
	if !result.StateRootWritable || !result.AgentbusStateRootWritable || !result.DaemonReachable {
		t.Fatalf("setup preflight fields = stateRootWritable=%t agentbusStateRootWritable=%t daemonReachable=%t, want all true", result.StateRootWritable, result.AgentbusStateRootWritable, result.DaemonReachable)
	}
	if result.AgentbusStateRoot == "" || result.AgentbusAutostartLockRoot == "" || !result.AgentbusAutostartLockRootWritable || !result.AdmissionStrictContainment || !result.Ready {
		t.Fatalf("setup D8 fields = root:%q lock:%q lockWritable:%t strict:%t ready:%t", result.AgentbusStateRoot, result.AgentbusAutostartLockRoot, result.AgentbusAutostartLockRootWritable, result.AdmissionStrictContainment, result.Ready)
	}
	if result.PendingSubmissionIntentCount == nil || *result.PendingSubmissionIntentCount != 0 || len(result.PendingSubmissionIntents) != 0 || result.UnresolvedCleanupArtifactCount == nil || *result.UnresolvedCleanupArtifactCount != 0 {
		t.Fatalf("setup pending=%v summaries=%#v unresolved=%v, want clean state", result.PendingSubmissionIntentCount, result.PendingSubmissionIntents, result.UnresolvedCleanupArtifactCount)
	}
	if result.PendingSubmissionIntents == nil {
		t.Fatalf("pendingSubmissionIntents is null, want an empty JSON array for a clean state")
	}
	if len(result.Skills) != 28 {
		t.Fatalf("skill statuses = %d, want 28: %#v", len(result.Skills), result.Skills)
	}
	for _, skill := range result.Skills {
		if skill.Target == "" || skill.Name == "" || skill.Path == "" {
			t.Fatalf("incomplete skill status: %#v", skill)
		}
		if skill.Status != "missing" {
			t.Fatalf("skill status = %#v, want missing in temporary HOME", skill)
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
		{name: "write", flags: []string{"--write"}},
		{name: "strict", flags: []string{"--strict-contract"}},
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
				if fake.submits[0].TaskSpec.Prompt != "do it" {
					t.Fatalf("prompt = %q, want original prompt for no-contract", fake.submits[0].TaskSpec.Prompt)
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
					t.Fatalf("policy prologue = %q, want included JSON Schema", got.Prologue)
				}
				if fake.submits[0].TaskSpec.Prompt != "do it" {
					t.Fatalf("JSON Schema prompt = %q, want original prompt", fake.submits[0].TaskSpec.Prompt)
				}
			} else if got.Prologue == "" || got.Contract.Shape == nil {
				t.Fatalf("policy = %#v, want delegate-report shape contract", got)
			} else if want := promptWithReportFormat(t, "do it"); fake.submits[0].TaskSpec.Prompt != want {
				t.Fatalf("delegate-report prompt = %q, want %q", fake.submits[0].TaskSpec.Prompt, want)
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
	// Both a malformed schema and an empty schema must fast-fail before any
	// submit. An empty schema is treated as no contract variant (engine
	// normalizes an empty/JSON-null variant to absent).
	for _, tc := range []struct {
		schema  string
		wantErr string
	}{
		{schema: `{"type":`, wantErr: "jsonSchema must be valid JSON"},
		{schema: "", wantErr: "contract must include exactly one"},
	} {
		t.Run(fmt.Sprintf("schema_%q", tc.schema), func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--output-schema", tc.schema}, nil, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("task code = 0, want pre-submit schema error; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantErr)
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

func TestTaskWaitDelegateReportValidationAndCorrection(t *testing.T) {
	goodReport := compliantReport()
	badReport := malformedDelegateReport()
	for _, tc := range []struct {
		name         string
		jobIDs       []string
		results      map[string]client.JobResult
		wantJobID    string
		wantSubmits  int
		wantStatus   engine.ContractStatus
		wantRetry    bool
		wantAttempts int
	}{
		{
			name:   "compliant_no_resubmit",
			jobIDs: []string{"job_report_1"},
			results: map[string]client.JobResult{
				"job_report_1": reportJobResult("job_report_1", goodReport),
			},
			wantJobID:    "job_report_1",
			wantSubmits:  1,
			wantStatus:   engine.ContractCompliant,
			wantAttempts: 1,
		},
		{
			name:   "noncompliant_one_report_only_resubmit",
			jobIDs: []string{"job_report_1", "job_report_2"},
			results: map[string]client.JobResult{
				"job_report_1": reportJobResult("job_report_1", badReport),
				"job_report_2": reportJobResult("job_report_2", goodReport),
			},
			wantJobID:    "job_report_2",
			wantSubmits:  2,
			wantStatus:   engine.ContractRetried,
			wantRetry:    true,
			wantAttempts: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &reportCorrectionRoundTripClient{hello: helloWithCapabilities(), jobIDs: tc.jobIDs, resultsByJobID: tc.results}
			restore := stubAgentbusClientGlobals(t, fake)
			defer restore()
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--wait", "--json"}, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
			}
			if len(fake.submits) != tc.wantSubmits {
				t.Fatalf("JobSubmit calls = %d, want %d", len(fake.submits), tc.wantSubmits)
			}
			for i, submit := range fake.submits {
				if submit.TaskSpec.Policy == nil || submit.TaskSpec.Policy.Contract == nil || submit.TaskSpec.Policy.Contract.Shape == nil {
					t.Fatalf("submit %d policy = %#v, want delegate-report shape policy", i, submit.TaskSpec.Policy)
				}
				if submit.TaskSpec.Policy.Retry != nil {
					t.Fatalf("submit %d retry = %#v, want nil delegate-report retry", i, submit.TaskSpec.Policy.Retry)
				}
			}

			var env TerminalEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("terminal JSON invalid: %v; raw=%q", err, stdout.String())
			}
			if env.JobID != tc.wantJobID || env.Status != engine.StateCompleted {
				t.Fatalf("terminal envelope job/status = %s/%s, want %s/%s", env.JobID, env.Status, tc.wantJobID, engine.StateCompleted)
			}
			if env.Contract.Status != tc.wantStatus || env.Contract.RetryUsed != tc.wantRetry || env.Contract.Attempts != tc.wantAttempts {
				t.Fatalf("contract = %#v, want status=%s retry=%t attempts=%d", env.Contract, tc.wantStatus, tc.wantRetry, tc.wantAttempts)
			}

			if tc.wantSubmits == 2 {
				correction := fake.submits[1]
				if correction.TaskSpec.Write {
					t.Fatal("correction submit Write=true, want read-only")
				}
				prompt := correction.TaskSpec.Prompt
				for _, want := range []string{
					"This is a report-format correction",
					badReport,
					"line 1 must be exactly one of: complete, partial, blocked",
					"add a section header `# Criteria scored` or `Criteria scored:`",
					"Emit ONLY the corrected report. Make NO other changes. Stay read-only.",
				} {
					if !strings.Contains(prompt, want) {
						t.Fatalf("correction prompt missing %q:\n%s", want, prompt)
					}
				}
				if correction.TaskSpec.Tags[reportCorrectionTag] != "true" || correction.TaskSpec.Tags[reportCorrectionOfTag] != "job_report_1" {
					t.Fatalf("correction tags = %#v, want report correction provenance", correction.TaskSpec.Tags)
				}
			}
		})
	}
}

func TestTaskWaitCorrectionFailureFallsBackToOriginalResult(t *testing.T) {
	badReport := malformedDelegateReport()
	original := reportJobResult("job_report_1", badReport)
	original.CleanupDisposition = cleanupDispositionVerifiedAbsent
	fake := &reportCorrectionRoundTripClient{
		hello:  helloWithCapabilities(),
		jobIDs: []string{"job_report_1", "job_report_2"},
		resultsByJobID: map[string]client.JobResult{
			"job_report_1": original,
			"job_report_2": {
				JobID:              "job_report_2",
				SessionID:          "session_job_report_2",
				State:              engine.StateFailed,
				CleanupDisposition: cleanupDispositionNoExecutionPossible,
			},
		},
	}
	restore := stubAgentbusClientGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--wait", "--json"}, nil, &stdout, &stderr)
	if want := engine.ExitCodeForState(engine.StateCompletedNoncompliant); code != want {
		t.Fatalf("task code = %d, want %d; stderr = %q stdout = %q", code, want, stderr.String(), stdout.String())
	}
	if len(fake.submits) != 2 {
		t.Fatalf("JobSubmit calls = %d, want original plus one correction attempt", len(fake.submits))
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("terminal JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if env.JobID != "job_report_1" || env.Status != engine.StateCompletedNoncompliant {
		t.Fatalf("terminal envelope job/status = %s/%s, want original completed_noncompliant", env.JobID, env.Status)
	}
	if env.ResultSHA256 == nil || *env.ResultSHA256 != rawSHA256(badReport) || env.ResultUnavailableReason != "" {
		t.Fatalf("result fields sha=%#v reason=%q, want original authoritative result", env.ResultSHA256, env.ResultUnavailableReason)
	}
	if env.CleanupDisposition != cleanupDispositionVerifiedAbsent {
		t.Fatalf("cleanup disposition = %q, want original %q", env.CleanupDisposition, cleanupDispositionVerifiedAbsent)
	}
	if !strings.Contains(stderr.String(), "warning: delegate-report correction job_report_2 for job_report_1 did not produce an authoritative result body; using original terminal result") {
		t.Fatalf("stderr = %q, want correction fallback warning", stderr.String())
	}
}

func TestTaskSubmitFailurePreservesIntentAndHandoffForRecovery(t *testing.T) {
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
	if len(fake.submits) != maxSubmissionAttempts {
		t.Fatalf("JobSubmit calls = %d, want retry attempts %d", len(fake.submits), maxSubmissionAttempts)
	}
	requestID := fake.submits[0].RequestID
	if !strings.Contains(stderr.String(), "delegate task --recover-request "+requestID) {
		t.Fatalf("stderr = %q, want recovery command for %s", stderr.String(), requestID)
	}
	if _, err := os.Stat(handoffResult.HandoffPath); err != nil {
		t.Fatalf("handoff file was not preserved for unresolved submission: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "job-input.*.prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("unacknowledged submit created job input files: %#v", matches)
	}
	intent, found, err := loadSubmissionIntent(stateDir, requestID)
	if err != nil || !found {
		t.Fatalf("submission intent found=%v err=%v", found, err)
	}
	if intent.Phase != submissionPhaseBlocked || intent.Params.TaskSpec.Prompt != promptWithReportFormat(t, "durable prompt") || intent.LastError == nil {
		t.Fatalf("intent = %#v, want blocked durable prompt with last error", intent)
	}
}

func TestTaskSubmissionIntentPersistFailureBeforeLaunchAborts(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID: "job_meta_warning",
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldSave := saveSubmissionIntent
	saveSubmissionIntent = func(string, submissionIntent) error {
		return errors.New("intent store read-only")
	}
	defer func() { saveSubmissionIntent = oldSave }()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do not launch"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task code = 0, want metadata failure; stdout = %q", stdout.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
	}
	if !strings.Contains(stderr.String(), "persist submission intent before launch: intent store read-only") {
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
			JobID:              "job_background_reap",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
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
			JobID:              "job_result_reap",
			State:              engine.StateCompleted,
			CleanupDisposition: cleanupDispositionVerifiedAbsent,
			Result: &engine.ResultInfo{
				SHA256: strings.Repeat("b", 64),
			},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
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

func TestTaskBackgroundTerminalDedupReturnsLaunchWithoutCorrection(t *testing.T) {
	badReport := malformedDelegateReport()
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID:        "job_background_dedup",
			State:        engine.StateCompleted,
			Deduplicated: true,
		},
		result: reportJobResult("job_background_dedup", badReport),
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "background prompt", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want only launch submit", len(fake.submits))
	}
	if len(fake.results) != 0 || len(fake.statuses) != 0 {
		t.Fatalf("result/status calls = %d/%d, want none for background launch", len(fake.results), len(fake.statuses))
	}
	var env LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("launch JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if env.JobID != "job_background_dedup" || env.Status != string(engine.StateCompleted) || !env.Deduplicated || env.ResultSHA256 != nil {
		t.Fatalf("launch envelope = %#v, want completed dedup launch without result", env)
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
			fake: fakeAgentbusClient{
				cancel: client.JobCancelResult{JobID: "job_cancel", State: engine.StateRunning},
				status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_cancel", State: engine.StateRunning}}},
			},
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

func TestCommandHelpExcludesRemovedLaunchFlags(t *testing.T) {
	for _, command := range []string{"task", "review", "adversarial-review"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_ = run([]string{command, "--help"}, nil, &stdout, &stderr)
			help := stdout.String() + stderr.String()
			for _, removed := range []string{"--re" + "sume", "--re" + "sume-session", "--fr" + "esh", "--em" + "bedded"} {
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

func TestTaskReadOnlyHintStaysOnStderr(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    []string
		wantHint bool
	}{
		{name: "read_only", wantHint: true},
		{name: "write", flags: []string{"--write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			args := append([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--background", "--json"}, tc.flags...)
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
			}
			var launch LaunchEnvelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
				t.Fatalf("stdout is not valid launch JSON: %v; raw=%q", err, stdout.String())
			}
			if strings.Contains(stdout.String(), readOnlyTaskHint) {
				t.Fatalf("stdout contains read-only hint: %q", stdout.String())
			}
			if got := strings.Count(stderr.String(), readOnlyTaskHint); (got == 1) != tc.wantHint {
				t.Fatalf("read-only hint count = %d, want present=%t; stderr=%q", got, tc.wantHint, stderr.String())
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
		return []byte(agentbusVersionFixtureOutput(minimumSupportedAgentbusVersion)), nil
	}
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	}
}

func agentbusVersionFixtureOutput(version string) string {
	return "agentbus " + version + "\n"
}

func helloWithCapabilities() client.HelloResult {
	return client.HelloResult{
		ProtocolVersion: 2,
		Backends:        []string{"codex", "claude"},
		Capabilities: map[string]bool{
			"admission.strictContainment": true,
			// Mirror the post-relocation agentbus, which advertises policy.shape as
			// false (delegate owns shape validation). The default report path must
			// still submit successfully against this.
			"policy.shape":      false,
			"policy.jsonSchema": true,
			"policy.named":      true,
			"policy.retry":      true,
			"models.reported":   true,
		},
	}
}

type fakeAgentbusClient struct {
	hello        client.HelloResult
	submits      []client.JobSubmitParams
	submitResult client.JobSubmitResult
	submitErr    error
	statuses     []client.JobStatusParams
	statusErr    error
	status       client.JobStatusResult
	results      []client.JobResultParams
	result       client.JobResult
	resultErr    error
	cancels      []client.JobCancelParams
	cancel       client.JobCancelResult
	cancelErr    error
}

type reportCorrectionRoundTripClient struct {
	hello          client.HelloResult
	jobIDs         []string
	resultsByJobID map[string]client.JobResult
	submits        []client.JobSubmitParams
	statuses       []client.JobStatusParams
	results        []client.JobResultParams
}

func (f *reportCorrectionRoundTripClient) Close() error { return nil }

func (f *reportCorrectionRoundTripClient) HelloResult() client.HelloResult { return f.hello }

func (f *reportCorrectionRoundTripClient) JobSubmit(_ context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	f.submits = append(f.submits, params)
	if len(f.submits) > len(f.jobIDs) {
		return client.JobSubmitResult{}, fmt.Errorf("unexpected JobSubmit call %d", len(f.submits))
	}
	return client.JobSubmitResult{JobID: f.jobIDs[len(f.submits)-1], State: engine.StateQueued}, nil
}

func (f *reportCorrectionRoundTripClient) JobStatus(_ context.Context, params client.JobStatusParams) (client.JobStatusResult, error) {
	f.statuses = append(f.statuses, params)
	result, ok := f.resultsByJobID[params.JobID]
	if !ok {
		return client.JobStatusResult{}, fmt.Errorf("unexpected JobStatus for %s", params.JobID)
	}
	return client.JobStatusResult{Jobs: []client.JobStatus{{
		JobID:              result.JobID,
		SessionID:          result.SessionID,
		State:              result.State,
		CleanupDisposition: result.CleanupDisposition,
		ModelReported:      result.ModelReported,
	}}}, nil
}

func (f *reportCorrectionRoundTripClient) JobResult(_ context.Context, params client.JobResultParams) (client.JobResult, error) {
	f.results = append(f.results, params)
	result, ok := f.resultsByJobID[params.JobID]
	if !ok {
		return client.JobResult{}, fmt.Errorf("unexpected JobResult for %s", params.JobID)
	}
	return result, nil
}

func (f *reportCorrectionRoundTripClient) JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error) {
	return client.JobCancelResult{}, errors.New("unexpected JobCancel")
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
	if f.submitResult.JobID != "" || f.submitResult.State != "" || f.submitResult.Deduplicated {
		submitted := f.submitResult
		if submitted.JobID == "" {
			submitted.JobID = jobID
		}
		if submitted.State == "" {
			submitted.State = engine.StateQueued
		}
		return submitted, nil
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
	return client.JobStatusResult{Jobs: []client.JobStatus{{
		JobID:              f.result.JobID,
		SessionID:          f.result.SessionID,
		State:              f.result.State,
		CleanupDisposition: f.result.CleanupDisposition,
		ModelReported:      f.result.ModelReported,
	}}}, nil
}

func (f *fakeAgentbusClient) JobResult(_ context.Context, params client.JobResultParams) (client.JobResult, error) {
	f.results = append(f.results, params)
	if f.resultErr != nil {
		return client.JobResult{}, f.resultErr
	}
	if f.result.JobID == "" {
		return client.JobResult{}, errors.New("result not ready")
	}
	return f.result, nil
}

func (f *fakeAgentbusClient) JobCancel(_ context.Context, params client.JobCancelParams) (client.JobCancelResult, error) {
	f.cancels = append(f.cancels, params)
	if f.cancelErr != nil {
		return client.JobCancelResult{}, f.cancelErr
	}
	if f.cancel.JobID != "" {
		return f.cancel, nil
	}
	return client.JobCancelResult{}, errors.New("unexpected JobCancel")
}

func compliantReport() string {
	spec, err := policy.DelegateReportSpec()
	if err != nil {
		panic("invalid delegate report test spec")
	}
	var shape struct {
		FirstLineEnum    []string `json:"firstLineEnum"`
		RequiredSections []string `json:"requiredSections"`
	}
	if len(spec.Shape) == 0 || json.Unmarshal(spec.Shape, &shape) != nil || len(shape.FirstLineEnum) == 0 {
		panic("invalid delegate report test spec")
	}
	lines := []string{shape.FirstLineEnum[0], ""}
	for _, section := range shape.RequiredSections {
		lines = append(lines, section+":", "- observed: "+strings.ToLower(section)+" fixture.", "")
	}
	return strings.Join(lines, "\n")
}

func malformedDelegateReport() string {
	report := compliantReport()
	report = strings.Replace(report, "complete", "done", 1)
	return strings.Replace(report, "Criteria scored:", "**Criteria scored**:", 1)
}

func reportJobResult(jobID, report string) client.JobResult {
	return client.JobResult{
		JobID:     jobID,
		SessionID: "session_" + jobID,
		State:     engine.StateCompleted,
		Result: &engine.ResultInfo{
			SHA256: rawSHA256(report),
			Bytes:  int64(len(report)),
			Text:   report,
		},
	}
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

func promptWithReportFormat(t *testing.T, prompt string) string {
	t.Helper()
	return strings.TrimRight(prompt, "\n") + "\n\n" + reportFormatBlock(t)
}

func reportFormatBlock(t *testing.T) string {
	t.Helper()
	block, err := policy.DelegateReportFormatBlock()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(block, "\n")
}

func ptr[T any](v T) *T {
	return &v
}

func rawSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
