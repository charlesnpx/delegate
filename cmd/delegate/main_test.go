package main

import "testing"

func TestVersionLine(t *testing.T) {
	const want = "delegate v0.0.0-dev"
	if got := versionLine(); got != want {
		t.Fatalf("versionLine() = %q, want %q", got, want)
	}
}
