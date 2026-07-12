// Package config loads and persists delegate user-level configuration.
package config

// Blank import pins the vendored TOML dependency for the offline (codex) worker;
// real code in this package replaces this with a direct import.
import _ "github.com/BurntSushi/toml"
