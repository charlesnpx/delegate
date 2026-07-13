package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureCodexSandboxFixtures(t *testing.T) {
	for _, tc := range []struct {
		name              string
		initial           *string
		wantAction        string
		wantRawFragments  []string
		wantByteIdentical bool
	}{
		{
			name:             "missing file",
			wantAction:       codexSandboxConfigured,
			wantRawFragments: []string{"[sandbox_workspace_write]", "writable_roots = ["},
		},
		{
			name:             "empty file",
			initial:          stringPtr(""),
			wantAction:       codexSandboxConfigured,
			wantRawFragments: []string{"[sandbox_workspace_write]", "writable_roots = ["},
		},
		{
			name:       "unrelated content and comments",
			initial:    stringPtr("# Preserve this user comment\nmodel = \"gpt-5\"\n\n[profiles.default]\nsandbox_mode = \"workspace-write\" # And this one\n"),
			wantAction: codexSandboxConfigured,
			wantRawFragments: []string{
				"# Preserve this user comment",
				"sandbox_mode = \"workspace-write\" # And this one",
			},
		},
		{
			name:             "existing section with one root",
			initial:          stringPtr("[sandbox_workspace_write]\n# Keep this comment.\nwritable_roots = [\n  \"STATE/agentbus\" # Keep this array comment.\n]\n"),
			wantAction:       codexSandboxConfigured,
			wantRawFragments: []string{"# Keep this comment.", "# Keep this array comment."},
		},
		{
			name:              "both roots are a byte-identical no-op",
			initial:           stringPtr("# This file must remain byte-for-byte unchanged.\n[sandbox_workspace_write]\nwritable_roots = [\n  \"STATE/agentbus\",\n  \"STATE/delegate\",\n]\n"),
			wantAction:        codexSandboxAlreadyConfigured,
			wantByteIdentical: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			paths := codexSandboxPaths{
				ConfigPath: filepath.Join(tmp, ".codex", "config.toml"),
				WritableRoots: []string{
					filepath.Join(tmp, "state", "agentbus"),
					filepath.Join(tmp, "state", "delegate"),
				},
			}
			initial := ""
			if tc.initial != nil {
				initial = strings.ReplaceAll(*tc.initial, "STATE", filepath.Join(tmp, "state"))
				if err := os.MkdirAll(filepath.Dir(paths.ConfigPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.ConfigPath, []byte(initial), 0o640); err != nil {
					t.Fatal(err)
				}
			}

			result := configureCodexSandboxAt(paths)
			if result.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q; warning=%q", result.Action, tc.wantAction, result.Warning)
			}
			raw, err := os.ReadFile(paths.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantByteIdentical && !bytes.Equal(raw, []byte(initial)) {
				t.Fatalf("already-configured config changed:\n got %q\nwant %q", raw, initial)
			}
			if tc.initial != nil {
				info, err := os.Stat(paths.ConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
					t.Fatalf("config mode = %o, want preserved %o", got, want)
				}
			}
			for _, fragment := range tc.wantRawFragments {
				if !strings.Contains(string(raw), fragment) {
					t.Fatalf("config missing preserved fragment %q:\n%s", fragment, raw)
				}
			}
			parsed, err := decodeCodexSandboxConfig(raw)
			if err != nil {
				t.Fatalf("result does not parse: %v\n%s", err, raw)
			}
			if !allWritableRootsPresent(parsed.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots) {
				t.Fatalf("writable_roots = %#v, want both %#v", parsed.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots)
			}
		})
	}
}

func TestConfigureCodexSandboxMalformedConfigIsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	paths := codexSandboxPaths{
		ConfigPath: filepath.Join(tmp, ".codex", "config.toml"),
		WritableRoots: []string{
			filepath.Join(tmp, "state", "agentbus"),
			filepath.Join(tmp, "state", "delegate"),
		},
	}
	initial := "[sandbox_workspace_write\nwritable_roots = [\"not valid\"]\n"
	if err := os.MkdirAll(filepath.Dir(paths.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	result := configureCodexSandboxAt(paths)
	if result.Action != codexSandboxSkipped {
		t.Fatalf("action = %q, want skipped", result.Action)
	}
	if !strings.Contains(result.Warning, "Add this snippet manually:") || !strings.Contains(result.Warning, paths.WritableRoots[0]) || !strings.Contains(result.Warning, paths.WritableRoots[1]) {
		t.Fatalf("warning = %q, want manual snippet with both roots", result.Warning)
	}
	raw, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != initial {
		t.Fatalf("malformed config changed:\n got %q\nwant %q", raw, initial)
	}
}

func TestResolveCodexSandboxPathsUsesXDGStateHome(t *testing.T) {
	paths, err := resolveCodexSandboxPaths(func(key string) string {
		switch key {
		case "CODEX_HOME":
			return "/opt/codex"
		case "XDG_STATE_HOME":
			return "/var/state"
		default:
			return ""
		}
	}, func() (string, error) { return "/home/delegate", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths.ConfigPath, "/opt/codex/config.toml"; got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
	if got, want := strings.Join(paths.WritableRoots, ","), "/var/state/agentbus,/var/state/delegate"; got != want {
		t.Fatalf("writable roots = %q, want %q", got, want)
	}
}

func stringPtr(value string) *string { return &value }
