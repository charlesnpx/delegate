package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/agentbus/engine/adapter/claudecli"
	"github.com/charlesnpx/agentbus/engine/adapter/codexcli"
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
				reports:   []string{compliantReport()},
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
			if launch.JobID != bus.jobID || launch.Status != string(engine.StateQueued) || launch.ResultSHA256 != nil || launch.SHA256 == "" {
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
			if terminal.Status != engine.StateCompleted || terminal.Kind != taskKind || terminal.ContractKind != contractKindShape {
				t.Fatalf("terminal envelope = %#v, want completed shape task", terminal)
			}
			if terminal.Contract.Status != engine.ContractCompliant || terminal.Contract.Attempts != 1 || terminal.Contract.RetryUsed {
				t.Fatalf("contract stamp = %#v, want one compliant validation", terminal.Contract)
			}
			if terminal.Contract.ContractSHA256 == "" || terminal.ResultSHA256 == nil || *terminal.ResultSHA256 != sha256Text(compliantReport()) || terminal.SHA256 == "" {
				t.Fatalf("terminal hashes = %#v, want contract, result, and envelope hashes", terminal)
			}
			if len(backend.turns) != 1 || backend.turns[0].Write {
				t.Fatalf("backend turn inputs = %#v, want one read-only launch", backend.turns)
			}
		})
	}
}

