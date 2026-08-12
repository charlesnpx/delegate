package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	delegateconfig "github.com/charlesnpx/delegate/internal/config"
	"github.com/charlesnpx/delegate/internal/handoff"
	skillpkg "github.com/charlesnpx/delegate/internal/skills"
)

type setupJSON struct {
	Schema                            int                            `json:"schema"`
	Delegate                          string                         `json:"delegate"`
	Agentbus                          setupAgentbus                  `json:"agentbus"`
	Config                            setupConfig                    `json:"config"`
	Skills                            []setupSkill                   `json:"skills"`
	StateRootWritable                 bool                           `json:"stateRootWritable"`
	AgentbusStateRoot                 string                         `json:"agentbusStateRoot"`
	AgentbusStateRootWritable         bool                           `json:"agentbusStateRootWritable"`
	AgentbusAutostartLockRoot         string                         `json:"agentbusAutostartLockRoot"`
	AgentbusAutostartLockRootWritable bool                           `json:"agentbusAutostartLockRootWritable"`
	AdmissionStrictContainment        bool                           `json:"admissionStrictContainment"`
	PendingSubmissionIntentCount      *int                           `json:"pendingSubmissionIntentCount"`
	PendingSubmissionIntents          []setupPendingSubmissionIntent `json:"pendingSubmissionIntents"`
	UnresolvedCleanupArtifactCount    *int                           `json:"unresolvedCleanupArtifactCount"`
	DaemonReachable                   bool                           `json:"daemonReachable"`
	Ready                             bool                           `json:"ready"`
	Warnings                          []string                       `json:"warnings,omitempty"`
}

// setupPendingSubmissionIntent is the bounded recovery information setup
// exposes for a durable pending submission intent. Setup projects only its
// request ID, phase, creation time, and bounded backend and origin labels; it
// neither adds nor changes durable state.
type setupPendingSubmissionIntent struct {
	RequestID string                    `json:"request_id"`
	Phase     string                    `json:"phase"`
	CreatedAt time.Time                 `json:"created_at"`
	Backend   string                    `json:"backend,omitempty"`
	Origin    *setupIntentOriginSummary `json:"origin,omitempty"`
}

// setupIntentOriginSummary contains only the coarse origin label needed to recognize
// a logical task in setup output.
type setupIntentOriginSummary struct {
	Skill string `json:"skill,omitempty"`
}

const (
	setupPendingSubmissionIntentLimit      = 20
	setupPendingSubmissionIntentLabelLimit = 80
)

type setupConfig struct {
	Path        string                  `json:"path"`
	Overridable bool                    `json:"overridable"`
	Defaults    delegateconfig.Backends `json:"defaults"`
}

type setupAgentbus struct {
	Found                   bool                 `json:"found"`
	Path                    string               `json:"path"`
	Version                 string               `json:"version,omitempty"`
	MinimumSupportedVersion string               `json:"minimumSupportedVersion"`
	VersionStatus           string               `json:"versionStatus"`
	ProtocolVersion         int                  `json:"protocolVersion"`
	Backends                []string             `json:"backends"`
	BackendMetadata         []client.BackendInfo `json:"backendMetadata,omitempty"`
	Capabilities            map[string]bool      `json:"capabilities"`
	Required                []string             `json:"requiredCapabilities"`
	Missing                 []string             `json:"missingCapabilities,omitempty"`
	CapabilitiesOK          bool                 `json:"capabilitiesOK"`

	// handshakeKnown distinguishes a deliberately skipped handshake from an
	// observed handshake whose fields happen to have zero values. It is never
	// serialized directly.
	handshakeKnown bool
}

