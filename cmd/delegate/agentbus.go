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
	"github.com/charlesnpx/agentbus/engine"
)

// minimumSupportedAgentbusVersion is the oldest installed agentbus binary this
// delegate build is known to work against. Update it when the
// github.com/charlesnpx/agentbus requirement in go.mod is bumped.
const minimumSupportedAgentbusVersion = "v0.10.0"

const (
	agentbusVersionStatusSupported = "supported"
	agentbusVersionStatusTooOld    = "too_old"
	agentbusVersionStatusUnknown   = "unknown"
)

type agentbusVersionAssessment struct {
	Status  string
	Warning string
}

type agentbusClient interface {
	Close() error
	HelloResult() client.HelloResult
	JobSubmit(context.Context, client.JobSubmitParams) (client.JobSubmitResult, error)
	JobStatus(context.Context, client.JobStatusParams) (client.JobStatusResult, error)
}

// validateBackend checks only that Agentbus advertised the selected backend.
// Model and effort are caller values that Agentbus/the backend owns.
func validateBackend(hello client.HelloResult, backend string) error {
	available := append([]string(nil), hello.Backends...)
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

var commandOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func connectCheckedAgentbus(ctx context.Context, opts client.Options, required []string, version string) (agentbusClient, client.HelloResult, error) {
	c, err := connectAgentbus(ctx, opts)
	if err != nil {
		return nil, client.HelloResult{}, err
	}
	hello := c.HelloResult()
	if err := requireCapabilities(hello, version, required); err != nil {
		_ = c.Close()
		return nil, client.HelloResult{}, err
	}
	return c, hello, nil
}

func connectAgentbusCommand(ctx context.Context, required []string) (agentbusClient, client.HelloResult, string, error) {
	stateRoot, err := resolveAgentbusStateRoot()
	if err != nil {
		return nil, client.HelloResult{}, "", err
	}
	c, hello, err := connectAgentbusCommandAtRoot(ctx, required, stateRoot)
	return c, hello, stateRoot, err
}

func connectAgentbusCommandAtRoot(ctx context.Context, required []string, stateRoot string) (agentbusClient, client.HelloResult, error) {
	stateRoot, err := canonicalizeAgentbusStateRoot("agentbus state root", stateRoot)
	if err != nil {
		return nil, client.HelloResult{}, err
	}
	path, version := optionalAgentbusBinaryVersion()
	opts := client.Options{StateRoot: stateRoot}
	if path != "" {
		opts.CommandPath = path
	}
	return connectCheckedAgentbus(ctx, opts, required, version)
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

func resolveAgentbusUserCacheRoot() (string, error) {
	return resolveAgentbusUserCacheRootFrom(os.UserCacheDir)
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

func resolveAgentbusAutostartLockRoot() (string, error) {
	return resolveAgentbusAutostartLockRootFrom(os.UserCacheDir)
}

func resolveAgentbusAutostartLockRootFrom(userCacheDir func() (string, error)) (string, error) {
	cacheRoot, err := resolveAgentbusUserCacheRootFrom(userCacheDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheRoot, "start-locks"), nil
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

func requireCapabilities(hello client.HelloResult, version string, required []string) error {
	for _, capName := range required {
		if !hello.Capabilities[capName] {
			return capabilityMissingError(hello, version, capName)
		}
	}
	return nil
}

func requiredCapabilitiesForPolicy(policy *engine.TurnPolicy) []string {
	required := []string{"admission.strictContainment"}
	if policy != nil && policy.Contract != nil {
		if policy.Contract.JSONSchema != nil {
			required = append(required, "policy.jsonSchema")
		}
		if policy.Contract.Named != "" {
			required = append(required, "policy.named")
		}
	}
	if policy != nil && policy.Retry != nil && policy.Retry.Max > 0 {
		required = append(required, "policy.retry")
	}
	return required
}

func setupRequiredCapabilities() []string {
	// Strict containment is the only capability required for every task;
	// JSON Schema and retry are required only when a task supplies a schema.
	return []string{"admission.strictContainment"}
}

func missingCapabilities(hello client.HelloResult, required []string) []string {
	var missing []string
	for _, capName := range required {
		if !hello.Capabilities[capName] {
			missing = append(missing, capName)
		}
	}
	return missing
}

func capabilityMissingError(hello client.HelloResult, version, capName string) error {
	return fmt.Errorf("%s lacks capability `%s`; run mise-en-place install agentbus", agentbusLabel(hello, version), capName)
}

func agentbusLabel(hello client.HelloResult, version string) string {
	version = strings.TrimSpace(version)
	if version != "" {
		if strings.HasPrefix(version, "agentbus ") {
			return version
		}
		return "agentbus " + version
	}
	if hello.ProtocolVersion > 0 {
		return fmt.Sprintf("agentbus protocol %d", hello.ProtocolVersion)
	}
	return "agentbus"
}

func agentbusBinary() (string, error) {
	path, err := lookPath("agentbus")
	if err != nil {
		return "", errors.New("agentbus binary not found; run mise-en-place install agentbus")
	}
	return path, nil
}

func agentbusVersion(path string) string {
	if path == "" {
		return ""
	}
	out, err := commandOutput(path, "version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) >= 2 && fields[0] == "agentbus" {
		return fields[1]
	}
	if len(fields) >= 1 {
		return fields[0]
	}
	return ""
}

// assessAgentbusVersion compares the version token already extracted by
// agentbusVersion with this build's declared floor. Discovery failures and
// malformed output stay warnings so a changed version-reporting format cannot
// make setup unusable.
func assessAgentbusVersion(version string) agentbusVersionAssessment {
	version = strings.TrimSpace(version)
	if version == "" {
		return agentbusVersionAssessment{
			Status:  agentbusVersionStatusUnknown,
			Warning: fmt.Sprintf("agentbus version could not be discovered; minimum supported version is %s (setup will not block readiness)", minimumSupportedAgentbusVersion),
		}
	}
	comparison, err := compareAgentbusSemver(version, minimumSupportedAgentbusVersion)
	if err != nil {
		return agentbusVersionAssessment{
			Status:  agentbusVersionStatusUnknown,
			Warning: fmt.Sprintf("agentbus version %q could not be parsed; minimum supported version is %s (setup will not block readiness)", version, minimumSupportedAgentbusVersion),
		}
	}
	if comparison < 0 {
		return agentbusVersionAssessment{Status: agentbusVersionStatusTooOld}
	}
	return agentbusVersionAssessment{Status: agentbusVersionStatusSupported}
}

func agentbusMinimumVersionError(version string) error {
	return fmt.Errorf("agentbus %s is older than the minimum supported version %s; run mise-en-place install agentbus to upgrade", version, minimumSupportedAgentbusVersion)
}

type agentbusSemver struct {
	major      string
	minor      string
	patch      string
	prerelease []string
}

func compareAgentbusSemver(a, b string) (int, error) {
	parsedA, err := parseAgentbusSemver(a)
	if err != nil {
		return 0, err
	}
	parsedB, err := parseAgentbusSemver(b)
	if err != nil {
		return 0, err
	}
	return compareParsedAgentbusSemver(parsedA, parsedB), nil
}

func parseAgentbusSemver(version string) (agentbusSemver, error) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return agentbusSemver{}, errors.New("empty semantic version")
	}

	coreAndPrerelease, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !validAgentbusSemverIdentifiers(build, false) {
		return agentbusSemver{}, fmt.Errorf("invalid semantic version build metadata %q", build)
	}
	core, prerelease, hasPrerelease := strings.Cut(coreAndPrerelease, "-")
	if hasPrerelease && !validAgentbusSemverIdentifiers(prerelease, true) {
		return agentbusSemver{}, fmt.Errorf("invalid semantic version prerelease %q", prerelease)
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return agentbusSemver{}, fmt.Errorf("semantic version %q must have major, minor, and patch components", version)
	}
	for _, part := range parts {
		if !validAgentbusSemverNumber(part) {
			return agentbusSemver{}, fmt.Errorf("invalid semantic version number %q", part)
		}
	}
	parsed := agentbusSemver{major: parts[0], minor: parts[1], patch: parts[2]}
	if hasPrerelease {
		parsed.prerelease = strings.Split(prerelease, ".")
	}
	return parsed, nil
}

func validAgentbusSemverNumber(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validAgentbusSemverIdentifiers(value string, rejectLeadingZeroNumbers bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, char := range identifier {
			if !((char >= '0' && char <= '9') || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '-') {
				return false
			}
			if char < '0' || char > '9' {
				numeric = false
			}
		}
		if numeric && rejectLeadingZeroNumbers && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func compareParsedAgentbusSemver(a, b agentbusSemver) int {
	for _, components := range [][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if comparison := compareAgentbusSemverNumber(components[0], components[1]); comparison != 0 {
			return comparison
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) == 0 {
		return 0
	}
	if len(a.prerelease) == 0 {
		return 1
	}
	if len(b.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(a.prerelease) && index < len(b.prerelease); index++ {
		if comparison := compareAgentbusSemverIdentifier(a.prerelease[index], b.prerelease[index]); comparison != 0 {
			return comparison
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}

func compareAgentbusSemverIdentifier(a, b string) int {
	aNumeric := validAgentbusSemverNumber(a)
	bNumeric := validAgentbusSemverNumber(b)
	if aNumeric && bNumeric {
		return compareAgentbusSemverNumber(a, b)
	}
	if aNumeric {
		return -1
	}
	if bNumeric {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareAgentbusSemverNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func optionalAgentbusBinaryVersion() (string, string) {
	path, err := lookPath("agentbus")
	if err != nil {
		return "", ""
	}
	return path, agentbusVersion(path)
}
