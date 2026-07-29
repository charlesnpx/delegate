package handoff

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/agentbus/engine"
)

const (
	jobInputPrefix = "job-input."
	jobInputSuffix = ".prompt"
)

// JobInputOptions controls creation of the durable backend input file.
type JobInputOptions struct {
	StateDir string
	JobID    string
	Prompt   ResolvedPrompt
	Hooks    Hooks
}

// JobInput identifies a private prompt file owned by one delegate job.
type JobInput struct {
	JobID string
	Path  string
}

// JobStateLookup returns the current state for a job id. found=false keeps the file.
type JobStateLookup func(jobID string) (state engine.JobState, found bool, err error)

// PersistJobInput writes the resolved prompt to a private, durable job-input file.
func PersistJobInput(opts JobInputOptions) (JobInput, error) {
	if opts.JobID == "" {
		return JobInput{}, errors.New("job id is required")
	}
	stateDir, err := prepareStateDir(opts.StateDir)
	if err != nil {
		return JobInput{}, err
	}
	pattern := jobInputPrefix + encodeJobID(opts.JobID) + ".*" + jobInputSuffix
	file, err := os.CreateTemp(stateDir, pattern)
	if err != nil {
		return JobInput{}, err
	}
	path := file.Name()
	input := JobInput{JobID: opts.JobID, Path: path}
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = closeFile()
			_, _ = removeFile(path, opts.Hooks)
		}
	}()
	if err := os.Chmod(path, fileMode); err != nil {
		return JobInput{}, err
	}
	if _, err := file.WriteString(opts.Prompt.Prompt); err != nil {
		return JobInput{}, err
	}
	if err := syncFile(file, path, opts.Hooks); err != nil {
		return JobInput{}, err
	}
	if err := closeFile(); err != nil {
		return JobInput{}, err
	}
	if err := syncDir(stateDir, opts.Hooks); err != nil {
		return JobInput{}, err
	}
	if opts.Prompt.Source == SourceHandoffPromptFile && opts.Prompt.HandoffPath != "" {
		handoffPath, err := validateHandoffPromptFile(opts.Prompt.HandoffPath, stateDir)
		if err != nil {
			return JobInput{}, err
		}
		if _, err := removeFile(handoffPath, opts.Hooks); err != nil {
			return JobInput{}, err
		}
	}
	cleanup = false
	return input, nil
}

// ReassociateJobInput renames a durable job-input file to carry the job id
// assigned by the execution service after submission.
func ReassociateJobInput(input JobInput, jobID string, hooks Hooks) (JobInput, error) {
	if jobID == "" {
		return input, errors.New("job id is required")
	}
	parsed, ok := ParseJobInputPath(input.Path)
	if !ok || parsed.JobID != input.JobID {
		return input, fmt.Errorf("invalid job-input path %q for job %q", input.Path, input.JobID)
	}
	if input.JobID == jobID {
		return input, nil
	}

	name := filepath.Base(input.Path)
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, jobInputPrefix), jobInputSuffix)
	dot := strings.IndexByte(trimmed, '.')
	if dot < 0 || dot == len(trimmed)-1 {
		return input, fmt.Errorf("invalid job-input path %q", input.Path)
	}
	target := filepath.Join(filepath.Dir(input.Path), jobInputPrefix+encodeJobID(jobID)+trimmed[dot:]+jobInputSuffix)
	if _, err := os.Lstat(target); err == nil {
		if _, sourceErr := os.Lstat(input.Path); errors.Is(sourceErr, os.ErrNotExist) {
			return JobInput{JobID: jobID, Path: target}, nil
		}
		return input, fmt.Errorf("job-input target already exists: %q", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return input, err
	}
	if err := os.Rename(input.Path, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, targetErr := os.Lstat(target); targetErr == nil {
				return JobInput{JobID: jobID, Path: target}, nil
			}
		}
		return input, err
	}
	reassociated := JobInput{JobID: jobID, Path: target}
	if err := syncDir(filepath.Dir(target), hooks); err != nil {
		return reassociated, err
	}
	return reassociated, nil
}

// DeleteJobInputOnSessionRecorded removes the job-input file once the backend session id is durable.
func DeleteJobInputOnSessionRecorded(input JobInput, hooks Hooks) (bool, error) {
	return removeFile(input.Path, hooks)
}

// DeleteJobInputOnPreLaunchTerminal removes the job-input file for terminal pre-launch failures.
func DeleteJobInputOnPreLaunchTerminal(input JobInput, state engine.JobState, hooks Hooks) (bool, error) {
	if !engine.IsTerminal(state) {
		return false, nil
	}
	return removeFile(input.Path, hooks)
}

// DeleteJobInputOnTerminalState removes the job-input file when state is terminal.
func DeleteJobInputOnTerminalState(input JobInput, state engine.JobState, hooks Hooks) (bool, error) {
	if !engine.IsTerminal(state) {
		return false, nil
	}
	return removeFile(input.Path, hooks)
}

// SweepTerminalJobInputs removes job-input files whose injected job state is terminal.
func SweepTerminalJobInputs(stateDir string, lookup JobStateLookup, hooks Hooks) ([]JobInput, error) {
	if lookup == nil {
		return nil, errors.New("job state lookup is required")
	}
	var err error
	stateDir, err = prepareStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []JobInput
	var joined error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		input, ok := ParseJobInputPath(filepath.Join(stateDir, entry.Name()))
		if !ok {
			continue
		}
		state, found, err := lookup(input.JobID)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", input.JobID, err))
			continue
		}
		if !found || !engine.IsTerminal(state) {
			continue
		}
		deleted, err := removeFile(input.Path, hooks)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", input.JobID, err))
			continue
		}
		if deleted {
			removed = append(removed, input)
		}
	}
	return removed, joined
}

// ParseJobInputPath recovers the job id from a job-input filename.
func ParseJobInputPath(path string) (JobInput, bool) {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, jobInputPrefix) || !strings.HasSuffix(name, jobInputSuffix) {
		return JobInput{}, false
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, jobInputPrefix), jobInputSuffix)
	parts := strings.Split(trimmed, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return JobInput{}, false
	}
	jobID, err := decodeJobID(parts[0])
	if err != nil || jobID == "" {
		return JobInput{}, false
	}
	return JobInput{JobID: jobID, Path: path}, true
}

func encodeJobID(jobID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(jobID))
}

func decodeJobID(encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
