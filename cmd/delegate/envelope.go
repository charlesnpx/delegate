package main

import (
	"encoding/json"
	"io"
	"time"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
)

const (
	envelopeSchema        = 2
	commandJSONSchema     = 1
	taskKind              = "task"
	reviewKind            = "review"
	adversarialReviewKind = "adversarial_review"
)

// LaunchEnvelope is the schema returned when delegate has launched a job.
type LaunchEnvelope struct {
	Schema         int                        `json:"schema"`
	RequestID      string                     `json:"request_id,omitempty"`
	JobID          string                     `json:"job_id"`
	Status         string                     `json:"status"`
	Deduplicated   bool                       `json:"deduplicated"`
	Model          config.DimensionResolution `json:"model"`
	Effort         config.DimensionResolution `json:"effort"`
	BackendProfile config.DimensionResolution `json:"backend_profile"`
	Timeout        config.DimensionResolution `json:"timeout"`
	ResultSHA256   *string                    `json:"result_sha256"`
}

// TerminalEnvelope is the schema returned by delegate result and task --wait.
type TerminalEnvelope struct {
	Schema                         int                        `json:"schema"`
	RequestID                      string                     `json:"request_id,omitempty"`
	JobID                          string                     `json:"job_id"`
	Status                         engine.JobState            `json:"status"`
	Deduplicated                   bool                       `json:"deduplicated"`
	CleanupDisposition             string                     `json:"cleanup_disposition,omitempty"`
	LateFinalization               bool                       `json:"late_finalization,omitempty"`
	AgentbusWarnings               []string                   `json:"agentbus_warnings,omitempty"`
	LocalArtifactsRetained         bool                       `json:"local_artifacts_retained,omitempty"`
	Kind                           string                     `json:"kind"`
	Contract                       *engine.ContractStamp      `json:"contract,omitempty"`
	ResultSHA256                   *string                    `json:"result_sha256"`
	ResultPath                     string                     `json:"result_path,omitempty"`
	ResultUnavailableReason        string                     `json:"result_unavailable_reason,omitempty"`
	FailureReason                  string                     `json:"failure_reason,omitempty"`
	FailureClass                   engine.FailureClass        `json:"failure_class,omitempty"`
	BackendError                   string                     `json:"backend_error,omitempty"`
	Model                          config.DimensionResolution `json:"model"`
	Effort                         config.DimensionResolution `json:"effort"`
	BackendProfile                 config.DimensionResolution `json:"backend_profile"`
	Timeout                        config.DimensionResolution `json:"timeout"`
	ModelReported                  string                     `json:"model_reported,omitempty"`
	ModelReportedUnavailableReason string                     `json:"model_reported_unavailable_reason,omitempty"`
	SubmittedAt                    *time.Time                 `json:"submitted_at,omitempty"`
	UpdatedAt                      *time.Time                 `json:"updated_at,omitempty"`
	StartedAt                      *time.Time                 `json:"started_at,omitempty"`
	HeartbeatAt                    *time.Time                 `json:"heartbeat_at,omitempty"`
	FinalAttemptStartedAt          *time.Time                 `json:"final_attempt_started_at,omitempty"`
	FinalAttemptEndedAt            *time.Time                 `json:"final_attempt_ended_at,omitempty"`
}

type terminalEnvelopeOptions struct {
	ModelEffort            config.ModelEffortResolution
	BackendProfile         config.DimensionResolution
	Timeout                config.DimensionResolution
	ModelReported          string
	ModelsReportedCapable  bool
	RequestID              string
	Deduplicated           bool
	DeduplicatedSet        bool
	CleanupDisposition     string
	LateFinalization       bool
	AgentbusWarnings       []string
	LocalArtifactsRetained bool
	ResultPath             string
	SubmittedAt            *time.Time
	UpdatedAt              *time.Time
	StartedAt              *time.Time
	HeartbeatAt            *time.Time
	FinalAttemptStartedAt  *time.Time
	FinalAttemptEndedAt    *time.Time
	FailureReason          string
	FailureClass           engine.FailureClass
}

type launchEnvelopeOptions struct {
	ModelEffort    config.ModelEffortResolution
	BackendProfile config.DimensionResolution
	Timeout        config.DimensionResolution
	RequestID      string
	Deduplicated   bool
}

