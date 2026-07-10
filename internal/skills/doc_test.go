package skills

import "testing"

func TestPackageRole(t *testing.T) {
	if packageRole != "skills" {
		t.Fatalf("packageRole = %q, want %q", packageRole, "skills")
	}
}
