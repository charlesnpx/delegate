package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
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
	terminal, err := newTerminalEnvelope("job_envelope", engine.StateCompleted, taskKind, &stamp, strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"kind":"task"`,
		`"contract":{"status":"compliant"`,
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

	terminal, err := newTerminalEnvelope("job_schema2", engine.StateCompleted, taskKind, nil, "", "", terminalEnvelopeOptions{
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

func TestTimeoutEnvelopeResolution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		submit     client.JobSubmitResult
		want       config.DimensionResolution
		wantSubmit *int64
	}{
		{
			name: "explicit timeout is flag sourced",
			args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "45s", "--background", "--json"},
			submit: client.JobSubmitResult{Timeout: &engine.TimeoutResolution{
				Requested: int64Pointer(45000),
				Effective: 45000,
				Source:    engine.TimeoutSourceClient,
			}},
			want:       config.DimensionResolution{Requested: "45s", Effective: "45s", Source: "flag"},
			wantSubmit: int64Pointer(45000),
		},
		{
			name: "omitted timeout stays unknown without daemon resolution",
			args: []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--background", "--json"},
			want: config.DimensionResolution{Source: "unknown"},
		},
		{
			name:       "explicit timeout stays unknown without daemon resolution",
			args:       []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "timeout", "--timeout", "45s", "--background", "--json"},
			want:       config.DimensionResolution{Requested: "45s", Source: "unknown"},
			wantSubmit: int64Pointer(45000),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities(), submitResult: tc.submit}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			var envelope LaunchEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("launch JSON=%q: %v", stdout.String(), err)
			}
			if envelope.Timeout != tc.want {
				t.Fatalf("timeout envelope=%#v, want %#v", envelope.Timeout, tc.want)
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submits=%d, want 1", len(fake.submits))
			}
			if got := fake.submits[0].TaskSpec.TimeoutMs; !equalInt64Pointers(got, tc.wantSubmit) {
				t.Fatalf("submitted timeout=%v, want %v", got, tc.wantSubmit)
			}
		})
	}

	t.Run("omitted timeout uses typed daemon resolution", func(t *testing.T) {
		resolution := timeoutResolutionForSubmission(0, false, client.JobSubmitResult{Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}})
		want := config.DimensionResolution{Effective: "30m0s", Source: "daemon"}
		if resolution != want {
			t.Fatalf("launch timeout=%#v, want %#v", resolution, want)
		}
	})

	t.Run("typed daemon response resolves terminal envelope", func(t *testing.T) {
		resolution := timeoutResolutionForSubmission(0, false, client.JobSubmitResult{Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}})
		terminal, err := newTerminalEnvelope("job_timeout", engine.StateTimedOut, taskKind, nil, "", "", terminalEnvelopeOptions{Timeout: resolution})
		if err != nil {
			t.Fatal(err)
		}
		want := config.DimensionResolution{Effective: "30m0s", Source: "daemon"}
		if terminal.Timeout != want {
			t.Fatalf("terminal timeout=%#v, want %#v", terminal.Timeout, want)
		}
	})

	t.Run("absent typed timeout stays unknown in JSON", func(t *testing.T) {
		resolution := timeoutResolutionForSubmission(0, false, client.JobSubmitResult{})
		launch, err := newLaunchEnvelopeWithOptions("job_timeout_unknown", engine.StateQueued, launchEnvelopeOptions{Timeout: resolution})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(launch)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if got, want := decoded["timeout"], map[string]any{"source": "unknown"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("timeout JSON=%#v, want %#v; raw=%s", got, want, raw)
		}
	})

	for _, command := range []string{"task", "review"} {
		t.Run(command+" help explains zero timeout", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_ = run([]string{command, "--help"}, nil, &stdout, &stderr)
			if !strings.Contains(stderr.String(), "0 leaves the deadline to the daemon default; envelope.timeout is authoritative") {
				t.Fatalf("help=%q", stderr.String())
			}
		})
	}
}

func TestMergeAcknowledgedJobMetadataUpgradesUnknownTimeout(t *testing.T) {
	merged := mergeAcknowledgedJobMetadata(
		jobMetadata{Timeout: config.DimensionResolution{Source: "unknown"}},
		jobMetadata{Timeout: config.DimensionResolution{Effective: "30m0s", Source: "daemon"}},
	)
	want := config.DimensionResolution{Effective: "30m0s", Source: "daemon"}
	if merged.Timeout != want {
		t.Fatalf("timeout=%#v, want %#v", merged.Timeout, want)
	}
}

func TestStatusOnlyTerminalTimeoutPrecedesLaunchMetadata(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "job_status_timeout"
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:   jobID,
		Kind:    taskKind,
		Timeout: config.DimensionResolution{Effective: "30m0s", Source: "daemon"},
	}); err != nil {
		t.Fatal(err)
	}
	terminal := terminalJobResultFromStatus(client.JobStatus{
		JobID:   jobID,
		State:   engine.StateOrphaned,
		Timeout: &engine.TimeoutResolution{Effective: int64((90 * time.Minute) / time.Millisecond), Source: engine.TimeoutSourceDaemonDefault},
	})
	env, err := terminalEnvelopeFromJobResultWithOptions(stateDir, terminal.result, terminal.envelopeOptions(terminalEnvelopeOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (config.DimensionResolution{Effective: "1h30m0s", Source: "daemon"}); env.Timeout != want {
		t.Fatalf("terminal timeout=%#v, want daemon status resolution %#v", env.Timeout, want)
	}
}

func TestLegacyTimeoutMetadataCannotSupplyTerminalEffectiveValue(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "job_legacy_timeout"
	legacy := jobMetadata{
		Schema: 1,
		JobID:  jobID,
		Kind:   taskKind,
		Timeout: config.DimensionResolution{
			Requested: "45s",
			Effective: "45s",
			Source:    "flag",
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := terminalEnvelopeFromJobResultWithOptions(stateDir, client.JobResult{
		JobID: jobID,
		State: engine.StateCompleted,
	}, terminalEnvelopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := config.DimensionResolution{Requested: "45s", Source: "unknown"}
	if env.Timeout != want {
		t.Fatalf("terminal timeout=%#v, want legacy effective value excluded as %#v", env.Timeout, want)
	}
}

func TestMissingBackendProfileMetadataEmitsUnknown(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		result: client.JobResult{JobID: "job_legacy_backend_profile", State: engine.StateCompleted},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	stateDir, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	jobID := "job_legacy_backend_profile"
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// This is pre-fix metadata: it has no backend_profile key at all.
	raw, err := json.Marshal(map[string]any{
		"schema": jobMetadataSchema,
		"job_id": jobID,
		"kind":   taskKind,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("result JSON=%q: %v", stdout.String(), err)
	}
	if want := (config.DimensionResolution{Source: "unknown"}); env.BackendProfile != want {
		t.Fatalf("result backend_profile=%#v, want missing metadata reported as %#v", env.BackendProfile, want)
	}
}

func TestSchemaLessTimeoutMetadataIsSanitizedWhenCleanupRewritesIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		schemaNull bool
	}{
		{name: "absent schema"},
		{name: "null schema", schemaNull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.Chmod(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			jobID := "job_schema_less_timeout"
			inputPath := filepath.Join(stateDir, "job-input.txt")
			if err := os.WriteFile(inputPath, []byte("prompt"), 0o600); err != nil {
				t.Fatal(err)
			}
			legacy := map[string]any{
				"job_id":         jobID,
				"kind":           taskKind,
				"job_input_path": inputPath,
				"timeout": map[string]string{
					"requested": "45s",
					"effective": "45s",
					"source":    "flag",
				},
			}
			if tc.schemaNull {
				legacy["schema"] = nil
			}
			raw, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			dir, err := jobMetadataDir(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), raw, 0o600); err != nil {
				t.Fatal(err)
			}

			// Terminal cleanup changes only local artifact bookkeeping, but it must
			// not launder the legacy requested timeout into schema-2 trust.
			if err := cleanupJobInput(stateDir, jobID, "", engine.StateCompleted, cleanupDispositionVerifiedAbsent, newLocalCleanupWarnings(io.Discard)); err != nil {
				t.Fatal(err)
			}
			meta, found, err := loadJobMetadata(stateDir, jobID)
			if err != nil || !found {
				t.Fatalf("rewritten metadata found=%v err=%v", found, err)
			}
			want := config.DimensionResolution{Requested: "45s", Source: "unknown"}
			if meta.Schema != jobMetadataSchema || meta.Timeout != want {
				t.Fatalf("rewritten metadata schema=%d timeout=%#v, want schema=%d timeout=%#v", meta.Schema, meta.Timeout, jobMetadataSchema, want)
			}

			env, err := terminalEnvelopeFromJobResultWithOptions(stateDir, client.JobResult{
				JobID: jobID,
				State: engine.StateCompleted,
			}, terminalEnvelopeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if env.Timeout != want {
				t.Fatalf("terminal timeout=%#v, want sanitized fallback %#v", env.Timeout, want)
			}
		})
	}
}

func int64Pointer(value int64) *int64 { return &value }

func equalInt64Pointers(left, right *int64) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
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
	if err := saveJobMetadata(stateDir, jobMetadata{JobID: "job_schema", Kind: taskKind}); err != nil {
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
	if env.Contract == nil || env.Contract.ContractSHA256 != validation.ContractSHA256 {
		t.Fatalf("terminal envelope = %#v, want Agentbus contract hash %q", env, validation.ContractSHA256)
	}
	if len(env.Contract.Missing) != len(validation.Missing) || env.Contract.Missing[0] != validation.Missing[0] {
		t.Fatalf("terminal missing = %#v, want schema violations %#v", env.Contract.Missing, validation.Missing)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"contractKind"`)) || !bytes.Contains(raw, []byte(`"contractSha256":"`+validation.ContractSHA256+`"`)) {
		t.Fatalf("terminal envelope JSON = %s, want only the Agentbus contract stamp", raw)
	}
}

