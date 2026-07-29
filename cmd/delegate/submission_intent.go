package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
)

const (
	submissionIntentSchema = 1

	submissionPhasePrepared     = "prepared"
	submissionPhaseInFlight     = "in_flight"
	submissionPhaseAcknowledged = "acknowledged"
	submissionPhaseBlocked      = "blocked"
	submissionPhaseRejected     = "rejected"
)

type submissionIntent struct {
	Schema            int                        `json:"schema"`
	RequestID         string                     `json:"request_id"`
	WorkspaceKey      string                     `json:"workspace_key"`
	AgentbusStateRoot string                     `json:"agentbus_state_root"`
	Params            client.JobSubmitParams     `json:"params"`
	Kind              string                     `json:"kind"`
	ContractKind      string                     `json:"contractKind"`
	Model             config.DimensionResolution `json:"model,omitempty"`
	Effort            config.DimensionResolution `json:"effort,omitempty"`
	Origin            *envelopeOrigin            `json:"origin,omitempty"`
	ReviewWorkspace   string                     `json:"review_workspace,omitempty"`
	Phase             string                     `json:"phase"`
	JobID             string                     `json:"job_id,omitempty"`
	Deduplicated      bool                       `json:"deduplicated,omitempty"`
	LastError         *agentbusError             `json:"last_error,omitempty"`
	CreatedAt         time.Time                  `json:"created_at"`
	UpdatedAt         time.Time                  `json:"updated_at"`
}

type agentbusError struct {
	Code             string `json:"code,omitempty"`
	Message          string `json:"message,omitempty"`
	AdmissionCause   string `json:"admission_cause,omitempty"`
	JobID            string `json:"job_id,omitempty"`
	Action           string `json:"action,omitempty"`
	RuntimeSupport   any    `json:"runtime_support,omitempty"`
	ExitCode         int    `json:"exit_code,omitempty"`
	Retryable        bool   `json:"retryable,omitempty"`
	PreserveIntent   bool   `json:"preserve_intent,omitempty"`
	Definitive       bool   `json:"definitive,omitempty"`
	OperatorGuidance bool   `json:"operator_guidance,omitempty"`
	Guidance         string `json:"guidance,omitempty"`
}

func agentbusErrorFromClassification(classification agentbusErrorClassification) *agentbusError {
	return &agentbusError{
		Code:             classification.Code,
		Message:          classification.Message,
		AdmissionCause:   classification.AdmissionCause,
		JobID:            classification.JobID,
		Action:           string(classification.Action),
		RuntimeSupport:   classification.RuntimeSupport,
		ExitCode:         classification.ExitCode,
		Retryable:        classification.Retryable,
		PreserveIntent:   classification.PreserveIntent,
		Definitive:       classification.Definitive,
		OperatorGuidance: classification.OperatorGuidance,
		Guidance:         classification.Guidance,
	}
}

var saveSubmissionIntent = saveSubmissionIntentFile

func saveSubmissionIntentFile(stateDir string, intent submissionIntent) error {
	if err := validateRequestID(intent.RequestID); err != nil {
		return err
	}
	if intent.Schema == 0 {
		intent.Schema = submissionIntentSchema
	}
	if err := validateSubmissionPhase(intent.Phase); err != nil {
		return err
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	dir, err := submissionIntentDir(stateDir)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWriteMetadata(filepath.Join(dir, encodedStateFilename(intent.RequestID)), raw, 0o600)
}

func loadSubmissionIntent(stateDir, requestID string) (submissionIntent, bool, error) {
	if err := validateRequestID(requestID); err != nil {
		return submissionIntent{}, false, err
	}
	dir, err := submissionIntentDir(stateDir)
	if err != nil {
		return submissionIntent{}, false, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, encodedStateFilename(requestID)))
	if errors.Is(err, os.ErrNotExist) {
		return submissionIntent{}, false, nil
	}
	if err != nil {
		return submissionIntent{}, false, err
	}
	var intent submissionIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return submissionIntent{}, false, err
	}
	if intent.RequestID != requestID {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q has request_id %q", requestID, intent.RequestID)
	}
	if err := validateSubmissionPhase(intent.Phase); err != nil {
		return submissionIntent{}, false, err
	}
	return intent, true, nil
}

func listSubmissionIntents(stateDir string) ([]submissionIntent, error) {
	dir, err := submissionIntentDir(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var intents []submissionIntent
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		requestID, ok := decodeStateFilename(entry.Name())
		if !ok {
			continue
		}
		intent, found, err := loadSubmissionIntent(stateDir, requestID)
		if err != nil {
			return nil, err
		}
		if found {
			intents = append(intents, intent)
		}
	}
	return intents, nil
}

func deleteSubmissionIntent(stateDir, requestID string) error {
	if err := validateRequestID(requestID); err != nil {
		return err
	}
	dir, err := submissionIntentDir(stateDir)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, encodedStateFilename(requestID)))
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

func transitionSubmissionIntent(stateDir string, intent *submissionIntent, phase string, mutate func(*submissionIntent)) error {
	if intent == nil {
		return errors.New("submission intent is nil")
	}
	if err := validateSubmissionPhase(phase); err != nil {
		return err
	}
	if mutate != nil {
		mutate(intent)
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	intent.Phase = phase
	return saveSubmissionIntent(stateDir, *intent)
}

func submissionIntentDir(stateDir string) (string, error) {
	dir, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	if err := handoff.EnsureStateDir(dir); err != nil {
		return "", err
	}
	submissions := filepath.Join(dir, "submissions")
	if err := os.MkdirAll(submissions, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(submissions, 0o700); err != nil {
		return "", err
	}
	return submissions, nil
}

func validateSubmissionPhase(phase string) error {
	switch phase {
	case submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseAcknowledged, submissionPhaseBlocked, submissionPhaseRejected:
		return nil
	default:
		return fmt.Errorf("invalid submission phase %q", phase)
	}
}

func encodedStateFilename(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id)) + ".json"
}

func decodeStateFilename(name string) (string, bool) {
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	encoded := strings.TrimSuffix(name, ".json")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(raw), true
}
