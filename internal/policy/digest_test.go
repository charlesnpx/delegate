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

func TestContractDigestOnlyPermitsPromptDesignatedOptionalProbes(t *testing.T) {
	digest := DelegateContractDigest()
	for _, want := range []string{
		"Denial of a required operation -> stop and return BLOCKED",
		"Never work around a required-work block with an alternate tool.",
		"Only an operation the governing task/prompt expressly calls optional or unnecessary may be skipped;",
		"never reclassify a denied operation.",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing required denial rule %q", want)
		}
	}
	if strings.Contains(digest, "A failed or denied optional/unnecessary probe is not blocking") {
		t.Fatal("digest permits workers to self-classify denied probes as optional")
	}
}
