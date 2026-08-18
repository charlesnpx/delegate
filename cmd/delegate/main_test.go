package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestVersionLine(t *testing.T) {
	const want = "delegate 0.0.0-dev"
	if got := versionLine(); got != want {
		t.Fatalf("versionLine() = %q, want %q", got, want)
	}
}

func TestRunHandoffCreateJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"handoff", "create", "--json"}, bytes.NewBufferString("from stdin"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var result struct {
		Schema      int    `json:"schema"`
		HandoffPath string `json:"handoff_path"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("handoff JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if result.Schema != 1 {
		t.Fatalf("schema = %d, want 1", result.Schema)
	}
	raw, err := os.ReadFile(result.HandoffPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "from stdin" {
		t.Fatalf("handoff file = %q, want %q", raw, "from stdin")
	}
	info, err := os.Stat(result.HandoffPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("handoff mode = %o, want 600", got)
	}
}

func TestRunHandoffCreateRequiresJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"handoff", "create"}, bytes.NewBufferString("prompt"), &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() code = 0, want error")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("requires --json")) {
		t.Fatalf("stderr = %q, want requires --json", stderr.String())
	}
}

func TestRunHelpAliases(t *testing.T) {
	for _, alias := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{alias}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%q) code = %d, want 0; stderr=%q", alias, code, stderr.String())
		}
		if !bytes.Contains(stdout.Bytes(), []byte("usage: delegate <command>")) {
			t.Fatalf("run(%q) stdout = %q, want usage text", alias, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("run(%q) stderr = %q, want empty", alias, stderr.String())
		}
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"nope"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("usage: delegate <command>")) {
		t.Fatalf("stderr = %q, want usage text", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte(`unknown command "nope"`)) {
		t.Fatalf("stderr = %q, want unknown command", stderr.String())
	}
}

func TestRunHandoffCreateRejectsPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"handoff", "create", "--json", "extra"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() code = 0, want error")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("does not accept positional")) {
		t.Fatalf("stderr = %q, want positional error", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "delegate 0.0.0-dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("version JSON invalid: %v; raw = %q", err, stdout.String())
	}
	if result.Version != Version {
		t.Fatalf("version = %q, want %q", result.Version, Version)
	}
}

func TestRunVersionFlagMatchesVersionSubcommand(t *testing.T) {
	for _, alias := range []string{"--version", "-version", "-V"} {
		var wantStdout, wantStderr bytes.Buffer
		wantCode := run([]string{"version"}, nil, &wantStdout, &wantStderr)

		var gotStdout, gotStderr bytes.Buffer
		gotCode := run([]string{alias}, nil, &gotStdout, &gotStderr)

		if gotCode != wantCode || gotStdout.String() != wantStdout.String() || gotStderr.String() != wantStderr.String() {
			t.Fatalf("run(%q) = code=%d stdout=%q stderr=%q, want code=%d stdout=%q stderr=%q",
				alias, gotCode, gotStdout.String(), gotStderr.String(),
				wantCode, wantStdout.String(), wantStderr.String())
		}
	}
}

func TestRunVersionFlagJSONMatchesVersionJSONSubcommand(t *testing.T) {
	var wantStdout, wantStderr bytes.Buffer
	wantCode := run([]string{"version", "--json"}, nil, &wantStdout, &wantStderr)

	var gotStdout, gotStderr bytes.Buffer
	gotCode := run([]string{"--version", "--json"}, nil, &gotStdout, &gotStderr)

	if gotCode != wantCode || gotStdout.String() != wantStdout.String() || gotStderr.String() != wantStderr.String() {
		t.Fatalf("run(--version --json) = code=%d stdout=%q stderr=%q, want code=%d stdout=%q stderr=%q",
			gotCode, gotStdout.String(), gotStderr.String(),
			wantCode, wantStdout.String(), wantStderr.String())
	}
}

func TestRunFlagParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"handoff", "create", "--bad"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() code = 0, want error")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("flag provided but not defined")) {
		t.Fatalf("stderr = %q, want flag parse error", stderr.String())
	}
}
