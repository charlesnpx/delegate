package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

const (
	publicStateQueued    client.PublicState = "queued"
	publicStateCompleted client.PublicState = "completed"
)

func TestTaskReceiptForwardsSubmittedValuesAndKeepsStdoutJSONOnly(t *testing.T) {
	timeout := &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault}
	fake := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:        "job_receipt",
			State:        publicStateQueued,
			Deduplicated: false,
			Timeout:      timeout,
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", cwd, "--prompt-file", "-",
		"--request-id", "automation/retry-1", "--resume", "job_killed", "--model", "unadvertised-model", "--effort", "unadvertised-effort",
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
	if receipt.RequestID != "automation/retry-1" || receipt.JobID != "job_receipt" || receipt.State != publicStateQueued || receipt.Deduplicated {
		t.Fatalf("receipt=%#v", receipt)
	}
	wantWorkspaceKey, err := workspaceKeyForLogicalWorkspace(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Model != "unadvertised-model" || receipt.Effort != "unadvertised-effort" {
		t.Fatalf("receipt model/effort=%q/%q", receipt.Model, receipt.Effort)
	}
	if receipt.Timeout == nil || receipt.Timeout.EffectiveMS != timeout.Effective || receipt.Timeout.Source != timeout.Source {
		t.Fatalf("receipt timeout=%#v, want Agentbus effective/source %#v", receipt.Timeout, timeout)
	}
	wantReceipt := fmt.Sprintf("{\"requestId\":\"automation/retry-1\",\"workspaceKey\":\"%s\",\"jobId\":\"job_receipt\",\"state\":\"queued\",\"deduplicated\":false,\"inputMode\":\"steerable\",\"model\":\"unadvertised-model\",\"effort\":\"unadvertised-effort\",\"timeout\":{\"effectiveMs\":1800000,\"source\":\"daemon_default\"}}\n", wantWorkspaceKey)
	if got := stdout.String(); got != wantReceipt {
		t.Fatalf("receipt bytes changed:\n got %q\nwant %q", got, wantReceipt)
	}
	var receiptFields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &receiptFields); err != nil {
		t.Fatal(err)
	}
	if got, want := len(receiptFields), 9; got != want {
		t.Fatalf("receipt fields=%#v, want exactly %d fields", receiptFields, want)
	}
	for _, key := range []string{"requestId", "workspaceKey", "jobId", "state", "deduplicated", "inputMode", "model", "effort", "timeout"} {
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
	if got := params.TaskSpec.InputMode; got != taskInputModeSteerable {
		t.Fatalf("submitted inputMode=%q, want %q", got, taskInputModeSteerable)
	}
	if params.RequestID != "automation/retry-1" || params.TaskSpec.Prompt != "inspect this" {
		t.Fatalf("submitted params=%#v", params)
	}
	if params.WorkspaceKey != wantWorkspaceKey || params.TaskSpec.ResumeJobID != "job_killed" {
		t.Fatalf("submitted workspace/resume=%q/%q, want %q/%q", params.WorkspaceKey, params.TaskSpec.ResumeJobID, wantWorkspaceKey, "job_killed")
	}
	if params.TaskSpec.Model == nil || *params.TaskSpec.Model != "unadvertised-model" || params.TaskSpec.Effort == nil || *params.TaskSpec.Effort != "unadvertised-effort" {
		t.Fatalf("submitted model/effort=%#v/%#v", params.TaskSpec.Model, params.TaskSpec.Effort)
	}
	if params.TaskSpec.Tags == nil || !mapsEqual(*params.TaskSpec.Tags, map[string]string{"ticket": "ABC-123", "owner": "qa"}) {
		t.Fatalf("tags=%#v", params.TaskSpec.Tags)
	}
	if len(fake.gets) != 0 {
		t.Fatalf("task inspected jobs after submit: gets=%d", len(fake.gets))
	}
}

func TestTaskRetainSessionSetsSubmittedSpec(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--retain-session",
	}, strings.NewReader("remember this"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	if !fake.submits[0].TaskSpec.RetainSession {
		t.Fatalf("submitted retainSession=%v, want true", fake.submits[0].TaskSpec.RetainSession)
	}
}

