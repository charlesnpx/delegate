package handoff

import (
	"encoding/base64"
	"errors"
	"os"

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
	Prompt   string
	Hooks    Hooks
}

// JobInput identifies a private prompt file owned by one delegate job.
type JobInput struct {
	JobID string
	Path  string
}

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
	if _, err := file.WriteString(opts.Prompt); err != nil {
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
	cleanup = false
	return input, nil
}

// DeleteJobInputOnTerminalState removes the job-input file when terminal cleanup is proven safe.
func DeleteJobInputOnTerminalState(input JobInput, state engine.JobState, cleanupDisposition string, hooks Hooks) (bool, error) {
	if !engine.IsTerminal(state) || !localCleanupSafe(cleanupDisposition) {
		return false, nil
	}
	return removeFile(input.Path, hooks)
}

func localCleanupSafe(disposition string) bool {
	return disposition == "no_execution_possible" || disposition == "verified_absent"
}

func encodeJobID(jobID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(jobID))
}
