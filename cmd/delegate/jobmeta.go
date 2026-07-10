package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
)

type jobMetadata struct {
	Schema       int       `json:"schema"`
	JobID        string    `json:"job_id"`
	Kind         string    `json:"kind"`
	ContractKind string    `json:"contractKind"`
	NoContract   bool      `json:"no_contract,omitempty"`
	JobInputPath string    `json:"job_input_path,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func saveJobMetadata(stateDir string, meta jobMetadata) error {
	if err := validateDelegateJobID(meta.JobID); err != nil {
		return err
	}
	if meta.Schema == 0 {
		meta.Schema = envelopeSchema
	}
	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWriteMetadata(filepath.Join(dir, meta.JobID+".json"), raw, 0o600)
}

func loadJobMetadata(stateDir, jobID string) (jobMetadata, bool, error) {
	if err := validateDelegateJobID(jobID); err != nil {
		return jobMetadata{}, false, err
	}
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return jobMetadata{}, false, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, jobID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return jobMetadata{}, false, nil
	}
	if err != nil {
		return jobMetadata{}, false, err
	}
	var meta jobMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return jobMetadata{}, false, err
	}
	if meta.JobID != jobID {
		return jobMetadata{}, false, fmt.Errorf("delegate job metadata %q has job_id %q", jobID, meta.JobID)
	}
	return meta, true, nil
}

func cleanupJobInput(stateDir, jobID, sessionID string, state engine.JobState) error {
	meta, found, err := loadJobMetadata(stateDir, jobID)
	if err != nil || !found || meta.JobInputPath == "" {
		return err
	}
	input := handoff.JobInput{JobID: jobID, Path: meta.JobInputPath}
	var deleted bool
	if sessionID != "" {
		deleted, err = handoff.DeleteJobInputOnSessionRecorded(input, handoff.Hooks{})
	} else if engine.IsTerminal(state) {
		deleted, err = handoff.DeleteJobInputOnTerminalState(input, state, handoff.Hooks{})
	}
	if err != nil || !deleted {
		return err
	}
	meta.JobInputPath = ""
	return saveJobMetadata(stateDir, meta)
}

func jobMetadataDir(stateDir string) (string, error) {
	dir, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	if err := handoff.EnsureStateDir(dir); err != nil {
		return "", err
	}
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(jobs, 0o700); err != nil {
		return "", err
	}
	return jobs, nil
}

func validateDelegateJobID(jobID string) error {
	if !strings.HasPrefix(jobID, "job_") || len(jobID) <= len("job_") || len(jobID) > 128 {
		return fmt.Errorf("invalid job id %q", jobID)
	}
	for _, r := range jobID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("invalid job id %q", jobID)
	}
	return nil
}

func atomicWriteMetadata(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}