func TestTaskRetainSessionDefaultsFalse(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-",
	}, strings.NewReader("do not retain this"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	if fake.submits[0].TaskSpec.RetainSession {
		t.Fatalf("submitted retainSession=%v, want false", fake.submits[0].TaskSpec.RetainSession)
	}
	raw, err := json.Marshal(fake.submits[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"retainSession"`) {
		t.Fatalf("submitted JSON=%s, want retainSession omitted", raw)
	}
}

func TestTaskLocalIPCSubmissionAndDefaultWireOmission(t *testing.T) {
	for _, test := range []struct {
		name      string
		flag      string
		wantLocal bool
	}{
		{name: "default"},
		{name: "requested", flag: "--local-ipc", wantLocal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithBackends()}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			args := []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-"}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader("run the test runner"), &stdout, &stderr); code != 0 {
				t.Fatalf("task code=%d stderr=%q", code, stderr.String())
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submits=%d, want 1", len(fake.submits))
			}
			spec := fake.submits[0].TaskSpec
			if spec.LocalIPC != test.wantLocal {
				t.Fatalf("submitted localIPC=%v, want %v", spec.LocalIPC, test.wantLocal)
			}
			if !test.wantLocal && spec.Verify {
				t.Fatalf("submitted verify=%v by default, want false", spec.Verify)
			}
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantLocal && !strings.Contains(string(raw), `"localIPC":true`) {
				t.Fatalf("submitted TaskSpec JSON=%s, want localIPC true", raw)
			}
			if !test.wantLocal && (strings.Contains(string(raw), `"localIPC"`) || strings.Contains(string(raw), `"verify"`)) {
				t.Fatalf("submitted TaskSpec JSON=%s, want localIPC and verify omitted", raw)
			}
		})
	}
}

func TestTaskVerifySubmitsVerifyTrue(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--verify",
	}, strings.NewReader("run the checks"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 || !fake.submits[0].TaskSpec.Verify {
		t.Fatalf("submits=%#v, want one submission with verify true", fake.submits)
	}
}

func TestTaskVerifyWriteIsRejectedBeforeConnecting(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	oldConnect := connectAgentbus
	connectCalls := 0
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		connectCalls++
		return fake, nil
	}
	defer func() { connectAgentbus = oldConnect }()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--verify", "--write",
	}, strings.NewReader("run the checks"), &stdout, &stderr); code == 0 {
		t.Fatalf("task code=0 stdout=%q, want contradictory options rejected", stdout.String())
	}
	for _, want := range []string{"--verify", "disposable copy", "cannot write the source"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q, missing %q", stderr.String(), want)
		}
	}
	if connectCalls != 0 || len(fake.submits) != 0 {
		t.Fatalf("connect calls=%d submissions=%d, want neither", connectCalls, len(fake.submits))
	}
}

func TestTaskReceiptIncludesRequestedExecutionFlags(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--verify", "--local-ipc",
	}, strings.NewReader("run the checks"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatalf("receipt JSON: %v; raw=%q", err, stdout.String())
	}
	for key, want := range map[string]string{"localIPC": "true", "verify": "true"} {
		if got := string(fields[key]); got != want {
			t.Fatalf("receipt %s=%s, want %s; fields=%v", key, got, want, fields)
		}
	}
	if len(fake.submits) != 1 || !fake.submits[0].TaskSpec.LocalIPC || !fake.submits[0].TaskSpec.Verify {
		t.Fatalf("submits=%#v, want both requested flags", fake.submits)
	}
}

func TestTaskNoSteerSubmitsFixedIndependentOfWriteAndResume(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-",
		"--no-steer", "--write", "--resume", "job_previous",
	}, strings.NewReader("verify this change"), &stdout, &stderr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	spec := fake.submits[0].TaskSpec
	if spec.InputMode != taskInputModeFixed || !spec.Write || spec.ResumeJobID != "job_previous" {
		t.Fatalf("submitted input mode/write/resume=%q/%v/%q", spec.InputMode, spec.Write, spec.ResumeJobID)
	}
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.InputMode != taskInputModeFixed {
		t.Fatalf("receipt inputMode=%q, want %q", receipt.InputMode, taskInputModeFixed)
	}
}

func TestTaskReceiptWriteFailureIncludesGeneratedRequestID(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
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
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_no_model_or_effort",
			State:   publicStateQueued,
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
			fake := &fakeAgentbusClient{hello: helloWithBackends()}
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
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID: "job_schema_timeout",
			State: publicStateQueued,
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
	if string(spec.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("output schema=%s", spec.OutputSchema)
	}
	if spec.TimeoutMS == nil || *spec.TimeoutMS != 45000 {
		t.Fatalf("timeout=%v", spec.TimeoutMS)
	}
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Timeout == nil || receipt.Timeout.EffectiveMS != requested || receipt.Timeout.Source != engine.TimeoutSourceClient {
		t.Fatalf("receipt timeout=%#v", receipt.Timeout)
	}
}

func TestBareNumberTimeoutIsRejectedWithUnitSuggestions(t *testing.T) {
	want := []string{"2400s", "40m"}
	_, taskErr := parseTaskOptions([]string{"--backend", "codex", "--prompt-file", "-", "--timeout", "2400"}, &bytes.Buffer{})
	if taskErr == nil {
		t.Fatal("bare numeric task timeout unexpectedly accepted")
	}
	for _, fragment := range want {
		if !strings.Contains(taskErr.Error(), fragment) {
			t.Fatalf("task timeout error=%q, want %q", taskErr, fragment)
		}
	}

	_, reviewErr := parseReviewOptions(reviewKind, []string{"--backend", "codex", "--timeout", "2400"}, &bytes.Buffer{})
	if reviewErr == nil {
		t.Fatal("bare numeric review timeout unexpectedly accepted")
	}
	for _, fragment := range want {
		if !strings.Contains(reviewErr.Error(), fragment) {
			t.Fatalf("review timeout error=%q, want %q", reviewErr, fragment)
		}
	}
}

func TestTaskRemovedFlagsAndPositionalPromptAreRejected(t *testing.T) {
	removed := []string{
		"--background", "--wait", "--json", "--prompt", "--prompt-stdin", "--handoff-prompt-file",
		"--output-schema", "--output-schema-stdin", "--output-schema-file", "--strict-model", "--origin",
		"--parent-client", "--parent-session", "--recover-request", "--steerable",
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
		fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: errors.New("lost response")}
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

	t.Run("typed daemon resume rejection", func(t *testing.T) {
		var daemonErr client.RPCError
		if err := json.Unmarshal([]byte(`{"Object":{"code":-32000,"message":"resume target job_killed is not resumable","data":{"code":"invalid_task_spec","jobId":"job_killed"}}}`), &daemonErr); err != nil {
			t.Fatal(err)
		}
		fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: &daemonErr}
		restore := stubAgentbusGlobals(t, fake)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--request-id", "caller/resume-after-error", "--resume", "job_killed"}, strings.NewReader("prompt"), &stdout, &stderr); code == 0 {
			t.Fatal("task succeeded")
		}
		if got, want := stdout.String(), ""; got != want {
			t.Fatalf("stdout=%q, want %q", got, want)
		}
		if got, want := stderr.String(), readOnlyTaskHint+"\n"+daemonErr.Error()+"\n"; got != want {
			t.Fatalf("stderr=%q, want read-only notice and unchanged daemon rejection %q", got, want)
		}
		if len(fake.gets) != 0 {
			t.Fatalf("task locally inspected resume target: gets=%#v", fake.gets)
		}
	})

	t.Run("success", func(t *testing.T) {
		fake := &fakeAgentbusClient{hello: helloWithBackends()}
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

func TestTaskOldDaemonRejectingInputModeExplainsUpgradeWithoutRetry(t *testing.T) {
	var daemonErr client.RPCError
	if err := json.Unmarshal([]byte(`{"Object":{"code":-32000,"message":"json: unknown field \"inputMode\"","data":{"code":"invalid_task_spec"}}}`), &daemonErr); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: &daemonErr}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-",
		"--request-id", "caller/steerable-task",
	}, strings.NewReader("inspect"), &stdout, &stderr)
	if code == 0 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q, want failed submission without receipt", code, stdout.String())
	}
	for _, want := range []string{
		"running agentbus daemon does not support steerable tasks",
		"must be upgraded",
		"--no-steer submits a fixed task",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q, missing %q", stderr.String(), want)
		}
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submit attempts=%d, want exactly one", len(fake.submits))
	}
}

func TestTaskOldDaemonRejectingExecutionFlagsExplainsUpgradeWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		flag  string
		field string
	}{
		{flag: "--verify", field: "verify"},
		{flag: "--local-ipc", field: "localIPC"},
	} {
		t.Run(test.field, func(t *testing.T) {
			message, err := json.Marshal(fmt.Sprintf(`json: unknown field %q`, test.field))
			if err != nil {
				t.Fatal(err)
			}
			rawErr := fmt.Sprintf(`{"Object":{"code":-32000,"message":%s,"data":{"code":"invalid_task_spec"}}}`, message)
			var daemonErr client.RPCError
			if err := json.Unmarshal([]byte(rawErr), &daemonErr); err != nil {
				t.Fatal(err)
			}
			fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: &daemonErr}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run([]string{
				"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", test.flag,
			}, strings.NewReader("run the checks"), &stdout, &stderr)
			if code == 0 || stdout.Len() != 0 {
				t.Fatalf("code=%d stdout=%q, want failed submission without receipt", code, stdout.String())
			}
			for _, want := range []string{test.flag, "must be upgraded"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr=%q, missing %q", stderr.String(), want)
				}
			}
			if len(fake.submits) != 1 {
				t.Fatalf("submit attempts=%d, want exactly one", len(fake.submits))
			}
		})
	}
}

func TestTaskUnrelatedInvalidTaskSpecKeepsDaemonError(t *testing.T) {
	var daemonErr client.RPCError
	if err := json.Unmarshal([]byte(`{"Object":{"code":-32000,"message":"taskSpec.prompt is empty","data":{"code":"invalid_task_spec"}}}`), &daemonErr); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{hello: helloWithBackends(), submitErr: &daemonErr}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-",
		"--request-id", "caller/invalid-prompt",
	}, strings.NewReader("inspect"), &stdout, &stderr)
	if code == 0 {
		t.Fatal("task succeeded, want daemon rejection")
	}
	if !strings.Contains(stderr.String(), daemonErr.Error()) {
		t.Fatalf("stderr=%q, want original daemon error %q", stderr.String(), daemonErr.Error())
	}
	if strings.Contains(stderr.String(), "does not support steerable tasks") || strings.Contains(stderr.String(), "--no-steer to submit") {
		t.Fatalf("stderr=%q, unrelated invalid_task_spec was misclassified", stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submit attempts=%d, want exactly one", len(fake.submits))
	}
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
	connectAgentbus = func(context.Context, client.Options) (agentbusClient, error) {
		return fake, nil
	}
	lookPath = func(string) (string, error) {
		return "/tmp/agentbus", nil
	}
	return func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
	}
}

func stubAgentbusTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("AGENTBUS_STATE_ROOT", "")
}

func helloWithBackends() client.HelloResult {
	return client.HelloResult{
		ProtocolVersion: 3,
		BackendMetadata: []client.BackendInfo{
			{Name: "codex"},
			{Name: "claude"},
		},
	}
}

type fakeAgentbusClient struct {
	hello        client.HelloResult
	submits      []client.JobSubmitParams
	submitResult client.JobSubmitResult
	submitErr    error
	gets         []client.JobGetParams
	getErr       error
	get          client.JobGetResult
}

func (f *fakeAgentbusClient) Close() error { return nil }

func (f *fakeAgentbusClient) HelloResult() client.HelloResult { return f.hello }

func (f *fakeAgentbusClient) JobSubmit(_ context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	f.submits = append(f.submits, params)
	if f.submitErr != nil {
		return client.JobSubmitResult{}, f.submitErr
	}
	jobID := "job_fake"
	if f.submitResult.JobID != "" || f.submitResult.State != "" || f.submitResult.Deduplicated || f.submitResult.Timeout != nil {
		submitted := f.submitResult
		if submitted.JobID == "" {
			submitted.JobID = jobID
		}
		if submitted.State == "" {
			submitted.State = publicStateQueued
		}
		return submitted, nil
	}
	return client.JobSubmitResult{JobID: jobID, State: publicStateQueued}, nil
}

func (f *fakeAgentbusClient) JobGet(_ context.Context, params client.JobGetParams) (client.JobGetResult, error) {
	f.gets = append(f.gets, params)
	if f.getErr != nil {
		return client.JobGetResult{}, f.getErr
	}
	return f.get, nil
}

func ptr[T any](v T) *T {
	return &v
}
