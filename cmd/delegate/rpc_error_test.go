package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/charlesnpx/agentbus/client"
)

func TestClassifyAgentbusRPCAdmissionCauses(t *testing.T) {
	tests := []struct {
		cause            string
		wantAction       agentbusRejectionAction
		wantExit         int
		wantRetryable    bool
		wantPreserve     bool
		wantOperatorHelp bool
	}{
		{
			cause:      admissionCauseMissingIdentity,
			wantAction: agentbusActionInternalInvariantFailure,
			wantExit:   agentbusExitDaemonRuntime,
		},
		{
			cause:         admissionCauseAdmissionClosing,
			wantAction:    agentbusActionRetrySameIdentity,
			wantExit:      agentbusExitDaemonRuntime,
			wantRetryable: true,
			wantPreserve:  true,
		},
		{
			cause:        admissionCauseReplayConflict,
			wantAction:   agentbusActionFailHardPreserveIntent,
			wantExit:     agentbusExitDaemonRuntime,
			wantPreserve: true,
		},
		{
			cause:      admissionCauseRequestExpired,
			wantAction: agentbusActionFailHardExplicitResubmission,
			wantExit:   agentbusExitDaemonRuntime,
		},
		{
			cause:        admissionCauseRequestFingerprintUnsupported,
			wantAction:   agentbusActionFailHardPreserveExactRequest,
			wantExit:     agentbusExitDaemonRuntime,
			wantPreserve: true,
		},
		{
			cause:            admissionCauseRootFailStopped,
			wantAction:       agentbusActionPreserveIntentOperatorGuidance,
			wantExit:         agentbusExitAuthority,
			wantPreserve:     true,
			wantOperatorHelp: true,
		},
		{
			cause:            admissionCauseRootCorrupt,
			wantAction:       agentbusActionPreserveIntentOperatorGuidance,
			wantExit:         agentbusExitAuthority,
			wantPreserve:     true,
			wantOperatorHelp: true,
		},
		{
			cause:            admissionCauseRootIdentityMismatch,
			wantAction:       agentbusActionPreserveIntentOperatorGuidance,
			wantExit:         agentbusExitAuthority,
			wantPreserve:     true,
			wantOperatorHelp: true,
		},
		{
			cause:            admissionCauseRootSealed,
			wantAction:       agentbusActionPreserveIntentOperatorGuidance,
			wantExit:         agentbusExitAuthority,
			wantPreserve:     true,
			wantOperatorHelp: true,
		},
		{
			cause:      admissionCauseUnsupportedBackend,
			wantAction: agentbusActionDefinitiveRejection,
			wantExit:   agentbusExitDaemonRuntime,
		},
		{
			cause:      admissionCauseUnfenceableBackend,
			wantAction: agentbusActionDefinitiveRejection,
			wantExit:   agentbusExitDaemonRuntime,
		},
		{
			cause:      admissionCauseUnavailableNativeRuntime,
			wantAction: agentbusActionDefinitiveRejection,
			wantExit:   agentbusExitDaemonRuntime,
		},
		{
			cause:      admissionCauseInvalidStrictConfig,
			wantAction: agentbusActionDefinitiveRejection,
			wantExit:   agentbusExitDaemonRuntime,
		},
	}
	for _, tc := range tests {
		t.Run(tc.cause, func(t *testing.T) {
			err := testRPCError(t, agentbusErrorBackendUnavailable, "admission rejected", tc.cause, "job_rpc", nil)
			classification, ok := classifyAgentbusError(err)
			if !ok {
				t.Fatal("classifyAgentbusError ok = false, want true")
			}
			if classification.Action != tc.wantAction || classification.ExitCode != tc.wantExit {
				t.Fatalf("classification action/exit = %s/%d, want %s/%d", classification.Action, classification.ExitCode, tc.wantAction, tc.wantExit)
			}
			if classification.Retryable != tc.wantRetryable || classification.PreserveIntent != tc.wantPreserve || classification.OperatorGuidance != tc.wantOperatorHelp {
				t.Fatalf("classification flags = retryable:%t preserve:%t operator:%t", classification.Retryable, classification.PreserveIntent, classification.OperatorGuidance)
			}
			if classification.Code != agentbusErrorBackendUnavailable || classification.AdmissionCause != tc.cause || classification.JobID != "job_rpc" {
				t.Fatalf("classification fields = %#v", classification)
			}
		})
	}
}

