package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/policy"
)

// TestReportCorrectionResultUsableRequiresDigest locks in the round-2 fix: a
// correction result is "usable" (allowed to replace the original) only when it
// carries a present result digest, matching the terminal envelope's
// authoritative-result criterion. Otherwise the envelope would emit
// completed_without_result and suppress the original body SHA + cleanup.
func TestReportCorrectionResultUsableRequiresDigest(t *testing.T) {
	body := compliantReport()
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])

	noDigest := client.JobResult{JobID: "corr", State: engine.StateCompleted, Result: &engine.ResultInfo{Text: body, Bytes: int64(len(body))}}
	if reportCorrectionResultUsable(noDigest) {
		t.Fatal("correction without a result digest must not be usable (would suppress the original)")
	}

	withDigest := client.JobResult{JobID: "corr", State: engine.StateCompleted, Result: &engine.ResultInfo{Text: body, Bytes: int64(len(body)), SHA256: digest}}
	if !reportCorrectionResultUsable(withDigest) {
		t.Fatal("correction with a present matching digest should be usable")
	}

	if reportCorrectionResultUsable(client.JobResult{JobID: "corr", State: engine.StateCompleted}) {
		t.Fatal("correction without a result must not be usable")
	}
}

// TestRunStatusTerminalJobEmitsJobsShapeWithExitCode locks in ⑧: `delegate
// status --job` must emit the {"jobs":[...]} JobStatusResult shape even when the
// job is terminal (it previously flipped to a flat TerminalEnvelope, breaking
// single-schema pollers at exactly the terminal transition). The terminal state
// still maps to the process exit code.
func TestRunStatusTerminalJobEmitsJobsShapeWithExitCode(t *testing.T) {
	finalAttemptStartedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	finalAttemptEndedAt := finalAttemptStartedAt.Add(time.Minute)
	for _, tc := range []struct {
		name                 string
		job                  client.JobStatus
		wantExitCode         int
		wantTerminalMetadata bool
	}{
		{
			name: "terminal job preserves failure metadata and timing",
			job: client.JobStatus{
				JobID:                 "job_term",
				State:                 engine.StateFailed,
				FailureReason:         "backend exited before producing a result",
				FailureClass:          engine.FailureClassBackendError,
				FinalAttemptStartedAt: &finalAttemptStartedAt,
				FinalAttemptEndedAt:   &finalAttemptEndedAt,
			},
			wantExitCode:         engine.ExitCodeForState(engine.StateFailed),
			wantTerminalMetadata: true,
		},
		{
			name:         "running job remains free of terminal metadata",
			job:          client.JobStatus{JobID: "job_running", State: engine.StateRunning},
			wantExitCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{
				hello:  helloWithCapabilities(),
				status: client.JobStatusResult{Jobs: []client.JobStatus{tc.job}},
			}
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			var stdout, stderr bytes.Buffer
			code := run([]string{"status", "--job", tc.job.JobID, "--json"}, nil, &stdout, &stderr)
			if code != tc.wantExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%q", code, tc.wantExitCode, stderr.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil {
				t.Fatalf("status JSON invalid: %v; raw=%q", err, stdout.String())
			}
			jobs, ok := envelope["jobs"].([]any)
			if !ok || len(jobs) != 1 {
				t.Fatalf("status JSON = %q, want one JobStatusResult job", stdout.String())
			}
			job, ok := jobs[0].(map[string]any)
			if !ok {
				t.Fatalf("status job = %#v, want object", jobs[0])
			}
			if _, flipped := envelope["contract"]; flipped {
				t.Fatalf("status JSON leaked a terminal-envelope 'contract' field (schema flip regressed): %q", stdout.String())
			}
			if tc.wantTerminalMetadata {
				if job["failureReason"] != tc.job.FailureReason || job["failureClass"] != string(tc.job.FailureClass) || job["finalAttemptStartedAt"] != finalAttemptStartedAt.Format(time.RFC3339) || job["finalAttemptEndedAt"] != finalAttemptEndedAt.Format(time.RFC3339) {
					t.Fatalf("status job=%#v, want failure metadata and final-attempt timing", job)
				}
				return
			}
			for _, field := range []string{"failureReason", "failureClass", "finalAttemptStartedAt", "finalAttemptEndedAt"} {
				if _, found := job[field]; found {
					t.Fatalf("running status job=%#v, unexpectedly contains %q", job, field)
				}
			}
		})
	}
}

// TestLocalReconstructedContractStampFromBody locks in ⑦: when agentbus returns
// no contract stamp (admission/v2 jobs) but a result body is present, delegate
// re-derives the true verdict from the body instead of defaulting to
// result_unavailable — and only with positive shape provenance.
func TestLocalReconstructedContractStampFromBody(t *testing.T) {
	reconstructCompliantReport := compliantReport()
	res := client.JobResult{
		JobID:  "j",
		State:  engine.StateCompleted,
		Result: &engine.ResultInfo{Text: reconstructCompliantReport, Bytes: int64(len(reconstructCompliantReport))},
	}

	stamp, ok := localReconstructedContractStamp(res, contractKindShape, true, reportValidationAttempt{attempts: 1})
	if !ok {
		t.Fatal("expected reconstruction from a present shape body")
	}
	if stamp.Status != engine.ContractCompliant {
		t.Fatalf("stamp status = %q, want compliant; missing=%v", stamp.Status, stamp.Missing)
	}

	sections := reportSections(t)
	lastSection := sections[len(sections)-1]
	noncompliant := strings.Replace(reconstructCompliantReport, lastSection+":", "Scope omitted:", 1)
	res.Result.Text = noncompliant
	res.Result.Bytes = int64(len(noncompliant))
	stamp, ok = localReconstructedContractStamp(res, contractKindShape, true, reportValidationAttempt{attempts: 1})
	if !ok || stamp.Status != engine.ContractNoncompliant {
		t.Fatalf("noncompliant reconstruction = (%v, %q), want noncompliant (never result_unavailable with a body)", ok, stamp.Status)
	}

	// Without metadata provenance the contract kind is unknown; reconstruction
	// must be refused rather than validate against the wrong (default shape) spec.
	if _, ok := localReconstructedContractStamp(res, contractKindShape, false, reportValidationAttempt{attempts: 1}); ok {
		t.Fatal("reconstruction without positive shape provenance must be refused")
	}
}

func reportSections(t *testing.T) []string {
	t.Helper()
	spec, err := policy.DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		RequiredSections []string `json:"requiredSections"`
	}
	if len(spec.Shape) == 0 || json.Unmarshal(spec.Shape, &shape) != nil || len(shape.RequiredSections) == 0 {
		t.Fatalf("delegate report spec = %#v, want sections", spec)
	}
	return shape.RequiredSections
}
