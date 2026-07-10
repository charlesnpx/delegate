package policy

import "testing"

func TestPackageRole(t *testing.T) {
	if packageRole != "policy" {
		t.Fatalf("packageRole = %q, want %q", packageRole, "policy")
	}
}
