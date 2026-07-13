package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	skillpkg "github.com/charlesnpx/delegate/internal/skills"
)

const stopReviewGateLine = "stop-review-gate: not available (planned v0.2)"

type setupJSON struct {
	Schema                    int           `json:"schema"`
	Delegate                  string        `json:"delegate"`
	Agentbus                  setupAgentbus `json:"agentbus"`
	Config                    setupConfig   `json:"config"`
	Skills                    []setupSkill  `json:"skills"`
	StateRootWritable         bool          `json:"stateRootWritable"`
	AgentbusStateRootWritable bool          `json:"agentbusStateRootWritable"`
	DaemonReachable           bool          `json:"daemonReachable"`
	StopReviewGate            string        `json:"stop_review_gate"`
}

type setupConfig struct {
	Path        string                  `json:"path"`
	Overridable bool                    `json:"overridable"`
	Defaults    delegateconfig.Backends `json:"defaults"`
}

type setupAgentbus struct {
	Found           bool                 `json:"found"`
	Path            string               `json:"path"`
	Version         string               `json:"version,omitempty"`
	ProtocolVersion int                  `json:"protocolVersion"`
	Backends        []string             `json:"backends"`
	BackendMetadata []client.BackendInfo `json:"backendMetadata,omitempty"`
	Capabilities    map[string]bool      `json:"capabilities"`
	Required        []string             `json:"requiredCapabilities"`
	CapabilitiesOK  bool                 `json:"capabilitiesOK"`
}

// setupSkill reports whether one managed skill is present and matches the
// release's generated source. Status is installed, missing, outdated, or
// unreadable. Setup reports each skill rather than assuming a whole target was
// installed together.
type setupSkill struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Status string `json:"status"`
}

func runSetup(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("delegate setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("delegate setup does not accept positional arguments")
	}
	path, err := agentbusBinary()
	if err != nil {
		return 0, err
	}
	version := agentbusVersion(path)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, hello, err := connectCheckedAgentbus(ctx, client.Options{CommandPath: path}, setupRequiredCapabilities(), version)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	preflight := setupStatePreflight()
	cfg, err := delegateconfig.Load()
	if err != nil {
		return 0, err
	}
	configPath, err := delegateconfig.Path()
	if err != nil {
		return 0, err
	}
	skills, err := installedSkills()
	if err != nil {
		return 0, err
	}
	if *jsonOut {
		return 0, writeJSONLine(stdout, setupJSON{
			Schema:   envelopeSchema,
			Delegate: versionLine(),
			Agentbus: setupAgentbus{
				Found:           true,
				Path:            path,
				Version:         version,
				ProtocolVersion: hello.ProtocolVersion,
				Backends:        hello.Backends,
				BackendMetadata: hello.BackendMetadata,
				Capabilities:    hello.Capabilities,
				Required:        setupRequiredCapabilities(),
				CapabilitiesOK:  true,
			},
			Config: setupConfig{
				Path:        configPath,
				Overridable: cfg.Overridable,
				Defaults:    cfg.Backend,
			},
			Skills:                    skills,
			StateRootWritable:         preflight.StateRootWritable,
			AgentbusStateRootWritable: preflight.AgentbusStateRootWritable,
			DaemonReachable:           true,
			StopReviewGate:            "not available (planned v0.2)",
		})
	}
	if _, err := fmt.Fprintf(stdout, "%s\nagentbus: %s\n", versionLine(), path); err != nil {
		return 0, err
	}
	if version != "" {
		if _, err := fmt.Fprintf(stdout, "agentbus version: %s\n", version); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(stdout, "agentbus discovery: found\nagentbus protocol: %d\ncapabilities: ok\n", hello.ProtocolVersion); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(stdout, "stateRootWritable: %t\nagentbusStateRootWritable: %t\ndaemonReachable: true\n", preflight.StateRootWritable, preflight.AgentbusStateRootWritable); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(stdout, "agentbus models.reported: %t\nconfig file: %s\nconfig overridable: %t\nconfig backend claude: model=%s effort=%s\nconfig backend codex: model=%s effort=%s\n", hello.Capabilities["models.reported"], configPath, cfg.Overridable, cfg.Backend.Claude.Model, cfg.Backend.Claude.Effort, cfg.Backend.Codex.Model, cfg.Backend.Codex.Effort); err != nil {
		return 0, err
	}
	for _, backend := range hello.Backends {
		line := "backend " + backend
		for _, meta := range hello.BackendMetadata {
			if meta.Name == backend {
				line += fmt.Sprintf(": models=%s efforts=%s", strings.Join(meta.Models, ","), strings.Join(meta.Efforts, ","))
			}
		}
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return 0, err
		}
	}
	for _, skill := range skills {
		if _, err := fmt.Fprintf(stdout, "skill %s (%s): %s\n", skill.Name, skill.Target, skill.Status); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", stopReviewGateLine); err != nil {
		return 0, err
	}
	return 0, nil
}

type setupStatePreflightResult struct {
	StateRootWritable         bool
	AgentbusStateRootWritable bool
}

func setupStatePreflight() setupStatePreflightResult {
	result := setupStatePreflightResult{}
	delegateRoot, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err == nil {
		if err := handoff.EnsureStateDir(delegateRoot); err == nil {
			result.StateRootWritable = directoryWritable(delegateRoot)
		}
	}
	agentbusRoot, err := engine.ResolveStateRoot()
	if err == nil {
		result.AgentbusStateRootWritable = directoryWritable(agentbusRoot)
	}
	return result
}

// directoryWritable proves both create and write access without leaving a
// probe file behind. The state directory itself is intentionally retained: a
// successful preflight is allowed to create the directory it reports usable.
func directoryWritable(path string) bool {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false
	}
	file, err := os.CreateTemp(path, ".delegate-setup-*")
	if err != nil {
		return false
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.WriteString("setup preflight\n"); err != nil {
		_ = file.Close()
		return false
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false
	}
	return file.Close() == nil
}

func installedSkills() ([]setupSkill, error) {
	var statuses []setupSkill
	for _, target := range []string{skillpkg.TargetClaude, skillpkg.TargetCodex} {
		root, err := skillpkg.TargetRoot(target, "", nil, nil)
		if err != nil {
			return nil, err
		}
		generated, err := skillpkg.Generate(target)
		if err != nil {
			return nil, err
		}
		for _, skill := range generated {
			path := filepath.Join(root, skillpkg.DecodeName(skill.EscapedName), "SKILL.md")
			statuses = append(statuses, setupSkill{
				Target: target,
				Name:   skill.Name,
				Path:   path,
				Status: skillInstallStatus(path, []byte(skill.Content)),
			})
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Target == statuses[j].Target {
			return statuses[i].Name < statuses[j].Name
		}
		return statuses[i].Target < statuses[j].Target
	})
	return statuses, nil
}

func skillInstallStatus(path string, expected []byte) string {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "unreadable"
	}
	if bytes.Equal(raw, expected) {
		return "installed"
	}
	return "outdated"
}
