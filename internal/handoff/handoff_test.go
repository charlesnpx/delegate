package handoff

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureStateDirRejectsPreExistingSymlink(t *testing.T) {
	root := canonicalTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "delegate")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveStateDir(StateConfig{StateDir: linkDir}); err == nil {
		t.Fatal("ResolveStateDir() error = nil, want symlink rejection")
	}
	if err := EnsureStateDir(linkDir); err == nil {
		t.Fatal("EnsureStateDir() error = nil, want symlink rejection")
	}
}

func TestEnsureStateDirRejectsWrongModeWithoutChmod(t *testing.T) {
	stateDir := filepath.Join(canonicalTempDir(t), "delegate")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(stateDir); err == nil {
		t.Fatal("EnsureStateDir() error = nil, want mode rejection")
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("%s mode = %o, want %o", stateDir, got, 0o755)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	return dir
}
