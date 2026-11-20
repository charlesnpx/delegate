package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestTaskIntentAndTimeoutWirePresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		intent  string
		timeout string
		want    string
	}{
		{"default", "", "", ""},
		{"inspect zero", "inspect", "0", "0"},
		{"edit zero duration", "edit", "0s", "0"},
		{"edit positive", "edit", "1500ms", "1500"},
		{"fractional components", "inspect", "500us500us", "1"},
		{"subnanosecond components", "inspect", "0.1ns999999.9ns", "1"},
		{"maximum", "inspect", "4h", "14400000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentbusClient{hello: helloWithBackends()}
			defer stubAgentbusGlobals(t, fake)()
			args := []string{"task", "--backend", "codex", "--cwd", t.TempDir(), "--prompt-file", "-"}
			if tc.intent != "" {
				args = append(args, "--intent", tc.intent)
			}
			if tc.timeout != "" {
				args = append(args, "--timeout", tc.timeout)
			}
			var out, stderr bytes.Buffer
			if code := run(args, strings.NewReader("inspect or edit"), &out, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			spec := fake.submits[0].TaskSpec
			wantIntent := engine.Intent(tc.intent)
			if wantIntent == "" {
				wantIntent = engine.IntentInspect
			}
			if spec.Intent != wantIntent {
				t.Fatalf("intent=%q want %q", spec.Intent, wantIntent)
			}
			assertTimeoutWire(t, spec, tc.want)
			var receipt taskSubmitReceipt
			if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Intent != wantIntent || receipt.Backend != "codex" || receipt.Review != nil {
				t.Fatalf("receipt=%#v", receipt)
			}
		})
	}
}

func assertTimeoutWire(t *testing.T, spec client.TaskSpec, want string) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	got, present := fields["timeoutMs"]
	if (want != "") != present || (present && string(got) != want) {
		t.Fatalf("TaskSpec JSON=%s: timeoutMs=%s present=%t, want %q", raw, got, present, want)
	}
}

func TestReviewTimeoutWirePresenceAndInspectIntent(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	defer stubAgentbusGlobals(t, fake)()
	repo := newReviewCommandRepository(t)
	fixture := newContractReviewFixture(t)
	for _, command := range []string{"review", "adversarial-review"} {
		for _, mode := range []string{"direct", "contract"} {
			for _, tc := range []struct{ duration, want string }{{"", ""}, {"0", "0"}, {"0s", "0"}, {"45s", "45000"}, {"4h", "14400000"}} {
				t.Run(command+"/"+mode+"/"+tc.duration, func(t *testing.T) {
					args := []string{command, "--backend", "codex", "--cwd", repo}
					if mode == "direct" {
						args = append(args, "--scope", "working-tree")
					} else {
						args = append(args, "--request-file", fixture.requestPath, "--artifact-file", fixture.artifactPath, "--charter-file", fixture.charterPath)
					}
					if tc.duration != "" {
						args = append(args, "--timeout", tc.duration)
					}
					var out, stderr bytes.Buffer
					if code := run(args, nil, &out, &stderr); code != 0 {
						t.Fatalf("code=%d stderr=%q", code, stderr.String())
					}
					spec := fake.submits[len(fake.submits)-1].TaskSpec
					if spec.Intent != engine.IntentInspect || spec.InputMode != taskInputModeFixed {
						t.Fatalf("intent/input=%q/%q", spec.Intent, spec.InputMode)
					}
					assertTimeoutWire(t, spec, tc.want)
				})
			}
		}
	}
}

func TestTaskAndReviewRejectInvalidTimeoutsAndIntent(t *testing.T) {
	for _, value := range []string{"-1s", "-0s", "1ns", "0.0001ns", "999us", "1.5ms", "1.0000000001ms", "4h1ms", "2400"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseTaskOptions([]string{"--backend", "codex", "--prompt-file", "-", "--timeout", value}, &bytes.Buffer{}); err == nil {
				t.Fatalf("task timeout %q accepted", value)
			}
			for _, kind := range []string{reviewKind, adversarialReviewKind} {
				if _, err := parseReviewOptions(kind, []string{"--backend", "codex", "--timeout", value}, &bytes.Buffer{}); err == nil {
					t.Fatalf("%s timeout %q accepted", kind, value)
				}
			}
		})
	}
	for _, intent := range []string{"", "write", "Inspect"} {
		if _, err := parseTaskOptions([]string{"--backend", "codex", "--prompt-file", "-", "--intent", intent}, &bytes.Buffer{}); err == nil {
			t.Fatalf("intent %q accepted", intent)
		}
	}
	for _, kind := range []string{reviewKind, adversarialReviewKind} {
		if _, err := parseReviewOptions(kind, []string{"--backend", "codex", "--intent", "edit"}, &bytes.Buffer{}); err == nil {
			t.Fatalf("%s accepted edit intent", kind)
		}
	}
}

