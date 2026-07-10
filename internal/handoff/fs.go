package handoff

import (
	"errors"
	"os"
	"path/filepath"
)

// SyncTarget identifies the durable boundary that has just been synced.
type SyncTarget string

const (
	// SyncFile means a file's contents and metadata have been synced.
	SyncFile SyncTarget = "file"
	// SyncDir means a directory entry has been synced.
	SyncDir SyncTarget = "dir"
)

// Hooks provide ordering points for lifecycle tests and embedders.
type Hooks struct {
	AfterSync    func(path string, target SyncTarget)
	BeforeRemove func(path string)
	AfterRemove  func(path string)
}

func syncFile(file *os.File, path string, hooks Hooks) error {
	if err := file.Sync(); err != nil {
		return err
	}
	if hooks.AfterSync != nil {
		hooks.AfterSync(path, SyncFile)
	}
	return nil
}

func syncDir(dir string, hooks Hooks) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if hooks.AfterSync != nil {
		hooks.AfterSync(dir, SyncDir)
	}
	return nil
}

func removeFile(path string, hooks Hooks) (bool, error) {
	if path == "" {
		return false, nil
	}
	if hooks.BeforeRemove != nil {
		hooks.BeforeRemove(path)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if hooks.AfterRemove != nil {
		hooks.AfterRemove(path)
	}
	if err := syncDir(filepath.Dir(path), hooks); err != nil {
		return true, err
	}
	return true, nil
}
