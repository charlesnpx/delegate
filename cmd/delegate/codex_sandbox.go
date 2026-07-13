package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	codexSandboxConfigured        = "configured"
	codexSandboxAlreadyConfigured = "already-configured"
	codexSandboxSkipped           = "skipped"
)

type codexSandboxConfig struct {
	SandboxWorkspaceWrite codexSandboxWorkspaceWrite `toml:"sandbox_workspace_write"`
}

type codexSandboxWorkspaceWrite struct {
	WritableRoots []string `toml:"writable_roots"`
}

type codexSandboxPaths struct {
	ConfigPath    string
	WritableRoots []string
}

// codexSandboxResult is deliberately additive: callers can surface the action
// without changing the existing managed-skill file inventory schema.
type codexSandboxResult struct {
	Action        string   `json:"action"`
	ConfigPath    string   `json:"configPath,omitempty"`
	WritableRoots []string `json:"writableRoots,omitempty"`
	Warning       string   `json:"warning,omitempty"`
}

var (
	sandboxTableHeaderRE = regexp.MustCompile(`(?m)^[\t ]*\[[\t ]*sandbox_workspace_write[\t ]*\][\t ]*(?:#.*)?$`)
	tomlTableHeaderRE    = regexp.MustCompile(`(?m)^[\t ]*\[`)
	writableRootsRE      = regexp.MustCompile(`(?m)^[\t ]*writable_roots[\t ]*=[\t ]*\[`)
)

// runConfigureCodexSandbox is an internal command used by the shell installer.
// It intentionally stays out of the public help text.
func runConfigureCodexSandbox(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate configure-codex-sandbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate configure-codex-sandbox does not accept positional arguments")
	}
	result := configureCodexSandbox()
	if *jsonOut {
		return 0, writeJSONLine(stdout, result)
	}
	if result.Warning != "" {
		_, err := fmt.Fprintf(stdout, "codex sandbox writable_roots %s: %s\n", result.Action, result.Warning)
		return 0, err
	}
	_, err := fmt.Fprintf(stdout, "codex sandbox writable_roots %s: %s\n", result.Action, result.ConfigPath)
	return 0, err
}

func configureCodexSandbox() codexSandboxResult {
	paths, err := resolveCodexSandboxPaths(os.Getenv, os.UserHomeDir)
	if err != nil {
		return codexSandboxResult{Action: codexSandboxSkipped, Warning: err.Error()}
	}
	return configureCodexSandboxAt(paths)
}

