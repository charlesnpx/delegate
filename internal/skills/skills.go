// Package skills installs delegate's one hand-authored managed skill.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	skillsource "github.com/charlesnpx/delegate/skills"
)

const (
	TargetClaude = "claude"
	TargetCodex  = "codex"
	TargetAll    = "all"

	skillName = "delegate"
)

// File is one installed or planned skill file.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

// Removal is one retired skill file removed or planned for removal.
type Removal struct {
	Path string `json:"path"`
}

// Result is the installed and removed skill files for one target.
type Result struct {
	Files   []File
	Removed []Removal
}

var retiredDelegateNames = []string{
	"delegate:setup",
	"delegate:status",
	"delegate:result",
	"delegate:cancel",
	"delegate:config",
	"delegate:rescue:claude",
	"delegate:rescue:codex",
	"delegate:rescue:cursor",
	"delegate:review:claude",
	"delegate:review:codex",
	"delegate:review:cursor",
	"delegate:adversarial-review:claude",
	"delegate:adversarial-review:codex",
	"delegate:adversarial-review:cursor",
}

var legacyNamesByTarget = map[string][]string{
	TargetClaude: {
		"codex:rescue",
		"codex:review",
		"codex:adversarial-review",
		"codex:status",
		"codex:result",
		"codex:cancel",
	},
	TargetCodex: {
		"claude:rescue",
		"claude:review",
		"claude:adversarial-review",
		"claude:status",
		"claude:result",
		"claude:cancel",
	},
}

// TargetRoot resolves the target skill root directory.
func TargetRoot(target, installRoot string, env func(string) string, homeDir func() (string, error)) (string, error) {
	if env == nil {
		env = os.Getenv
	}
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	if target != TargetClaude && target != TargetCodex {
		return "", fmt.Errorf("unsupported skill target %q", target)
	}
	if installRoot != "" {
		if !filepath.IsAbs(installRoot) {
			return "", errors.New("install root must be absolute")
		}
		root := filepath.Clean(installRoot)
		switch target {
		case TargetClaude:
			return filepath.Join(root, ".claude", "skills"), nil
		case TargetCodex:
			if codexHome := env("CODEX_HOME"); codexHome != "" {
				if !filepath.IsAbs(codexHome) {
					return "", errors.New("CODEX_HOME must be absolute")
				}
				codexHome = filepath.Clean(codexHome)
				if isPathInside(root, codexHome) {
					return filepath.Join(codexHome, "skills"), nil
				}
			}
			return filepath.Join(root, ".codex", "skills"), nil
		}
	}

	home, err := homeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("home directory is empty")
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("home directory must be absolute")
	}
	home = filepath.Clean(home)
	switch target {
	case TargetClaude:
		return filepath.Join(home, ".claude", "skills"), nil
	case TargetCodex:
		codexHome := env("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		if !filepath.IsAbs(codexHome) {
			return "", errors.New("CODEX_HOME must be absolute")
		}
		return filepath.Join(filepath.Clean(codexHome), "skills"), nil
	default:
		return "", fmt.Errorf("unsupported skill target %q", target)
	}
}

// Plan returns the files that would exist for target.
func Plan(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "plan")
}

// Install writes the static skill for target.
func Install(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "install")
}

// Uninstall removes the static skill for target.
func Uninstall(target, installRoot string, env func(string) string, homeDir func() (string, error)) (map[string]Result, error) {
	return apply(target, installRoot, env, homeDir, "uninstall")
}

func apply(target, installRoot string, env func(string) string, homeDir func() (string, error), op string) (map[string]Result, error) {
	targets, err := expandTargets(target)
	if err != nil {
		return nil, err
	}
	results := make(map[string]Result, len(targets))
	for _, targetName := range targets {
		root, err := TargetRoot(targetName, installRoot, env, homeDir)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(root, skillName, "SKILL.md")
		result := Result{Files: []File{{Path: path}}}
		switch op {
		case "plan":
		case "install":
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(skillsource.Delegate), 0o644); err != nil {
				return nil, err
			}
			result.Files[0].SHA256 = sha256Text(skillsource.Delegate)
		case "uninstall":
			if err := os.RemoveAll(filepath.Dir(path)); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported operation %q", op)
		}

		for _, name := range retiredNamesForTarget(targetName) {
			retiredPath := filepath.Join(root, name, "SKILL.md")
			result.Removed = append(result.Removed, Removal{Path: retiredPath})
			switch op {
			case "plan":
			case "install", "uninstall":
				if err := os.RemoveAll(filepath.Dir(retiredPath)); err != nil {
					return nil, err
				}
			}
		}
		results[targetName] = result
	}
	return results, nil
}

func retiredNamesForTarget(target string) []string {
	names := make([]string, 0, len(retiredDelegateNames)+len(legacyNamesByTarget[target]))
	names = append(names, retiredDelegateNames...)
	names = append(names, legacyNamesByTarget[target]...)
	return names
}

func expandTargets(target string) ([]string, error) {
	switch target {
	case TargetClaude, TargetCodex:
		return []string{target}, nil
	case TargetAll:
		return []string{TargetClaude, TargetCodex}, nil
	default:
		return nil, fmt.Errorf("target must be claude, codex, or all")
	}
}

func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func isPathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
