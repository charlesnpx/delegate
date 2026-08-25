package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

// TestRescueSmokeFixtures exercises the actual handoff and task/result CLI flow
// used by the two rescue skills. engineSmokeClient is intentionally small: it
// presents the daemon client seam while driving a vendored engine.Backend and
// engine.Session, so the fixture does not depend on either real backend CLI.
func TestRescueSmokeFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origin  string
		backend string
	}{
		{
			name:    "claude_code_orchestrates_codex_rescue",
			origin:  "delegate:rescue:codex",
			backend: "codex",
		},
		{
			name:    "codex_orchestrates_claude_rescue",
			origin:  "delegate:rescue:claude",
			backend: "claude",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			backend := &recordingBackend{
				name:      tc.backend,
				sessionID: "session_" + tc.backend,
				reports:   []string{testResultText()},
			}
			bus := &engineSmokeClient{
				fakeAgentbusClient: fakeAgentbusClient{hello: helloWithCapabilities()},
				backend:            backend,
				jobID:              "job_smoke_" + tc.backend,
			}
			restore := stubAgentbusClientGlobals(t, bus)
			defer restore()

			const prompt = "Investigate the small rescue task and report the result."
			var handoffOut, handoffErr bytes.Buffer
			if code := run([]string{"handoff", "create", "--json"}, strings.NewReader(prompt), &handoffOut, &handoffErr); code != 0 {
				t.Fatalf("handoff create code = %d, stderr = %q", code, handoffErr.String())
			}
			var handoffResult struct {
				HandoffPath string `json:"handoff_path"`
			}
			if err := json.Unmarshal(handoffOut.Bytes(), &handoffResult); err != nil {
				t.Fatalf("handoff JSON invalid: %v; raw = %q", err, handoffOut.String())
			}

			var launchOut, launchErr bytes.Buffer
			launchArgs := []string{
				"task", "--backend", tc.backend,
				"--origin", tc.origin,
				"--cwd", t.TempDir(),
				"--handoff-prompt-file", handoffResult.HandoffPath,
				"--background", "--json",
			}
			if code := run(launchArgs, nil, &launchOut, &launchErr); code != 0 {
				t.Fatalf("task launch code = %d, stderr = %q", code, launchErr.String())
			}
			var launch LaunchEnvelope
			if err := json.Unmarshal(launchOut.Bytes(), &launch); err != nil {
				t.Fatalf("launch JSON invalid: %v; raw = %q", err, launchOut.String())
			}
			if launch.JobID != bus.jobID || launch.Status != string(engine.StateQueued) || launch.ResultSHA256 != nil {
				t.Fatalf("launch envelope = %#v, want queued envelope for %s", launch, bus.jobID)
			}
			if len(bus.submits) != 1 {
				t.Fatalf("submissions = %d, want 1", len(bus.submits))
			}
			if got := bus.submits[0].TaskSpec.Tags["delegate.origin"]; got != tc.origin {
				t.Fatalf("delegate.origin = %q, want %q", got, tc.origin)
			}
			if got := bus.submits[0].TaskSpec.Prompt; got != prompt {
				t.Fatalf("handoff prompt = %q, want %q", got, prompt)
			}
			if _, err := os.Stat(handoffResult.HandoffPath); !os.IsNotExist(err) {
				t.Fatalf("handoff file remains after launch: %v", err)
			}

			var terminalOut, terminalErr bytes.Buffer
			if code := run([]string{"result", "--job", launch.JobID, "--json"}, nil, &terminalOut, &terminalErr); code != 0 {
				t.Fatalf("result code = %d, stderr = %q", code, terminalErr.String())
			}
			var terminal TerminalEnvelope
			if err := json.Unmarshal(terminalOut.Bytes(), &terminal); err != nil {
				t.Fatalf("terminal JSON invalid: %v; raw = %q", err, terminalOut.String())
			}
			if terminal.Status != engine.StateCompleted || terminal.Kind != taskKind {
				t.Fatalf("terminal envelope = %#v, want completed task", terminal)
			}
			if terminal.Contract != nil {
				t.Fatalf("contract stamp = %#v, want omitted without a policy", terminal.Contract)
			}
			if terminal.ResultSHA256 == nil || *terminal.ResultSHA256 != sha256Text(testResultText()) {
				t.Fatalf("terminal hashes = %#v, want result hash", terminal)
			}
			if len(backend.turns) != 1 || backend.turns[0].Write {
				t.Fatalf("backend turn inputs = %#v, want one read-only launch", backend.turns)
			}
			if effective := backend.turns[0].Prompt; effective != prompt {
				t.Fatalf("effective prompt = %q, want %q", effective, prompt)
			}
		})
	}
}

