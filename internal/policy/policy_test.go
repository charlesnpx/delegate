package policy

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/engine"
)

func TestResolveTurnPolicyFlagMatrix(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		flags     Flags
		wantNil   bool
		wantRetry bool
	}{
		{name: "read_only", flags: Flags{}},
		{name: "write", flags: Flags{Write: true}, wantRetry: true},
		{name: "strict", flags: Flags{StrictContract: true}, wantRetry: true},
		{name: "write_strict", flags: Flags{Write: true, StrictContract: true}, wantRetry: true},
		{name: "no_contract", flags: Flags{NoContract: true}, wantNil: true},
		{name: "write_no_contract", flags: Flags{Write: true, NoContract: true}, wantNil: true},
		{name: "strict_no_contract", flags: Flags{StrictContract: true, NoContract: true}, wantNil: true},
		{name: "write_strict_no_contract", flags: Flags{Write: true, StrictContract: true, NoContract: true}, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTurnPolicy(tc.flags)
			if err != nil {
				t.Fatalf("ResolveTurnPolicy() error = %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("ResolveTurnPolicy() = %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("ResolveTurnPolicy() = nil, want policy")
			}
			if got.Prologue != DelegateContractDigest() {
				t.Fatal("policy prologue does not match bundled digest")
			}
			if got.Contract == nil || !reflect.DeepEqual(*got.Contract, spec) {
				t.Fatalf("policy contract = %#v, want bundled spec %#v", got.Contract, spec)
			}
			if tc.wantRetry {
				if got.Retry == nil {
					t.Fatal("policy retry = nil, want retry")
				}
				if got.Retry.Max != 1 {
					t.Fatalf("retry max = %d, want 1", got.Retry.Max)
				}
				if !strings.Contains(got.Retry.Template, "{{missing}}") {
					t.Fatalf("retry template %q does not contain {{missing}}", got.Retry.Template)
				}
			} else if got.Retry != nil {
				t.Fatalf("policy retry = %#v, want nil", got.Retry)
			}
		})
	}
}

func TestResolveTurnPolicyJSONSchemaContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["schema_version"],"properties":{"schema_version":{"const":"1"}}}`)
	got, err := ResolveTurnPolicy(Flags{JSONSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Contract == nil {
		t.Fatalf("policy = %#v, want JSON Schema policy", got)
	}
	if got.Contract.Shape != nil || got.Contract.Named != "" || !reflect.DeepEqual(got.Contract.JSONSchema, schema) {
		t.Fatalf("contract = %#v, want JSON Schema only", got.Contract)
	}
	if got.Retry == nil || got.Retry.Max != 1 || got.Retry.Template != CorrectiveRetryTemplate {
		t.Fatalf("retry = %#v, want default corrective retry", got.Retry)
	}
	if !strings.Contains(got.Prologue, string(schema)) || strings.Contains(got.Prologue, DelegateContractDigest()) {
		t.Fatalf("prologue = %q, want JSON Schema instruction without delegate-report digest", got.Prologue)
	}

	if _, err := ResolveTurnPolicy(Flags{JSONSchema: json.RawMessage(`{"type":`)}); err == nil || !strings.Contains(err.Error(), "jsonSchema must be valid JSON") {
		t.Fatalf("invalid schema error = %v, want engine JSON Schema validation error", err)
	}
}

func TestDisabledStamp(t *testing.T) {
	stamp := DisabledStamp()
	if stamp.Status != engine.ContractDisabled {
		t.Fatalf("status = %q, want %q", stamp.Status, engine.ContractDisabled)
	}
	if stamp.Reason != NoContractFlagReason {
		t.Fatalf("reason = %q, want %q", stamp.Reason, NoContractFlagReason)
	}
	if stamp.Attempts != 0 || stamp.RetryUsed {
		t.Fatalf("attempts/retry = %d/%v, want 0/false", stamp.Attempts, stamp.RetryUsed)
	}
	if stamp.Missing == nil || len(stamp.Missing) != 0 {
		t.Fatalf("missing = %#v, want empty non-nil slice", stamp.Missing)
	}
}

func TestDelegateReportSpecAndRegistryImmutability(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := engine.ContractSHA256(spec)
	if err != nil {
		t.Fatal(err)
	}

	registry := engine.NewPolicyRegistry()
	gotHash, err := RegisterDelegateReport(registry)
	if err != nil {
		t.Fatalf("RegisterDelegateReport() error = %v", err)
	}
	if gotHash != hash {
		t.Fatalf("registered hash = %q, want bundled data hash %q", gotHash, hash)
	}
	gotHash, err = RegisterDelegateReport(registry)
	if err != nil {
		t.Fatalf("RegisterDelegateReport() second call error = %v", err)
	}
	if gotHash != hash {
		t.Fatalf("registered hash on second call = %q, want %q", gotHash, hash)
	}

	resolved, resolvedHash, err := registry.Resolve(DelegateReportContractName)
	if err != nil {
		t.Fatalf("Resolve(%q) error = %v", DelegateReportContractName, err)
	}
	if resolvedHash != hash {
		t.Fatalf("resolved hash = %q, want %q", resolvedHash, hash)
	}
	if !equalContractSpec(resolved, spec) {
		t.Fatalf("resolved spec = %#v, want %#v", resolved, spec)
	}

	mutated := spec
	mutated.Shape = &engine.ShapeSpec{FirstLineEnum: []string{"complete"}}
	_, err = registry.Register(DelegateReportContractName, mutated)
	var conflict engine.NameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("mutated register error = %v, want NameConflictError", err)
	}
}

func equalContractSpec(got, want engine.ContractSpec) bool {
	normalize := func(spec engine.ContractSpec) engine.ContractSpec {
		if spec.Shape == nil {
			return spec
		}
		shape := *spec.Shape
		if len(shape.FirstLineEnum) == 0 {
			shape.FirstLineEnum = nil
		}
		if len(shape.RequiredSections) == 0 {
			shape.RequiredSections = nil
		}
		if len(shape.RequiredAttestations) == 0 {
			shape.RequiredAttestations = nil
		}
		spec.Shape = &shape
		return spec
	}
	return reflect.DeepEqual(normalize(got), normalize(want))
}

func TestDelegateReportSpecShape(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.JSONSchema) != 0 || spec.Named != "" || spec.Shape == nil {
		t.Fatalf("spec variant = %#v, want shape only", spec)
	}
	want := engine.ShapeSpec{
		FirstLineEnum:        []string{"complete", "partial", "blocked"},
		RequiredSections:     []string{"Criteria scored", "Receipts", "Verification", "Scope boundary"},
		RequiredAttestations: []string{},
		EvidenceHeuristic:    true,
	}
	if !reflect.DeepEqual(*spec.Shape, want) {
		b, _ := json.MarshalIndent(spec.Shape, "", "  ")
		t.Fatalf("shape spec = %s, want %#v", b, want)
	}
}

func TestDigestBundleEqualsFile(t *testing.T) {
	if strings.TrimSpace(DelegateContractDigest()) == "" {
		t.Fatal("bundled digest is empty")
	}
	file, err := os.ReadFile("digest/delegate-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	if DelegateContractDigest() != string(file) {
		t.Fatal("bundled digest does not match digest/delegate-contract.md")
	}
}

func TestRetryTemplateAndPolicyValidateWithEngine(t *testing.T) {
	policy, err := ResolveTurnPolicy(Flags{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	compliant := `complete

Criteria scored:
- observed: fixture criteria are satisfied at example/thing.go:12.

Receipts:
- observed: fixture command "example-check --fixture" exit 0.

Verification:
- observed: fixture command "example-verify --fixture" exit 0.

Scope boundary:
- observed: fixture validation is limited to the delegate report shape at example/scope.md:3.
`
	if strings.Contains(compliant, "inferred:") || strings.Contains(compliant, "assumed:") {
		t.Fatal("compliant fixture must exercise all-observed attestations")
	}
	validation, err := engine.ValidatePolicyText(compliant, policy, engine.NewPolicyRegistry(), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ValidatePolicyText(compliant) error = %v", err)
	}
	if validation.Stamp == nil {
		t.Fatal("compliant validation stamp = nil")
	}
	if validation.Stamp.Status != engine.ContractCompliant {
		t.Fatalf("compliant status = %q, want %q; missing=%v", validation.Stamp.Status, engine.ContractCompliant, validation.Stamp.Missing)
	}

	missingScope := strings.Replace(compliant, "Scope boundary:", "Scope omitted:", 1)
	validation, err = engine.ValidatePolicyText(missingScope, policy, engine.NewPolicyRegistry(), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ValidatePolicyText(missing) error = %v", err)
	}
	if validation.Stamp == nil {
		t.Fatal("missing-section validation stamp = nil")
	}
	if validation.Stamp.Status != engine.ContractNoncompliant {
		t.Fatalf("missing-section status = %q, want %q", validation.Stamp.Status, engine.ContractNoncompliant)
	}
	if !reflect.DeepEqual(validation.Stamp.Missing, []string{"section:Scope boundary"}) {
		t.Fatalf("missing = %#v, want section:Scope boundary", validation.Stamp.Missing)
	}
}
