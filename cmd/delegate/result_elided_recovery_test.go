package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

// TestTerminalEnvelopeRecoversElidedResultBody locks in the fix for the
// experience-report headline bug (result_unavailable 14/14, observed on the
// v0.6.0 tag). agentbus elides inline Result.Text for bodies at/above its inline
// cap and returns only ResultPath+SHA256+Bytes (authorityResultInfo sets
// TextElided=true). The pre-#20 delegate treated an empty inline Text as an
// absent result and skipped the shape contract; current main must instead read
// the on-disk body from ResultPath (sha-verified) and reconstruct the shape
// verdict locally — never falling through to result_unavailable.
func TestTerminalEnvelopeRecoversElidedResultBody(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(stateDir, jobMetadata{JobID: "job_elided", Kind: taskKind, ContractKind: contractKindShape}); err != nil {
		t.Fatal(err)
	}

	body := compliantReport()
	resultPath := filepath.Join(stateDir, "job_elided.txt")
	if err := os.WriteFile(resultPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])

	// Elided result: no inline Text, body reachable only via ResultPath.
	result := client.JobResult{
		JobID: "job_elided",
		State: engine.StateCompleted,
		Result: &engine.ResultInfo{
			ResultPath: resultPath,
			SHA256:     digest,
			Bytes:      int64(len(body)),
			TextElided: true,
		},
	}

	env, err := terminalEnvelopeFromJobResult(stateDir, result, true)
	if err != nil {
		t.Fatalf("terminalEnvelopeFromJobResult: %v", err)
	}
	if env.ContractKind != contractKindShape {
		t.Fatalf("contractKind = %q, want shape", env.ContractKind)
	}
	if env.Contract.Status != engine.ContractCompliant {
		t.Fatalf("contract status = %q (missing=%v), want compliant reconstructed from elided body — not skipped/result_unavailable", env.Contract.Status, env.Contract.Missing)
	}
	if env.Contract.Attempts != 1 {
		t.Fatalf("contract attempts = %d, want 1 (contract actually ran)", env.Contract.Attempts)
	}
	if env.ResultUnavailableReason != "" {
		t.Fatalf("result_unavailable_reason = %q, want empty (body recovered from ResultPath)", env.ResultUnavailableReason)
	}
	if env.ResultSHA256 == nil || *env.ResultSHA256 != digest {
		t.Fatalf("result_sha256 = %v, want %q", env.ResultSHA256, digest)
	}
	if env.Status != engine.StateCompleted {
		t.Fatalf("status = %q, want completed", env.Status)
	}
}