// MarshalJSON preserves the established setup JSON after a handshake, while a
// known-too-old binary reports only discovery facts instead of manufacturing
// protocol or capability values that were never observed.
func (agentbus setupAgentbus) MarshalJSON() ([]byte, error) {
	if agentbus.handshakeKnown {
		type setupAgentbusWithHandshake setupAgentbus
		return json.Marshal(setupAgentbusWithHandshake(agentbus))
	}
	return json.Marshal(struct {
		Found                   bool   `json:"found"`
		Path                    string `json:"path"`
		Version                 string `json:"version,omitempty"`
		MinimumSupportedVersion string `json:"minimumSupportedVersion"`
		VersionStatus           string `json:"versionStatus"`
	}{
		Found:                   agentbus.Found,
		Path:                    agentbus.Path,
		Version:                 agentbus.Version,
		MinimumSupportedVersion: agentbus.MinimumSupportedVersion,
		VersionStatus:           agentbus.VersionStatus,
	})
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
	versionAssessment := assessAgentbusVersion(version)
	if versionAssessment.Status == agentbusVersionStatusTooOld {
		// Do not connect a binary already known to be unsupported: connecting can
		// autostart its daemon, and an unsupported daemon must not be started.
		return setupTooOldAgentbusResult(*jsonOut, stdout, path, version, versionAssessment)
	}
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
	if versionAssessment.Warning != "" {
		preflight.Warnings = append(preflight.Warnings, versionAssessment.Warning)
	}
	pendingSubmissionIntentCount, pendingSubmissionIntents, countWarnings := setupPendingSubmissionIntents(preflight.DelegateStateRoot)
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
				Found:                   true,
				Path:                    path,
				Version:                 version,
				MinimumSupportedVersion: minimumSupportedAgentbusVersion,
				VersionStatus:           versionAssessment.Status,
				ProtocolVersion:         hello.ProtocolVersion,
				Backends:                hello.Backends,
				BackendMetadata:         hello.BackendMetadata,
				Capabilities:            hello.Capabilities,
				Required:                requiredCapabilities,
				Missing:                 missingCapabilities,
				CapabilitiesOK:          capabilitiesOK,
				handshakeKnown:          true,
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
			PendingSubmissionIntentCount:      pendingSubmissionIntentCount,
			PendingSubmissionIntents:          pendingSubmissionIntents,
			UnresolvedCleanupArtifactCount:    unresolvedCleanupArtifacts,
			DaemonReachable:                   true,
			Ready:                             ready,
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
	if _, err := fmt.Fprintf(stdout, "agentbus minimum supported version: %s\nagentbus version status: %s\n", minimumSupportedAgentbusVersion, versionAssessment.Status); err != nil {
		return 0, err
	}
	capabilityStatus := "ok"
	if !capabilitiesOK {
		capabilityStatus = "missing " + strings.Join(missingCapabilities, ",")
	}
	if _, err := fmt.Fprintf(stdout, "agentbus discovery: found\nagentbus protocol: %d\ncapabilities: %s\nadmission.strictContainment: %t\n", hello.ProtocolVersion, capabilityStatus, hello.Capabilities["admission.strictContainment"]); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(stdout, "agentbusStateRoot: %s\nagentbusStateRootWritable: %t\nagentbusAutostartLockRoot: %s\nagentbusAutostartLockRootWritable: %t\nstateRootWritable: %t\npendingSubmissionIntentCount: %s\nunresolvedCleanupArtifactCount: %s\ndaemonReachable: true\nready: %t\n", preflight.AgentbusStateRoot, preflight.AgentbusStateRootWritable, preflight.AgentbusAutostartLockRoot, preflight.AgentbusAutostartLockRootWritable, preflight.StateRootWritable, setupCountText(pendingSubmissionIntentCount), setupCountText(unresolvedCleanupArtifacts), ready); err != nil {
		return 0, err
	}
	for _, warning := range preflight.Warnings {
		if _, err := fmt.Fprintf(stdout, "warning: %s\n", warning); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(stdout, "agentbus models.reported: %t\nconfig file: %s\nconfig overridable: %t\nconfig backend claude: model=%s effort=%s\nconfig backend codex: model=%s effort=%s\nconfig backend cursor: model=%s effort=%s\n", hello.Capabilities["models.reported"], configPath, cfg.Overridable, cfg.Backend.Claude.Model, cfg.Backend.Claude.Effort, cfg.Backend.Codex.Model, cfg.Backend.Codex.Effort, cfg.Backend.Cursor.Model, cfg.Backend.Cursor.Effort); err != nil {
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
	if readinessErr != nil {
		return 1, readinessErr
	}
	return 0, nil
}

func setupTooOldAgentbusResult(jsonOut bool, stdout io.Writer, path, version string, versionAssessment agentbusVersionAssessment) (int, error) {
	if jsonOut {
		if err := writeJSONLine(stdout, setupJSON{
			Schema:   commandJSONSchema,
			Delegate: versionLine(),
			Agentbus: setupAgentbus{
				Found:                   true,
				Path:                    path,
				Version:                 version,
				MinimumSupportedVersion: minimumSupportedAgentbusVersion,
				VersionStatus:           versionAssessment.Status,
			},
			Ready: false,
		}); err != nil {
			return 0, err
		}
	} else if _, err := fmt.Fprintf(stdout, "%s\nagentbus: %s\nagentbus version: %s\nagentbus minimum supported version: %s\nagentbus version status: %s\nready: false\n", versionLine(), path, version, minimumSupportedAgentbusVersion, versionAssessment.Status); err != nil {
		return 0, err
	}
	// Setup readiness failures conventionally use exit status 1.
	return 1, agentbusMinimumVersionError(version)
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
		errs = append(errs, setupStateRootReadinessError(preflight.DelegateStateRoot, preflight.StateRootReason))
	}
	return errors.Join(errs...)
}

func setupWritableReadinessError(name, path string) error {
	if path == "" {
		return fmt.Errorf("%s writable check failed", name)
	}
	return fmt.Errorf("%s is not writable: %s", name, path)
}

// setupStateRootReadinessError reports the precise reason the delegate state
// root is unusable rather than the generic "not writable" — a mode mismatch is
// the common case and needs a different fix (chmod), not a permissions grant.
func setupStateRootReadinessError(path, reason string) error {
	if reason == "" {
		return setupWritableReadinessError("delegate state root", path)
	}
	msg := fmt.Sprintf("delegate state root is not usable: %s", reason)
	if path != "" && strings.Contains(reason, "want 700") {
		msg += fmt.Sprintf("; run: chmod 700 %s", path)
	}
	return errors.New(msg)
}

type setupStatePreflightResult struct {
	StateRootWritable                 bool
	StateRootReason                   string
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
		result.StateRootWritable, result.StateRootReason = setupDelegateStateRootWritable(delegateRoot)
		if result.StateRootReason != "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("delegate state root is not usable: %s", result.StateRootReason))
		}
	} else {
		result.StateRootReason = err.Error()
	}
	if agentbusErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("agentbus state root was not checked because %v", agentbusErr))
		return result
	}
	result.AgentbusStateRoot = agentbusRoot
	result.AgentbusStateRootWritable = directoryMayBeWritableByMode(agentbusRoot)
	lockRoot, err := resolveAgentbusAutostartLockRoot()
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("agentbus autostart lock root was not checked because %v", err))
		return result
	}
	result.AgentbusAutostartLockRoot = lockRoot
	result.AgentbusAutostartLockRootWritable = directoryMayBeWritableByMode(lockRoot)
	return result
}

