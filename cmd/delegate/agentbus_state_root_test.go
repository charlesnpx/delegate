package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestResolveAgentbusStateRoot(t *testing.T) {
	t.Run("env set absolute canonicalizes", func(t *testing.T) {
		tmp := t.TempDir()
		actual := filepath.Join(tmp, "actual")
		if err := os.Mkdir(actual, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(tmp, "link")
		if err := os.Symlink(actual, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		got, err := resolveAgentbusStateRootFrom(func(key string) string {
			if key == "AGENTBUS_STATE_ROOT" {
				return link
			}
			return ""
		}, func() (string, error) {
			return filepath.Join(tmp, "home"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(actual)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("resolved root = %q, want canonical %q", got, want)
		}
	})

	t.Run("env relative is usage error", func(t *testing.T) {
		_, err := resolveAgentbusStateRootFrom(func(key string) string {
			if key == "AGENTBUS_STATE_ROOT" {
				return "relative-agentbus"
			}
			return ""
		}, func() (string, error) {
			return t.TempDir(), nil
		})
		var usageErr agentbusStateRootUsageError
		if !errors.As(err, &usageErr) {
			t.Fatalf("error = %v, want agentbusStateRootUsageError", err)
		}
		if usageErr.Name != "AGENTBUS_STATE_ROOT" || usageErr.Value != "relative-agentbus" {
			t.Fatalf("usage error = %#v, want AGENTBUS_STATE_ROOT relative-agentbus", usageErr)
		}
	})

	t.Run("env unset uses default", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := resolveAgentbusStateRootFrom(func(string) string { return "" }, func() (string, error) {
			return home, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want, err := canonicalizeAgentbusStateRoot("test default", filepath.Join(home, ".local", "state", "agentbus"))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("resolved root = %q, want default %q", got, want)
		}
	})
}

func TestConnectAgentbusCommandPassesResolvedStateRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "agentbus")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))

	oldConnect := connectAgentbus
	oldLookPath := lookPath
	oldCommandOutput := commandOutput
	var captured client.Options
	connectAgentbus = func(_ context.Context, opts client.Options) (agentbusClient, error) {
		captured = opts
		return &fakeAgentbusClient{hello: helloWithCapabilities()}, nil
	}
	lookPath = func(string) (string, error) {
		return "/tmp/agentbus", nil
	}
	commandOutput = func(string, ...string) ([]byte, error) {
		return []byte("agentbus v0.6.0\n"), nil
	}
	t.Cleanup(func() {
		connectAgentbus = oldConnect
		lookPath = oldLookPath
		commandOutput = oldCommandOutput
	})

	c, _, resolved, err := connectAgentbusCommand(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if captured.StateRoot != resolved {
		t.Fatalf("client.Options.StateRoot = %q, want resolved root %q", captured.StateRoot, resolved)
	}
	wantRoot, err := canonicalizeAgentbusStateRoot("test root", root)
	if err != nil {
		t.Fatal(err)
	}
	if captured.StateRoot != wantRoot {
		t.Fatalf("client.Options.StateRoot = %q, want env root %q", captured.StateRoot, wantRoot)
	}
}

func TestStatusRoutingUsesRecordedRootForJobAndCurrentRootForAll(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		metadata   *jobMetadata
		wantRoot   func(currentRoot, recordedRoot string) string
		wantStatus client.JobStatusParams
	}{
		{
			name: "job recorded root",
			args: []string{"status", "--job", "job_recorded_root", "--json"},
			metadata: &jobMetadata{
				JobID:             "job_recorded_root",
				Kind:              taskKind,
				AgentbusStateRoot: "RECORDED",
			},
			wantRoot: func(_, recordedRoot string) string { return recordedRoot },
			wantStatus: client.JobStatusParams{
				JobID: "job_recorded_root",
			},
		},
		{
			name:     "job without metadata current root",
			args:     []string{"status", "--job", "job_current_root", "--json"},
			wantRoot: func(currentRoot, _ string) string { return currentRoot },
			wantStatus: client.JobStatusParams{
				JobID: "job_current_root",
			},
		},
		{
			name:     "all current root only",
			args:     []string{"status", "--json"},
			metadata: &jobMetadata{JobID: "job_other_root", Kind: taskKind, AgentbusStateRoot: "RECORDED"},
			wantRoot: func(currentRoot, _ string) string { return currentRoot },
			wantStatus: client.JobStatusParams{
				All: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			currentRootRaw := filepath.Join(tmp, "current-agentbus")
			recordedRootRaw := filepath.Join(tmp, "recorded-agentbus")
			for _, root := range []string{currentRootRaw, recordedRootRaw} {
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			currentRoot, err := canonicalizeAgentbusStateRoot("current root", currentRootRaw)
			if err != nil {
				t.Fatal(err)
			}
			recordedRoot, err := canonicalizeAgentbusStateRoot("recorded root", recordedRootRaw)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTBUS_STATE_ROOT", currentRootRaw)
			t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
			if tc.metadata != nil {
				meta := *tc.metadata
				if meta.AgentbusStateRoot == "RECORDED" {
					meta.AgentbusStateRoot = recordedRootRaw
				}
				if err := saveJobMetadata("", meta); err != nil {
					t.Fatal(err)
				}
			}

			fake := &fakeAgentbusClient{
				hello: helloWithCapabilities(),
				status: client.JobStatusResult{Jobs: []client.JobStatus{{
					JobID: tc.wantStatus.JobID,
					State: engine.StateRunning,
				}}},
			}
			oldConnect := connectAgentbus
			oldLookPath := lookPath
			oldCommandOutput := commandOutput
			var roots []string
			connectAgentbus = func(_ context.Context, opts client.Options) (agentbusClient, error) {
				roots = append(roots, opts.StateRoot)
				return fake, nil
			}
			lookPath = func(string) (string, error) {
				return "/tmp/agentbus", nil
			}
			commandOutput = func(string, ...string) ([]byte, error) {
				return []byte("agentbus v0.6.0\n"), nil
			}
			t.Cleanup(func() {
				connectAgentbus = oldConnect
				lookPath = oldLookPath
				commandOutput = oldCommandOutput
			})

			var stdout, stderr bytes.Buffer
			code := run(tc.args, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("status code = %d, stderr = %q", code, stderr.String())
			}
			if len(roots) != 1 {
				t.Fatalf("connection roots = %#v, want one root", roots)
			}
			if want := tc.wantRoot(currentRoot, recordedRoot); roots[0] != want {
				t.Fatalf("connection root = %q, want %q", roots[0], want)
			}
			if len(fake.statuses) != 1 || fake.statuses[0] != tc.wantStatus {
				t.Fatalf("status calls = %#v, want %#v", fake.statuses, tc.wantStatus)
			}
		})
	}
}

func TestAgentbusStateRootForJobUsesRecordedRootBeforeCurrentEnv(t *testing.T) {
	tmp := t.TempDir()
	recordedRootRaw := filepath.Join(tmp, "recorded-agentbus")
	if err := os.Mkdir(recordedRootRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", "relative-agentbus")
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	jobID := "job_recorded_root_invalid_current_env"
	if err := saveJobMetadata("", jobMetadata{
		JobID:             jobID,
		Kind:              taskKind,
		AgentbusStateRoot: recordedRootRaw,
	}); err != nil {
		t.Fatal(err)
	}

	want, err := canonicalizeAgentbusStateRoot("recorded root", recordedRootRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                     string
		allowCorruptRootFallback bool
	}{
		{name: "read fallback allowed", allowCorruptRootFallback: true},
		{name: "cancel strict", allowCorruptRootFallback: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agentbusStateRootForJob("", jobID, nil, tc.allowCorruptRootFallback)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("agentbus root=%q, want recorded root %q", got, want)
			}
		})
	}
}

