package policy

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

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
		{name: "write", flags: Flags{Write: true}},
		{name: "strict", flags: Flags{StrictContract: true}},
		{name: "write_strict", flags: Flags{Write: true, StrictContract: true}},
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
	mutated.Shape = json.RawMessage(`{"firstLineEnum":["complete"]}`)
	_, err = registry.Register(DelegateReportContractName, mutated)
	var conflict engine.NameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("mutated register error = %v, want NameConflictError", err)
	}
}

func equalContractSpec(got, want engine.ContractSpec) bool {
	return got.Named == want.Named &&
		equalJSONRaw(got.JSONSchema, want.JSONSchema) &&
		equalJSONRaw(got.Shape, want.Shape)
}

// TestValidateShapeFailsClosedOnEmptyShape locks in that delegate — now the sole
// authoritative report validator — never treats a constraint-less/null/empty shape
// as "everything compliant".
func TestValidateShapeFailsClosedOnEmptyShape(t *testing.T) {
	for _, raw := range []string{"null", "{}", `{"firstLineEnum":[],"requiredSections":[]}`} {
		spec := engine.ContractSpec{Shape: json.RawMessage(raw)}
		if got, err := ValidateShape("arbitrary malformed output", spec); err == nil {
			t.Fatalf("ValidateShape(shape=%q) = %#v, nil error; want fail-closed error", raw, got)
		}
	}
}