func TestClassifyAgentbusRPCExitCodesAndRetryableAmbiguousErrors(t *testing.T) {
	t.Run("unknown job exit", func(t *testing.T) {
		err := testRPCError(t, agentbusErrorUnknownJob, "job not found", "", "job_missing", nil)
		classification, ok := classifyAgentbusError(err)
		if !ok {
			t.Fatal("classifyAgentbusError ok = false, want true")
		}
		if classification.ExitCode != agentbusExitUnknownJob || classification.Action != agentbusActionUnknownJob {
			t.Fatalf("classification = %#v, want unknown-job exit/action", classification)
		}
	})

	t.Run("blank cause backend unavailable retryable", func(t *testing.T) {
		err := testRPCError(t, agentbusErrorBackendUnavailable, "backend unavailable", "", "job_backend", nil)
		classification, ok := classifyAgentbusError(err)
		if !ok {
			t.Fatal("classifyAgentbusError ok = false, want true")
		}
		if !classification.Retryable || !classification.PreserveIntent || classification.Action != agentbusActionAmbiguousRetrySameRequest {
			t.Fatalf("classification = %#v, want retryable ambiguous backend_unavailable", classification)
		}
		if classification.ExitCode != agentbusExitDaemonRuntime {
			t.Fatalf("exit = %d, want %d", classification.ExitCode, agentbusExitDaemonRuntime)
		}
	})

	t.Run("transport retryable", func(t *testing.T) {
		classification, ok := classifyAgentbusError(agentbusOperationError(errors.New("connection reset")))
		if !ok {
			t.Fatal("classifyAgentbusError ok = false, want true")
		}
		if classification.Code != agentbusErrorTransport || !classification.Retryable || !classification.PreserveIntent {
			t.Fatalf("classification = %#v, want retryable transport error", classification)
		}
		if classification.ExitCode != agentbusExitDaemonRuntime {
			t.Fatalf("exit = %d, want %d", classification.ExitCode, agentbusExitDaemonRuntime)
		}
	})
}

func TestJSONAgentbusErrorEnvelopePreservesRPCFields(t *testing.T) {
	rpcErr := testRPCError(t, agentbusErrorBackendUnavailable, "native runtime unavailable", admissionCauseUnavailableNativeRuntime, "job_json", map[string]any{
		"class":       "native_runtime",
		"cause":       "sandbox_unavailable",
		"attempts":    2,
		"cleanupSafe": false,
	})
	fake := &fakeAgentbusClient{
		hello:     helloWithCapabilities(),
		statusErr: rpcErr,
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--job", "job_json", "--json"}, nil, &stdout, &stderr)
	if code != agentbusExitDaemonRuntime {
		t.Fatalf("status code = %d, want %d; stderr=%q", code, agentbusExitDaemonRuntime, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty JSON error path", stderr.String())
	}
	var envelope struct {
		Code           string         `json:"code"`
		Message        string         `json:"message"`
		AdmissionCause string         `json:"admission_cause"`
		JobID          string         `json:"job_id"`
		RuntimeSupport map[string]any `json:"runtime_support"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil {
		t.Fatalf("JSON error envelope invalid: %v; raw=%q", err, stdout.String())
	}
	if envelope.Code != agentbusErrorBackendUnavailable || envelope.Message != "native runtime unavailable" || envelope.AdmissionCause != admissionCauseUnavailableNativeRuntime || envelope.JobID != "job_json" {
		t.Fatalf("error envelope = %#v", envelope)
	}
	if envelope.RuntimeSupport["class"] != "native_runtime" || envelope.RuntimeSupport["cause"] != "sandbox_unavailable" || envelope.RuntimeSupport["attempts"] != float64(2) || envelope.RuntimeSupport["cleanupSafe"] != false {
		t.Fatalf("runtime_support = %#v", envelope.RuntimeSupport)
	}
}

func testRPCError(t *testing.T, stableCode, message, admissionCause, jobID string, runtimeSupport map[string]any) error {
	t.Helper()
	data := map[string]any{"code": stableCode}
	if admissionCause != "" {
		data["admissionCause"] = admissionCause
	}
	if jobID != "" {
		data["jobId"] = jobID
	}
	if runtimeSupport != nil {
		data["runtimeSupport"] = runtimeSupport
	}
	raw, err := json.Marshal(map[string]any{
		"Object": map[string]any{
			"code":    -32000,
			"message": message,
			"data":    data,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rpcErr client.RPCError
	if err := json.Unmarshal(raw, &rpcErr); err != nil {
		t.Fatalf("unmarshal RPCError: %v; raw=%s", err, raw)
	}
	return &rpcErr
}
