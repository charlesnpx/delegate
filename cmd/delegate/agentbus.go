package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

type agentbusClient interface {
	Close() error
	HelloResult() client.HelloResult
	JobSubmit(context.Context, client.JobSubmitParams) (client.JobSubmitResult, error)
	JobGet(context.Context, client.JobGetParams) (client.JobGetResult, error)
}

// validateBackend checks only that Agentbus advertised the selected backend.
// Model and effort are caller values that Agentbus/the backend owns.
func validateBackend(hello client.HelloResult, backend string) error {
	available := make([]string, 0, len(hello.BackendMetadata))
	for _, info := range hello.BackendMetadata {
		available = append(available, info.Name)
	}
	sort.Strings(available)
	for _, name := range available {
		if name == backend {
			return nil
		}
	}
	return fmt.Errorf("unknown backend %q; available backends: %s", backend, strings.Join(available, ", "))
}

var connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
	return client.Connect(ctx, opts)
}

var lookPath = exec.LookPath

const agentbusModulePath = "github.com/charlesnpx/agentbus"

var agentbusClientRelease = detectedAgentbusRelease()

func detectedAgentbusRelease() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return agentbusReleaseFromBuildInfo(info)
}

func agentbusReleaseFromBuildInfo(info *debug.BuildInfo) string {
	if info != nil {
		for _, dependency := range info.Deps {
			if dependency.Path != agentbusModulePath {
				continue
			}
			if dependency.Replace != nil {
				dependency = dependency.Replace
			}
			release := normalizeAgentbusRelease(dependency.Version)
			// A development snapshot or a local replacement does not identify a
			// released Agentbus build, even if go.mod requests a release tag.
			if strings.Contains(release, "-") {
				return "unknown"
			}
			return release
		}
	}
	return "unknown"
}

func normalizeAgentbusRelease(release string) string {
	release = strings.TrimSpace(release)
	if release == "" || release == "dev" || release == "(devel)" || strings.HasSuffix(release, "-dev") {
		return "unknown"
	}
	return strings.TrimPrefix(release, "v")
}

func writeAgentbusNotice(w io.Writer, hello client.HelloResult) error {
	clientRelease := normalizeAgentbusRelease(agentbusClientRelease)
	daemonRelease := normalizeAgentbusRelease(hello.DaemonRelease)
	executable := hello.DaemonExecutable
	if executable == "" {
		executable = "unknown"
	}
	difference := ""
	if clientRelease != "unknown" && daemonRelease != "unknown" && clientRelease != daemonRelease {
		difference = "; releases differ"
	}
	_, err := fmt.Fprintf(w, "notice: connected agentbus daemon release %s, protocol %d, executable %s; client library release %s%s\n", daemonRelease, hello.ProtocolVersion, executable, clientRelease, difference)
	return err
}

type agentbusSelectionKey struct{}

type agentbusSelection struct {
	once sync.Once
	path string
	err  error
}

func withAgentbusSelection(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentbusSelectionKey{}, &agentbusSelection{})
}

func selectedAgentbus(ctx context.Context) (string, error) {
	selection, _ := ctx.Value(agentbusSelectionKey{}).(*agentbusSelection)
	if selection == nil {
		selection = &agentbusSelection{}
	}
	selection.once.Do(func() { selection.path, selection.err = lookPath("agentbus") })
	return selection.path, selection.err
}

func connectAgentbusCommand(ctx context.Context) (agentbusClient, client.HelloResult, string, error) {
	stateRoot, err := engine.ResolveStateRoot()
	if err == nil {
		stateRoot, err = absoluteAgentbusStateRoot("agentbus state root", stateRoot)
	}
	if err != nil {
		return nil, client.HelloResult{}, "", err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, stateRoot)
	return c, hello, stateRoot, err
}

func connectAgentbusCommandAtRoot(ctx context.Context, stateRoot string) (agentbusClient, client.HelloResult, error) {
	stateRoot, err := absoluteAgentbusStateRoot("agentbus state root", stateRoot)
	if err != nil {
		return nil, client.HelloResult{}, err
	}
	var disableAutoStart bool
	switch value := os.Getenv("AGENTBUS_NO_AUTOSTART"); value {
	case "", "0":
	case "1":
		disableAutoStart = true
	default:
		return nil, client.HelloResult{}, fmt.Errorf("AGENTBUS_NO_AUTOSTART must be 0, 1, or empty; got %q", value)
	}
	path, lookupErr := selectedAgentbus(ctx)
	opts := client.Options{Release: agentbusClientRelease, StateRoot: stateRoot, CommandPath: path, DisableAutoStart: disableAutoStart}
	if lookupErr != nil {
		// A running daemon may still be reachable. If it is not, fail the
		// selected startup instead of letting the client search PATH again.
		opts.Starter = func(context.Context, client.StartOptions) (client.StartResult, error) {
			return client.StartResult{}, fmt.Errorf("resolve agentbus executable on PATH for autostart: %w", lookupErr)
		}
	}
	c, err := connectAgentbus(ctx, opts)
	if err != nil {
		if path != "" && !disableAutoStart {
			return nil, client.HelloResult{}, fmt.Errorf("connect to agentbus (autostart executable %q): %w", path, err)
		}
		return nil, client.HelloResult{}, err
	}
	return c, c.HelloResult(), nil
}

func absoluteAgentbusStateRoot(label, root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	// Preserve symlinks: the selected path also names the daemon socket, and
	// resolving it may select a different (or too-long) socket pathname.
	return abs, nil
}
