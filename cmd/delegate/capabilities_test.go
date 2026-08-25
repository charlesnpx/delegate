package main

import (
	"encoding/json"
	"testing"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/policy"
)

// TestRequiredCapabilitiesForDefaultReportPolicyOmitsShape locks in the
// post-relocation compatibility contract: the default delegate-report path
// carries a shape contract, but delegate now owns shape validation, so it must
// NOT require the agentbus policy.shape capability. The post-relocation agentbus
// advertises policy.shape=false while still accepting/stamping shape contracts,
// so requiring it here would wrongly reject the normal managed path.
func TestRequiredCapabilitiesForDefaultReportPolicyOmitsShape(t *testing.T) {
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if turnPolicy == nil || turnPolicy.Contract == nil || turnPolicy.Contract.Shape == nil {
		t.Fatalf("default turn policy = %#v, want a shape contract", turnPolicy)
	}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	assertContains(t, required, "admission.strictContainment")
	assertNotContains(t, required, "policy.shape")
	assertNotContains(t, required, "policy.jsonSchema")
	assertNotContains(t, required, "policy.retry")
}

// TestRequiredCapabilitiesForJSONSchemaPolicyRequiresSchemaAndRetry proves the
// relocation did not weaken the capabilities delegate genuinely depends on: a
// JSON-Schema job still requires policy.jsonSchema (agentbus performs the schema
// validation) and, because it carries a corrective retry, policy.retry.
func TestRequiredCapabilitiesForJSONSchemaPolicyRequiresSchemaAndRetry(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["schema_version"],"properties":{"schema_version":{"const":"1"}}}`)
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{JSONSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	assertContains(t, required, "admission.strictContainment")
	assertContains(t, required, "policy.jsonSchema")
	assertContains(t, required, "policy.retry")
	assertNotContains(t, required, "policy.shape")
}

// TestRequiredCapabilitiesForNoContractPolicy proves --no-contract needs only
// strict containment.
func TestRequiredCapabilitiesForNoContractPolicy(t *testing.T) {
	turnPolicy, err := policy.ResolveTurnPolicy(policy.Flags{NoContract: true})
	if err != nil {
		t.Fatal(err)
	}
	if turnPolicy != nil {
		t.Fatalf("no-contract turn policy = %#v, want nil", turnPolicy)
	}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	if len(required) != 1 || required[0] != "admission.strictContainment" {
		t.Fatalf("no-contract required = %#v, want only admission.strictContainment", required)
	}
}

// TestRetryPolicyRequiresRetryCapability guards the retry branch directly.
func TestRetryPolicyRequiresRetryCapability(t *testing.T) {
	turnPolicy := &engine.TurnPolicy{Retry: &engine.RetryPolicy{Max: 1}}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	assertContains(t, required, "policy.retry")
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, v := range got {
		if v == want {
			return
		}
	}
	t.Fatalf("required = %#v, want to contain %q", got, want)
}

func assertNotContains(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, v := range got {
		if v == unwanted {
			t.Fatalf("required = %#v, want NOT to contain %q", got, unwanted)
		}
	}
}
