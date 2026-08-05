package skills

import (
	"os"
	"strings"
	"testing"
)

func TestVersionMatchesRepoVersionFile(t *testing.T) {
	raw, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(raw))
	want := "v" + trimmed
	if Version != want {
		t.Fatalf("skills.Version = %q, want %q from VERSION file value %q", Version, want, trimmed)
	}
}
