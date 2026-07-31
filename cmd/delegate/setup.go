package main

import (
	"bytes"
	"context"
	"errors"
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
	Schema                            int           `json:"schema"`
	Delegate                          string        `json:"delegate"`
	Agentbus                          setupAgentbus `json:"agentbus"`
	Config                            setupConfig   `json:"config"`
	Skills                            []setupSkill  `json:"skills"`
	StateRootWritable                 bool          `json:"stateRootWritable"`
	AgentbusStateRoot                 string        `json:"agentbusStateRoot"`
	AgentbusStateRootWritable         bool          `json:"agentbusStateRootWritable"`
	AgentbusAutostartLockRoot         string        `json:"agentbusAutostartLockRoot"`
	AgentbusAutostartLockRootWritable bool          `json:"agentbusAutostartLockRootWritable"`
	AdmissionStrictContainment        bool          `json:"admissionStrictContainment"`
	PendingSubmissionIntentCount      int           `json:"pendingSubmissionIntentCount"`
	UnresolvedCleanupArtifactCount    int           `json:"unresolvedCleanupArtifactCount"`
	DaemonReachable                   bool          `json:"daemonReachable"`
	Ready                             bool          `json:"ready"`
	StopReviewGate                    string        `json:"stop_review_gate"`
	Warnings                          []string      `json:"warnings,omitempty"`
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
	Missing         []string             `json:"missingCapabilities,omitempty"`
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
	agentbusRoot, err := resolveAgentbusStateRoot()
	if err != nil {
		return 0, err
	}
	c, err := connectAgentbus(ctx, client.Options{CommandPath: path, StateRoot: agentbusRoot})
	if err != nil {
		return agentbusCommandErrorResult(*jsonOut, stdout, agentbusOperationError(err))
	}
	defer c.Close()
	hello := c.HelloResult()
	requiredCapabilities := setupRequiredCapabilities()
	missingCapabilities := missingCapabilities(hello, requiredCapabilities)
	capabilitiesOK := len(missingCapabilities) == 0
	preflight := setupStatePreflightWithAgentbusRoot(agentbusRoot, nil)
	pendingSubmissionIntents, countWarnings := setupPendingSubmissionIntentCount(preflight.DelegateStateRoot)
	preflight.Warnings = append(preflight.Warnings, countWarnings...)
	unresolvedCleanupArtifacts, countWarnings := setupUnresolvedCleanupArtifactCount(preflight.DelegateStateRoot)
	preflight.Warnings = append(preflight.Warnings, countWarnings...)
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
	ready := capabilitiesOK && preflight.StateRootWritable && preflight.AgentbusStateRootWritable && preflight.AgentbusAutostartLockRootWritable
	readinessErr := setupReadinessError(hello, version, missingCapabilities, preflight)
	if *jsonOut {
		err := writeJSONLine(stdout, setupJSON{
			Schema:   commandJSONSchema,
			Delegate: versionLine(),
			Agentbus: setupAgentbus{
				Found:           true,
				Path:            path,
				Version:         version,
				ProtocolVersion: hello.ProtocolVersion,
				Backends:        hello.Backends,
				BackendMetadata: hello.BackendMetadata,
				Capabilities:    hello.Capabilities,
				Required:        requiredCapabilities,
				Missing:         missingCapabilities,
				CapabilitiesOK:  capabilitiesOK,
			},
			Config: setupConfig{
				Path:        configPath,
				Overridable: cfg.Overridable,
				Defaults:    cfg.Backend,
			},
			Skills:                            skills,
			StateRootWritable:                 preflight.StateRootWritable,
			AgentbusStateRoot:                 preflight.AgentbusStateRoot,
			AgentbusStateRootWritable:         preflight.AgentbusStateRootWritable,
			AgentbusAutostartLockRoot:         preflight.AgentbusAutostartLockRoot,
			AgentbusAutostartLockRootWritable: preflight.AgentbusAutostartLockRootWritable,
			AdmissionStrictContainment:        hello.Capabilities["admission.strictContainment"],
			PendingSubmissionIntentCount:      pendingSubmissionIntents,
			UnresolvedCleanupArtifactCount:    unresolvedCleanupArtifacts,
			DaemonReachable:                   true,
			Ready:                             ready,
			StopReviewGate:                    "not available (planned v0.2)",
			Warnings:                          preflight.Warnings,
		})
		if err != nil {
			return 0, err
		}
		if readinessErr != nil {
			return 1, readinessErr
		}
		return 0, nil
	}
	if _, err := fmt.Fprintf(stdout, "%s\nagentbus: %s\n", versionLine(), path); err != nil {
		return 0, err
	}
	if version != "" {
		if _, err := fmt.Fprintf(stdout, "agentbus version: %s\n", version); err != nil {
			return 0, err
		}
	}
	capabilityStatus := "ok"
	if !capabilitiesOK {
		capabilityStatus = "missing " + strings.Join(missingCapabilities, ",")
	}
	if _, err := fmt.Fprintf(stdout, "agentbus discovery: found\nagentbus protocol: %d\ncapabilities: %s\nadmission.strictContainment: %t\n", hello.ProtocolVersion, capabilityStatus, hello.Capabilities["admission.strictContainment"]); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(stdout, "agentbusStateRoot: %s\nagentbusStateRootWritable: %t\nagentbusAutostartLockRoot: %s\nagentbusAutostartLockRootWritable: %t\nstateRootWritable: %t\npendingSubmissionIntentCount: %d\nunresolvedCleanupArtifactCount: %d\ndaemonReachable: true\nready: %t\n", preflight.AgentbusStateRoot, preflight.AgentbusStateRootWritable, preflight.AgentbusAutostartLockRoot, preflight.AgentbusAutostartLockRootWritable, preflight.StateRootWritable, pendingSubmissionIntents, unresolvedCleanupArtifacts, ready); err != nil {
		return 0, err
	}
	for _, warning := range preflight.Warnings {
		if _, err := fmt.Fprintf(stdout, "warning: %s\n", warning); err != nil {
			return 0, err
		}
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
	if readinessErr != nil {
		return 1, readinessErr
	}
	return 0, nil
}

func setupReadinessError(hello client.HelloResult, version string, missingCapabilities []string, preflight setupStatePreflightResult) error {
	var errs []error
	if len(missingCapabilities) > 0 {
		errs = append(errs, capabilityMissingError(hello, version, missingCapabilities[0]))
	}
	if !preflight.AgentbusStateRootWritable {
		errs = append(errs, setupWritableReadinessError("agentbus state root", preflight.AgentbusStateRoot))
	}
	if !preflight.AgentbusAutostartLockRootWritable {
		errs = append(errs, setupWritableReadinessError("agentbus autostart lock root", preflight.AgentbusAutostartLockRoot))
	}
	if !preflight.StateRootWritable {
		errs = append(errs, setupWritableReadinessError("delegate state root", preflight.DelegateStateRoot))
	}
	return errors.Join(errs...)
}

func setupWritableReadinessError(name, path string) error {
	if path == "" {
		return fmt.Errorf("%s writable check failed", name)
	}
	return fmt.Errorf("%s is not writable: %s", name, path)
}

type setupStatePreflightResult struct {
	StateRootWritable                 bool
	DelegateStateRoot                 string
	AgentbusStateRoot                 string
	AgentbusStateRootWritable         bool
	AgentbusAutostartLockRoot         string
	AgentbusAutostartLockRootWritable bool
	Warnings                          []string
}

func setupStatePreflight() setupStatePreflightResult {
	agentbusRoot, agentbusErr := resolveAgentbusStateRoot()
	return setupStatePreflightWithAgentbusRoot(agentbusRoot, agentbusErr)
}

func setupStatePreflightWithAgentbusRoot(agentbusRoot string, agentbusErr error) setupStatePreflightResult {
	result := setupStatePreflightResult{}
	delegateRoot, err := handoff.ResolveStateDir(handoff.StateConfig{})
	if err == nil {
		result.DelegateStateRoot = delegateRoot
		if err := handoff.EnsureStateDir(delegateRoot); err == nil {
			result.StateRootWritable = directoryWritable(delegateRoot)
		}
	}
	if agentbusErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("agentbus state root was not probed because %v", agentbusErr))
		return result
	}
	result.AgentbusStateRoot = agentbusRoot
	result.AgentbusStateRootWritable = directoryWritable(agentbusRoot)
	lockRoot, err := resolveAgentbusAutostartLockRoot()
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("agentbus autostart lock root was not probed because %v", err))
		return result
	}
	result.AgentbusAutostartLockRoot = lockRoot
	result.AgentbusAutostartLockRootWritable = directoryWritable(lockRoot)
	return result
}