// TestEmbeddedContractTierSmoke validates delegate's tier matrix against the
// vendored CLI adapters. The fake executable records its argv for every turn,
// which makes the write downgrade on a corrective resume observable without a
// real backend account or network turn.
func TestEmbeddedContractTierSmoke(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newBackend func(string) engine.Backend
	}{
		{name: "codex", newBackend: func(binary string) engine.Backend { return codexcli.New(codexcli.Options{Binary: binary}) }},
		{name: "claude", newBackend: func(binary string) engine.Backend { return claudecli.New(claudecli.Options{Binary: binary}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("read_only_shapeless_is_noncompliant_without_retry", func(t *testing.T) {
				backend, recorder := fakeAdapterBackend(t, tc.name, []string{"shapeless reply"}, tc.newBackend)
				terminal, code := runEmbeddedTier(t, tc.name, backend, nil)
				if code != engine.ExitCodeForState(engine.StateCompletedNoncompliant) {
					t.Fatalf("exit code = %d, want noncompliant exit code", code)
				}
				if terminal.Status != engine.StateCompletedNoncompliant || terminal.Contract.Status != engine.ContractNoncompliant || terminal.Contract.Attempts != 1 || terminal.Contract.RetryUsed {
					t.Fatalf("read-only terminal = %#v, want one noncompliant attempt", terminal)
				}
				if got := recorder.argv(t, 1); len(got) == 0 {
					t.Fatal("fake backend did not record the initial argv")
				} else {
					assertReadOnlyArgv(t, tc.name, got, false)
				}
				recorder.assertCalls(t, 1)
			})

			t.Run("write_retries_once_with_read_only_resume", func(t *testing.T) {
				backend, recorder := fakeAdapterBackend(t, tc.name, []string{"shapeless reply", compliantReport()}, tc.newBackend)
				terminal, code := runEmbeddedTier(t, tc.name, backend, []string{"--write"})
				if code != 0 {
					t.Fatalf("exit code = %d, want 0", code)
				}
				if terminal.Status != engine.StateCompleted || terminal.Contract.Status != engine.ContractRetried || terminal.Contract.Attempts != 2 || !terminal.Contract.RetryUsed {
					t.Fatalf("write terminal = %#v, want retried compliant result", terminal)
				}
				recorder.assertCalls(t, 2)
				assertWriteArgv(t, tc.name, recorder.argv(t, 1))
				assertReadOnlyArgv(t, tc.name, recorder.argv(t, 2), true)
			})

			t.Run("no_contract_disables_stamp_without_retry", func(t *testing.T) {
				backend, recorder := fakeAdapterBackend(t, tc.name, []string{"shapeless reply"}, tc.newBackend)
				terminal, code := runEmbeddedTier(t, tc.name, backend, []string{"--no-contract"})
				if code != 0 {
					t.Fatalf("exit code = %d, want 0", code)
				}
				if terminal.Status != engine.StateCompleted || terminal.ContractKind != contractKindNone || terminal.Contract.Status != engine.ContractDisabled || terminal.Contract.Reason != "no_contract_flag" || terminal.Contract.Attempts != 0 {
					t.Fatalf("no-contract terminal = %#v, want disabled stamp", terminal)
				}
				recorder.assertCalls(t, 1)
				assertReadOnlyArgv(t, tc.name, recorder.argv(t, 1), false)
			})
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
		Prompt:  effectivePrompt(params.TaskSpec.Prompt, params.TaskSpec.Policy),
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

type adapterRecorder struct {
	dir string
}

func fakeAdapterBackend(t *testing.T, name string, reports []string, construct func(string) engine.Backend) (engine.Backend, adapterRecorder) {
	t.Helper()
	dir := t.TempDir()
	responses := make([]string, 0, len(reports))
	for _, report := range reports {
		responses = append(responses, fakeAdapterResult(name, report))
	}
	countPath := filepath.Join(dir, "calls")
	argsDir := filepath.Join(dir, "argv")
	if err := os.Mkdir(argsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var cases strings.Builder
	for i, response := range responses {
		fmt.Fprintf(&cases, "%d) printf '%%s\\n' %s ;;\n", i+1, shellQuote(response))
	}
	if len(responses) > 0 {
		fmt.Fprintf(&cases, "*) printf '%%s\\n' %s ;;\n", shellQuote(responses[len(responses)-1]))
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
count_file=%s
args_dir=%s
n=0
if [ -f "$count_file" ]; then
  n=$(cat "$count_file")
fi
n=$((n + 1))
printf '%%s\n' "$n" > "$count_file"
printf '%%s\n' "$@" > "$args_dir/$n"
case "$n" in
%s
esac
`, shellQuote(countPath), shellQuote(argsDir), cases.String())
	binary := filepath.Join(dir, "fake-"+name)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return construct(binary), adapterRecorder{dir: dir}
}

func fakeAdapterResult(backend, report string) string {
	var value any
	switch backend {
	case "codex":
		value = map[string]string{
			"type":               "task_complete",
			"session_id":         "session_smoke",
			"last_agent_message": report,
		}
	case "claude":
		value = map[string]string{
			"type":       "result",
			"session_id": "session_smoke",
			"result":     report,
		}
	default:
		panic("unsupported fake adapter backend: " + backend)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func runEmbeddedTier(t *testing.T, backendName string, backend engine.Backend, flags []string) (TerminalEnvelope, int) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	restore := stubEmbeddedGlobals(t, "job_tier_"+backendName, backend)
	defer restore()
	args := []string{
		"task", "--backend", backendName,
		"--cwd", t.TempDir(),
		"--prompt", "return a delegate report",
		"--embedded", "--wait",
	}
	args = append(args, flags...)
	var stdout, stderr bytes.Buffer
	code := run(args, nil, &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("embedded task stderr = %q", stderr.String())
	}
	var terminal TerminalEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &terminal); err != nil {
		t.Fatalf("terminal JSON invalid: %v; raw = %q", err, stdout.String())
	}
	return terminal, code
}

func (r adapterRecorder) argv(t *testing.T, call int) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "argv", fmt.Sprintf("%d", call)))
	if err != nil {
		t.Fatalf("read recorded argv %d: %v", call, err)
	}
	trimmed := strings.TrimSuffix(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (r adapterRecorder) assertCalls(t *testing.T, want int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls"))
	if err != nil {
		t.Fatalf("read fake backend call count: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != fmt.Sprint(want) {
		t.Fatalf("fake backend calls = %q, want %d", got, want)
	}
}

func assertWriteArgv(t *testing.T, backend string, args []string) {
	t.Helper()
	switch backend {
	case "codex":
		assertArgPair(t, args, "--sandbox", "workspace-write")
		assertNoArg(t, args, "--ignore-user-config")
	case "claude":
		assertHasArg(t, args, "--dangerously-skip-permissions")
		assertNoArg(t, args, "--permission-mode")
	default:
		t.Fatalf("unknown backend %q", backend)
	}
}

func assertReadOnlyArgv(t *testing.T, backend string, args []string, wantResume bool) {
	t.Helper()
	switch backend {
	case "codex":
		assertArgPair(t, args, "--sandbox", "read-only")
		assertHasArg(t, args, "--ignore-user-config")
		if wantResume {
			assertArgPair(t, args, "resume", "session_smoke")
		}
	case "claude":
		assertNoArg(t, args, "--dangerously-skip-permissions")
		assertArgPair(t, args, "--permission-mode", "dontAsk")
		assertHasArg(t, args, "--strict-mcp-config")
		if wantResume {
			assertArgPair(t, args, "--resume", "session_smoke")
		}
	default:
		t.Fatalf("unknown backend %q", backend)
	}
}

func assertHasArg(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("argv = %q, want %q", args, want)
}

func assertNoArg(t *testing.T, args []string, prohibited string) {
	t.Helper()
	for _, arg := range args {
		if arg == prohibited {
			t.Fatalf("argv = %q, must not include %q", args, prohibited)
		}
	}
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("argv = %q, want %s %s", args, flag, value)
}

func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
