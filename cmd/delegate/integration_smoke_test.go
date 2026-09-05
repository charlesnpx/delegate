package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

// TestManagedSkillSmokeFixture exercises the stdin launch shape generated for
// the managed delegate skill: task receives a private prompt through stdin and returns a receipt.
func TestManagedSkillSmokeFixture(t *testing.T) {
	prompt := "Investigate the managed skill task and report the result."
	bus := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_smoke",
			State:   publicStateQueued,
			Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
		},
	}
	restore := stubAgentbusGlobals(t, bus)
	defer restore()

	var submitOut, submitErr bytes.Buffer
	if code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-", "--tag", "skill=delegate"}, strings.NewReader(prompt), &submitOut, &submitErr); code != 0 {
		t.Fatalf("task code=%d stderr=%q", code, submitErr.String())
	}
	var receipt taskSubmitReceipt
	if err := json.Unmarshal(submitOut.Bytes(), &receipt); err != nil {
		t.Fatalf("receipt JSON invalid: %v; raw=%q", err, submitOut.String())
	}
	if receipt.JobID != "job_smoke" || receipt.State != publicStateQueued || receipt.Timeout == nil || receipt.Timeout.Source != engine.TimeoutSourceDaemonDefault {
		t.Fatalf("receipt=%#v", receipt)
	}
	if len(bus.submits) != 1 || bus.submits[0].TaskSpec.Prompt != prompt || bus.submits[0].TaskSpec.Tags == nil || (*bus.submits[0].TaskSpec.Tags)["skill"] != "delegate" {
		t.Fatalf("submitted=%#v", bus.submits)
	}
}