func TestTaskPolicyWiring(t *testing.T) {
	t.Run("no schema submits nil policy", func(t *testing.T) {
		fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--background"}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
		}
		if len(fake.submits) != 1 || fake.submits[0].TaskSpec.Policy != nil {
			t.Fatalf("submits = %#v, want one nil-policy task", fake.submits)
		}
	})

	t.Run("schema file reaches Agentbus policy unchanged", func(t *testing.T) {
		schema := []byte(`{"type":"object","required":["schema_version"]}`)
		path := filepath.Join(t.TempDir(), "output-schema.json")
		if err := os.WriteFile(path, schema, 0o600); err != nil {
			t.Fatal(err)
		}
		fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--output-schema-file", path, "--background"}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
		}
		if len(fake.submits) != 1 {
			t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
		}
		policy := fake.submits[0].TaskSpec.Policy
		if policy == nil || policy.Contract == nil || !bytes.Equal(policy.Contract.JSONSchema, schema) || policy.Contract.Shape != nil || policy.Contract.Named != "" {
			t.Fatalf("policy contract = %#v, want JSON Schema bytes only", policy)
		}
		if policy.Prologue != "" || policy.Retry == nil || policy.Retry.Max != 1 || !strings.Contains(policy.Retry.Template, "{{missing}}") {
			t.Fatalf("policy = %#v, want direct contract and one Agentbus retry", policy)
		}
	})
}