// setupPendingSubmissionIntents returns a nil count (rendered as JSON null /
// "uncounted") when the value could not be computed, so an orchestrator gating
// on it cannot misread a "not counted" warning as a clean zero. The count is
// authoritative; summaries are capped to keep setup preflight output bounded.
func setupPendingSubmissionIntents(stateDir string) (*int, []setupPendingSubmissionIntent, []string) {
	if stateDir == "" {
		return nil, nil, []string{"pending submission intents were not counted because delegate state root was not resolved"}
	}
	intents, err := listSetupSubmissionIntents(stateDir)
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("pending submission intents were not counted because %v", err)}
	}
	pending := make([]submissionIntent, 0, len(intents))
	for _, intent := range intents {
		if submissionIntentPending(intent) {
			pending = append(pending, intent)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].RequestID < pending[j].RequestID
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	count := len(pending)
	limit := min(count, setupPendingSubmissionIntentLimit)
	summaries := make([]setupPendingSubmissionIntent, 0, limit)
	for _, intent := range pending[:limit] {
		summaries = append(summaries, setupPendingSubmissionIntent{
			RequestID: intent.RequestID,
			Phase:     intent.Phase,
			CreatedAt: intent.CreatedAt,
			Backend:   setupBoundedIntentLabel(intent.Params.TaskSpec.Backend),
			Origin:    setupIntentOrigin(intent.Origin),
		})
	}
	return &count, summaries, nil
}

// setupCountText renders a nil (uncounted) count distinctly from a real zero in
// the human-readable setup output, mirroring the JSON null.
func setupCountText(count *int) string {
	if count == nil {
		return "uncounted"
	}
	return strconv.Itoa(*count)
}

func submissionIntentPending(intent submissionIntent) bool {
	switch intent.Phase {
	case submissionPhasePrepared, submissionPhaseInFlight, submissionPhaseBlocked:
		return true
	default:
		return false
	}
}

func setupUnresolvedCleanupArtifactCount(stateDir string) (*int, []string) {
	if stateDir == "" {
		return nil, []string{"unresolved cleanup artifacts were not counted because delegate state root was not resolved"}
	}
	metas, err := listSetupJobMetadata(stateDir)
	if err != nil {
		return nil, []string{fmt.Sprintf("unresolved cleanup artifacts were not counted because %v", err)}
	}
	count := 0
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
	return &count, nil
}

func retainedArtifactExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func listSetupJobMetadata(stateDir string) ([]jobMetadata, error) {
	dir, err := setupJobMetadataDir(stateDir)
	if err != nil {
		return nil, err
	}
	if err := setupReadableDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
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
		meta, found, err := loadSetupJobMetadata(dir, jobID)
		if err != nil {
			return nil, err
		}
		if found {
			metas = append(metas, meta)
		}
	}
	return metas, nil
}

