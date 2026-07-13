package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	delegateconfig "github.com/charlesnpx/delegate/internal/config"
)

type configListJSON struct {
	Path        string                  `json:"path"`
	Overridable bool                    `json:"overridable"`
	Backend     delegateconfig.Backends `json:"backend"`
}

func runConfig(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("delegate config requires list, get, set, or unset")
	}
	switch args[0] {
	case "list":
		return runConfigList(args[1:], stdout, stderr)
	case "get":
		return runConfigGet(args[1:], stdout, stderr)
	case "set":
		return runConfigSet(args[1:], stdout, stderr)
	case "unset":
		return runConfigUnset(args[1:], stdout, stderr)
	default:
		return 0, fmt.Errorf("unknown delegate config command %q; use list, get, set, or unset", args[0])
	}
}

func runConfigList(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate config list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate config list does not accept positional arguments")
	}
	cfg, err := delegateconfig.Load()
	if err != nil {
		return 0, err
	}
	path, err := delegateconfig.Path()
	if err != nil {
		return 0, err
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, configListJSON{Path: path, Overridable: cfg.Overridable, Backend: cfg.Backend})
	}
	_, err = fmt.Fprintf(stdout, "config file: %s\noverridable: %t\nbackend.claude.model: %s\nbackend.claude.effort: %s\nbackend.codex.model: %s\nbackend.codex.effort: %s\n", path, cfg.Overridable, cfg.Backend.Claude.Model, cfg.Backend.Claude.Effort, cfg.Backend.Codex.Model, cfg.Backend.Codex.Effort)
	return 0, err
}

func runConfigGet(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("usage: delegate config get <key>")
	}
	cfg, err := delegateconfig.Load()
	if err != nil {
		return 0, err
	}
	value, err := cfg.Get(args[0])
	if err != nil {
		return 0, err
	}
	_, err = fmt.Fprintln(stdout, value)
	return 0, err
}

func runConfigSet(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) != 2 {
		return 0, fmt.Errorf("usage: delegate config set <key> <value>")
	}
	cfg, err := delegateconfig.Load()
	if err != nil {
		return 0, err
	}
	if err := cfg.Set(args[0], args[1]); err != nil {
		return 0, err
	}
	warnUnknownConfigValue(context.Background(), args[0], args[1], stderr)
	if err := delegateconfig.Save(cfg); err != nil {
		return 0, err
	}
	return 0, nil
}

func runConfigUnset(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("usage: delegate config unset <key>")
	}
	cfg, err := delegateconfig.Load()
	if err != nil {
		return 0, err
	}
	if err := cfg.Unset(args[0]); err != nil {
		return 0, err
	}
	if err := delegateconfig.Save(cfg); err != nil {
		return 0, err
	}
	return 0, nil
}

func warnUnknownConfigValue(ctx context.Context, key, value string, stderr io.Writer) {
	backend, dimension, ok := configBackendDimension(key)
	if !ok || value == "" {
		return
	}
	c, hello, err := connectAgentbusCommand(ctx, nil)
	if err != nil {
		return
	}
	defer c.Close()
	for _, metadata := range hello.BackendMetadata {
		if metadata.Name != backend {
			continue
		}
		advertised := metadata.Models
		if dimension == "effort" {
			advertised = metadata.Efforts
		}
		if len(advertised) > 0 && !containsString(advertised, value) {
			_, _ = fmt.Fprintf(stderr, "warning: %s\n", unadvertisedBackendValueWarning(dimension, value, backend, advertised))
		}
		return
	}
}

func configBackendDimension(key string) (backend, dimension string, ok bool) {
	switch key {
	case delegateconfig.KeyClaudeModel:
		return "claude", "model", true
	case delegateconfig.KeyClaudeEffort:
		return "claude", "effort", true
	case delegateconfig.KeyCodexModel:
		return "codex", "model", true
	case delegateconfig.KeyCodexEffort:
		return "codex", "effort", true
	default:
		return "", "", false
	}
}
