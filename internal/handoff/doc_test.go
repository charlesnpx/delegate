package handoff

import "testing"

func TestPackageRole(t *testing.T) {
	if packageRole != "handoff" {
		t.Fatalf("packageRole = %q, want %q", packageRole, "handoff")
	}
}