// mustParseShape parses the opaque contract shape into delegate's local view.
func mustParseShape(t *testing.T, spec engine.ContractSpec) reportShape {
	t.Helper()
	shape, err := parseReportShape(spec)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

// equalJSONRaw compares two raw JSON variant fields semantically (ignoring
// formatting). agentbus now stores the contract shape as opaque bytes.
func equalJSONRaw(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var av, bv interface{}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func TestDelegateReportSpecShape(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.JSONSchema) != 0 || spec.Named != "" || len(spec.Shape) == 0 {
		t.Fatalf("spec variant = %#v, want shape only", spec)
	}
	shape, err := parseReportShape(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(shape.FirstLineEnum) == 0 || len(shape.RequiredSections) == 0 {
		t.Fatalf("bundled shape = %#v, want first line enum and sections", shape)
	}
	var bundled struct {
		Shape json.RawMessage `json:"shape"`
	}
	if err := json.Unmarshal(DelegateReportSpecJSON(), &bundled); err != nil {
		t.Fatal(err)
	}
	if !equalJSONRaw(spec.Shape, bundled.Shape) {
		t.Fatalf("shape spec = %s, want bundled shape %s", spec.Shape, bundled.Shape)
	}
}

func TestDigestBundleEqualsFile(t *testing.T) {
	if strings.TrimSpace(DelegateContractDigest()) == "" {
		t.Fatal("bundled digest is empty")
	}
	// The digest intentionally documents the required report shape in prose (the
	// first-line words and section headers) so delegated workers can satisfy the
	// contract; TestContractDigestStatesReportShape guards that direction. It must
	// still not leak the internal contract identifiers used by the validator.
	forbidden := []string{"firstLineEnum", "section:", "requiredSections"}
	for _, fragment := range forbidden {
		if strings.Contains(DelegateContractDigest(), fragment) {
			t.Fatalf("bundled digest contains internal contract identifier %q", fragment)
		}
	}
	file, err := os.ReadFile("digest/delegate-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	if DelegateContractDigest() != string(file) {
		t.Fatal("bundled digest does not match digest/delegate-contract.md")
	}
}

func TestDelegateReportFormatBlockRoundTrip(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Shape == nil {
		t.Fatal("delegate report spec has no shape")
	}
	report := canonicalReportFromSpec(t, spec)
	valid, err := engine.ValidateContract(report, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !valid.Valid {
		t.Fatalf("canonical report missing = %#v", valid.Missing)
	}

	block, err := DelegateReportFormatBlock()
	if err != nil {
		t.Fatal(err)
	}
	shape, err := parseReportShape(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range shape.FirstLineEnum {
		if !strings.Contains(block, "\n"+value+"\n") {
			t.Fatalf("format block missing first-line enum %q:\n%s", value, block)
		}
	}
	for _, section := range shape.RequiredSections {
		if !strings.Contains(block, "\n# "+section+"\n") {
			t.Fatalf("format block missing heading %q:\n%s", section, block)
		}
		if strings.Contains(block, section+":") {
			t.Fatalf("format block advertises colon label for %q:\n%s", section, block)
		}
	}

	firstSection := mustParseShape(t, spec).RequiredSections[0]
	for _, tc := range []struct {
		name        string
		text        string
		wantMissing string
	}{
		{
			name:        "bold_only_section",
			text:        strings.Replace(report, "# "+firstSection, "**"+firstSection+"**", 1),
			wantMissing: "section:" + firstSection,
		},
		{
			name:        "capitalized_first_line",
			text:        strings.Replace(report, mustParseShape(t, spec).FirstLineEnum[0], capitalizeASCII(mustParseShape(t, spec).FirstLineEnum[0]), 1),
			wantMissing: "firstLineEnum",
		},
		{
			name:        "decorated_first_line",
			text:        strings.Replace(report, mustParseShape(t, spec).FirstLineEnum[0], mustParseShape(t, spec).FirstLineEnum[0]+" - done", 1),
			wantMissing: "firstLineEnum",
		},
		{
			name:        "headings_inside_code_fence",
			text:        fencedReportFromSpec(t, spec),
			wantMissing: "section:" + mustParseShape(t, spec).RequiredSections[0],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Shape is validated client-side by delegate now (agentbus treats the
			// contract shape as opaque identity).
			got, err := ValidateShape(tc.text, spec)
			if err != nil {
				t.Fatal(err)
			}
			if got.Compliant || !containsString(got.Violations, tc.wantMissing) {
				t.Fatalf("validation violations = %#v, want %q", got.Violations, tc.wantMissing)
			}
		})
	}
}

func TestAppendReportFormatBlockPlacesGeneratedBlockLast(t *testing.T) {
	policy, err := ResolveTurnPolicy(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := AppendReportFormatBlock("do the task\n", policy)
	if err != nil {
		t.Fatal(err)
	}
	block, err := DelegateReportFormatBlock()
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := strings.TrimRight(block, "\n")
	if !strings.HasPrefix(got, "do the task\n\n") || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("prompt = %q, want task body followed by format block %q", got, wantSuffix)
	}
	if strings.Contains(got, DelegateContractDigest()) {
		t.Fatal("format append should not inline the prologue digest")
	}

	unchanged, err := AppendReportFormatBlock("json task", &engine.TurnPolicy{Contract: &engine.ContractSpec{JSONSchema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != "json task" {
		t.Fatalf("JSON Schema prompt = %q, want unchanged", unchanged)
	}
}

func TestValidateDelegateReportShapeMinimal(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatal(err)
	}
	report := canonicalReportFromSpec(t, spec)
	firstSection := mustParseShape(t, spec).RequiredSections[0]
	var fencedViolations []string
	for _, section := range mustParseShape(t, spec).RequiredSections {
		fencedViolations = append(fencedViolations, "section:"+section)
	}
	for _, tc := range []struct {
		name           string
		text           string
		wantCompliant  bool
		wantViolations []string
	}{
		{
			name:          "compliant",
			text:          report,
			wantCompliant: true,
		},
		{
			name:           "wrong_first_line",
			text:           strings.Replace(report, mustParseShape(t, spec).FirstLineEnum[0], "done", 1),
			wantViolations: []string{"firstLineEnum"},
		},
		{
			name:           "missing_section",
			text:           strings.Replace(report, "# "+firstSection, "# Different", 1),
			wantViolations: []string{"section:" + firstSection},
		},
		{
			name:           "bold_section_rejected",
			text:           strings.Replace(report, "# "+firstSection, "**"+firstSection+"**", 1),
			wantViolations: []string{"section:" + firstSection},
		},
		{
			name:           "headings_inside_code_fence",
			text:           fencedReportFromSpec(t, spec),
			wantViolations: fencedViolations,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateDelegateReportShape(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if got.Compliant != tc.wantCompliant || !reflect.DeepEqual(got.Violations, tc.wantViolations) {
				t.Fatalf("ValidateDelegateReportShape() = %#v, want compliant=%t violations=%#v", got, tc.wantCompliant, tc.wantViolations)
			}
		})
	}
}

func canonicalReportFromSpec(t *testing.T, spec engine.ContractSpec) string {
	t.Helper()
	if spec.Shape == nil || len(mustParseShape(t, spec).FirstLineEnum) == 0 {
		t.Fatalf("spec = %#v, want first-line shape", spec)
	}
	lines := []string{mustParseShape(t, spec).FirstLineEnum[0], ""}
	for _, section := range mustParseShape(t, spec).RequiredSections {
		lines = append(lines, "# "+section, "- observed: "+strings.ToLower(section)+" fixture.", "")
	}
	return strings.Join(lines, "\n")
}

func fencedReportFromSpec(t *testing.T, spec engine.ContractSpec) string {
	t.Helper()
	if spec.Shape == nil || len(mustParseShape(t, spec).FirstLineEnum) == 0 {
		t.Fatalf("spec = %#v, want first-line shape", spec)
	}
	lines := []string{mustParseShape(t, spec).FirstLineEnum[0], "", "```md"}
	for _, section := range mustParseShape(t, spec).RequiredSections {
		lines = append(lines, "# "+section, "- observed: fenced fixture.", "")
	}
	lines = append(lines, "```", "")
	return strings.Join(lines, "\n")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func capitalizeASCII(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func TestDelegateReportShapeValidatesCompliantAndMissingSection(t *testing.T) {
	policy, err := ResolveTurnPolicy(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	compliant := canonicalReportFromSpec(t, *policy.Contract)
	if strings.Contains(compliant, "inferred:") || strings.Contains(compliant, "assumed:") {
		t.Fatal("compliant fixture must exercise all-observed attestations")
	}
	// Report-shape validation is delegate-owned and client-side now.
	validation, err := ValidateShape(compliant, *policy.Contract)
	if err != nil {
		t.Fatalf("ValidateShape(compliant) error = %v", err)
	}
	if !validation.Compliant {
		t.Fatalf("compliant report violations = %#v, want compliant", validation.Violations)
	}

	sections := mustParseShape(t, *policy.Contract).RequiredSections
	lastSection := sections[len(sections)-1]
	missingScope := strings.Replace(compliant, "# "+lastSection, "# Scope omitted", 1)
	validation, err = ValidateShape(missingScope, *policy.Contract)
	if err != nil {
		t.Fatalf("ValidateShape(missing) error = %v", err)
	}
	if validation.Compliant {
		t.Fatal("missing-section report reported compliant, want noncompliant")
	}
	wantViolations := []string{"section:" + lastSection}
	if !reflect.DeepEqual(validation.Violations, wantViolations) {
		t.Fatalf("violations = %#v, want %v", validation.Violations, wantViolations)
	}
}
