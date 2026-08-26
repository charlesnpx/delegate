package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
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
	if code := run([]string{"config", "set", delegateconfig.KeyCursorModel, "cursor-model"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("set cursor model code = %d stderr=%q", code, stderr.String())
	}
	if code := run([]string{"config", "set", delegateconfig.KeyCursorEffort, "medium"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("set cursor effort code = %d stderr=%q", code, stderr.String())
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
	if listed.Path == "" || listed.Overridable || listed.Backend.Claude.Model != "unadvertised-model" || listed.Backend.Cursor.Model != "cursor-model" || listed.Backend.Cursor.Effort != "medium" {
		t.Fatalf("list = %#v", listed)
	}
	stdout.Reset()
	if code := run([]string{"config", "list"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "backend.cursor.model: cursor-model\nbackend.cursor.effort: medium\n") {
		t.Fatalf("list = %q", stdout.String())
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
