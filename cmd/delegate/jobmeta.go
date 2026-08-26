package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
	reviewpkg "github.com/charlesnpx/delegate/internal/review"
)

const reviewWorkspaceMetadataDirectoryName = "review-workspaces"

// reviewWorkspaceMetadata is the sole Delegate-owned record left for review.
// Agentbus owns the job outcome; this record only joins that durable outcome to
// the private workspace that contains sanitized repository content.
type reviewWorkspaceMetadata struct {
	JobID             string `json:"job_id"`
	Workspace         string `json:"workspace"`
	AgentbusStateRoot string `json:"agentbus_state_root"`
}

func encodedStateFilename(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value)) + ".json"
}

func saveReviewWorkspaceMetadata(stateDir string, meta reviewWorkspaceMetadata) error {
	if err := validateReviewWorkspaceMetadata(meta); err != nil {
		return err
	}
	dir, err := reviewWorkspaceMetadataDir(stateDir)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWriteReviewWorkspaceMetadata(filepath.Join(dir, encodedStateFilename(meta.JobID)), raw)
}

func loadReviewWorkspaceMetadata(stateDir string) ([]reviewWorkspaceMetadata, []error) {
	dir, err := reviewWorkspaceMetadataDir(stateDir)
	if err != nil {
		return nil, []error{err}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}
	metadata := make([]reviewWorkspaceMetadata, 0, len(entries))
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		var record reviewWorkspaceMetadata
		if err := json.Unmarshal(raw, &record); err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		if err := validateReviewWorkspaceMetadata(record); err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		metadata = append(metadata, record)
	}
	return metadata, errs
}

func deleteReviewWorkspaceMetadata(stateDir, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("invalid review workspace job id %q", jobID)
	}
	dir, err := reviewWorkspaceMetadataDir(stateDir)
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

// sweepReviewWorkspaces makes one status read per retained review job when a
// later review starts. It deliberately does not wait or poll: a non-terminal
// or unreadable job remains recorded for the next review invocation.
func sweepReviewWorkspaces(ctx context.Context, stateDir string) (errs []error) {
	metadata, loadErrs := loadReviewWorkspaceMetadata(stateDir)
	errs = append(errs, loadErrs...)
	clients := map[string]agentbusClient{}
	clientErrs := map[string]error{}
	defer func() {
		for root, c := range clients {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close Agentbus client for %s: %w", root, err))
			}
		}
	}()

	for _, record := range metadata {
		c, ok := clients[record.AgentbusStateRoot]
		if !ok {
			if connectErr, failed := clientErrs[record.AgentbusStateRoot]; failed {
				errs = append(errs, fmt.Errorf("inspect review workspace for job %s: %w", record.JobID, connectErr))
				continue
			}
			var err error
			c, _, err = connectAgentbusCommandAtRoot(ctx, nil, record.AgentbusStateRoot)
			if err != nil {
				clientErrs[record.AgentbusStateRoot] = err
				errs = append(errs, fmt.Errorf("inspect review workspace for job %s: %w", record.JobID, err))
				continue
			}
			clients[record.AgentbusStateRoot] = c
		}

		status, err := c.JobStatus(ctx, client.JobStatusParams{JobID: record.JobID})
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect review workspace for job %s: %w", record.JobID, err))
			continue
		}
		if len(status.Jobs) != 1 || status.Jobs[0].JobID != record.JobID {
			errs = append(errs, fmt.Errorf("inspect review workspace for job %s: Agentbus returned no matching job", record.JobID))
			continue
		}
		if !engine.IsTerminal(status.Jobs[0].State) {
			continue
		}
		if err := reviewpkg.CleanupWorkspace(stateDir, record.Workspace); err != nil {
			errs = append(errs, fmt.Errorf("remove review workspace for terminal job %s: %w", record.JobID, err))
			continue
		}
		if err := deleteReviewWorkspaceMetadata(stateDir, record.JobID); err != nil {
			errs = append(errs, fmt.Errorf("remove review workspace record for terminal job %s: %w", record.JobID, err))
		}
	}
	return errs
}

func validateReviewWorkspaceMetadata(meta reviewWorkspaceMetadata) error {
	if meta.JobID == "" {
		return fmt.Errorf("invalid review workspace job id %q", meta.JobID)
	}
	if meta.Workspace == "" || !filepath.IsAbs(meta.Workspace) {
		return fmt.Errorf("invalid review workspace %q", meta.Workspace)
	}
	if meta.AgentbusStateRoot == "" || !filepath.IsAbs(meta.AgentbusStateRoot) {
		return fmt.Errorf("invalid Agentbus state root %q", meta.AgentbusStateRoot)
	}
	return nil
}

func reviewWorkspaceMetadataDir(stateDir string) (string, error) {
	dir, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	if err := handoff.EnsureStateDir(dir); err != nil {
		return "", err
	}
	dir = filepath.Join(dir, reviewWorkspaceMetadataDirectoryName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func atomicWriteReviewWorkspaceMetadata(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
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
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}
