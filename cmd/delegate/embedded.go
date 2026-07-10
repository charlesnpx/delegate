package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/handoff"
)

var embeddedBackend = func(name string) (engine.Backend, bool) {
	return nil, false
}

var nowUTC = func() time.Time {
	return time.Now().UTC()
}

var newJobID = randomJobID

func runEmbeddedTask(ctx context.Context, opts taskOptions, resolved handoff.ResolvedPrompt, turnPolicy *engine.TurnPolicy) (taskRunResult, error) {
	backend, ok := embeddedBackend(opts.Backend)
	if !ok {
		return taskRunResult{}, fmt.Errorf("embedded backend %q unavailable; use daemon mode", opts.Backend)
	}
	store, err := engine.NewStore(engine.StoreConfig{CWD: opts.CWD})
	if err != nil {
		return taskRunResult{}, err
	}
	jobID, err := newJobID()
	if err != nil {
		return taskRunResult{}, err
	}
	if _, err := persistDelegateJob(opts, resolved, jobID, contractKindForPolicy(turnPolicy, opts.NoContract)); err != nil {
		return taskRunResult{}, err
	}
	now := nowUTC()
	record := &engine.JobRecord{
		JobID:      jobID,
		Backend:    opts.Backend,
		Foreground: true,
		State:      engine.StateQueued,
		Tags:       taskTags(opts),
		CreatedAt:  now,
		UpdatedAt:  now,
		Policy:     turnPolicy,
	}
	if err := store.Save(record); err != nil {
		_ = cleanupJobInput(opts.StateDir, jobID, "", engine.StateFailed)
		return taskRunResult{}, err
	}
	if err := transitionEmbeddedJob(store, jobID, engine.StateStarting, nil); err != nil {
		return taskRunResult{}, err
	}
	session, err := startEmbeddedSession(ctx, backend, opts)
	if err != nil {
		_ = transitionEmbeddedJob(store, jobID, engine.StateFailed, nil)
		_ = cleanupJobInput(opts.StateDir, jobID, "", engine.StateFailed)
		return taskRunResult{}, err
	}
	logPaths := engine.LogPaths{
		Stdout: filepath.Join(store.Layout().Logs, jobID+".stdout.log"),
		Stderr: filepath.Join(store.Layout().Logs, jobID+".stderr.log"),
	}
	if err := transitionEmbeddedJob(store, jobID, engine.StateRunning, func(record *engine.JobRecord) {
		record.SessionID = session.ID()
		record.BackendSessionID = session.ID()
		record.LogPaths = logPaths
	}); err != nil {
		return taskRunResult{}, err
	}
	if err := cleanupJobInput(opts.StateDir, jobID, session.ID(), engine.StateRunning); err != nil {
		return taskRunResult{}, err
	}
	raw, stamp, resolvedContract, state, err := runEmbeddedTurnPipeline(ctx, store, session, jobID, resolved.Prompt, opts, turnPolicy, logPaths)
	if err != nil {
		return taskRunResult{}, err
	}
	resultInfo, err := store.WriteResult(jobID, raw, 0)
	if err != nil {
		return taskRunResult{}, err
	}
	if err := transitionEmbeddedJob(store, jobID, state, func(record *engine.JobRecord) {
		record.Result = &resultInfo
		record.Contract = stamp
		record.ResolvedContract = resolvedContract
	}); err != nil {
		return taskRunResult{}, err
	}
	if err := cleanupJobInput(opts.StateDir, jobID, "", state); err != nil {
		return taskRunResult{}, err
	}
	jobResult := client.JobResult{
		JobID:     jobID,
		SessionID: session.ID(),
		State:     state,
		Result:    &resultInfo,
		Contract:  stamp,
	}
	env, err := terminalEnvelopeFromJobResult(opts.StateDir, jobResult)
	if err != nil {
		return taskRunResult{}, err
	}
	return taskRunResult{Terminal: &env}, nil
}

func startEmbeddedSession(ctx context.Context, backend engine.Backend, opts taskOptions) (engine.Session, error) {
	sessionOpts := engine.SessionOpts{
		CWD:     opts.CWD,
		Write:   opts.Write,
		Model:   opts.Model,
		Effort:  opts.Effort,
		Timeout: opts.Timeout,
	}
	if opts.ResumeSession != "" {
		return backend.Resume(ctx, opts.ResumeSession, sessionOpts)
	}
	return backend.Start(ctx, sessionOpts)
}

