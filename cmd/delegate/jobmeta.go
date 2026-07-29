package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	reviewpkg "github.com/charlesnpx/delegate/internal/review"
)

const (
	backendDiagnosticMaxBytes  = 2 * 1024
	backendDiagnosticReadBytes = 64 * 1024
	backendDiagnosticTruncated = "\n[truncated]"
)

type jobMetadata struct {
	Schema            int                        `json:"schema"`
	JobID             string                     `json:"job_id"`
	Kind              string                     `json:"kind"`
	Backend           string                     `json:"backend,omitempty"`
	CWD               string                     `json:"cwd,omitempty"`
	SessionID         string                     `json:"session_id,omitempty"`
	ContractKind      string                     `json:"contractKind"`
	NoContract        bool                       `json:"no_contract,omitempty"`
	JobInputPath      string                     `json:"job_input_path,omitempty"`
	ReviewWorkspace   string                     `json:"review_workspace,omitempty"`
	AgentbusStateRoot string                     `json:"agentbus_state_root,omitempty"`
	Provisional       bool                       `json:"provisional,omitempty"`
	AdoptedJobID      string                     `json:"adopted_job_id,omitempty"`
	BackendError      string                     `json:"backend_error,omitempty"`
	Model             config.DimensionResolution `json:"model,omitempty"`
	Effort            config.DimensionResolution `json:"effort,omitempty"`
	Origin            *envelopeOrigin            `json:"origin,omitempty"`
	CreatedAt         time.Time                  `json:"created_at"`
	UpdatedAt         time.Time                  `json:"updated_at"`
}

func captureBackendError(stateDir string, job client.JobStatus) error {
	if !engine.IsTerminal(job.State) || job.LogPaths.Stderr == "" {
		return nil
	}
	file, err := os.Open(job.LogPaths.Stderr)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, backendDiagnosticReadBytes+1))
	if err != nil {
		return err
	}
	detail := string(raw)
	if detail == "" {
		return nil
	}
	meta, found, err := loadJobMetadata(stateDir, job.JobID)
	if err != nil || !found {
		return err
	}
	if meta.JobInputPath != "" {
		if prompt, readErr := os.ReadFile(meta.JobInputPath); readErr == nil && len(prompt) > 0 {
			detail = strings.ReplaceAll(detail, string(prompt), "[redacted: submitted prompt]")
		}
	}
	detail = strings.TrimSpace(reviewpkg.RedactSecretLikeDiagnostic(detail))
	if len(detail) > backendDiagnosticMaxBytes {
		keep := backendDiagnosticMaxBytes - len(backendDiagnosticTruncated)
		detail = strings.ToValidUTF8(detail[:keep], "") + backendDiagnosticTruncated
	}
	if detail == "" {
		return nil
	}
	meta.BackendError = detail
	return saveJobMetadata(stateDir, meta)
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

func deleteJobMetadata(stateDir, jobID string) error {
	if err := validateDelegateJobID(jobID); err != nil {
		return err
	}
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, jobID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

func cleanupJobInput(stateDir, jobID, sessionID string, state engine.JobState) error {
	meta, found, err := loadJobMetadata(stateDir, jobID)
	if err != nil || !found {
		return err
	}
	changed := false
	if sessionID != "" && meta.SessionID != sessionID {
		meta.SessionID = sessionID
		changed = true
	}
	if meta.JobInputPath != "" && (sessionID != "" || engine.IsTerminal(state)) {
		input := handoff.JobInput{JobID: jobID, Path: meta.JobInputPath}
		if sessionID != "" {
			_, err = handoff.DeleteJobInputOnSessionRecorded(input, handoff.Hooks{})
		} else {
			_, err = handoff.DeleteJobInputOnTerminalState(input, state, handoff.Hooks{})
		}
		if err != nil {
			return err
		}
		meta.JobInputPath = ""
		changed = true
	}
	if meta.ReviewWorkspace != "" && engine.IsTerminal(state) {
		if err := reviewpkg.CleanupWorkspace(stateDir, meta.ReviewWorkspace); err != nil {
			return err
		}
		meta.ReviewWorkspace = ""
		changed = true
	}
	if !changed {
		return nil
	}
	return saveJobMetadata(stateDir, meta)
}

func provisionalJobMetadataOlderThan(stateDir string, cutoff time.Time) ([]jobMetadata, error) {
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var provisional []jobMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		jobID := strings.TrimSuffix(entry.Name(), ".json")
		if err := validateDelegateJobID(jobID); err != nil {
			continue
		}
		meta, found, err := loadJobMetadata(stateDir, jobID)
		if err != nil {
			return nil, err
		}
		if !found || !meta.Provisional || meta.CreatedAt.IsZero() || meta.CreatedAt.After(cutoff) {
			continue
		}
		provisional = append(provisional, meta)
	}
	return provisional, nil
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
