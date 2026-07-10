package handoff

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	fileMode = 0o600
	dirMode  = 0o700
)

// StateConfig controls state directory resolution.
type StateConfig struct {
	StateDir string
	Env      func(string) string
	HomeDir  func() (string, error)
}

// CreateOptions controls handoff file creation.
type CreateOptions struct {
	StateDir string
	Reader   io.Reader
	Hooks    Hooks
}

// CreateResult is the JSON handoff creation envelope.
type CreateResult struct {
	Schema      int    `json:"schema"`
	HandoffPath string `json:"handoff_path"`
}

// ResolveStateDir returns the delegate state directory using the XDG state fallback.
func ResolveStateDir(cfg StateConfig) (string, error) {
	if cfg.StateDir != "" {
		return resolveConfiguredStateDir("state dir", cfg.StateDir)
	}
	env := cfg.Env
	if env == nil {
		env = os.Getenv
	}
	if xdg := env("XDG_STATE_HOME"); xdg != "" {
		if err := validateConfiguredPath("XDG_STATE_HOME", xdg); err != nil {
			return "", err
		}
		return canonicalizeStatePath(filepath.Join(xdg, "delegate"))
	}
	homeDir := cfg.HomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("home directory is empty")
	}
	if !filepath.IsAbs(home) {
		home, err = filepath.Abs(home)
		if err != nil {
			return "", err
		}
	}
	return canonicalizeStatePath(filepath.Join(home, ".local", "state", "delegate"))
}

// EnsureStateDir creates the delegate state directory with private permissions.
func EnsureStateDir(dir string) error {
	var err error
	if dir == "" {
		dir, err = ResolveStateDir(StateConfig{})
		if err != nil {
			return err
		}
	} else {
		dir, err = ResolveStateDir(StateConfig{StateDir: dir})
		if err != nil {
			return err
		}
	}
	if err := rejectExistingStateDirSymlink(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state dir %q must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("state dir %q is not a directory", dir)
	}
	if got := info.Mode().Perm(); got != dirMode {
		return fmt.Errorf("state dir %q mode = %o, want %o", dir, got, dirMode)
	}
	return nil
}

func prepareStateDir(dir string) (string, error) {
	stateDir, err := ResolveStateDir(StateConfig{StateDir: dir})
	if err != nil {
		return "", err
	}
	if err := EnsureStateDir(stateDir); err != nil {
		return "", err
	}
	return stateDir, nil
}

func resolveConfiguredStateDir(label, path string) (string, error) {
	if err := validateConfiguredPath(label, path); err != nil {
		return "", err
	}
	return canonicalizeStatePath(path)
}

func validateConfiguredPath(label, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be absolute", label)
	}
	if hasParentTraversal(path) {
		return fmt.Errorf("%s must not contain parent traversal", label)
	}
	return nil
}

func hasParentTraversal(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func canonicalizeStatePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	if err := rejectExistingStateDirSymlink(clean); err != nil {
		return "", err
	}

	existing := clean
	var missing []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing parent for state dir %q", clean)
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for _, part := range missing {
		resolved = filepath.Join(resolved, part)
	}
	return filepath.Clean(resolved), nil
}

func rejectExistingStateDirSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state dir %q must not be a symlink", path)
	}
	return nil
}

// Create writes stdin-style prompt content to a private handoff file.
func Create(opts CreateOptions) (CreateResult, error) {
	reader := opts.Reader
	if reader == nil {
		reader = os.Stdin
	}
	stateDir, err := prepareStateDir(opts.StateDir)
	if err != nil {
		return CreateResult{}, err
	}
	file, err := os.CreateTemp(stateDir, "handoff-*.prompt")
	if err != nil {
		return CreateResult{}, err
	}
	path := file.Name()
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = closeFile()
			_ = os.Remove(path)
		}
	}()
	if err := os.Chmod(path, fileMode); err != nil {
		return CreateResult{}, err
	}
	if _, err := io.Copy(file, reader); err != nil {
		return CreateResult{}, err
	}
	if err := syncFile(file, path, opts.Hooks); err != nil {
		return CreateResult{}, err
	}
	if err := closeFile(); err != nil {
		return CreateResult{}, err
	}
	if err := syncDir(stateDir, opts.Hooks); err != nil {
		return CreateResult{}, err
	}
	cleanup = false
	return CreateResult{Schema: 1, HandoffPath: path}, nil
}

// MarshalCreateResult returns the canonical one-line JSON response for handoff creation.
func MarshalCreateResult(result CreateResult) ([]byte, error) {
	if result.Schema == 0 {
		result.Schema = 1
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
