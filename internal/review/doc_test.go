package review

import "testing"

func TestPackageRole(t *testing.T) {
	if packageRole != "review" {
		t.Fatalf("packageRole = %q, want %q", packageRole, "review")
	}
}