func (e TerminalEnvelope) MarshalJSON() ([]byte, error) {
	finalAttemptStartedAt, finalAttemptEndedAt := completeFinalAttemptTiming(e.FinalAttemptStartedAt, e.FinalAttemptEndedAt)
	type terminalEnvelopeJSON struct {
		Schema                         int                        `json:"schema"`
		RequestID                      string                     `json:"request_id,omitempty"`
		JobID                          string                     `json:"job_id"`
		Status                         engine.JobState            `json:"status"`
		Deduplicated                   bool                       `json:"deduplicated"`
		CleanupDisposition             string                     `json:"cleanup_disposition,omitempty"`
		LateFinalization               bool                       `json:"late_finalization,omitempty"`
		AgentbusWarnings               []string                   `json:"agentbus_warnings,omitempty"`
		LocalArtifactsRetained         bool                       `json:"local_artifacts_retained,omitempty"`
		Kind                           string                     `json:"kind"`
		Contract                       *engine.ContractStamp      `json:"contract,omitempty"`
		ResultSHA256                   *string                    `json:"result_sha256"`
		ResultPath                     string                     `json:"result_path,omitempty"`
		ResultUnavailableReason        string                     `json:"result_unavailable_reason,omitempty"`
		FailureReason                  string                     `json:"failure_reason,omitempty"`
		FailureClass                   engine.FailureClass        `json:"failure_class,omitempty"`
		BackendError                   string                     `json:"backend_error,omitempty"`
		Model                          config.DimensionResolution `json:"model"`
		Effort                         config.DimensionResolution `json:"effort"`
		BackendProfile                 config.DimensionResolution `json:"backend_profile"`
		Timeout                        config.DimensionResolution `json:"timeout"`
		ModelReported                  string                     `json:"model_reported,omitempty"`
		ModelReportedUnavailableReason string                     `json:"model_reported_unavailable_reason,omitempty"`
		SubmittedAt                    *time.Time                 `json:"submitted_at,omitempty"`
		UpdatedAt                      *time.Time                 `json:"updated_at,omitempty"`
		StartedAt                      *time.Time                 `json:"started_at,omitempty"`
		HeartbeatAt                    *time.Time                 `json:"heartbeat_at,omitempty"`
		FinalAttemptStartedAt          *time.Time                 `json:"final_attempt_started_at,omitempty"`
		FinalAttemptEndedAt            *time.Time                 `json:"final_attempt_ended_at,omitempty"`
	}
	return json.Marshal(terminalEnvelopeJSON{
		Schema:                         e.Schema,
		RequestID:                      e.RequestID,
		JobID:                          e.JobID,
		Status:                         e.Status,
		Deduplicated:                   e.Deduplicated,
		CleanupDisposition:             e.CleanupDisposition,
		LateFinalization:               e.LateFinalization,
		AgentbusWarnings:               e.AgentbusWarnings,
		LocalArtifactsRetained:         e.LocalArtifactsRetained,
		Kind:                           e.Kind,
		Contract:                       e.Contract,
		ResultSHA256:                   e.ResultSHA256,
		ResultPath:                     e.ResultPath,
		ResultUnavailableReason:        e.ResultUnavailableReason,
		FailureReason:                  e.FailureReason,
		FailureClass:                   e.FailureClass,
		BackendError:                   e.BackendError,
		Model:                          e.Model,
		Effort:                         e.Effort,
		BackendProfile:                 e.BackendProfile,
		Timeout:                        e.Timeout,
		ModelReported:                  e.ModelReported,
		ModelReportedUnavailableReason: e.ModelReportedUnavailableReason,
		SubmittedAt:                    e.SubmittedAt,
		UpdatedAt:                      e.UpdatedAt,
		StartedAt:                      e.StartedAt,
		HeartbeatAt:                    e.HeartbeatAt,
		FinalAttemptStartedAt:          finalAttemptStartedAt,
		FinalAttemptEndedAt:            finalAttemptEndedAt,
	})
}

func newLaunchEnvelope(jobID string, state engine.JobState, resolutions ...config.ModelEffortResolution) (LaunchEnvelope, error) {
	return newLaunchEnvelopeWithOptions(jobID, state, launchEnvelopeOptions{
		ModelEffort: normalizedModelEffort(resolutions...),
	})
}

func newLaunchEnvelopeWithOptions(jobID string, state engine.JobState, option launchEnvelopeOptions) (LaunchEnvelope, error) {
	modelEffort := normalizedModelEffort(option.ModelEffort)
	env := LaunchEnvelope{
		Schema:         envelopeSchema,
		RequestID:      option.RequestID,
		JobID:          jobID,
		Status:         launchStatus(state),
		Deduplicated:   option.Deduplicated,
		Model:          modelEffort.Model,
		Effort:         modelEffort.Effort,
		BackendProfile: normalizedBackendProfile(option.BackendProfile),
		Timeout:        normalizedTimeout(option.Timeout),
	}
	return env, nil
}

