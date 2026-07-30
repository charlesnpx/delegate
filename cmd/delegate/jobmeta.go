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
	jobMetadataSchema          = 1

	cleanupDispositionNoExecutionPossible = "no_execution_possible"
	cleanupDispositionVerifiedAbsent      = "verified_absent"
	cleanupDispositionUnresolved          = "unresolved"

	cleanupDispositionUnresolvedWarning = "Agentbus reported cleanupDisposition=unresolved; delegate retained local job artifacts because backend absence is unproven"
)

var (
	deleteJobInputOnTerminalState = handoff.DeleteJobInputOnTerminalState
	cleanupReviewWorkspace        = reviewpkg.CleanupWorkspace
)

type jobMetadata struct {
	Schema             int                        `json:"schema"`
	JobID              string                     `json:"job_id"`
	RequestID          string                     `json:"request_id,omitempty"`
	WorkspaceKey       string                     `json:"workspace_key,omitempty"`
	Kind               string                     `json:"kind"`
	Backend            string                     `json:"backend,omitempty"`
	CWD                string                     `json:"cwd,omitempty"`
	SessionID          string                     `json:"session_id,omitempty"`
	ContractKind       string                     `json:"contractKind"`
	NoContract         bool                       `json:"no_contract,omitempty"`
	JobInputPath       string                     `json:"job_input_path,omitempty"`
	ReviewWorkspace    string                     `json:"review_workspace,omitempty"`
	AgentbusStateRoot  string                     `json:"agentbus_state_root,omitempty"`
	SubmissionState    engine.JobState            `json:"submission_state,omitempty"`
	State              engine.JobState            `json:"state,omitempty"`
	CleanupDisposition string                     `json:"cleanupDisposition,omitempty"`
	Deduplicated       bool                       `json:"deduplicated,omitempty"`
	BackendError       string                     `json:"backend_error,omitempty"`
	Model              config.DimensionResolution `json:"model,omitempty"`
	Effort             config.DimensionResolution `json:"effort,omitempty"`
	Origin             *envelopeOrigin            `json:"origin,omitempty"`
	CreatedAt          time.Time                  `json:"created_at"`
	UpdatedAt          time.Time                  `json:"updated_at"`
}

func localCleanupSafe(disposition string) bool {
	return disposition == cleanupDispositionNoExecutionPossible || disposition == cleanupDispositionVerifiedAbsent
}

type localCleanupWarnings struct {
	writer io.Writer
	seen   map[string]struct{}
}

func newLocalCleanupWarnings(writer io.Writer) *localCleanupWarnings {
	if writer == nil {
		return nil
	}
	return &localCleanupWarnings{writer: writer, seen: map[string]struct{}{}}
}

func (warnings *localCleanupWarnings) warn(jobID, message string) error {
	if warnings == nil || warnings.writer == nil || message == "" {
		return nil
	}
	key := jobID + "\x00" + message
	if _, ok := warnings.seen[key]; ok {
		return nil
	}
	warnings.seen[key] = struct{}{}
	_, err := fmt.Fprintf(warnings.writer, "warning: %s\n", message)
	return err
}

func warnLocalArtifactsRetained(warnings *localCleanupWarnings, jobID string, state engine.JobState, cleanupDisposition string) error {
	message, ok := localArtifactsRetainedWarning(state, cleanupDisposition)
	if !ok {
		return nil
	}
	return warnings.warn(jobID, message)
}

func localArtifactsRetainedWarning(state engine.JobState, cleanupDisposition string) (string, bool) {
	if !engine.IsTerminal(state) {
		return "", false
	}
	if cleanupDisposition == cleanupDispositionUnresolved {
		return cleanupDispositionUnresolvedWarning, true
	}
	if cleanupDisposition == "" {
		return "Agentbus did not report cleanupDisposition for a terminal job; delegate retained local job artifacts because backend absence is unproven", true
	}
	if !localCleanupSafe(cleanupDisposition) {
		return fmt.Sprintf("Agentbus reported cleanupDisposition=%s; delegate retained local job artifacts because backend absence is unproven", cleanupDisposition), true
	}
	return "", false
}

func warnLocalCleanupFailure(warnings *localCleanupWarnings, jobID, artifact string, err error) error {
	if err == nil {
		return nil
	}
	return warnings.warn(jobID, fmt.Sprintf("Delegate could not remove local %s; local job artifacts were retained: %v", artifact, err))
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
		meta.Schema = jobMetadataSchema
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
	return atomicWriteMetadata(filepath.Join(dir, encodedStateFilename(meta.JobID)), raw, 0o600)
}

func loadJobMetadata(stateDir, jobID string) (jobMetadata, bool, error) {
	if err := validateDelegateJobID(jobID); err != nil {
		return jobMetadata{}, false, err
	}
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return jobMetadata{}, false, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, encodedStateFilename(jobID)))
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
	err = os.Remove(filepath.Join(dir, encodedStateFilename(jobID)))
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

func cleanupJobInput(stateDir, jobID, sessionID string, state engine.JobState, cleanupDisposition string, warnings *localCleanupWarnings) error {
	meta, found, err := loadJobMetadata(stateDir, jobID)
	if err != nil || !found {
		return err
	}
	changed := false
	if sessionID != "" && meta.SessionID != sessionID {
		meta.SessionID = sessionID
		changed = true
	}
	if meta.State != state {
		meta.State = state
		changed = true
	}
	if meta.CleanupDisposition != cleanupDisposition {
		meta.CleanupDisposition = cleanupDisposition
		changed = true
	}
	cleanupSafe := engine.IsTerminal(state) && localCleanupSafe(cleanupDisposition)
	retainedArtifacts := meta.JobInputPath != "" || meta.ReviewWorkspace != ""
	if retainedArtifacts && !cleanupSafe {
		if err := warnLocalArtifactsRetained(warnings, jobID, state, cleanupDisposition); err != nil {
			return err
		}
	}
	if meta.JobInputPath != "" && cleanupSafe {
		input := handoff.JobInput{JobID: jobID, Path: meta.JobInputPath}
		_, err = deleteJobInputOnTerminalState(input, state, cleanupDisposition, handoff.Hooks{})
		if err != nil {
			if warnErr := warnLocalCleanupFailure(warnings, jobID, "job input", err); warnErr != nil {
				return warnErr
			}
		} else {
			meta.JobInputPath = ""
			changed = true
		}
	}
	if meta.ReviewWorkspace != "" && cleanupSafe {
		if err := cleanupReviewWorkspace(stateDir, meta.ReviewWorkspace); err != nil {
			if warnErr := warnLocalCleanupFailure(warnings, jobID, "review workspace", err); warnErr != nil {
				return warnErr
			}
		} else {
			meta.ReviewWorkspace = ""
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := saveJobMetadata(stateDir, meta); err != nil {
		if warnErr := warnings.warn(jobID, fmt.Sprintf("Delegate could not persist local cleanup metadata; terminal outcome was preserved: %v", err)); warnErr != nil {
			return warnErr
		}
	}
	return nil
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
	if jobID == "" {
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
