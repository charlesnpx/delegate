package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configDirMode  = 0o700
	configFileMode = 0o600

	KeyOverridable  = "overridable"
	KeyClaudeModel  = "backend.claude.model"
	KeyClaudeEffort = "backend.claude.effort"
	KeyCodexModel   = "backend.codex.model"
	KeyCodexEffort  = "backend.codex.effort"
)

// Config is delegate's user-level model and effort preference configuration.
// Overridable defaults to true when the config file or its key is absent.
type Config struct {
	Overridable bool     `json:"overridable"`
	Backend     Backends `json:"backend"`

	overridableSet bool
}

// Backends groups configuration by agentbus backend name.
type Backends struct {
	Claude Defaults `json:"claude"`
	Codex  Defaults `json:"codex"`
}

// Defaults holds optional model and effort values for one backend.
type Defaults struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type diskConfig struct {
	Overridable *bool        `toml:"overridable"`
	Backend     diskBackends `toml:"backend"`
}

type diskBackends struct {
	Claude diskDefaults `toml:"claude"`
	Codex  diskDefaults `toml:"codex"`
}

type diskDefaults struct {
	Model  *string `toml:"model"`
	Effort *string `toml:"effort"`
}

// Path returns the absolute user-level delegate config path.
func Path() (string, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve delegate config home: %w", err)
		}
		if home == "" {
			return "", errors.New("resolve delegate config home: home directory is empty")
		}
		root = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("delegate config root %q must be absolute", root)
	}
	return filepath.Join(filepath.Clean(root), "delegate", "config.toml"), nil
}

