package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
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
	JobResult(context.Context, client.JobResultParams) (client.JobResult, error)
	JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error)
}

// timeoutCapturingClient decodes the daemon's additive timeout field beside
// the pinned Agentbus client's typed response. The pinned response types do
// not retain that field, so this wrapper owns the full response decode while
// preserving the existing typed interface for callers.
type timeoutCapturingClient struct {
	opts      client.Options
	conn      net.Conn
	reader    *bufio.Reader
	hello     client.HelloResult
	writeMu   sync.Mutex
	requestMu sync.Mutex
	mu        sync.Mutex
	closed    bool
	ids       uint64

	submittedTimeouts map[string]config.DimensionResolution
	resultTimeouts    map[string]config.DimensionResolution
	statusTimeouts    map[string]config.DimensionResolution
}

func newTimeoutCapturingClient(ctx context.Context, opts client.Options) (*timeoutCapturingClient, error) {
	if opts.StateRoot == "" {
		stateRoot, err := engine.ResolveStateRoot()
		if err != nil {
			return nil, err
		}
		opts.StateRoot = stateRoot
	}
	// Connect through the pinned client first so its established autostart and
	// connection behavior remains in effect before this client retains additive
	// response fields from the wire.
	typed, err := client.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	_ = typed.Close()
	socketPath := opts.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(opts.StateRoot, "agentbus.sock")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	client := &timeoutCapturingClient{
		opts:              opts,
		conn:              conn,
		reader:            bufio.NewReader(conn),
		submittedTimeouts: make(map[string]config.DimensionResolution),
		resultTimeouts:    make(map[string]config.DimensionResolution),
		statusTimeouts:    make(map[string]config.DimensionResolution),
	}
	if err := client.helloRequest(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (c *timeoutCapturingClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *timeoutCapturingClient) HelloResult() client.HelloResult { return c.hello }

func (c *timeoutCapturingClient) JobSubmit(ctx context.Context, params client.JobSubmitParams) (client.JobSubmitResult, error) {
	var response struct {
		JobID        string          `json:"jobId"`
		State        engine.JobState `json:"state"`
		Deduplicated bool            `json:"deduplicated,omitempty"`
		Timeout      json.RawMessage `json:"timeout,omitempty"`
	}
	if err := c.do(ctx, "job.submit", params, &response); err != nil {
		return client.JobSubmitResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if resolution, ok := timeoutResolutionFromWire(response.Timeout); ok {
		c.submittedTimeouts[response.JobID] = resolution
	} else {
		delete(c.submittedTimeouts, response.JobID)
	}
	return client.JobSubmitResult{JobID: response.JobID, State: response.State, Deduplicated: response.Deduplicated}, nil
}

func (c *timeoutCapturingClient) JobStatus(ctx context.Context, params client.JobStatusParams) (client.JobStatusResult, error) {
	var response struct {
		Jobs []struct {
			client.JobStatus
			Timeout json.RawMessage `json:"timeout,omitempty"`
		} `json:"jobs"`
	}
	if err := c.do(ctx, "job.status", params, &response); err != nil {
		return client.JobStatusResult{}, err
	}
	result := client.JobStatusResult{Jobs: make([]client.JobStatus, 0, len(response.Jobs))}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, job := range response.Jobs {
		result.Jobs = append(result.Jobs, job.JobStatus)
		if resolution, ok := timeoutResolutionFromWire(job.Timeout); ok {
			c.statusTimeouts[job.JobID] = resolution
		} else {
			delete(c.statusTimeouts, job.JobID)
		}
	}
	return result, nil
}

func (c *timeoutCapturingClient) JobResult(ctx context.Context, params client.JobResultParams) (client.JobResult, error) {
	var response struct {
		client.JobResult
		Timeout json.RawMessage `json:"timeout,omitempty"`
	}
	if err := c.do(ctx, "job.result", params, &response); err != nil {
		return client.JobResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if resolution, ok := timeoutResolutionFromWire(response.Timeout); ok {
		c.resultTimeouts[response.JobID] = resolution
	} else {
		delete(c.resultTimeouts, response.JobID)
	}
	return response.JobResult, nil
}

func (c *timeoutCapturingClient) JobCancel(ctx context.Context, params client.JobCancelParams) (client.JobCancelResult, error) {
	var response client.JobCancelResult
	if err := c.do(ctx, "job.cancel", params, &response); err != nil {
		return client.JobCancelResult{}, err
	}
	return response, nil
}

func (c *timeoutCapturingClient) submittedTimeoutResolution(jobID string) (config.DimensionResolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resolution, ok := c.submittedTimeouts[jobID]
	return resolution, ok
}

func (c *timeoutCapturingClient) resultTimeoutResolution(jobID string) (config.DimensionResolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resolution, ok := c.resultTimeouts[jobID]
	return resolution, ok
}

func (c *timeoutCapturingClient) statusTimeoutResolution(jobID string) (config.DimensionResolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resolution, ok := c.statusTimeouts[jobID]
	return resolution, ok
}

func (c *timeoutCapturingClient) helloRequest(ctx context.Context) error {
	token, err := c.token()
	if err != nil {
		return err
	}
	var hello client.HelloResult
	if err := c.do(ctx, "protocol.hello", map[string]any{"clientProtocolVersion": 2, "token": token}, &hello); err != nil {
		return err
	}
	if hello.ProtocolVersion != 2 {
		return &client.ProtocolVersionMismatchError{Expected: 2, Received: hello.ProtocolVersion}
	}
	c.hello = hello
	return nil
}

func (c *timeoutCapturingClient) token() (string, error) {
	if c.opts.Token != "" {
		return c.opts.Token, nil
	}
	raw, err := os.ReadFile(filepath.Join(c.opts.StateRoot, "token"))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("agentbus token file is empty")
	}
	return token, nil
}

func (c *timeoutCapturingClient) do(ctx context.Context, method string, params, result any) error {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.mu.Lock()
	if c.closed || c.conn == nil {
		c.mu.Unlock()
		return errors.New("agentbus client is closed")
	}
	c.ids++
	id := strconv.FormatUint(c.ids, 10)
	conn := c.conn
	reader := c.reader
	c.mu.Unlock()
	request := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  any             `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: json.RawMessage(strconv.Quote(id)), Method: method, Params: params}
	c.writeMu.Lock()
	err := writeAgentbusWireFrame(ctx, conn, request)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	line, err := readAgentbusWireFrame(ctx, conn, reader)
	if err != nil {
		return err
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &response); err != nil {
		return err
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		raw, err := json.Marshal(map[string]json.RawMessage{"Object": response.Error})
		if err != nil {
			return err
		}
		var rpcError client.RPCError
		if err := json.Unmarshal(raw, &rpcError); err != nil {
			return err
		}
		return &rpcError
	}
	return json.Unmarshal(response.Result, result)
}

func writeAgentbusWireFrame(ctx context.Context, conn net.Conn, value any) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
		defer conn.SetWriteDeadline(time.Time{})
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(raw, '\n'))
	return err
}

func readAgentbusWireFrame(ctx context.Context, conn net.Conn, reader *bufio.Reader) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
		defer conn.SetReadDeadline(time.Time{})
	}
	return reader.ReadBytes('\n')
}

func validateBackend(hello client.HelloResult, backend, model, effort string, stderr io.Writer) error {
	available := append([]string(nil), hello.Backends...)
	sort.Strings(available)
	found := false
	for _, name := range available {
		if name == backend {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown backend %q; available backends: %s", backend, strings.Join(available, ", "))
	}
	for _, meta := range hello.BackendMetadata {
		if meta.Name != backend {
			continue
		}
		if model != "" && len(meta.Models) > 0 && !containsString(meta.Models, model) {
			if _, err := fmt.Fprintf(stderr, "warning: %s\n", unadvertisedBackendValueWarning("model", model, backend, meta.Models)); err != nil {
				return err
			}
		}
		if effort != "" && len(meta.Efforts) > 0 && !containsString(meta.Efforts, effort) {
			if _, err := fmt.Fprintf(stderr, "warning: %s\n", unadvertisedBackendValueWarning("effort", effort, backend, meta.Efforts)); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func unadvertisedBackendValueWarning(dimension, value, backend string, advertised []string) string {
	return fmt.Sprintf("%s %q is not advertised by agentbus for backend %q (advertised: %s); passing through — the backend is authoritative", dimension, value, backend, strings.Join(advertised, ", "))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var connectAgentbus = func(ctx context.Context, opts client.Options) (agentbusClient, error) {
	return newTimeoutCapturingClient(ctx, opts)
}

var lookPath = exec.LookPath

var commandOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func connectCheckedAgentbus(ctx context.Context, opts client.Options, required []string, version string) (agentbusClient, client.HelloResult, error) {
	c, err := connectAgentbus(ctx, opts)
	if err != nil {
		return nil, client.HelloResult{}, agentbusOperationError(err)
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
		// No policy.shape requirement: delegate owns report-shape validation and
		// the one client-side corrective retry. agentbus accepts, stores, and
		// stamps a shape contract as opaque identity regardless of the
		// (informational) policy.shape capability flag, so requiring it here would
		// wrongly reject the normal managed path against a post-relocation agentbus.
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
	// Strict containment is the only capability delegate strictly needs: shape
	// validation is now client-side, and JSON-Schema/named/retry are required only
	// when a job actually uses them (see requiredCapabilitiesForPolicy).
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
