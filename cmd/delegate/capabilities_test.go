package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
	turnPolicy := turnPolicyForSchema(schema)
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

func TestTaskRejectsMissingRequiredCapabilityBeforeSubmission(t *testing.T) {
	tests := []struct {
		name    string
		missing string
		schema  bool
	}{
		{name: "universal strict containment", missing: "admission.strictContainment"},
		{name: "schema JSON Schema", missing: "policy.jsonSchema", schema: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithCapabilities()}
			fake.hello.Capabilities[tc.missing] = false
			restore := stubAgentbusGlobals(t, fake)
			defer restore()

			cwd := t.TempDir()
			args := []string{"task", "--backend", "codex", "--cwd", cwd, "--prompt-file", "-"}
			if tc.schema {
				schemaPath := filepath.Join(cwd, "schema.json")
				if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--schema-file", schemaPath)
			}

			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader("prompt"), &stdout, &stderr); code == 0 {
				t.Fatalf("run(%q) unexpectedly succeeded", args)
			}
			if !strings.Contains(stderr.String(), "lacks capability `"+tc.missing+"`") {
				t.Fatalf("stderr=%q, want capability error for %q", stderr.String(), tc.missing)
			}
			if len(fake.submits) != 0 {
				t.Fatalf("JobSubmit calls=%d, want 0", len(fake.submits))
			}
		})
	}
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
