package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

type agentbusClient interface {
	Close() error
	HelloResult() client.HelloResult
	JobSubmit(context.Context, client.JobSubmitParams) (client.JobSubmitResult, error)
	JobStatus(context.Context, client.JobStatusParams) (client.JobStatusResult, error)
	JobResult(context.Context, client.JobResultParams) (client.JobResult, error)
	JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error)
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

func connectAgentbusCommand(ctx context.Context, required []string) (agentbusClient, client.HelloResult, error) {
	path, version := optionalAgentbusBinaryVersion()
	opts := client.Options{}
	if path != "" {
		opts.CommandPath = path
	}
	return connectCheckedAgentbus(ctx, opts, required, version)
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
		if policy.Contract.Shape != nil {
			required = append(required, "policy.shape")
		}
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
	return []string{"policy.shape", "policy.retry"}
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

func optionalAgentbusBinaryVersion() (string, string) {
	path, err := lookPath("agentbus")
	if err != nil {
		return "", ""
	}
	return path, agentbusVersion(path)
}
