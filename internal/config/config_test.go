package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingUsesDefaultSemantics(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Overridable || cfg.Backend.Claude.Model != "" || cfg.Backend.Codex.Effort != "" {
		t.Fatalf("missing config = %#v, want overridable with no defaults", cfg)
	}
}

func TestLoadRejectsMalformedAndUnknownKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"malformed": "overridable = [\n",
		"unknown":   "overridable = true\n[backend.claude]\nmodle = \"typo\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("Load() error = %v, want config path %q", err, path)
			}
			if name == "unknown" && !strings.Contains(err.Error(), "modle") {
				t.Fatalf("unknown-key error = %v, want key name", err)
			}
		})
	}
}

func TestSaveRoundTripUsesPrivateAtomicDestination(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	cfg := Config{Overridable: false, Backend: Backends{
		Claude: Defaults{Model: "opus", Effort: "high"},
		Codex:  Defaults{Model: "gpt-test", Effort: "xhigh"},
	}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != configFileMode {
		t.Fatalf("config mode = %o, want %o", got, configFileMode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != configDirMode {
		t.Fatalf("config dir mode = %o, want %o", got, configDirMode)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Overridable || loaded.Backend.Claude.Model != "opus" || loaded.Backend.Claude.Effort != "high" || loaded.Backend.Codex.Model != "gpt-test" || loaded.Backend.Codex.Effort != "xhigh" {
		t.Fatalf("round-trip config = %#v", loaded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config.toml-") {
			t.Fatalf("atomic temporary file left behind: %s", entry.Name())
		}
	}
}

func TestSaveAllowsSymlinkedConfigDirectoryWithoutChangingTargetPermissions(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "delegate-config-target")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "delegate")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	cfg := Config{Overridable: false, Backend: Backends{
		Codex: Defaults{Model: "gpt-test", Effort: "high"},
	}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	resolvedPath := filepath.Join(target, "config.toml")
	fileInfo, err := os.Stat(resolvedPath)
	if err != nil {
		t.Fatalf("config was not written to symlink target: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != configFileMode {
		t.Fatalf("config mode = %o, want %o", got, configFileMode)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o750 {
		t.Fatalf("pre-existing symlink target mode = %o, want 750", got)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Overridable || loaded.Backend.Codex.Model != "gpt-test" || loaded.Backend.Codex.Effort != "high" {
		t.Fatalf("round-trip config = %#v", loaded)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config.toml-") {
			t.Fatalf("atomic temporary file left behind: %s", entry.Name())
		}
	}
}

func TestAccessorsValidateKeysAndUnset(t *testing.T) {
	cfg := Config{Overridable: true}
	if err := cfg.Set(KeyClaudeModel, "opus"); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Get(KeyClaudeModel); err != nil || got != "opus" {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	if err := cfg.Set(KeyOverridable, "wrong"); err == nil {
		t.Fatal("Set(overridable, wrong) succeeded")
	}
	if err := cfg.Unset(KeyClaudeModel); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Get(KeyClaudeModel); err != nil || got != "" {
		t.Fatalf("Get after unset = %q, %v", got, err)
	}
	if _, err := cfg.Get("backend.claude.modle"); err == nil || !strings.Contains(err.Error(), "allowed keys") {
		t.Fatalf("Get unknown error = %v", err)
	}
	if err := cfg.Set(KeyOverridable, "false"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Unset(KeyOverridable); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Get(KeyOverridable); err != nil || got != "true" {
		t.Fatalf("Get overridable after unset = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "does-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sanity error check = %v", err)
	}
}
