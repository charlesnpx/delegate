package policy

import (
	"strings"
	"testing"
)

// TestContractDigestStatesReportShape guards against the digest drifting out of
// sync with the shape ValidateShape actually enforces. The always-on digest is
// the surface delegated workers read to learn the report format; if it omits
// the first-line enum or any required section header, workers cannot satisfy
// the contract and every report is scored noncompliant by construction.
func TestContractDigestStatesReportShape(t *testing.T) {
	spec, err := DelegateReportSpec()
	if err != nil {
		t.Fatalf("DelegateReportSpec: %v", err)
	}
	shape, err := parseReportShape(spec)
	if err != nil {
		t.Fatalf("parseReportShape: %v", err)
	}
	digest := DelegateContractDigest()
	if strings.TrimSpace(digest) == "" {
		t.Fatal("DelegateContractDigest is empty")
	}
	for _, status := range shape.FirstLineEnum {
		if !strings.Contains(digest, status) {
			t.Errorf("digest does not mention first-line status word %q", status)
		}
	}
	for _, section := range shape.RequiredSections {
		if !strings.Contains(digest, section) {
			t.Errorf("digest does not mention required section header %q", section)
		}
	}
}

func TestContractDigestDoesNotLetTaskOptionalityOverrideRequiredOperations(t *testing.T) {
	rule := rule11Text(DelegateContractDigest())
	if !rule11StatesIndependentRequirementBlocks(rule) {
		t.Error("rule 11 must say that a denial blocks when an operation remains independently required despite task optionality")
	}
	if rule11AdmitsTaskOverride(rule) {
		t.Error("rule 11 must not let task-declared optionality permit an independently required operation to be skipped or reclassified")
	}
	for _, override := range []string{
		"A task may pre-classify independently required operations as optional.",
		"A prompt can waive a separately mandated operation.",
		"Task instructions are allowed to supersede other required work.",
	} {
		if !rule11AdmitsTaskOverride(rule + " " + override) {
			t.Errorf("rule 11 guard did not reject permissive override %q", override)
		}
	}

	paraphrase := "A task may label an operation optional only when no separate rule makes it mandatory; if it is mandatory and denied, return BLOCKED."
	if !rule11StatesIndependentRequirementBlocks(paraphrase) || rule11AdmitsTaskOverride(paraphrase) {
		t.Error("rule 11 guard rejected an equivalent paraphrase")
	}
}

// rule11Text returns rule 11, including wrapped continuation lines.
func rule11Text(digest string) string {
	var rule []string
	found := false
	for _, line := range strings.Split(digest, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "11." || strings.HasPrefix(line, "11. "):
			found = true
			rule = append(rule, strings.TrimSpace(strings.TrimPrefix(line, "11.")))
		case found && (line == "12." || strings.HasPrefix(line, "12. ")):
			return strings.Join(rule, " ")
		case found:
			rule = append(rule, line)
		}
	}
	return strings.Join(rule, " ")
}

func rule11StatesIndependentRequirementBlocks(rule string) bool {
	rule = strings.ToLower(rule)
	return hasTaskOptionality(rule) &&
		hasRequiredDenialBlock(rule) &&
		hasIndependentRequirementProtection(rule)
}

// rule11AdmitsTaskOverride flags an unqualified authorization for a task or
// prompt to make work optional. It is intentionally a focused policy-text
// check, not an English parser: such authorization is safe only when its own
// clause protects independently required work.
func rule11AdmitsTaskOverride(rule string) bool {
	for _, clause := range strings.FieldsFunc(strings.ToLower(rule), func(r rune) bool {
		return r == '.' || r == ';' || r == ':'
	}) {
		if hasTaskDeclaration(clause) &&
			hasOptionalityOrOverride(clause) &&
			hasOverrideAuthorization(clause) &&
			!hasIndependentRequirementProtection(clause) {
			return true
		}
	}
	return false
}

func hasTaskOptionality(text string) bool {
	return hasTaskDeclaration(text) &&
		containsAny(text, "optional", "unnecessary", "skippable", "omit")
}

func hasTaskDeclaration(text string) bool {
	return containsAny(text, "task", "prompt", "instruction")
}

func hasIndependentRequirement(text string) bool {
	return containsAny(text, "independent", "other rule", "another rule", "different rule", "separate rule", "separately", "elsewhere") &&
		containsAny(text, "required", "require", "mandatory", "mandat", "oblig", "must")
}

func hasRequiredDenialBlock(text string) bool {
	return containsAny(text, "denial", "denied", "deny") &&
		containsAny(text, "block", "stop", "return blocked") &&
		containsAny(text, "required", "require", "mandatory", "mandat", "oblig", "must")
}

func hasIndependentRequirementProtection(text string) bool {
	return hasIndependentRequirement(text) &&
		(containsAny(text, "cannot", "can't", "must not", "may not", "never", "does not", "doesn't") ||
			(containsAny(text, "only", "unless", "except") && containsAny(text, "no ", "not ", "without ")) ||
			(containsAny(text, "despite", "even if") && containsAny(text, "remain", "stays", "stay", "still")))
}

func hasOptionalityOrOverride(text string) bool {
	return containsAny(text, "optional", "unnecessary", "skippable", "omit", "reclassif", "classif", "override", "supersed", "waive", "skip")
}

func hasOverrideAuthorization(text string) bool {
	return containsAny(text,
		" may ", " can ", "allowed", "permit", "authoriz", "enable", "excuse",
		"reclassif", "pre-classif", "classif", "override", "supersed", "waive", "skip")
}

func containsAny(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}