func TestTaskAndReviewRejectExplicitEmptyCWDAndSelections(t *testing.T) {
	for _, name := range []string{"cwd", "model", "effort"} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTaskOptions([]string{"--backend", "codex", "--prompt-file", "-", "--" + name, ""}, &bytes.Buffer{}); err == nil {
				t.Fatalf("task accepted empty --%s", name)
			}
			if _, err := parseReviewOptions(reviewKind, []string{"--backend", "codex", "--" + name, ""}, &bytes.Buffer{}); err == nil {
				t.Fatalf("review accepted empty --%s", name)
			}
		})
	}
}

func TestTaskHelpDescribesHostExecution(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "--help"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, fragment := range []string{"host access", "not an enforced permission boundary", "build/test", "-intent", "default 2h", "unlimited"} {
		if !strings.Contains(stderr.String(), fragment) {
			t.Fatalf("help=%q missing %q", stderr.String(), fragment)
		}
	}
}

func TestAgentbusConnectionConfiguration(t *testing.T) {
	stubAgentbusTestEnvironment(t)
	oldConnect, oldLookPath := connectAgentbus, lookPath
	defer func() { connectAgentbus, lookPath = oldConnect, oldLookPath }()
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	var seen []client.Options
	lookups := 0
	lookPath = func(string) (string, error) { lookups++; return "/selected/agentbus", nil }
	connectAgentbus = func(_ context.Context, opts client.Options) (agentbusClient, error) {
		seen = append(seen, opts)
		return fake, nil
	}
	for _, value := range []string{"", "0", "1"} {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			t.Setenv("AGENTBUS_NO_AUTOSTART", value)
			ctx := withAgentbusSelection(context.Background())
			before := lookups
			for i := 0; i < 2; i++ {
				if _, _, _, err := connectAgentbusCommand(ctx); err != nil {
					t.Fatal(err)
				}
				got := seen[len(seen)-1]
				if got.DisableAutoStart != (value == "1") || got.CommandPath != "/selected/agentbus" || got.Release != agentbusClientRelease {
					t.Fatalf("options=%#v", got)
				}
			}
			if lookups-before != 1 {
				t.Fatalf("lookups=%d, want 1", lookups-before)
			}
		})
	}
	t.Setenv("AGENTBUS_NO_AUTOSTART", "true")
	if _, _, _, err := connectAgentbusCommand(withAgentbusSelection(context.Background())); err == nil || !strings.Contains(err.Error(), "AGENTBUS_NO_AUTOSTART") {
		t.Fatalf("invalid value error=%v", err)
	}
}