func TestRemovedContractFlagsAreUnknown(t *testing.T) {
	for _, flag := range []string{"--strict-contract", "--no-contract"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", flag}, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "flag provided but not defined") {
				t.Fatalf("code=%d stderr=%q, want unknown flag", code, stderr.String())
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

func TestTaskOutputSchemaFileAndStdinReachPolicy(t *testing.T) {
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
		})
	}
}

func TestTaskWaitSubmitsOnlyOneJob(t *testing.T) {
	resultText := testResultText()
	result := client.JobResult{
		JobID:     "job_result_1",
		SessionID: "session_result_1",
		State:     engine.StateCompleted,
		Result: &engine.ResultInfo{
			Text:   resultText,
			SHA256: rawSHA256(resultText),
			Bytes:  int64(len(resultText)),
		},
	}
	fake := &fakeAgentbusClient{
		hello:        helloWithCapabilities(),
		submitResult: client.JobSubmitResult{JobID: result.JobID, State: engine.StateCompleted},
		result:       result,
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--wait", "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want exactly one task job", len(fake.submits))
	}
}

func TestResultFetchesOnceAndNeverSubmits(t *testing.T) {
	jobID := "job_result_once"
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:  jobID,
			State:  engine.StateCompleted,
			Result: &engine.ResultInfo{SHA256: strings.Repeat("a", 64)},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"result", "--job", jobID, "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("result code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if len(fake.results) != 1 || len(fake.submits) != 0 {
		t.Fatalf("job.result calls=%d job.submit calls=%d, want 1 and 0", len(fake.results), len(fake.submits))
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
	if intent.Phase != submissionPhaseBlocked || intent.Params.TaskSpec.Prompt != "durable prompt" || intent.LastError == nil {
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

func TestTaskBackgroundTerminalDedupReturnsLaunchWithoutResultFetch(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID:        "job_background_dedup",
			State:        engine.StateCompleted,
			Deduplicated: true,
		},
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

func TestTaskAdvertisedCatalogValuesKeepNormalEnvelopeResolution(t *testing.T) {
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
		"--model", "sonnet", "--effort", "medium",
		"--prompt", "use advertised values", "--background", "--json",
	}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("task code = %d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
	}
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
		t.Fatalf("stdout is not a single launch envelope: %v; raw=%q", err, stdout.String())
	}
	if launch.Model.Source != "flag" || launch.Effort.Source != "flag" {
		t.Fatalf("launch model/effort source = %q/%q, want flag/flag", launch.Model.Source, launch.Effort.Source)
	}
	if launch.Model.Validated != nil || launch.Effort.Validated != nil {
		t.Fatalf("advertised launch validation markers = %#v/%#v, want omitted", launch.Model.Validated, launch.Effort.Validated)
	}
	if strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("advertised values warning = %q", stderr.String())
	}
}