func TestAgentbusStateRootForJobFallsBackToCurrentEnvWithoutRecordedRoot(t *testing.T) {
	t.Setenv("AGENTBUS_STATE_ROOT", "relative-agentbus")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, tc := range []struct {
		name                     string
		allowCorruptRootFallback bool
	}{
		{name: "read fallback allowed", allowCorruptRootFallback: true},
		{name: "cancel strict", allowCorruptRootFallback: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := agentbusStateRootForJob("", "job_without_recorded_root", nil, tc.allowCorruptRootFallback)
			var usageErr agentbusStateRootUsageError
			if !errors.As(err, &usageErr) {
				t.Fatalf("agentbusStateRootForJob error=%v, want current env usage error", err)
			}
			if usageErr.Name != "AGENTBUS_STATE_ROOT" || usageErr.Value != "relative-agentbus" {
				t.Fatalf("usage error=%#v, want AGENTBUS_STATE_ROOT relative-agentbus", usageErr)
			}
		})
	}
}

func TestAgentbusStateRootForJobCorruptMetadataFallsBackWithWarning(t *testing.T) {
	tmp, defaultRoot := agentbusStateRootFallbackTestEnv(t)
	stateDir := filepath.Join(tmp, "delegate-state")
	jobID := "job_corrupt_metadata"
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	got, err := agentbusStateRootForJob(stateDir, jobID, &stderr, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultRoot {
		t.Fatalf("agentbus root=%q, want default root %q", got, defaultRoot)
	}
	assertRecordedRootFallbackWarning(t, stderr.String(), jobID, "unexpected end of JSON input")
}

func TestAgentbusStateRootForJobCorruptMetadataStrictReturnsError(t *testing.T) {
	tmp, defaultRoot := agentbusStateRootFallbackTestEnv(t)
	stateDir := filepath.Join(tmp, "delegate-state")
	jobID := "job_corrupt_metadata"
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, encodedStateFilename(jobID)), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	got, err := agentbusStateRootForJob(stateDir, jobID, &stderr, false)
	if err == nil {
		t.Fatal("agentbusStateRootForJob error=nil, want corrupt metadata error")
	}
	if got != "" || got == defaultRoot {
		t.Fatalf("agentbus root=%q, want no fallback to default root %q", got, defaultRoot)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("use recorded AgentBus state root for job %q", jobID)) {
		t.Fatalf("error=%v, want recorded root context", err)
	}
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("error=%v, want corrupt JSON detail", err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr=%q, want no fallback warning", stderr.String())
	}
}