func newTerminalEnvelope(jobID string, state engine.JobState, kind string, stamp *engine.ContractStamp, resultSHA256, backendError string, options ...terminalEnvelopeOptions) (TerminalEnvelope, error) {
	option := terminalEnvelopeOptions{}
	if len(options) > 0 {
		option = options[0]
	}
	modelEffort := normalizedModelEffort(option.ModelEffort)
	finalAttemptStartedAt, finalAttemptEndedAt := completeFinalAttemptTiming(option.FinalAttemptStartedAt, option.FinalAttemptEndedAt)
	env := TerminalEnvelope{
		Schema:                         envelopeSchema,
		RequestID:                      option.RequestID,
		JobID:                          jobID,
		Status:                         state,
		Deduplicated:                   option.Deduplicated,
		CleanupDisposition:             option.CleanupDisposition,
		LateFinalization:               option.LateFinalization,
		AgentbusWarnings:               append([]string(nil), option.AgentbusWarnings...),
		LocalArtifactsRetained:         option.LocalArtifactsRetained,
		Kind:                           kind,
		Contract:                       stamp,
		FailureReason:                  option.FailureReason,
		FailureClass:                   option.FailureClass,
		BackendError:                   backendError,
		Model:                          modelEffort.Model,
		Effort:                         modelEffort.Effort,
		BackendProfile:                 normalizedBackendProfile(option.BackendProfile),
		Timeout:                        normalizedTimeout(option.Timeout),
		ModelReported:                  option.ModelReported,
		ModelReportedUnavailableReason: modelReportedUnavailableReason(option.ModelsReportedCapable, option.ModelReported),
		SubmittedAt:                    option.SubmittedAt,
		UpdatedAt:                      option.UpdatedAt,
		StartedAt:                      option.StartedAt,
		HeartbeatAt:                    option.HeartbeatAt,
		FinalAttemptStartedAt:          finalAttemptStartedAt,
		FinalAttemptEndedAt:            finalAttemptEndedAt,
		ResultPath:                     option.ResultPath,
	}
	if resultSHA256 != "" {
		env.ResultSHA256 = &resultSHA256
	} else {
		env.ResultUnavailableReason = resultUnavailableReason(state)
	}
	return env, nil
}

// completeFinalAttemptTiming returns only a complete, non-zero final-attempt
// timing pair. Agentbus promises terminal jobs have the pair together; keeping
// that invariant here avoids exposing a partial or fabricated duration when a
// stale or malformed response is encountered.
func completeFinalAttemptTiming(startedAt, endedAt *time.Time) (*time.Time, *time.Time) {
	if startedAt == nil || endedAt == nil || startedAt.IsZero() || endedAt.IsZero() {
		return nil, nil
	}
	return startedAt, endedAt
}

func resultUnavailableReason(state engine.JobState) string {
	switch state {
	case engine.StateCompleted:
		return "completed_without_result"
	case engine.StateCompletedNoncompliant:
		return "completed_noncompliant_without_result"
	case engine.StateFailed:
		return "failed_without_result"
	case engine.StateTimedOut:
		return "timed_out_without_result"
	case engine.StateInterrupted:
		return "interrupted_without_result"
	case engine.StateCanceled:
		return "canceled_without_result"
	case engine.StateOrphaned:
		return "orphaned_without_result"
	case engine.StateReaped:
		return "reaped_without_result"
	case engine.StateQuarantined:
		return "quarantined_without_result"
	case engine.StateQueued:
		return "queued_without_result"
	case engine.StateStarting:
		return "starting_without_result"
	case engine.StateRunning:
		return "running_without_result"
	case engine.StateRetrying:
		return "retrying_without_result"
	case "":
		return "state_unknown_without_result"
	default:
		return string(state) + "_without_result"
	}
}

func normalizedModelEffort(values ...config.ModelEffortResolution) config.ModelEffortResolution {
	resolved := config.ModelEffortResolution{}
	if len(values) > 0 {
		resolved = values[0]
	}
	if resolved.Model.Source == "" {
		resolved.Model.Source = "default"
	}
	if resolved.Effort.Source == "" {
		resolved.Effort.Source = "default"
	}
	return resolved
}

func normalizedTimeout(resolution config.DimensionResolution) config.DimensionResolution {
	if resolution.Source == "" {
		resolution.Source = "unknown"
	}
	return resolution
}

// normalizedBackendProfile preserves a missing profile as explicitly unknown.
// Terminal envelopes can be reconstructed after a context loss, so an absent
// local metadata record must not be mistaken for the default read-only profile.
func normalizedBackendProfile(resolution config.DimensionResolution) config.DimensionResolution {
	if resolution.Source == "" {
		resolution.Source = "unknown"
	}
	return resolution
}

func modelReportedUnavailableReason(capable bool, modelReported string) string {
	if modelReported != "" {
		return ""
	}
	if !capable {
		return "agentbus_capability_missing"
	}
	// Non-causal on purpose. A capable backend with an empty model can mean the
	// backend genuinely never emitted its model (e.g. codex app-server) OR that
	// agentbus captured it but lost the runtime-only record before this result
	// was read (idle shutdown, restart, or FIFO eviction of reportedModels).
	// Delegate cannot distinguish those from the result alone, so it must not
	// assert the causal claim "backend_did_not_report" here.
	return "model_report_unavailable"
}

func launchStatus(state engine.JobState) string {
	return string(state)
}

func writeJSONLine(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}
