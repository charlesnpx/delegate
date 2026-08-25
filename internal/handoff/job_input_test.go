package handoff

import (
	"errors"
	"os"
	"testing"

	"github.com/charlesnpx/agentbus/engine"
)

func TestPersistAndCleanJobInput(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := PersistJobInput(JobInputOptions{
		StateDir: stateDir,
		JobID:    "job_input_test",
		Prompt:   "private prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(input.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "private prompt" {
		t.Fatalf("input=%q", raw)
	}
	deleted, err := DeleteJobInputOnTerminalState(input, engine.StateCompleted, "verified_absent", Hooks{})
	if err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	if _, err := os.Stat(input.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input remains: %v", err)
	}
}

func TestJobInputCleanupRequiresTerminalSafetyProof(t *testing.T) {
	input := JobInput{JobID: "job", Path: "ignored"}
	if deleted, err := DeleteJobInputOnTerminalState(input, engine.StateRunning, "verified_absent", Hooks{}); err != nil || deleted {
		t.Fatalf("running deleted=%v err=%v", deleted, err)
	}
	if deleted, err := DeleteJobInputOnTerminalState(input, engine.StateCompleted, "unresolved", Hooks{}); err != nil || deleted {
		t.Fatalf("unresolved deleted=%v err=%v", deleted, err)
	}
}