func TestTaskPassesThroughUnadvertisedCatalogModelAndEffort(t *testing.T) {
	fake := &fakeAgentbusClient{hello: client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"claude"},
		Capabilities:    helloWithCapabilities().Capabilities,
		BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"sonnet"}, Efforts: []string{"medium"}}},
	}, result: client.JobResult{JobID: "job_fake", State: engine.StateCompleted}}
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
	if launch.Model.Validated == nil || *launch.Model.Validated || launch.Effort.Validated == nil || *launch.Effort.Validated {
		t.Fatalf("launch validation markers = %#v/%#v, want false/false", launch.Model.Validated, launch.Effort.Validated)
	}
	if launch.Model.Source != "flag" || launch.Effort.Source != "flag" {
		t.Fatalf("launch model/effort source = %q/%q, want flag/flag", launch.Model.Source, launch.Effort.Source)
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
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"result", "--job", launch.JobID, "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("result code = %d stderr=%q", code, stderr.String())
	}
	var terminal TerminalEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &terminal); err != nil {
		t.Fatalf("stdout is not a terminal envelope: %v; raw=%q", err, stdout.String())
	}
	if terminal.Model.Validated == nil || *terminal.Model.Validated || terminal.Effort.Validated == nil || *terminal.Effort.Validated {
		t.Fatalf("terminal validation markers = %#v/%#v, want false/false", terminal.Model.Validated, terminal.Effort.Validated)
	}
}

func TestTaskStrictModelRejectsUnadvertisedCatalogValue(t *testing.T) {
	fake := &fakeAgentbusClient{hello: client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"claude"},
		Capabilities:    helloWithCapabilities().Capabilities,
		BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"sonnet"}, Efforts: []string{"medium"}}},
	}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	// "Rejected before submission" has to mean no durable artifact, not merely
	// no JobSubmit call: an intent persisted before the refusal would leave a
	// recoverable pending record for a job that never existed.
	intentWrites := 0
	oldSave := saveSubmissionIntent
	saveSubmissionIntent = func(dir string, intent submissionIntent) error {
		intentWrites++
		return oldSave(dir, intent)
	}
	defer func() { saveSubmissionIntent = oldSave }()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "claude", "--cwd", t.TempDir(),
		"--model", "not-sonnet", "--prompt", "reject before launch", "--background", "--json", "--strict-model",
	}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("task code = 0, want strict-model rejection; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(fake.submits) != 0 {
		t.Fatalf("JobSubmit calls = %d, want 0", len(fake.submits))
	}
	if intentWrites != 0 {
		t.Fatalf("submission intent writes = %d, want 0 before a strict-model refusal", intentWrites)
	}
	if stdout.Len() != 0 {
		t.Fatalf("strict-model stdout = %q, want no launch envelope", stdout.String())
	}
	// The warning and the rejection share the identifying half but must not
	// share the trailing clause: nothing passed through, so saying so would be
	// false in the very message explaining the refusal.
	identity := `model "not-sonnet" is not advertised by agentbus for backend "claude" (advertised: sonnet)`
	wantWarning := "warning: " + identity + "; passing through — the backend is authoritative"
	wantRejection := identity + "; --strict-model rejected it before submission"
	if !strings.Contains(stderr.String(), wantWarning) {
		t.Fatalf("strict-model stderr = %q, want warning %q", stderr.String(), wantWarning)
	}
	if !strings.Contains(stderr.String(), wantRejection) {
		t.Fatalf("strict-model stderr = %q, want rejection %q", stderr.String(), wantRejection)
	}
	if strings.Count(stderr.String(), "passing through") != 1 {
		t.Fatalf("strict-model stderr = %q, want the passing-through clause only in the warning", stderr.String())
	}
}