func runEmbeddedTurnPipeline(ctx context.Context, store *engine.Store, session engine.Session, jobID, prompt string, opts taskOptions, turnPolicy *engine.TurnPolicy, logPaths engine.LogPaths) ([]byte, *engine.ContractStamp, *engine.ContractSpec, engine.JobState, error) {
	raw, final, err := collectEmbeddedTurn(ctx, store, session, jobID, effectivePrompt(prompt, turnPolicy), opts.Write, opts.Timeout, logPaths)
	if err != nil {
		stamp, resolvedContract := skippedStampForPolicy(turnPolicy, engine.SkipBackendError)
		return []byte(err.Error()), stamp, resolvedContract, engine.StateFailed, nil
	}
	if !final {
		stamp, resolvedContract := skippedStampForPolicy(turnPolicy, engine.SkipNoFinalMessage)
		return []byte{}, stamp, resolvedContract, engine.StateFailed, nil
	}
	if _, err := store.WriteResult(jobID, raw, 0); err != nil {
		return nil, nil, nil, "", err
	}
	stamp, resolvedContract, err := validateEmbeddedResult(raw, turnPolicy, 1, false)
	if err != nil {
		return nil, nil, nil, "", err
	}
	if stamp != nil && stamp.Status == engine.ContractNoncompliant && turnPolicy != nil && turnPolicy.Retry != nil && turnPolicy.Retry.Max == 1 {
		if err := transitionEmbeddedJob(store, jobID, engine.StateRetrying, nil); err != nil {
			return nil, nil, nil, "", err
		}
		retryPrompt := engine.RenderRetryTemplate(turnPolicy.Retry.Template, stamp.Missing)
		raw, final, err = collectEmbeddedTurn(ctx, store, session, jobID, retryPrompt, false, opts.Timeout, logPaths)
		if err != nil {
			stamp, resolvedContract = skippedStampForPolicy(turnPolicy, engine.SkipBackendError)
			return []byte(err.Error()), stamp, resolvedContract, engine.StateFailed, nil
		}
		if !final {
			stamp, resolvedContract = skippedStampForPolicy(turnPolicy, engine.SkipNoFinalMessage)
			return []byte{}, stamp, resolvedContract, engine.StateFailed, nil
		}
		if _, err := store.WriteResult(jobID, raw, 0); err != nil {
			return nil, nil, nil, "", err
		}
		stamp, resolvedContract, err = validateEmbeddedResult(raw, turnPolicy, 2, true)
		if err != nil {
			return nil, nil, nil, "", err
		}
	}
	if stamp != nil && stamp.Status == engine.ContractNoncompliant {
		return raw, stamp, resolvedContract, engine.StateCompletedNoncompliant, nil
	}
	return raw, stamp, resolvedContract, engine.StateCompleted, nil
}

func collectEmbeddedTurn(ctx context.Context, store *engine.Store, session engine.Session, jobID, prompt string, write bool, timeout time.Duration, logPaths engine.LogPaths) ([]byte, bool, error) {
	events, err := session.Turn(ctx, engine.TurnInput{
		Prompt:   prompt,
		Write:    write,
		Timeout:  timeout,
		LogPaths: logPaths,
		OnProcessStart: func(ref engine.ProcessRef, childPID int) {
			_, _ = store.Update(jobID, func(record *engine.JobRecord) (bool, error) {
				record.Worker = ref
				record.BackendChildPID = childPID
				return true, nil
			})
		},
	})
	if err != nil {
		return nil, false, err
	}
	var final []byte
	for event := range events {
		switch event.Type {
		case engine.EventResultMessage:
			text := event.RawText
			if text == "" {
				text = event.Text
			}
			final = []byte(text)
		case engine.EventTerminalError:
			if len(final) == 0 {
				text := event.RawText
				if text == "" {
					text = event.Text
				}
				if text == "" {
					text = "backend terminal error"
				}
				return []byte(text), false, fmt.Errorf("%s", text)
			}
		}
	}
	return final, final != nil, nil
}

func validateEmbeddedResult(raw []byte, turnPolicy *engine.TurnPolicy, attempts int, retryUsed bool) (*engine.ContractStamp, *engine.ContractSpec, error) {
	if turnPolicy == nil || turnPolicy.Contract == nil {
		return nil, nil, nil
	}
	registry := engine.NewPolicyRegistry()
	resolved, name, _, err := engine.ResolveContract(*turnPolicy.Contract, registry)
	if err != nil {
		return nil, nil, err
	}
	result, err := engine.ValidateContract(string(raw), resolved)
	if err != nil {
		return nil, nil, err
	}
	stamp := engine.StampValidation(attempts, retryUsed, name, result, nowUTC())
	return &stamp, &resolved, nil
}

func skippedStampForPolicy(turnPolicy *engine.TurnPolicy, reason engine.SkippedReason) (*engine.ContractStamp, *engine.ContractSpec) {
	if turnPolicy == nil || turnPolicy.Contract == nil {
		return nil, nil
	}
	registry := engine.NewPolicyRegistry()
	resolved, name, hash, err := engine.ResolveContract(*turnPolicy.Contract, registry)
	if err != nil {
		return nil, nil
	}
	stamp := engine.SkippedContractStamp(reason, 0, false, name, hash)
	return &stamp, &resolved
}

func effectivePrompt(prompt string, turnPolicy *engine.TurnPolicy) string {
	if turnPolicy == nil || strings.TrimSpace(turnPolicy.Prologue) == "" {
		return prompt
	}
	return strings.TrimRight(turnPolicy.Prologue, "\n") + "\n\n" + prompt
}

func transitionEmbeddedJob(store *engine.Store, jobID string, state engine.JobState, mutate func(*engine.JobRecord)) error {
	_, err := store.Update(jobID, func(record *engine.JobRecord) (bool, error) {
		if mutate != nil {
			mutate(record)
		}
		if record.State == state {
			return true, nil
		}
		if err := record.Transition(state, nowUTC()); err != nil {
			return false, err
		}
		return true, nil
	})
	return err
}

func randomJobID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "job_" + hex.EncodeToString(b[:]), nil
}
