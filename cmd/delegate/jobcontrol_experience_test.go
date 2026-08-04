package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

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

const reconstructCompliantReport = `complete

Criteria scored:
- observed: fixture criteria satisfied at example/thing.go:12.

Receipts:
- observed: fixture command exit 0.

Verification:
- observed: fixture command exit 0.

Scope boundary:
- observed: limited to the delegate report shape at example/scope.md:3.
`

// TestLocalReconstructedContractStampFromBody locks in ⑦: when agentbus returns
// no contract stamp (admission/v2 jobs) but a result body is present, delegate
// re-derives the true verdict from the body instead of defaulting to
// result_unavailable — and only with positive shape provenance.
func TestLocalReconstructedContractStampFromBody(t *testing.T) {
	res := client.JobResult{
		JobID:  "j",
		State:  engine.StateCompleted,
		Result: &engine.ResultInfo{Text: reconstructCompliantReport, Bytes: int64(len(reconstructCompliantReport))},
	}

	stamp, ok := localReconstructedContractStamp(res, contractKindShape, true)
	if !ok {
		t.Fatal("expected reconstruction from a present shape body")
	}
	if stamp.Status != engine.ContractCompliant {
		t.Fatalf("stamp status = %q, want compliant; missing=%v", stamp.Status, stamp.Missing)
	}

	noncompliant := strings.Replace(reconstructCompliantReport, "Scope boundary:", "Scope omitted:", 1)
	res.Result.Text = noncompliant
	res.Result.Bytes = int64(len(noncompliant))
	stamp, ok = localReconstructedContractStamp(res, contractKindShape, true)
	if !ok || stamp.Status != engine.ContractNoncompliant {
		t.Fatalf("noncompliant reconstruction = (%v, %q), want noncompliant (never result_unavailable with a body)", ok, stamp.Status)
	}

	// Without metadata provenance the contract kind is unknown; reconstruction
	// must be refused rather than validate against the wrong (default shape) spec.
	if _, ok := localReconstructedContractStamp(res, contractKindShape, false); ok {
		t.Fatal("reconstruction without positive shape provenance must be refused")
	}
}
