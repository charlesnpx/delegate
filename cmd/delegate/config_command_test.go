package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
)

func TestConfigCommandRoundTrip(t *testing.T) {
	fake := &fakeAgentbusClient{hello: client.HelloResult{
		ProtocolVersion: 1,
		Backends:        []string{"claude", "codex"},
		Capabilities:    helloWithCapabilities().Capabilities,
		BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"known-model"}, Efforts: []string{"known-effort"}}},
	}}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "set", delegateconfig.KeyClaudeModel, "unadvertised-model"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("set code = %d stderr=%q", code, stderr.String())
	}
	cfg, err := delegateconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend.Claude.Model != "unadvertised-model" {
		t.Fatalf("saved model = %q", cfg.Backend.Claude.Model)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "set", delegateconfig.KeyOverridable, "false"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("set overridable code = %d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"config", "get", delegateconfig.KeyOverridable}, nil, &stdout, &stderr); code != 0 || stdout.String() != "false\n" {
		t.Fatalf("get code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"config", "list", "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	var listed configListJSON
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("list JSON = %q: %v", stdout.String(), err)
	}
	if listed.Path == "" || listed.Overridable || listed.Backend.Claude.Model != "unadvertised-model" {
		t.Fatalf("list = %#v", listed)
	}
	if code := run([]string{"config", "unset", delegateconfig.KeyClaudeModel}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("unset code=%d stderr=%q", code, stderr.String())
	}
	cfg, err = delegateconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend.Claude.Model != "" {
		t.Fatalf("unset model = %q", cfg.Backend.Claude.Model)
	}
	if code := run([]string{"config", "set", "backend.claude.modle", "value"}, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "allowed keys") {
		t.Fatalf("unknown key code=%d stderr=%q", code, stderr.String())
	}
}

func TestTaskEnvelopeUsesLockedConfigAndReportedModel(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: client.HelloResult{
			ProtocolVersion: 1,
			Backends:        []string{"claude"},
			Capabilities:    helloWithCapabilities().Capabilities,
			BackendMetadata: []client.BackendInfo{{Name: "claude", Models: []string{"configured-model"}, Efforts: []string{"configured-effort"}}},
		},
		result: client.JobResult{JobID: "job_locked", State: engine.StateCompleted, ModelReported: "backend-resolved-model"},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	cfg := delegateconfig.Config{Overridable: false, Backend: delegateconfig.Backends{Claude: delegateconfig.Defaults{Model: "configured-model", Effort: "configured-effort"}}}
	if err := delegateconfig.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "claude", "--cwd", t.TempDir(), "--model", "requested-model", "--effort", "requested-effort", "--prompt", "do it", "--wait"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 || fake.submits[0].TaskSpec.Model != "configured-model" || fake.submits[0].TaskSpec.Effort != "configured-effort" {
		t.Fatalf("submitted task = %#v", fake.submits)
	}
	var env TerminalEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("terminal JSON = %q: %v", stdout.String(), err)
	}
	if env.Model.Requested != "requested-model" || env.Model.Effective != "configured-model" || env.Model.Source != "config-locked" {
		t.Fatalf("model envelope = %#v", env.Model)
	}
	if env.Effort.Requested != "requested-effort" || env.Effort.Effective != "configured-effort" || env.Effort.Source != "config-locked" {
		t.Fatalf("effort envelope = %#v", env.Effort)
	}
	if env.ModelReported != "backend-resolved-model" || env.ModelReportedUnavailableReason != "" {
		t.Fatalf("reported-model envelope = %#v", env)
	}
	meta, found, err := loadJobMetadata("", "job_locked")
	if err != nil || !found || meta.Model.Effective != "configured-model" || meta.Effort.Effective != "configured-effort" {
		t.Fatalf("metadata=%#v found=%v err=%v", meta, found, err)
	}
}

func TestTerminalEnvelopeModelReportedFallbackReasons(t *testing.T) {
	modelEffort := delegateconfig.ModelEffortResolution{
		Model:  delegateconfig.DimensionResolution{Requested: "requested", Effective: "effective", Source: "flag"},
		Effort: delegateconfig.DimensionResolution{Source: "default"},
	}
	for _, tc := range []struct {
		name, reported, wantReason string
		capable                    bool
	}{
		{name: "reported", capable: true, reported: "backend-model"},
		{name: "backend did not report", capable: true, wantReason: "backend_did_not_report"},
		{name: "capability missing", wantReason: "agentbus_capability_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := newTerminalEnvelope("job_model", engine.StateCompleted, taskKind, contractKindShape, engine.ContractStamp{}, "", "", terminalEnvelopeOptions{ModelEffort: modelEffort, ModelReported: tc.reported, ModelsReportedCapable: tc.capable})
			if err != nil {
				t.Fatal(err)
			}
			if env.Model != modelEffort.Model || env.ModelReported != tc.reported || env.ModelReportedUnavailableReason != tc.wantReason {
				t.Fatalf("envelope = %#v", env)
			}
		})
	}
}
