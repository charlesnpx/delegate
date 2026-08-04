package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

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
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		status: client.JobStatusResult{Jobs: []client.JobStatus{{JobID: "job_term", State: engine.StateCompletedNoncompliant}}},
		result: client.JobResult{JobID: "job_term", State: engine.StateCompletedNoncompliant},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--job", "job_term", "--json"}, nil, &stdout, &stderr)
	if want := engine.ExitCodeForState(engine.StateCompletedNoncompliant); code != want {
		t.Fatalf("exit code = %d, want %d (terminal state mapped to exit); stderr=%q", code, want, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &envelope); err != nil {
		t.Fatalf("status JSON invalid: %v; raw=%q", err, stdout.String())
	}
	if _, ok := envelope["jobs"]; !ok {
		t.Fatalf("status JSON = %q, want JobStatusResult {jobs:[...]} shape for a terminal job", stdout.String())
	}
	if _, flipped := envelope["contract"]; flipped {
		t.Fatalf("status JSON leaked a terminal-envelope 'contract' field (schema flip regressed): %q", stdout.String())
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
	if spec.Shape == nil || len(spec.Shape.RequiredSections) == 0 {
		t.Fatalf("delegate report spec = %#v, want sections", spec)
	}
	return spec.Shape.RequiredSections
}
