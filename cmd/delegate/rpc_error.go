package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/charlesnpx/agentbus/client"
)

const (
	agentbusErrorBackendUnavailable = "backend_unavailable"
	agentbusErrorUnknownJob         = "unknown_job"
	agentbusErrorTransport          = "transport_error"

	admissionCauseMissingIdentity               = "missing_identity"
	admissionCauseAdmissionClosing              = "admission_closing"
	admissionCauseReplayConflict                = "replay_conflict"
	admissionCauseRequestExpired                = "request_expired"
	admissionCauseRequestFingerprintUnsupported = "request_fingerprint_unsupported"
	admissionCauseRootFailStopped               = "root_fail_stopped"
	admissionCauseRootCorrupt                   = "root_corrupt"
	admissionCauseRootIdentityMismatch          = "root_identity_mismatch"
	admissionCauseRootSealed                    = "root_sealed"
	admissionCauseUnsupportedBackend            = "unsupported_backend"
	admissionCauseUnfenceableBackend            = "unfenceable_backend"
	admissionCauseUnavailableNativeRuntime      = "unavailable_native_runtime"
	admissionCauseInvalidStrictConfig           = "invalid_strict_config"
)

const (
	agentbusExitDaemonRuntime = 11
	agentbusExitUnknownJob    = 10
	agentbusExitAuthority     = 12
)

type agentbusRejectionAction string

const (
	agentbusActionUnknownJob                     agentbusRejectionAction = "unknown_job"
	agentbusActionInternalInvariantFailure       agentbusRejectionAction = "internal_delegate_invariant_failure"
	agentbusActionRetrySameIdentity              agentbusRejectionAction = "retry_same_identity"
	agentbusActionFailHardPreserveIntent         agentbusRejectionAction = "fail_hard_preserve_intent"
	agentbusActionFailHardExplicitResubmission   agentbusRejectionAction = "fail_hard_explicit_resubmission_required"
	agentbusActionFailHardPreserveExactRequest   agentbusRejectionAction = "fail_hard_preserve_exact_request"
	agentbusActionPreserveIntentOperatorGuidance agentbusRejectionAction = "preserve_intent_operator_guidance"
	agentbusActionDefinitiveRejection            agentbusRejectionAction = "definitive_rejection"
	agentbusActionAmbiguousRetrySameRequest      agentbusRejectionAction = "ambiguous_retry_same_request"
)

type agentbusErrorClassification struct {
	Code             string
	Message          string
	AdmissionCause   string
	JobID            string
	RuntimeSupport   any
	Action           agentbusRejectionAction
	ExitCode         int
	Retryable        bool
	PreserveIntent   bool
	Definitive       bool
	OperatorGuidance bool
	Guidance         string
}

