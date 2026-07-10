package handoff

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	handoffPromptPrefix = "handoff-"
	handoffPromptSuffix = ".prompt"
)

func readHandoffPromptFile(path, stateDir string) ([]byte, string, error) {
	canonicalPath, err := validateHandoffPromptFile(path, stateDir)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(canonicalPath)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		_ = file.Close()
	}()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	pathInfo, err := os.Lstat(canonicalPath)
	if err != nil {
		return nil, "", err
	}
	if !os.SameFile(pathInfo, fileInfo) {
		return nil, "", fmt.Errorf("handoff prompt file %q changed during validation", canonicalPath)
	}
	if err := validateHandoffPromptFileInfo(canonicalPath, pathInfo); err != nil {
		return nil, "", err
	}

	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, "", err
	}
	return raw, canonicalPath, nil
}

func validateHandoffPromptFile(path, stateDir string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("handoff prompt file path is empty")
	}
	if !isHandoffPromptFilename(filepath.Base(path)) {
		return "", fmt.Errorf("handoff prompt file %q must match %s*%s", path, handoffPromptPrefix, handoffPromptSuffix)
	}

	resolvedStateDir, err := prepareStateDir(stateDir)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pathInfo, err := os.Lstat(absPath)
	if err != nil {
		return "", err
	}
	if err := validateHandoffPromptFileInfo(absPath, pathInfo); err != nil {
		return "", err
	}
	canonicalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}
	if !pathWithinDir(canonicalPath, resolvedStateDir) {
		return "", fmt.Errorf("handoff prompt file %q is outside state dir %q", canonicalPath, resolvedStateDir)
	}
	return canonicalPath, nil
}

func validateHandoffPromptFileInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("handoff prompt file %q must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("handoff prompt file %q must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != fileMode {
		return fmt.Errorf("handoff prompt file %q mode = %o, want %o", path, got, fileMode)
	}
	return nil
}

func isHandoffPromptFilename(name string) bool {
	return strings.HasPrefix(name, handoffPromptPrefix) &&
		strings.HasSuffix(name, handoffPromptSuffix) &&
		len(name) > len(handoffPromptPrefix)+len(handoffPromptSuffix)
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