func TestAgentbusStateRootForJobUncanonicalizableRecordedRootFallsBackWithWarning(t *testing.T) {
	tmp, defaultRoot := agentbusStateRootFallbackTestEnv(t)
	stateDir := filepath.Join(tmp, "delegate-state")
	jobID := "job_relative_recorded_root"
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:             jobID,
		Kind:              taskKind,
		AgentbusStateRoot: "relative-agentbus",
	}); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	got, err := agentbusStateRootForJob(stateDir, jobID, &stderr, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultRoot {
		t.Fatalf("agentbus root=%q, want default root %q", got, defaultRoot)
	}
	assertRecordedRootFallbackWarning(t, stderr.String(), jobID, "must be absolute")
}

func TestAgentbusStateRootForJobValidRecordedRootReturnsRecordedWithoutWarning(t *testing.T) {
	tmp, _ := agentbusStateRootFallbackTestEnv(t)
	stateDir := filepath.Join(tmp, "delegate-state")
	recordedRootRaw := filepath.Join(tmp, "recorded-agentbus")
	if err := os.Mkdir(recordedRootRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	recordedRoot, err := canonicalizeAgentbusStateRoot("recorded root", recordedRootRaw)
	if err != nil {
		t.Fatal(err)
	}
	jobID := "job_valid_recorded_root"
	if err := saveJobMetadata(stateDir, jobMetadata{
		JobID:             jobID,
		Kind:              taskKind,
		AgentbusStateRoot: recordedRootRaw,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name                     string
		allowCorruptRootFallback bool
	}{
		{name: "read fallback allowed", allowCorruptRootFallback: true},
		{name: "cancel strict", allowCorruptRootFallback: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got, err := agentbusStateRootForJob(stateDir, jobID, &stderr, tc.allowCorruptRootFallback)
			if err != nil {
				t.Fatal(err)
			}
			if got != recordedRoot {
				t.Fatalf("agentbus root=%q, want recorded root %q", got, recordedRoot)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr=%q, want no warning", stderr.String())
			}
		})
	}
}

func TestAgentbusStateRootForJobFallsBackForAbsentMetadataAndInvalidJobID(t *testing.T) {
	tmp, defaultRoot := agentbusStateRootFallbackTestEnv(t)
	stateDir := filepath.Join(tmp, "delegate-state")
	for _, tc := range []struct {
		name  string
		jobID string
	}{
		{name: "absent metadata", jobID: "job_without_metadata"},
		{name: "invalid empty job id", jobID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []struct {
				name                     string
				allowCorruptRootFallback bool
			}{
				{name: "read fallback allowed", allowCorruptRootFallback: true},
				{name: "cancel strict", allowCorruptRootFallback: false},
			} {
				t.Run(mode.name, func(t *testing.T) {
					var stderr bytes.Buffer
					got, err := agentbusStateRootForJob(stateDir, tc.jobID, &stderr, mode.allowCorruptRootFallback)
					if err != nil {
						t.Fatal(err)
					}
					if got != defaultRoot {
						t.Fatalf("agentbus root=%q, want default root %q", got, defaultRoot)
					}
					if stderr.String() != "" {
						t.Fatalf("stderr=%q, want no warning", stderr.String())
					}
				})
			}
		})
	}
}

func agentbusStateRootFallbackTestEnv(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	defaultRootRaw := filepath.Join(tmp, "default-agentbus")
	if err := os.Mkdir(defaultRootRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", defaultRootRaw)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	defaultRoot, err := resolveAgentbusStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	return tmp, defaultRoot
}

func assertRecordedRootFallbackWarning(t *testing.T, warning, jobID, wantDetail string) {
	t.Helper()
	wantPrefix := fmt.Sprintf("warning: delegate could not use recorded AgentBus state root for job %q; using default resolved AgentBus state root instead, so status/result may report a different same-ID job or not find the intended job: ", jobID)
	if !strings.HasPrefix(warning, wantPrefix) {
		t.Fatalf("warning=%q, want prefix %q", warning, wantPrefix)
	}
	if !strings.Contains(warning, wantDetail) {
		t.Fatalf("warning=%q, want detail %q", warning, wantDetail)
	}
	if strings.Count(warning, "\n") != 1 || !strings.HasSuffix(warning, "\n") {
		t.Fatalf("warning=%q, want one line ending with newline", warning)
	}
}