func setupPendingSubmissionIntentCount(stateDir string) (int, []string) {
	if stateDir == "" {
		return 0, []string{"pending submission intents were not counted because delegate state root was not resolved"}
	}
	intents, err := listSubmissionIntents(stateDir)
	if err != nil {
		return 0, []string{fmt.Sprintf("pending submission intents were not counted because %v", err)}
	}
	var count int
	for _, intent := range intents {
		if submissionIntentPending(intent) {
			count++
		}
	}
	return count, nil
}

func submissionIntentPending(intent submissionIntent) bool {
	switch intent.Phase {
	case submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseBlocked:
		return true
	default:
		return false
	}
}

func setupUnresolvedCleanupArtifactCount(stateDir string) (int, []string) {
	if stateDir == "" {
		return 0, []string{"unresolved cleanup artifacts were not counted because delegate state root was not resolved"}
	}
	metas, err := listJobMetadata(stateDir)
	if err != nil {
		return 0, []string{fmt.Sprintf("unresolved cleanup artifacts were not counted because %v", err)}
	}
	var count int
	for _, meta := range metas {
		if !engine.IsTerminal(meta.State) || localCleanupSafe(meta.CleanupDisposition) {
			continue
		}
		if retainedArtifactExists(meta.JobInputPath) {
			count++
		}
		if retainedArtifactExists(meta.ReviewWorkspace) {
			count++
		}
	}
	return count, nil
}

func retainedArtifactExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func listJobMetadata(stateDir string) ([]jobMetadata, error) {
	dir, err := jobMetadataDir(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var metas []jobMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		jobID, ok := decodeStateFilename(entry.Name())
		if !ok {
			continue
		}
		meta, found, err := loadJobMetadata(stateDir, jobID)
		if err != nil {
			return nil, err
		}
		if found {
			metas = append(metas, meta)
		}
	}
	return metas, nil
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
