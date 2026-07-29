package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestAcknowledgedJobMetadataRecordsAgentbusStateRoot(t *testing.T) {
	tmp := t.TempDir()
	rootRaw := filepath.Join(tmp, "agentbus")
	if err := os.Mkdir(rootRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", rootRaw)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	root, err := canonicalizeAgentbusStateRoot("test root", rootRaw)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentbusClient{
		hello:  helloWithCapabilities(),
		result: client.JobResult{JobID: "job_metadata_root", State: engine.StateRunning},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()
	t.Setenv("AGENTBUS_STATE_ROOT", rootRaw)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt", "record root", "--background", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task code = %d, stderr = %q", code, stderr.String())
	}
	meta, found, err := loadJobMetadata("", "job_metadata_root")
	if err != nil || !found {
		t.Fatalf("load metadata found=%v err=%v", found, err)
	}
	if meta.AgentbusStateRoot != root {
		t.Fatalf("metadata AgentbusStateRoot = %q, want %q", meta.AgentbusStateRoot, root)
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
				ContractKind:      contractKindShape,
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
			metadata: &jobMetadata{JobID: "job_other_root", Kind: taskKind, ContractKind: contractKindShape, AgentbusStateRoot: "RECORDED"},
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
