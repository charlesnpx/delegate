package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/agentbus/client"
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

func connectCheckedAgentbus(ctx context.Context, opts client.Options) (agentbusClient, client.HelloResult, error) {
	c, err := connectAgentbus(ctx, opts)
	if err != nil {
		return nil, client.HelloResult{}, err
	}
	return c, c.HelloResult(), nil
}

func connectAgentbusCommand(ctx context.Context) (agentbusClient, client.HelloResult, string, error) {
	stateRoot, err := resolveAgentbusStateRoot()
	if err != nil {
		return nil, client.HelloResult{}, "", err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, stateRoot)
	return c, hello, stateRoot, err
}

func connectAgentbusCommandAtRoot(ctx context.Context, stateRoot string) (agentbusClient, client.HelloResult, error) {
	stateRoot, err := canonicalizeAgentbusStateRoot("agentbus state root", stateRoot)
	if err != nil {
		return nil, client.HelloResult{}, err
	}
	path, _ := lookPath("agentbus")
	opts := client.Options{StateRoot: stateRoot}
	if path != "" {
		opts.CommandPath = path
	}
	return connectCheckedAgentbus(ctx, opts)
}

type agentbusStateRootUsageError struct {
	Name  string
	Value string
}

func (err agentbusStateRootUsageError) Error() string {
	return fmt.Sprintf("%s %q must be absolute", err.Name, err.Value)
}

func resolveAgentbusStateRoot() (string, error) {
	return resolveAgentbusStateRootFrom(os.Getenv, os.UserHomeDir)
}

func resolveAgentbusUserCacheRootFrom(userCacheDir func() (string, error)) (string, error) {
	cacheDir, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir for Agentbus autostart locks: %w", err)
	}
	if cacheDir == "" {
		return "", errors.New("user cache directory is empty")
	}
	return canonicalizeAgentbusStateRoot("user cache directory", filepath.Join(cacheDir, "agentbus"))
}

func resolveAgentbusStateRootFrom(env func(string) string, userHomeDir func() (string, error)) (string, error) {
	if root := env("AGENTBUS_STATE_ROOT"); root != "" {
		return canonicalizeAgentbusStateRoot("AGENTBUS_STATE_ROOT", root)
	}
	if stateHome := env("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", agentbusStateRootUsageError{Name: "XDG_STATE_HOME", Value: stateHome}
		}
		return canonicalizeAgentbusStateRoot("agentbus state root", filepath.Join(stateHome, "agentbus"))
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("home directory is empty")
	}
	return canonicalizeAgentbusStateRoot("agentbus state root", filepath.Join(home, ".local", "state", "agentbus"))
}

func canonicalizeAgentbusStateRoot(label, root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if !filepath.IsAbs(root) {
		return "", agentbusStateRootUsageError{Name: label, Value: root}
	}
	clean := filepath.Clean(root)
	if evaluated, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(evaluated), nil
	}
	return evalSymlinksAsFeasible(clean), nil
}

func evalSymlinksAsFeasible(path string) string {
	missing := []string{}
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			if evaluated, evalErr := filepath.EvalSymlinks(current); evalErr == nil {
				parts := append([]string{evaluated}, missing...)
				return filepath.Clean(filepath.Join(parts...))
			}
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		missing = append([]string{filepath.Base(current)}, missing...)
	}
	return path
}