func setupDelegateStateRootWritable(path string) (bool, string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if directoryMayBeWritableByMode(path) {
			return true, ""
		}
		return false, fmt.Sprintf("state dir %q is absent and its nearest existing ancestor has no write and execute mode", path)
	}
	if err != nil {
		return false, err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Sprintf("state dir %q must not be a symlink", path)
	}
	if !info.IsDir() {
		return false, fmt.Sprintf("state dir %q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return false, fmt.Sprintf("state dir %q mode = %o, want %o", path, got, 0o700)
	}
	if !directoryMayBeWritableByMode(path) {
		return false, fmt.Sprintf("state dir %q has no write and execute mode", path)
	}
	return true, ""
}

// directoryMayBeWritableByMode observes only directory mode bits. For a
// missing path it observes the nearest existing ancestor's mode, which is the
// information available without creating the path or writing a probe file.
// It does not establish that a future create or write will succeed.
func directoryMayBeWritableByMode(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return false
			}
			return modeHasWriteAndExecute(info.Mode().Perm())
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func modeHasWriteAndExecute(mode os.FileMode) bool {
	return mode&0o300 == 0o300
}

func setupSubmissionIntentDir(stateDir string) (string, error) {
	dir, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "submissions"), nil
}

func setupJobMetadataDir(stateDir string) (string, error) {
	dir, err := handoff.ResolveStateDir(handoff.StateConfig{StateDir: stateDir})
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "jobs"), nil
}

func setupReadableDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("setup state directory %q must not be a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("setup state path %q is not a directory", path)
	}
	return nil
}

func listSetupSubmissionIntents(stateDir string) ([]submissionIntent, error) {
	dir, err := setupSubmissionIntentDir(stateDir)
	if err != nil {
		return nil, err
	}
	if err := setupReadableDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var intents []submissionIntent
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		requestID, ok := decodeStateFilename(entry.Name())
		if !ok {
			continue
		}
		intent, found, err := loadSetupSubmissionIntent(dir, requestID)
		if err != nil {
			return nil, err
		}
		if found {
			intents = append(intents, intent)
		}
	}
	return intents, nil
}

func loadSetupSubmissionIntent(dir, requestID string) (submissionIntent, bool, error) {
	if err := validateRequestID(requestID); err != nil {
		return submissionIntent{}, false, err
	}
	path := filepath.Join(dir, encodedStateFilename(requestID))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return submissionIntent{}, false, nil
	}
	if err != nil {
		return submissionIntent{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q is not a regular file", requestID)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return submissionIntent{}, false, nil
	}
	if err != nil {
		return submissionIntent{}, false, err
	}
	var intent submissionIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return submissionIntent{}, false, err
	}
	if intent.Schema != submissionIntentSchema {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q has unsupported schema %d", requestID, intent.Schema)
	}
	if intent.RequestID != requestID {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q has request_id %q", requestID, intent.RequestID)
	}
	if intent.Params.RequestID != intent.RequestID {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q has params request_id %q", requestID, intent.Params.RequestID)
	}
	if intent.Params.WorkspaceKey != intent.WorkspaceKey {
		return submissionIntent{}, false, fmt.Errorf("submission intent %q has params workspace_key %q", requestID, intent.Params.WorkspaceKey)
	}
	if err := validateSubmissionPhase(intent.Phase); err != nil {
		return submissionIntent{}, false, err
	}
	return intent, true, nil
}

func loadSetupJobMetadata(dir, jobID string) (jobMetadata, bool, error) {
	if err := validateDelegateJobID(jobID); err != nil {
		return jobMetadata{}, false, err
	}
	path := filepath.Join(dir, encodedStateFilename(jobID))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return jobMetadata{}, false, nil
	}
	if err != nil {
		return jobMetadata{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return jobMetadata{}, false, fmt.Errorf("delegate job metadata %q is not a regular file", jobID)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return jobMetadata{}, false, nil
	}
	if err != nil {
		return jobMetadata{}, false, err
	}
	var meta jobMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return jobMetadata{}, false, err
	}
	if meta.JobID != jobID {
		return jobMetadata{}, false, fmt.Errorf("delegate job metadata %q has job_id %q", jobID, meta.JobID)
	}
	return meta, true, nil
}

func setupIntentOrigin(origin *envelopeOrigin) *setupIntentOriginSummary {
	if origin == nil {
		return nil
	}
	label := setupBoundedIntentLabel(origin.Skill)
	if label == "" {
		return nil
	}
	return &setupIntentOriginSummary{Skill: label}
}

func setupBoundedIntentLabel(value string) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= setupPendingSubmissionIntentLabelLimit {
		return value
	}
	end := setupPendingSubmissionIntentLabelLimit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
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
