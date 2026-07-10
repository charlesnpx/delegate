package handoff

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/engine"
)

func TestPackageRole(t *testing.T) {
	if packageRole != "handoff" {
		t.Fatalf("packageRole = %q, want %q", packageRole, "handoff")
	}
}

func TestPromptSourceUsageDocumentsARGVVisibility(t *testing.T) {
	for name, usage := range map[string]string{
		"prompt":     PromptFlagUsage,
		"positional": PositionalPromptUsage,
	} {
		if !strings.Contains(usage, "argv") {
			t.Fatalf("%s usage does not document argv visibility: %q", name, usage)
		}
		if !strings.Contains(usage, "history") {
			t.Fatalf("%s usage does not document shell history visibility: %q", name, usage)
		}
	}
}

func TestResolvePromptSources(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	handoffFile := filepath.Join(dir, "handoff.txt")
	if err := os.WriteFile(promptFile, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffFile, []byte("from handoff"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		src  PromptSources
		want ResolvedPrompt
	}{
		{
			name: "prompt",
			src:  PromptSources{Prompt: "inline"},
			want: ResolvedPrompt{Prompt: "inline", Source: SourcePrompt},
		},
		{
			name: "empty explicit prompt",
			src:  PromptSources{PromptSet: true},
			want: ResolvedPrompt{Source: SourcePrompt},
		},
		{
			name: "prompt file",
			src:  PromptSources{PromptFile: promptFile},
			want: ResolvedPrompt{Prompt: "from file", Source: SourcePromptFile},
		},
		{
			name: "stdin",
			src:  PromptSources{PromptStdin: true, Stdin: strings.NewReader("from stdin")},
			want: ResolvedPrompt{Prompt: "from stdin", Source: SourcePromptStdin},
		},
		{
			name: "handoff file",
			src:  PromptSources{HandoffPromptFile: handoffFile},
			want: ResolvedPrompt{Prompt: "from handoff", Source: SourceHandoffPromptFile, HandoffPath: handoffFile},
		},
		{
			name: "positional",
			src:  PromptSources{Positional: []string{"from", "argv"}},
			want: ResolvedPrompt{Prompt: "from argv", Source: SourcePositional},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePrompt(tt.src)
			if err != nil {
				t.Fatalf("ResolvePrompt() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ResolvePrompt() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolvePromptSourceExclusivityMatrix(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	handoffFile := filepath.Join(dir, "handoff.txt")
	if err := os.WriteFile(promptFile, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffFile, []byte("from handoff"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := []struct {
		name  string
		apply func(*PromptSources)
	}{
		{name: "--prompt", apply: func(s *PromptSources) { s.PromptSet = true; s.Prompt = "inline" }},
		{name: "--prompt-file", apply: func(s *PromptSources) { s.PromptFile = promptFile }},
		{name: "--prompt-stdin", apply: func(s *PromptSources) { s.PromptStdin = true; s.Stdin = strings.NewReader("stdin") }},
		{name: "--handoff-prompt-file", apply: func(s *PromptSources) { s.HandoffPromptFile = handoffFile }},
		{name: "positional", apply: func(s *PromptSources) { s.Positional = []string{"positional"} }},
	}
	for i := range sources {
		for j := range sources {
			if i == j {
				continue
			}
			t.Run(sources[i].name+"+"+sources[j].name, func(t *testing.T) {
				var src PromptSources
				sources[i].apply(&src)
				sources[j].apply(&src)
				_, err := ResolvePrompt(src)
				if !errors.Is(err, ErrMultiplePromptSources) {
					t.Fatalf("ResolvePrompt() error = %v, want ErrMultiplePromptSources", err)
				}
			})
		}
	}
}

func TestResolvePromptRequiresSource(t *testing.T) {
	_, err := ResolvePrompt(PromptSources{})
	if !errors.Is(err, ErrNoPromptSource) {
		t.Fatalf("ResolvePrompt() error = %v, want ErrNoPromptSource", err)
	}
}

func TestCreateHandoffUsesPrivateModes(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state", "delegate")
	result, err := Create(CreateOptions{
		StateDir: stateDir,
		Reader:   strings.NewReader("secret prompt"),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.Schema != 1 {
		t.Fatalf("schema = %d, want 1", result.Schema)
	}
	assertMode(t, stateDir, 0o700)
	assertMode(t, result.HandoffPath, 0o600)
	raw, err := os.ReadFile(result.HandoffPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "secret prompt" {
		t.Fatalf("handoff file = %q, want %q", raw, "secret prompt")
	}
}

func TestPersistJobInputUsesPrivateModeAndUnlinksHandoffAfterDurableWrite(t *testing.T) {
	stateDir := t.TempDir()
	handoffResult, err := Create(CreateOptions{
		StateDir: stateDir,
		Reader:   strings.NewReader("durable prompt"),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	prompt, err := ResolvePrompt(PromptSources{HandoffPromptFile: handoffResult.HandoffPath})
	if err != nil {
		t.Fatalf("ResolvePrompt() error = %v", err)
	}
	var order []string
	hooks := Hooks{
		AfterSync: func(path string, target SyncTarget) {
			switch {
			case target == SyncFile && strings.HasPrefix(filepath.Base(path), jobInputPrefix):
				order = append(order, "job-input-file-sync")
			case target == SyncDir && path == stateDir:
				order = append(order, "state-dir-sync")
			}
		},
		BeforeRemove: func(path string) {
			if path == handoffResult.HandoffPath {
				order = append(order, "handoff-unlink")
			}
		},
	}
	input, err := PersistJobInput(JobInputOptions{
		StateDir: stateDir,
		JobID:    "job-123",
		Prompt:   prompt,
		Hooks:    hooks,
	})
	if err != nil {
		t.Fatalf("PersistJobInput() error = %v", err)
	}
	if _, err := os.Stat(handoffResult.HandoffPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff file still exists or unexpected stat error: %v", err)
	}
	assertMode(t, input.Path, 0o600)
	raw, err := os.ReadFile(input.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "durable prompt" {
		t.Fatalf("job input = %q, want %q", raw, "durable prompt")
	}
	want := []string{"job-input-file-sync", "state-dir-sync", "handoff-unlink"}
	if len(order) < len(want) || !reflect.DeepEqual(order[:len(want)], want) {
		t.Fatalf("order = %#v, want prefix %#v", order, want)
	}
}

func TestDeleteJobInputHooks(t *testing.T) {
	t.Run("session recorded", func(t *testing.T) {
		input := persistTestJobInput(t, "session-job", engine.StateRunning)
		deleted, err := DeleteJobInputOnSessionRecorded(input, Hooks{})
		if err != nil {
			t.Fatalf("DeleteJobInputOnSessionRecorded() error = %v", err)
		}
		if !deleted {
			t.Fatal("DeleteJobInputOnSessionRecorded() deleted = false, want true")
		}
		assertMissing(t, input.Path)
	})
	t.Run("terminal pre-launch failure", func(t *testing.T) {
		input := persistTestJobInput(t, "prelaunch-job", engine.StateStarting)
		deleted, err := DeleteJobInputOnPreLaunchTerminal(input, Hooks{})
		if err != nil {
			t.Fatalf("DeleteJobInputOnPreLaunchTerminal() error = %v", err)
		}
		if !deleted {
			t.Fatal("DeleteJobInputOnPreLaunchTerminal() deleted = false, want true")
		}
		assertMissing(t, input.Path)
	})
	t.Run("non-terminal state keeps file", func(t *testing.T) {
		input := persistTestJobInput(t, "running-job", engine.StateRunning)
		deleted, err := DeleteJobInputOnTerminalState(input, engine.StateRunning, Hooks{})
		if err != nil {
			t.Fatalf("DeleteJobInputOnTerminalState() error = %v", err)
		}
		if deleted {
			t.Fatal("DeleteJobInputOnTerminalState() deleted running job")
		}
		assertExists(t, input.Path)
	})
	for _, state := range []engine.JobState{
		engine.StateCompleted,
		engine.StateCompletedNoncompliant,
		engine.StateFailed,
		engine.StateTimedOut,
		engine.StateInterrupted,
		engine.StateCanceled,
		engine.StateReaped,
		engine.StateQuarantined,
	} {
		t.Run(string(state), func(t *testing.T) {
			input := persistTestJobInput(t, "terminal-"+string(state), state)
			deleted, err := DeleteJobInputOnTerminalState(input, state, Hooks{})
			if err != nil {
				t.Fatalf("DeleteJobInputOnTerminalState() error = %v", err)
			}
			if !deleted {
				t.Fatal("DeleteJobInputOnTerminalState() deleted = false, want true")
			}
			assertMissing(t, input.Path)
		})
	}
}

func TestSweepTerminalJobInputs(t *testing.T) {
	stateDir := t.TempDir()
	completed := persistTestJobInputInDir(t, stateDir, "job-completed")
	running := persistTestJobInputInDir(t, stateDir, "job-running")
	unknown := persistTestJobInputInDir(t, stateDir, "job-unknown")
	states := map[string]engine.JobState{
		"job-completed": engine.StateCompleted,
		"job-running":   engine.StateRunning,
	}
	removed, err := SweepTerminalJobInputs(stateDir, func(jobID string) (engine.JobState, bool, error) {
		state, ok := states[jobID]
		return state, ok, nil
	}, Hooks{})
	if err != nil {
		t.Fatalf("SweepTerminalJobInputs() error = %v", err)
	}
	if len(removed) != 1 || removed[0].JobID != "job-completed" {
		t.Fatalf("removed = %#v, want job-completed only", removed)
	}
	assertMissing(t, completed.Path)
	assertExists(t, running.Path)
	assertExists(t, unknown.Path)
}

func TestMarshalCreateResult(t *testing.T) {
	raw, err := MarshalCreateResult(CreateResult{HandoffPath: "/tmp/handoff"})
	if err != nil {
		t.Fatalf("MarshalCreateResult() error = %v", err)
	}
	want := []byte(`{"schema":1,"handoff_path":"/tmp/handoff"}` + "\n")
	if !bytes.Equal(raw, want) {
		t.Fatalf("MarshalCreateResult() = %q, want %q", raw, want)
	}
}

func persistTestJobInput(t *testing.T, jobID string, _ engine.JobState) JobInput {
	t.Helper()
	return persistTestJobInputInDir(t, t.TempDir(), jobID)
}

func persistTestJobInputInDir(t *testing.T, stateDir, jobID string) JobInput {
	t.Helper()
	input, err := PersistJobInput(JobInputOptions{
		StateDir: stateDir,
		JobID:    jobID,
		Prompt: ResolvedPrompt{
			Prompt: "prompt for " + jobID,
			Source: SourcePrompt,
		},
	})
	if err != nil {
		t.Fatalf("PersistJobInput() error = %v", err)
	}
	return input
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or stat error = %v", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s stat error = %v", path, err)
	}
}