func TestTaskStrictModelAllowsUnknownEffortCatalog(t *testing.T) {
	fake := &fakeAgentbusClient{hello: client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"claude"},
		Capabilities:    helloWithCapabilities().Capabilities,
		BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"sonnet"}, Efforts: nil}},
	}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "claude", "--cwd", t.TempDir(),
		"--model", "sonnet", "--effort", "unlisted-effort", "--strict-model",
		"--prompt", "allow unknown catalog", "--background", "--json",
	}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("task code = %d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("JobSubmit calls = %d, want 1", len(fake.submits))
	}
	var launch LaunchEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &launch); err != nil {
		t.Fatalf("stdout is not a single launch envelope: %v; raw=%q", err, stdout.String())
	}
	if launch.Effort.Validated != nil {
		t.Fatalf("unknown effort catalog validation marker = %#v, want omitted", launch.Effort.Validated)
	}
	if strings.Contains(stderr.String(), "warning: effort") {
		t.Fatalf("unknown effort catalog warning = %q", stderr.String())
	}
	// An unknown catalog means the value is unverifiable, not wrong, so it must
	// still reach the backend unchanged. Absence of a marker and a warning does
	// not prove that: an implementation that silently cleared or replaced the
	// value would satisfy both assertions while breaking passthrough.
	if got := fake.submits[0].TaskSpec.Effort; got != "unlisted-effort" {
		t.Fatalf("submitted effort = %q, want the unadvertised value passed through unchanged", got)
	}
	if launch.Effort.Effective != "unlisted-effort" || launch.Effort.Source != "flag" {
		t.Fatalf("envelope effort = %#v, want effective=%q source=flag", launch.Effort, "unlisted-effort")
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

func TestTaskLaunchEnvelopeReportsEffectiveBackendProfile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  config.DimensionResolution
	}{
		{
			name: "read_only_without_write",
			want: config.DimensionResolution{Effective: backendProfileReadOnly, Source: "default"},
		},
		{
			name:  "read_only_with_explicit_write_false",
			flags: []string{"--write=false"},
			want:  config.DimensionResolution{Effective: backendProfileReadOnly, Source: "flag"},
		},
		{
			name:  "workspace_write_with_write",
			flags: []string{"--write"},
			want:  config.DimensionResolution{Effective: backendProfileWorkspaceWrite, Source: "flag"},
		},
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
			var envelope LaunchEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("launch JSON=%q: %v", stdout.String(), err)
			}
			if envelope.BackendProfile != tc.want {
				t.Fatalf("launch backend_profile=%#v, want %#v", envelope.BackendProfile, tc.want)
			}
		})
	}
}

func TestTaskTerminalEnvelopeReportsEffectiveBackendProfile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  config.DimensionResolution
	}{
		{
			name: "read_only_without_write",
			want: config.DimensionResolution{Effective: backendProfileReadOnly, Source: "default"},
		},
		{
			name:  "workspace_write_with_write",
			flags: []string{"--write"},
			want:  config.DimensionResolution{Effective: backendProfileWorkspaceWrite, Source: "flag"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello:        helloWithCapabilities(),
				submitResult: client.JobSubmitResult{JobID: "job_profile", State: engine.StateQueued},
				result:       client.JobResult{JobID: "job_profile", State: engine.StateCompleted},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			args := append([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "do it", "--background", "--json"}, tc.flags...)
			var stdout, stderr bytes.Buffer
			if code := run(args, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("launch code = %d, stderr = %q", code, stderr.String())
			}
			var launch LaunchEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &launch); err != nil {
				t.Fatalf("launch JSON=%q: %v", stdout.String(), err)
			}
			stdout.Reset()
			stderr.Reset()
			if code := run([]string{"result", "--job", launch.JobID, "--json"}, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("result code = %d, stderr = %q", code, stderr.String())
			}
			var envelope TerminalEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("terminal JSON=%q: %v", stdout.String(), err)
			}
			if envelope.BackendProfile != tc.want {
				t.Fatalf("terminal backend_profile=%#v, want %#v", envelope.BackendProfile, tc.want)
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
	stubAgentbusTestEnvironment(t)
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

// stubAgentbusTestEnvironment pins environment inputs used by fake-Agentbus
// command tests. The lock root differs by OS:
// os.UserCacheDir uses HOME/Library/Caches on Darwin and XDG_CACHE_HOME on
// Unix, so both HOME and XDG_CACHE_HOME must be isolated.
func stubAgentbusTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("AGENTBUS_STATE_ROOT", "")
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
	if f.submitResult.JobID != "" || f.submitResult.State != "" || f.submitResult.Deduplicated || f.submitResult.Timeout != nil {
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
		Timeout:            f.result.Timeout,
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

func testResultText() string {
	return "fixture result\n"
}

func ptr[T any](v T) *T {
	return &v
}

func rawSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