type agentbusErrorEnvelope struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	AdmissionCause string `json:"admission_cause,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	RuntimeSupport any    `json:"runtime_support,omitempty"`
	Guidance       string `json:"operator_guidance,omitempty"`
}

func (classification agentbusErrorClassification) Envelope() agentbusErrorEnvelope {
	return agentbusErrorEnvelope{
		Code:           classification.Code,
		Message:        classification.Message,
		AdmissionCause: classification.AdmissionCause,
		JobID:          classification.JobID,
		RuntimeSupport: classification.RuntimeSupport,
		Guidance:       classification.Guidance,
	}
}

type agentbusTransportError struct {
	Err error
}

func (err *agentbusTransportError) Error() string {
	if err == nil || err.Err == nil {
		return agentbusErrorTransport
	}
	return err.Err.Error()
}

func (err *agentbusTransportError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func agentbusOperationError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr *client.RPCError
	if errors.As(err, &rpcErr) {
		return err
	}
	var transportErr *agentbusTransportError
	if errors.As(err, &transportErr) {
		return err
	}
	return &agentbusTransportError{Err: err}
}

func classifyAgentbusError(err error) (agentbusErrorClassification, bool) {
	if err == nil {
		return agentbusErrorClassification{}, false
	}
	var rpcErr *client.RPCError
	if errors.As(err, &rpcErr) {
		return classifyAgentbusRPCError(rpcErr), true
	}
	var transportErr *agentbusTransportError
	if errors.As(err, &transportErr) {
		return agentbusErrorClassification{
			Code:           agentbusErrorTransport,
			Message:        transportErr.Error(),
			Action:         agentbusActionAmbiguousRetrySameRequest,
			ExitCode:       agentbusExitDaemonRuntime,
			Retryable:      true,
			PreserveIntent: true,
		}, true
	}
	return agentbusErrorClassification{}, false
}

func classifyAgentbusRPCError(rpcErr *client.RPCError) agentbusErrorClassification {
	data := rpcErr.Object.Data
	classification := agentbusErrorClassification{
		Code:           data.Code,
		Message:        rpcErr.Object.Message,
		AdmissionCause: data.AdmissionCause,
		JobID:          data.JobID,
		RuntimeSupport: data.RuntimeSupport,
		Action:         agentbusActionDefinitiveRejection,
		ExitCode:       agentbusExitDaemonRuntime,
		Definitive:     true,
	}
	if classification.Code == "" {
		classification.Code = fmt.Sprintf("rpc_%d", rpcErr.Object.Code)
	}
	if classification.Message == "" {
		classification.Message = rpcErr.Error()
	}
	if classification.Code == agentbusErrorUnknownJob {
		classification.Action = agentbusActionUnknownJob
		classification.ExitCode = agentbusExitUnknownJob
		return classification
	}
	switch classification.AdmissionCause {
	case admissionCauseMissingIdentity:
		classification.Action = agentbusActionInternalInvariantFailure
	case admissionCauseAdmissionClosing:
		classification.Action = agentbusActionRetrySameIdentity
		classification.Retryable = true
		classification.PreserveIntent = true
		classification.Definitive = false
	case admissionCauseReplayConflict:
		classification.Action = agentbusActionFailHardPreserveIntent
		classification.PreserveIntent = true
	case admissionCauseRequestExpired:
		classification.Action = agentbusActionFailHardExplicitResubmission
	case admissionCauseRequestFingerprintUnsupported:
		classification.Action = agentbusActionFailHardPreserveExactRequest
		classification.PreserveIntent = true
	case admissionCauseRootFailStopped, admissionCauseRootCorrupt, admissionCauseRootIdentityMismatch, admissionCauseRootSealed:
		classification.Action = agentbusActionPreserveIntentOperatorGuidance
		classification.ExitCode = agentbusExitAuthority
		classification.PreserveIntent = true
		classification.OperatorGuidance = true
		classification.Guidance = "Agentbus authority rejected admission for the configured state root; inspect the root condition and resolve it before resubmitting"
	case admissionCauseUnsupportedBackend, admissionCauseUnfenceableBackend, admissionCauseUnavailableNativeRuntime, admissionCauseInvalidStrictConfig:
		classification.Action = agentbusActionDefinitiveRejection
	case "":
		if classification.Code == agentbusErrorBackendUnavailable {
			classification.Action = agentbusActionAmbiguousRetrySameRequest
			classification.Retryable = true
			classification.PreserveIntent = true
			classification.Definitive = false
		}
	}
	return classification
}

func agentbusCommandErrorResult(jsonOut bool, stdout io.Writer, err error) (int, error) {
	classification, ok := classifyAgentbusError(err)
	if !ok {
		return 0, err
	}
	var unresolved submissionUnresolvedError
	if errors.As(err, &unresolved) {
		classification.Message = unresolved.Error()
		classification.Guidance = fmt.Sprintf("delegate task --recover-request %s", unresolved.RequestID)
	}
	if jsonOut {
		return classification.ExitCode, writeJSONLine(stdout, classification.Envelope())
	}
	if classification.Guidance != "" {
		return classification.ExitCode, agentbusGuidedError{Err: err, Guidance: classification.Guidance}
	}
	return classification.ExitCode, err
}

type agentbusGuidedError struct {
	Err      error
	Guidance string
}

func (err agentbusGuidedError) Error() string {
	if err.Err == nil {
		return err.Guidance
	}
	if err.Guidance == "" {
		return err.Err.Error()
	}
	return fmt.Sprintf("%s; %s", err.Err, err.Guidance)
}

func (err agentbusGuidedError) Unwrap() error {
	return err.Err
}