func TestAgentbusRootResolutionAndNoAutostart(t *testing.T) {
	stubAgentbusTestEnvironment(t)
	oldConnect, oldLookPath := connectAgentbus, lookPath
	defer func() { connectAgentbus, lookPath = oldConnect, oldLookPath }()
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	var selectedRoot string
	lookPath = func(string) (string, error) { return "/selected/agentbus", nil }
	connectAgentbus = func(_ context.Context, opts client.Options) (agentbusClient, error) {
		selectedRoot = opts.StateRoot
		return fake, nil
	}
	xdg := os.Getenv("XDG_STATE_HOME")
	_, _, root, err := connectAgentbusCommand(withAgentbusSelection(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	absoluteXDG, err := filepath.Abs(xdg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(absoluteXDG, "agentbus", "v2"); root != want || selectedRoot != want {
		t.Fatalf("roots=%q/%q want %q", root, selectedRoot, want)
	}
	explicit := filepath.Join(absoluteXDG, "exact-root")
	t.Setenv("AGENTBUS_STATE_ROOT", explicit)
	_, _, root, err = connectAgentbusCommand(withAgentbusSelection(context.Background()))
	if err != nil || root != explicit || selectedRoot != explicit {
		t.Fatalf("roots=%q/%q err=%v", root, selectedRoot, err)
	}

	target := t.TempDir()
	alias := filepath.Join(t.TempDir(), "selected-root")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_STATE_ROOT", alias)
	_, _, root, err = connectAgentbusCommand(withAgentbusSelection(context.Background()))
	if err != nil || root != alias || selectedRoot != alias {
		t.Fatalf("symlink roots=%q/%q want %q, err=%v", root, selectedRoot, alias, err)
	}

	// Use the real client against an absent socket and a sentinel executable.
	marker := filepath.Join(t.TempDir(), "started")
	executable := filepath.Join(t.TempDir(), "agentbus")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch \"$AGENTBUS_TEST_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_TEST_MARKER", marker)
	t.Setenv("AGENTBUS_NO_AUTOSTART", "1")
	lookPath = func(string) (string, error) { return executable, nil }
	connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
		return client.Connect(ctx, opts)
	}
	if _, _, _, err := connectAgentbusCommand(withAgentbusSelection(context.Background())); err == nil {
		t.Fatal("absent daemon unexpectedly connected")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("autostart marker exists or stat failed: %v", err)
	}
}

func TestAgentbusMissingExecutableDoesNotFallBack(t *testing.T) {
	stubAgentbusTestEnvironment(t)
	oldConnect, oldLookPath := connectAgentbus, lookPath
	defer func() { connectAgentbus, lookPath = oldConnect, oldLookPath }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
		if opts.Starter == nil {
			t.Fatal("missing executable left default starter enabled")
		}
		_, err := opts.Starter(ctx, client.StartOptions{})
		return nil, err
	}
	_, _, _, err := connectAgentbusCommand(withAgentbusSelection(context.Background()))
	if err == nil || !strings.Contains(err.Error(), "resolve agentbus executable on PATH") {
		t.Fatalf("error=%v", err)
	}
}

func TestAgentbusLibraryReleaseFromBuildInfo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		replace *debug.Module
		want    string
	}{
		{"release", "v0.18.0", nil, "0.18.0"},
		{"local replacement", "v0.18.0", &debug.Module{Path: "/candidate"}, "unknown"},
		{"versioned replacement", "v0.18.0", &debug.Module{Path: agentbusModulePath, Version: "v0.18.1"}, "0.18.1"},
		{"snapshot", "v0.18.1-0.20261119000000-abcdefabcdef", nil, "unknown"},
		{"development", "(devel)", nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{Deps: []*debug.Module{{Path: agentbusModulePath, Version: tc.version, Replace: tc.replace}}}
			if got := agentbusReleaseFromBuildInfo(info); got != tc.want {
				t.Fatalf("release=%q, want %q", got, tc.want)
			}
		})
	}
	if got := agentbusReleaseFromBuildInfo(nil); got != "unknown" {
		t.Fatalf("missing build info release=%q", got)
	}
}

func TestAgentbusReleaseNoticeUsesAgentbusVersions(t *testing.T) {
	oldRelease, oldVersion := agentbusClientRelease, Version
	defer func() { agentbusClientRelease, Version = oldRelease, oldVersion }()
	Version = "0.15.0"
	for _, tc := range []struct {
		client, daemon string
		different      bool
	}{
		{"0.18.0", "0.18.0", false},
		{"0.18.0", "0.18.1", true},
		{"unknown", "0.18.0", false},
	} {
		agentbusClientRelease = tc.client
		var notice bytes.Buffer
		err := writeAgentbusNotice(&notice, client.HelloResult{ProtocolVersion: 4, DaemonRelease: tc.daemon, DaemonExecutable: "/daemon/agentbus"})
		if err != nil {
			t.Fatal(err)
		}
		text := notice.String()
		for _, fragment := range []string{tc.client, tc.daemon, "protocol 4", "/daemon/agentbus"} {
			if !strings.Contains(text, fragment) {
				t.Fatalf("notice=%q missing %q", text, fragment)
			}
		}
		if strings.Contains(text, "releases differ") != tc.different || strings.Contains(text, "0.15.0") {
			t.Fatalf("notice=%q, mismatches=%t", text, tc.different)
		}
	}
}
