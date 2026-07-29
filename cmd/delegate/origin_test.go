package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestCaptureTaskOriginEnvironmentAndOverrides(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	got := captureTaskOrigin("delegate:rescue:claude", optionalStringFlag{}, optionalStringFlag{}, env(map[string]string{
		"CLAUDECODE":             "1",
		"CLAUDE_CODE_SESSION_ID": "claude-session",
		"AI_AGENT":               "claude-agent",
		"DELEGATE_DEPTH":         "2",
	}))
	want := envelopeOrigin{
		Skill:           "delegate:rescue:claude",
		ParentClient:    "claude-code",
		ParentSessionID: "claude-session",
		ParentAgent:     "claude-agent",
		Depth:           "3",
	}
	if got != want {
		t.Fatalf("captured origin = %#v, want %#v", got, want)
	}

	got = captureTaskOrigin("delegate:review:codex", optionalStringFlag{value: "manual-client", set: true}, optionalStringFlag{value: "manual-session", set: true}, env(map[string]string{
		"CLAUDECODE":             "1",
		"CLAUDE_CODE_SESSION_ID": "env-session",
		"AI_AGENT":               "parent-agent",
	}))
	want = envelopeOrigin{
		Skill:           "delegate:review:codex",
		ParentClient:    "manual-client",
		ParentSessionID: "manual-session",
		ParentAgent:     "parent-agent",
		Depth:           "1",
	}
	if got != want {
		t.Fatalf("overridden origin = %#v, want %#v", got, want)
	}

	for _, tc := range []struct {
		parentDepth string
		want        string
	}{
		{want: "1"},
		{parentDepth: "8", want: "9"},
		{parentDepth: "not-a-number", want: "1"},
	} {
		if got := nextDelegateDepth(tc.parentDepth); got != tc.want {
			t.Fatalf("nextDelegateDepth(%q) = %q, want %q", tc.parentDepth, got, tc.want)
		}
	}
}

func TestTaskOriginTagsAndEnvelopes(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithCapabilities(),
		result: client.JobResult{
			JobID:  "job_origin",
			State:  engine.StateCompleted,
			Result: &engine.ResultInfo{SHA256: "result-origin"},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "env-session")
	t.Setenv("AI_AGENT", "parent-agent")
	t.Setenv("DELEGATE_DEPTH", "4")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "audit linkage", "--wait",
		"--origin", "delegate:rescue:codex", "--parent-client", "manual-client", "--parent-session", "manual-session",
	}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, stderr.String())
	}
	if len(fake.submits) != 1 {
		t.Fatalf("submissions = %d, want 1", len(fake.submits))
	}
	wantTags := map[string]string{
		"delegate.kind":   taskKind,
		delegateOriginTag: "delegate:rescue:codex",
		parentClientTag:   "manual-client",
		parentSessionTag:  "manual-session",
		parentAgentTag:    "parent-agent",
		delegateDepthTag:  "5",
	}
	gotTags := fake.submits[0].TaskSpec.Tags
	requestID := gotTags[delegateRequestIDTag]
	if err := validateRequestID(requestID); err != nil {
		t.Fatalf("delegate.request_id = %q: %v", requestID, err)
	}
	delete(gotTags, delegateRequestIDTag)
	if !reflect.DeepEqual(gotTags, wantTags) {
		t.Fatalf("TaskSpec tags = %#v, want %#v", gotTags, wantTags)
	}

	var terminal TerminalEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &terminal); err != nil {
		t.Fatalf("terminal JSON = %q: %v", stdout.String(), err)
	}
	wantOrigin := &envelopeOrigin{
		Skill:           "delegate:rescue:codex",
		ParentClient:    "manual-client",
		ParentSessionID: "manual-session",
		ParentAgent:     "parent-agent",
		Depth:           "5",
	}
	if !reflect.DeepEqual(terminal.Origin, wantOrigin) {
		t.Fatalf("terminal origin = %#v, want %#v", terminal.Origin, wantOrigin)
	}
}