func resolveCodexSandboxPaths(env func(string) string, homeDir func() (string, error)) (codexSandboxPaths, error) {
	home, err := homeDir()
	if err != nil {
		return codexSandboxPaths{}, fmt.Errorf("resolve home for Codex sandbox: %w", err)
	}
	home, err = absoluteCleanPath("home directory", home)
	if err != nil {
		return codexSandboxPaths{}, err
	}

	codexHome := env("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexHome, err = absoluteCleanPath("CODEX_HOME", codexHome)
	if err != nil {
		return codexSandboxPaths{}, err
	}

	stateHome := env("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	stateHome, err = absoluteCleanPath("XDG_STATE_HOME", stateHome)
	if err != nil {
		return codexSandboxPaths{}, err
	}
	return codexSandboxPaths{
		ConfigPath: filepath.Join(codexHome, "config.toml"),
		WritableRoots: []string{
			filepath.Join(stateHome, "agentbus"),
			filepath.Join(stateHome, "delegate"),
		},
	}, nil
}

func absoluteCleanPath(label, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s %q must be absolute", label, path)
	}
	return filepath.Clean(path), nil
}

func configureCodexSandboxAt(paths codexSandboxPaths) codexSandboxResult {
	result := codexSandboxResult{
		ConfigPath:    paths.ConfigPath,
		WritableRoots: append([]string(nil), paths.WritableRoots...),
	}
	raw, err := os.ReadFile(paths.ConfigPath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return codexSandboxSkippedResult(result, fmt.Sprintf("read Codex config %q: %v", paths.ConfigPath, err))
	}
	if !exists {
		raw = nil
	}

	parsed, err := decodeCodexSandboxConfig(raw)
	if err != nil {
		return codexSandboxSkippedResult(result, fmt.Sprintf("TOML parse failed for Codex config %q: %v", paths.ConfigPath, err))
	}
	if allWritableRootsPresent(parsed.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots) {
		result.Action = codexSandboxAlreadyConfigured
		return result
	}

	missing := missingWritableRoots(parsed.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots)
	candidate, err := spliceCodexSandboxRoots(raw, missing)
	if err != nil {
		return codexSandboxSkippedResult(result, fmt.Sprintf("could not minimally update Codex config %q: %v", paths.ConfigPath, err))
	}
	validated, err := decodeCodexSandboxConfig(candidate)
	if err != nil {
		return codexSandboxSkippedResult(result, fmt.Sprintf("generated TOML did not parse for Codex config %q: %v", paths.ConfigPath, err))
	}
	if !allWritableRootsPresent(validated.SandboxWorkspaceWrite.WritableRoots, paths.WritableRoots) {
		return codexSandboxSkippedResult(result, fmt.Sprintf("generated TOML for Codex config %q did not contain both writable roots", paths.ConfigPath))
	}

	mode := os.FileMode(0o600)
	if exists {
		info, statErr := os.Stat(paths.ConfigPath)
		if statErr != nil {
			return codexSandboxSkippedResult(result, fmt.Sprintf("stat Codex config %q: %v", paths.ConfigPath, statErr))
		}
		mode = info.Mode().Perm()
	}
	if err := atomicWriteCodexConfig(paths.ConfigPath, candidate, mode); err != nil {
		return codexSandboxSkippedResult(result, fmt.Sprintf("write Codex config %q: %v", paths.ConfigPath, err))
	}
	result.Action = codexSandboxConfigured
	return result
}

func codexSandboxSkippedResult(result codexSandboxResult, reason string) codexSandboxResult {
	result.Action = codexSandboxSkipped
	result.Warning = reason + ". Add this snippet manually:\n" + codexSandboxSnippet(result.WritableRoots)
	return result
}

func codexSandboxSnippet(roots []string) string {
	var b strings.Builder
	b.WriteString("[sandbox_workspace_write]\n")
	b.WriteString("writable_roots = [\n")
	for _, root := range roots {
		fmt.Fprintf(&b, "  %s,\n", strconv.Quote(root))
	}
	b.WriteString("]")
	return b.String()
}

func decodeCodexSandboxConfig(raw []byte) (codexSandboxConfig, error) {
	var config codexSandboxConfig
	if _, err := toml.Decode(string(raw), &config); err != nil {
		return codexSandboxConfig{}, err
	}
	return config, nil
}

func allWritableRootsPresent(current, required []string) bool {
	return len(missingWritableRoots(current, required)) == 0
}

func missingWritableRoots(current, required []string) []string {
	missing := make([]string, 0, len(required))
	for _, root := range required {
		found := false
		for _, present := range current {
			if present == root {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, root)
		}
	}
	return missing
}

func spliceCodexSandboxRoots(raw []byte, missing []string) ([]byte, error) {
	if len(missing) == 0 {
		return append([]byte(nil), raw...), nil
	}
	start, end, found := sandboxWorkspaceWriteTable(raw)
	if !found {
		return appendSandboxWorkspaceWriteTable(raw, missing), nil
	}
	assignment := writableRootsRE.FindIndex(raw[start:end])
	if assignment == nil {
		return insertWritableRootsInSandboxTable(raw, end, missing), nil
	}
	arrayStart := start + assignment[1] - 1 // The expression is anchored on the opening '['.
	arrayEnd, err := tomlArrayEnd(raw, arrayStart)
	if err != nil || arrayEnd >= end {
		if err == nil {
			err = errors.New("writable_roots array extends beyond its table")
		}
		return nil, err
	}
	return appendRootsToTOMLArray(raw, arrayStart, arrayEnd, missing), nil
}

// sandboxWorkspaceWriteTable returns the content bounds of the explicit table.
// We avoid converting TOML through a serializer because that loses user comments
// and formatting; all additions below are byte splices into the original text.
func sandboxWorkspaceWriteTable(raw []byte) (int, int, bool) {
	header := sandboxTableHeaderRE.FindIndex(raw)
	if header == nil {
		return 0, 0, false
	}
	start := header[1]
	end := len(raw)
	if next := tomlTableHeaderRE.FindIndex(raw[start:]); next != nil {
		end = start + next[0]
	}
	return start, end, true
}

func appendSandboxWorkspaceWriteTable(raw []byte, roots []string) []byte {
	out := append([]byte(nil), raw...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n\n")) {
		out = append(out, '\n')
	}
	out = append(out, []byte(codexSandboxSnippet(roots)+"\n")...)
	return out
}

func insertWritableRootsInSandboxTable(raw []byte, at int, roots []string) []byte {
	addition := codexSandboxSnippet(roots)
	addition = strings.TrimPrefix(addition, "[sandbox_workspace_write]\n")
	if at > 0 && raw[at-1] != '\n' {
		addition = "\n" + addition
	}
	addition += "\n"
	out := make([]byte, 0, len(raw)+len(addition))
	out = append(out, raw[:at]...)
	out = append(out, addition...)
	out = append(out, raw[at:]...)
	return out
}

func tomlArrayEnd(raw []byte, start int) (int, error) {
	if start < 0 || start >= len(raw) || raw[start] != '[' {
		return 0, errors.New("writable_roots opening bracket was not found")
	}
	depth := 0
	var quote byte
	escaped := false
	for i := start; i < len(raw); i++ {
		ch := raw[i]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			quote = ch
		case '#':
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("writable_roots array has no closing bracket")
}

func appendRootsToTOMLArray(raw []byte, start, end int, roots []string) []byte {
	var addition strings.Builder
	if tomlArrayHasValuesWithoutTrailingComma(raw, start, end) {
		addition.WriteByte(',')
	}
	for _, root := range roots {
		addition.WriteString("\n  ")
		addition.WriteString(strconv.Quote(root))
		addition.WriteByte(',')
	}
	addition.WriteByte('\n')
	out := make([]byte, 0, len(raw)+addition.Len())
	out = append(out, raw[:end]...)
	out = append(out, addition.String()...)
	out = append(out, raw[end:]...)
	return out
}

func tomlArrayHasValuesWithoutTrailingComma(raw []byte, start, end int) bool {
	var significant bytes.Buffer
	var quote byte
	escaped := false
	for i := start + 1; i < end; i++ {
		ch := raw[i]
		if quote != 0 {
			significant.WriteByte(ch)
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			significant.WriteByte(ch)
			continue
		}
		if ch == '#' {
			for i < end && raw[i] != '\n' {
				i++
			}
			continue
		}
		significant.WriteByte(ch)
	}
	trimmed := bytes.TrimSpace(significant.Bytes())
	return len(trimmed) > 0 && trimmed[len(trimmed)-1] != ','
}

func atomicWriteCodexConfig(path string, raw []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Codex config directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config.toml-*")
	if err != nil {
		return fmt.Errorf("create temporary Codex config in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("preserve Codex config mode: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("write temporary Codex config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary Codex config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary Codex config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace Codex config: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open Codex config directory for sync: %w", err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("sync Codex config directory: %w", err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("close Codex config directory: %w", err)
	}
	cleanup = false
	return nil
}
