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

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
)

func TestTaskReceiptForwardsSubmittedValuesAndKeepsStdoutJSONOnly(t *testing.T) {
	timeout := &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID:        "job_receipt",
			State:        engine.StateQueued,
			Deduplicated: false,
			Timeout:      timeout,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	if err := delegateconfig.Save(delegateconfig.Config{
		Overridable: false,
		Backend: delegateconfig.Backends{
			Codex: delegateconfig.Defaults{Model: "configured-model", Effort: "configured-effort"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-",
		"--request-id", "automation/retry-1", "--model", "unadvertised-model", "--effort", "unadvertised-effort",
		"--tag", "ticket=ABC-123", "--tag", "owner=qa",
	}, strings.NewReader("inspect this"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Fatalf("stdout=%q, want one JSON line", stdout.String())
	}
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("stdout is not receipt JSON: %v; raw=%q", err, stdout.String())
	}
	if receipt.RequestID != "automation/retry-1" || receipt.JobID != "job_receipt" || receipt.State != engine.StateQueued || receipt.Deduplicated {
		t.Fatalf("receipt=%#v", receipt)
	}
	if receipt.Model != "unadvertised-model" || receipt.Effort != "unadvertised-effort" {
		t.Fatalf("receipt model/effort=%q/%q", receipt.Model, receipt.Effort)
	}
	if receipt.Timeout == nil || receipt.Timeout.EffectiveMS != timeout.Effective || receipt.Timeout.Source != timeout.Source {
		t.Fatalf("receipt timeout=%#v, want Agentbus effective/source %#v", receipt.Timeout, timeout)
	}
	var receiptFields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &receiptFields); err != nil {
		t.Fatal(err)
	}
	if got, want := len(receiptFields), 7; got != want {
		t.Fatalf("receipt fields=%#v, want exactly %d fields", receiptFields, want)
	}
	for _, key := range []string{"requestId", "jobId", "state", "deduplicated", "model", "effort", "timeout"} {
		if _, found := receiptFields[key]; !found {
			t.Fatalf("receipt fields=%#v, missing %q", receiptFields, key)
		}
	}
	var timeoutFields map[string]json.RawMessage
	if err := json.Unmarshal(receiptFields["timeout"], &timeoutFields); err != nil {
		t.Fatal(err)
	}
	if got, want := len(timeoutFields), 2; got != want {
		t.Fatalf("timeout fields=%#v, want exactly %d fields", timeoutFields, want)
	}
	for _, key := range []string{"effectiveMs", "source"} {
		if _, found := timeoutFields[key]; !found {
			t.Fatalf("timeout fields=%#v, missing %q", timeoutFields, key)
		}
	}
	if !strings.Contains(stderr.String(), readOnlyTaskHint) {
		t.Fatalf("stderr=%q, want read-only notice", stderr.String())
	}
	if strings.Contains(stderr.String(), "unadvertised") {
		t.Fatalf("stderr=%q, task must not validate model/effort catalogs", stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	params := fake.submits[0]
	if params.RequestID != "automation/retry-1" || params.TaskSpec.Prompt != "inspect this" {
		t.Fatalf("submitted params=%#v", params)
	}
	if params.TaskSpec.Model != "unadvertised-model" || params.TaskSpec.Effort != "unadvertised-effort" {
		t.Fatalf("submitted model/effort=%q/%q", params.TaskSpec.Model, params.TaskSpec.Effort)
	}
	if got, want := params.TaskSpec.Tags, map[string]string{"ticket": "ABC-123", "owner": "qa"}; !mapsEqual(got, want) {
		t.Fatalf("tags=%#v, want %#v", got, want)
	}
	if len(fake.statuses) != 0 || len(fake.results) != 0 {
		t.Fatalf("task polled after submit: statuses=%d results=%d", len(fake.statuses), len(fake.results))
	}
}

func TestTaskReceiptWriteFailureIncludesGeneratedRequestID(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stderr bytes.Buffer
	_, err := runTask(
		[]string{"--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-"},
		strings.NewReader("inspect this"),
		failingTaskReceiptWriter{err: errors.New("closed pipe")},
		&stderr,
	)
	if err == nil {
		t.Fatal("runTask() error = nil, want receipt write failure")
	}
	if len(fake.submits) != 1 || fake.submits[0].RequestID == "" {
		t.Fatalf("submits=%#v, want one submission with a generated request ID", fake.submits)
	}
	if !strings.Contains(err.Error(), fake.submits[0].RequestID) {
		t.Fatalf("error=%q, missing request ID %q", err, fake.submits[0].RequestID)
	}
}

func TestTaskReceiptOmitsUnsetModelAndEffort(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_no_model_or_effort",
			State:   engine.StateQueued,
			Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--write"}, strings.NewReader("inspect this"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	var receiptFields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &receiptFields); err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"model", "effort"} {
		if _, found := receiptFields[omitted]; found {
			t.Fatalf("receipt fields=%#v, unexpectedly contains %q", receiptFields, omitted)
		}
	}
	if len(fake.submits) != 1 || !fake.submits[0].TaskSpec.Write {
		t.Fatalf("submitted=%#v, want write task", fake.submits)
	}
	if stderr.Len() != 0 {
		t.Fatalf("write task stderr=%q, want no read-only notice", stderr.String())
	}
}

func TestTaskPromptFileReadsStdinAndDisk(t *testing.T) {
	for _, tc := range []struct {
		name      string
		promptArg string
		stdin     string
		want      string
	}{
		{name: "stdin", promptArg: "-", stdin: "from stdin", want: "from stdin"},
		{name: "file", stdin: "ignored", want: "from file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()
			cwd := t.TempDir()
			promptArg := tc.promptArg
			if promptArg == "" {
				promptArg = filepath.Join(cwd, "prompt.txt")
				if err := os.WriteFile(promptArg, []byte(tc.want), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--prompt-file", promptArg}, strings.NewReader(tc.stdin), &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if len(fake.submits) != 1 || fake.submits[0].TaskSpec.Prompt != tc.want {
				t.Fatalf("submits=%#v, want prompt %q", fake.submits, tc.want)
			}
		})
	}
}

func TestTaskSchemaFileAndTimeoutReachAgentbus(t *testing.T) {
	requested := int64(45000)
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		submitResult: client.JobSubmitResult{
			JobID: "job_schema_timeout",
			State: engine.StateQueued,
			Timeout: &engine.TimeoutResolution{
				Requested: &requested,
				Effective: requested,
				Source:    engine.TimeoutSourceClient,
			},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	cwd := t.TempDir()
	schemaPath := filepath.Join(cwd, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--prompt-file", "-", "--schema-file", schemaPath, "--timeout", "45s"}, strings.NewReader("schema task"), &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d", len(fake.submits))
	}
	spec := fake.submits[0].TaskSpec
	if spec.Policy == nil || spec.Policy.Contract == nil || string(spec.Policy.Contract.JSONSchema) != `{"type":"object"}` {
		t.Fatalf("policy=%#v", spec.Policy)
	}
	if spec.TimeoutMs == nil || *spec.TimeoutMs != 45000 {
		t.Fatalf("timeout=%v", spec.TimeoutMs)
	}
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Timeout == nil || receipt.Timeout.EffectiveMS != requested || receipt.Timeout.Source != engine.TimeoutSourceClient {
		t.Fatalf("receipt timeout=%#v", receipt.Timeout)
	}
}

func TestTaskRemovedFlagsAndPositionalPromptAreRejected(t *testing.T) {
	removed := []string{
		"--background", "--wait", "--json", "--prompt", "--prompt-stdin", "--handoff-prompt-file",
		"--output-schema", "--output-schema-stdin", "--output-schema-file", "--strict-model", "--origin",
		"--parent-client", "--parent-session", "--recover-request",
	}
	for _, flagName := range removed {
		t.Run(flagName, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"task", "--backend", "codex", "--prompt-file", "-", flagName}, strings.NewReader("prompt"), &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), "flag provided but not defined") {
				t.Fatalf("flag %s: code=%d stderr=%q", flagName, code, stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--prompt-file", "-", "positional prompt"}, strings.NewReader("prompt"), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "does not accept positional") {
		t.Fatalf("positional code=%d stderr=%q", code, stderr.String())
	}
}

func TestTaskRejectsMalformedTagAndInvalidRequestID(t *testing.T) {
	for _, args := range [][]string{
		{"task", "--backend", "codex", "--prompt-file", "-", "--tag", "missing-equals"},
		{"task", "--backend", "codex", "--prompt-file", "-", "--tag", "=missing-key"},
		{"task", "--backend", "codex", "--prompt-file", "-", "--request-id", "contains space"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader("prompt"), &stdout, &stderr); code == 0 {
			t.Fatalf("run(%q) unexpectedly succeeded", args)
		}
	}
	if err := validateRequestID("automation/retry-α"); err != nil {
		t.Fatalf("Agentbus-compatible request id rejected: %v", err)
	}
}

func TestTaskFailureReportsRequestIDAndDoesNotWriteLocalState(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		fake := &fakeAgentbusClient{hello: helloWithCapabilities(), submitErr: errors.New("lost response")}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--request-id", "caller/retry-after-error"}, strings.NewReader("prompt"), &stdout, &stderr); code == 0 {
			t.Fatal("task succeeded")
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "caller/retry-after-error") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("success", func(t *testing.T) {
		fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()
		cwd := t.TempDir()
		stateHome := t.TempDir()
		stateRoot := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)
		t.Setenv("AGENTBUS_STATE_ROOT", stateRoot)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"task", "--backend", "codex", "--cwd", cwd, "--prompt-file", "-"}, strings.NewReader("prompt"), &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		for _, dir := range []string{cwd, stateHome, stateRoot} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("task wrote under %s: %#v", dir, entries)
			}
		}
	})
}

func mapsEqual(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

type failingTaskReceiptWriter struct {
	err error
}

func (w failingTaskReceiptWriter) Write([]byte) (int, error) {
	return 0, w.err
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
			"policy.shape":                false,
			"policy.jsonSchema":           true,
			"policy.named":                true,
			"policy.retry":                true,
			"models.reported":             true,
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
