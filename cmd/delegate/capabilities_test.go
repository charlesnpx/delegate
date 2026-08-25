package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/engine"
)

func TestRequiredCapabilitiesForNilPolicy(t *testing.T) {
	required := requiredCapabilitiesForPolicy(nil)
	if len(required) != 1 || required[0] != "admission.strictContainment" {
		t.Fatalf("required = %#v, want only admission.strictContainment", required)
	}
}

func TestRequiredCapabilitiesForJSONSchemaPolicy(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["schema_version"]}`)
	turnPolicy := turnPolicyForOutputSchema(schema)
	if turnPolicy == nil || turnPolicy.Contract == nil || string(turnPolicy.Contract.JSONSchema) != string(schema) {
		t.Fatalf("turn policy = %#v, want JSON Schema contract", turnPolicy)
	}
	if turnPolicy.Retry == nil || turnPolicy.Retry.Max != 1 || !strings.Contains(turnPolicy.Retry.Template, "{{missing}}") {
		t.Fatalf("retry policy = %#v, want one retry with {{missing}}", turnPolicy.Retry)
	}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	assertContains(t, required, "admission.strictContainment")
	assertContains(t, required, "policy.jsonSchema")
	assertContains(t, required, "policy.retry")
	assertNotContains(t, required, "policy.shape")
}

func TestRetryPolicyRequiresRetryCapability(t *testing.T) {
	turnPolicy := &engine.TurnPolicy{Retry: &engine.RetryPolicy{Max: 1}}
	required := requiredCapabilitiesForPolicy(turnPolicy)
	assertContains(t, required, "policy.retry")
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, value := range got {
		if value == want {
			return
		}
	}
	t.Fatalf("required = %#v, want to contain %q", got, want)
}

func assertNotContains(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, value := range got {
		if value == unwanted {
			t.Fatalf("required = %#v, want not to contain %q", got, unwanted)
		}
	}
}
