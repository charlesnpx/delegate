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

	contractKindShape      = "shape"
	contractKindJSONSchema = "jsonSchema"
	contractKindNone       = "none"
)

// LaunchEnvelope is the schema returned when delegate has launched a job.
type LaunchEnvelope struct {
	Schema       int                        `json:"schema"`
	RequestID    string                     `json:"request_id,omitempty"`
	JobID        string                     `json:"job_id"`
	Status       string                     `json:"status"`
	Deduplicated bool                       `json:"deduplicated"`
	Model        config.DimensionResolution `json:"model"`
	Effort       config.DimensionResolution `json:"effort"`
	ResultSHA256 *string                    `json:"result_sha256"`
	Origin       *envelopeOrigin            `json:"origin,omitempty"`
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
	ContractKind                   string                     `json:"contractKind"`
	Contract                       engine.ContractStamp       `json:"contract"`
	ResultSHA256                   *string                    `json:"result_sha256"`
	ResultPath                     string                     `json:"result_path,omitempty"`
	ResultUnavailableReason        string                     `json:"result_unavailable_reason,omitempty"`
	BackendError                   string                     `json:"backend_error,omitempty"`
	Model                          config.DimensionResolution `json:"model"`
	Effort                         config.DimensionResolution `json:"effort"`
	ModelReported                  string                     `json:"model_reported,omitempty"`
	ModelReportedUnavailableReason string                     `json:"model_reported_unavailable_reason,omitempty"`
	SubmittedAt                    *time.Time                 `json:"submitted_at,omitempty"`
	UpdatedAt                      *time.Time                 `json:"updated_at,omitempty"`
	Origin                         *envelopeOrigin            `json:"origin,omitempty"`
}

type terminalEnvelopeOptions struct {
	ModelEffort            config.ModelEffortResolution
	ModelReported          string
	ModelsReportedCapable  bool
	Origin                 envelopeOrigin
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
}

type launchEnvelopeOptions struct {
	ModelEffort  config.ModelEffortResolution
	Origin       envelopeOrigin
	RequestID    string
	Deduplicated bool
}

func (e TerminalEnvelope) MarshalJSON() ([]byte, error) {
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
		ContractKind                   string                     `json:"contractKind"`
		Contract                       map[string]any             `json:"contract"`
		ResultSHA256                   *string                    `json:"result_sha256"`
		ResultPath                     string                     `json:"result_path,omitempty"`
		ResultUnavailableReason        string                     `json:"result_unavailable_reason,omitempty"`
		BackendError                   string                     `json:"backend_error,omitempty"`
		Model                          config.DimensionResolution `json:"model"`
		Effort                         config.DimensionResolution `json:"effort"`
		ModelReported                  string                     `json:"model_reported,omitempty"`
		ModelReportedUnavailableReason string                     `json:"model_reported_unavailable_reason,omitempty"`
		SubmittedAt                    *time.Time                 `json:"submitted_at,omitempty"`
		UpdatedAt                      *time.Time                 `json:"updated_at,omitempty"`
		Origin                         *envelopeOrigin            `json:"origin,omitempty"`
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
		ContractKind:                   e.ContractKind,
		Contract:                       contractStampEnvelopeValue(e.Contract),
		ResultSHA256:                   e.ResultSHA256,
		ResultPath:                     e.ResultPath,
		ResultUnavailableReason:        e.ResultUnavailableReason,
		BackendError:                   e.BackendError,
		Model:                          e.Model,
		Effort:                         e.Effort,
		ModelReported:                  e.ModelReported,
		ModelReportedUnavailableReason: e.ModelReportedUnavailableReason,
		SubmittedAt:                    e.SubmittedAt,
		UpdatedAt:                      e.UpdatedAt,
		Origin:                         e.Origin,
	})
}

func contractStampEnvelopeValue(stamp engine.ContractStamp) map[string]any {
	stamp = normalizeContractStamp(stamp)
	out := map[string]any{
		"status":    stamp.Status,
		"missing":   stamp.Missing,
		"reason":    stamp.Reason,
		"attempts":  stamp.Attempts,
		"retryUsed": stamp.RetryUsed,
	}
	if stamp.ContractName != "" {
		out["contractName"] = stamp.ContractName
	}
	if stamp.ContractSHA256 != "" {
		out["contractSha256"] = stamp.ContractSHA256
	}
	if !stamp.ValidatedAt.IsZero() {
		out["validatedAt"] = stamp.ValidatedAt
	}
	return out
}

func newLaunchEnvelope(jobID string, state engine.JobState, resolutions ...config.ModelEffortResolution) (LaunchEnvelope, error) {
	return newLaunchEnvelopeWithOrigin(jobID, state, envelopeOrigin{}, resolutions...)
}

func newLaunchEnvelopeWithOrigin(jobID string, state engine.JobState, origin envelopeOrigin, resolutions ...config.ModelEffortResolution) (LaunchEnvelope, error) {
	return newLaunchEnvelopeWithOptions(jobID, state, launchEnvelopeOptions{
		ModelEffort: normalizedModelEffort(resolutions...),
		Origin:      origin,
	})
}

func newLaunchEnvelopeWithOptions(jobID string, state engine.JobState, option launchEnvelopeOptions) (LaunchEnvelope, error) {
	modelEffort := normalizedModelEffort(option.ModelEffort)
	env := LaunchEnvelope{
		Schema:       envelopeSchema,
		RequestID:    option.RequestID,
		JobID:        jobID,
		Status:       launchStatus(state),
		Deduplicated: option.Deduplicated,
		Model:        modelEffort.Model,
		Effort:       modelEffort.Effort,
		Origin:       envelopeOriginPointer(option.Origin),
	}
	return env, nil
}

func newTerminalEnvelope(jobID string, state engine.JobState, kind, contractKind string, stamp engine.ContractStamp, resultSHA256, backendError string, options ...terminalEnvelopeOptions) (TerminalEnvelope, error) {
	stamp = normalizeContractStamp(stamp)
	option := terminalEnvelopeOptions{}
	if len(options) > 0 {
		option = options[0]
	}
	modelEffort := normalizedModelEffort(option.ModelEffort)
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
		ContractKind:                   contractKind,
		Contract:                       stamp,
		BackendError:                   backendError,
		Model:                          modelEffort.Model,
		Effort:                         modelEffort.Effort,
		ModelReported:                  option.ModelReported,
		ModelReportedUnavailableReason: modelReportedUnavailableReason(option.ModelsReportedCapable, option.ModelReported),
		SubmittedAt:                    option.SubmittedAt,
		UpdatedAt:                      option.UpdatedAt,
		ResultPath:                     option.ResultPath,
		Origin:                         envelopeOriginPointer(option.Origin),
	}
	if resultSHA256 != "" {
		env.ResultSHA256 = &resultSHA256
	} else {
		env.ResultUnavailableReason = resultUnavailableReason(state)
	}
	return env, nil
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

func modelReportedUnavailableReason(capable bool, modelReported string) string {
	if modelReported != "" {
		return ""
	}
	if !capable {
		return "agentbus_capability_missing"
	}
	return "backend_did_not_report"
}

func launchStatus(state engine.JobState) string {
	return string(state)
}

func normalizeContractStamp(stamp engine.ContractStamp) engine.ContractStamp {
	if stamp.Missing == nil {
		stamp.Missing = []string{}
	}
	return stamp
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