// Load reads the user-level configuration. A missing config is valid and returns
// default semantics: overridable=true and no backend defaults.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Overridable: true}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read delegate config %q: %w", path, err)
	}
	var disk diskConfig
	meta, err := toml.Decode(string(raw), &disk)
	if err != nil {
		return Config{}, fmt.Errorf("decode delegate config %q: %w", path, err)
	}
	if unknown := meta.Undecoded(); len(unknown) > 0 {
		keys := make([]string, 0, len(unknown))
		for _, key := range unknown {
			keys = append(keys, key.String())
		}
		return Config{}, fmt.Errorf("decode delegate config %q: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	return fromDisk(disk), nil
}

// Save atomically persists cfg beneath the user config directory with private
// permissions. The temporary file is created beside the destination so rename
// remains atomic.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// Race-free creation tracking: chmod below applies only when THIS call's
	// os.Mkdir created the leaf directory, never when it already existed or
	// appeared concurrently.
	dirCreated := false
	switch err := os.Mkdir(dir, configDirMode); {
	case err == nil:
		dirCreated = true
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dir), configDirMode); err != nil {
			return fmt.Errorf("create delegate config parent directory %q: %w", filepath.Dir(dir), err)
		}
		switch err := os.Mkdir(dir, configDirMode); {
		case err == nil:
			dirCreated = true
		case errors.Is(err, os.ErrExist):
		default:
			return fmt.Errorf("create delegate config directory %q: %w", dir, err)
		}
	case errors.Is(err, os.ErrExist):
	default:
		return fmt.Errorf("create delegate config directory %q: %w", dir, err)
	}
	// Symlinked config directories are deliberate; their pre-existing permissions
	// are the user's responsibility.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve delegate config directory %q: %w", dir, err)
	}
	info, err := os.Stat(resolvedDir)
	if err != nil {
		return fmt.Errorf("stat delegate config directory %q: %w", resolvedDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("delegate config directory %q is not a directory", resolvedDir)
	}
	if dirCreated {
		if err := os.Chmod(resolvedDir, configDirMode); err != nil {
			return fmt.Errorf("set delegate config directory mode %q: %w", resolvedDir, err)
		}
	}
	path = filepath.Join(resolvedDir, filepath.Base(path))

	var encoded strings.Builder
	if err := toml.NewEncoder(&encoded).Encode(toDisk(cfg)); err != nil {
		return fmt.Errorf("encode delegate config %q: %w", path, err)
	}
	tmp, err := os.CreateTemp(resolvedDir, ".config.toml-*")
	if err != nil {
		return fmt.Errorf("create delegate config temporary file in %q: %w", resolvedDir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(configFileMode); err != nil {
		return fmt.Errorf("set delegate config temporary file mode %q: %w", tmpPath, err)
	}
	if _, err := tmp.WriteString(encoded.String()); err != nil {
		return fmt.Errorf("write delegate config temporary file %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync delegate config temporary file %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close delegate config temporary file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace delegate config %q: %w", path, err)
	}
	dirFile, err := os.Open(resolvedDir)
	if err != nil {
		return fmt.Errorf("open delegate config directory %q for sync: %w", resolvedDir, err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("sync delegate config directory %q: %w", resolvedDir, err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("close delegate config directory %q: %w", resolvedDir, err)
	}
	cleanup = false
	return nil
}

// Get returns a value for one supported configuration key.
func (c Config) Get(key string) (string, error) {
	switch key {
	case KeyOverridable:
		return fmt.Sprintf("%t", c.Overridable), nil
	case KeyClaudeModel:
		return c.Backend.Claude.Model, nil
	case KeyClaudeEffort:
		return c.Backend.Claude.Effort, nil
	case KeyCodexModel:
		return c.Backend.Codex.Model, nil
	case KeyCodexEffort:
		return c.Backend.Codex.Effort, nil
	default:
		return "", unsupportedKeyError(key)
	}
}

// Set validates and assigns one supported configuration key.
func (c *Config) Set(key, value string) error {
	switch key {
	case KeyOverridable:
		switch value {
		case "true":
			c.Overridable = true
		case "false":
			c.Overridable = false
		default:
			return fmt.Errorf("%s must be true or false", KeyOverridable)
		}
		c.overridableSet = true
	case KeyClaudeModel:
		c.Backend.Claude.Model = value
	case KeyClaudeEffort:
		c.Backend.Claude.Effort = value
	case KeyCodexModel:
		c.Backend.Codex.Model = value
	case KeyCodexEffort:
		c.Backend.Codex.Effort = value
	default:
		return unsupportedKeyError(key)
	}
	return nil
}

// Unset removes one supported key. Unsetting overridable restores its default
// true behavior rather than storing a value.
func (c *Config) Unset(key string) error {
	switch key {
	case KeyOverridable:
		c.Overridable = true
		c.overridableSet = false
	case KeyClaudeModel:
		c.Backend.Claude.Model = ""
	case KeyClaudeEffort:
		c.Backend.Claude.Effort = ""
	case KeyCodexModel:
		c.Backend.Codex.Model = ""
	case KeyCodexEffort:
		c.Backend.Codex.Effort = ""
	default:
		return unsupportedKeyError(key)
	}
	return nil
}

// DefaultsFor returns the configured defaults for backend, or empty defaults
// for unrecognised backends.
func (c Config) DefaultsFor(backend string) Defaults {
	switch backend {
	case "claude":
		return c.Backend.Claude
	case "codex":
		return c.Backend.Codex
	default:
		return Defaults{}
	}
}

func unsupportedKeyError(key string) error {
	return fmt.Errorf("unsupported config key %q; allowed keys: %s, %s, %s, %s, %s", key, KeyOverridable, KeyClaudeModel, KeyClaudeEffort, KeyCodexModel, KeyCodexEffort)
}

func fromDisk(disk diskConfig) Config {
	cfg := Config{Overridable: true}
	if disk.Overridable != nil {
		cfg.Overridable = *disk.Overridable
		cfg.overridableSet = true
	}
	if disk.Backend.Claude.Model != nil {
		cfg.Backend.Claude.Model = *disk.Backend.Claude.Model
	}
	if disk.Backend.Claude.Effort != nil {
		cfg.Backend.Claude.Effort = *disk.Backend.Claude.Effort
	}
	if disk.Backend.Codex.Model != nil {
		cfg.Backend.Codex.Model = *disk.Backend.Codex.Model
	}
	if disk.Backend.Codex.Effort != nil {
		cfg.Backend.Codex.Effort = *disk.Backend.Codex.Effort
	}
	return cfg
}

func toDisk(cfg Config) diskConfig {
	disk := diskConfig{
		Backend: diskBackends{
			Claude: diskDefaults{Model: stringPointerIfSet(cfg.Backend.Claude.Model), Effort: stringPointerIfSet(cfg.Backend.Claude.Effort)},
			Codex:  diskDefaults{Model: stringPointerIfSet(cfg.Backend.Codex.Model), Effort: stringPointerIfSet(cfg.Backend.Codex.Effort)},
		},
	}
	if cfg.overridableSet || !cfg.Overridable {
		disk.Overridable = boolPointer(cfg.Overridable)
	}
	return disk
}

func stringPointerIfSet(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func boolPointer(value bool) *bool { return &value }
