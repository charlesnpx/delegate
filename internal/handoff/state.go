package handoff

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
		return cfg.StateDir, nil
	}
	env := cfg.Env
	if env == nil {
		env = os.Getenv
	}
	if xdg := env("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "delegate"), nil
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
	return filepath.Join(home, ".local", "state", "delegate"), nil
}

// EnsureStateDir creates the delegate state directory with private permissions.
func EnsureStateDir(dir string) error {
	if dir == "" {
		var err error
		dir, err = ResolveStateDir(StateConfig{})
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	return os.Chmod(dir, dirMode)
}

// Create writes stdin-style prompt content to a private handoff file.
func Create(opts CreateOptions) (CreateResult, error) {
	reader := opts.Reader
	if reader == nil {
		reader = os.Stdin
	}
	stateDir, err := ResolveStateDir(StateConfig{StateDir: opts.StateDir})
	if err != nil {
		return CreateResult{}, err
	}
	if err := EnsureStateDir(stateDir); err != nil {
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