type recordingBackend struct {
	name      string
	sessionID string
	reports   []string
	starts    []engine.SessionOpts
	turns     []engine.TurnInput
	next      int
}

func (b *recordingBackend) Name() string { return b.name }

func (b *recordingBackend) Preflight(context.Context) (engine.Health, error) {
	return engine.Health{Backend: b.name}, nil
}

func (b *recordingBackend) Start(_ context.Context, opts engine.SessionOpts) (engine.Session, error) {
	b.starts = append(b.starts, opts)
	return &recordingSession{backend: b}, nil
}

func (b *recordingBackend) Resume(_ context.Context, _ string, opts engine.SessionOpts) (engine.Session, error) {
	b.starts = append(b.starts, opts)
	return &recordingSession{backend: b}, nil
}

type recordingSession struct{ backend *recordingBackend }

func (s *recordingSession) ID() string { return s.backend.sessionID }

func (s *recordingSession) Turn(_ context.Context, input engine.TurnInput) (<-chan engine.Event, error) {
	s.backend.turns = append(s.backend.turns, input)
	if s.backend.next >= len(s.backend.reports) {
		return nil, fmt.Errorf("unexpected turn %d", s.backend.next+1)
	}
	report := s.backend.reports[s.backend.next]
	s.backend.next++
	events := make(chan engine.Event, 1)
	events <- engine.Event{Type: engine.EventResultMessage, Text: report, RawText: report}
	close(events)
	return events, nil
}

func (s *recordingSession) Interrupt(context.Context) error { return nil }

// engineSmokeClient adapts a real engine backend/session to the minimal daemon
// client used by delegate's background task and result commands.
type engineSmokeClient struct {
	fakeAgentbusClient
	backend engine.Backend
	jobID   string
}

func (c *engineSmokeClient) JobSubmit(ctx context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	c.submits = append(c.submits, params)
	session, err := c.backend.Start(ctx, engine.SessionOpts{
		CWD:     params.TaskSpec.CWD,
		Write:   params.TaskSpec.Write,
		Model:   params.TaskSpec.Model,
		Effort:  params.TaskSpec.Effort,
		Timeout: millisDuration(params.TaskSpec.TimeoutMs),
	})
	if err != nil {
		return client.JobSubmitResult{}, err
	}
	events, err := session.Turn(ctx, engine.TurnInput{
		Prompt:  smokeEffectivePrompt(params.TaskSpec.Prompt, params.TaskSpec.Policy),
		Write:   params.TaskSpec.Write,
		Timeout: millisDuration(params.TaskSpec.TimeoutMs),
	})
	if err != nil {
		return client.JobSubmitResult{}, err
	}
	var result string
	for event := range events {
		if event.Type == engine.EventResultMessage {
			result = event.RawText
			if result == "" {
				result = event.Text
			}
		}
	}
	validation, err := engine.ValidatePolicyText(result, params.TaskSpec.Policy, engine.NewPolicyRegistry(), time.Unix(1, 0).UTC())
	if err != nil {
		return client.JobSubmitResult{}, err
	}
	c.result = client.JobResult{
		JobID:     c.jobID,
		SessionID: session.ID(),
		State:     engine.StateCompleted,
		Result: &engine.ResultInfo{
			ResultPath: "fake://" + c.jobID,
			SHA256:     sha256Text(result),
			Bytes:      int64(len(result)),
			Text:       result,
		},
		Contract: validation.Stamp,
	}
	return client.JobSubmitResult{JobID: c.jobID, State: engine.StateQueued}, nil
}

func millisDuration(value *int64) time.Duration {
	if value == nil {
		return 0
	}
	return time.Duration(*value) * time.Millisecond
}

func smokeEffectivePrompt(prompt string, turnPolicy *engine.TurnPolicy) string {
	if turnPolicy == nil || strings.TrimSpace(turnPolicy.Prologue) == "" {
		return prompt
	}
	return strings.TrimRight(turnPolicy.Prologue, "\n") + "\n\n" + prompt
}

func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
