package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

type agentbusClient interface {
	Close() error
	Hello(context.Context) (client.HelloResult, error)
	HelloResult() client.HelloResult
	SessionStart(context.Context, client.SessionStartParams) (client.SessionStartResult, error)
	SessionResume(context.Context, client.SessionResumeParams) (client.SessionStartResult, error)
	TurnStart(context.Context, client.TurnStartParams) (client.TurnStartResult, <-chan client.TurnNotification, error)
	JobSubmit(context.Context, client.JobSubmitParams) (client.JobSubmitResult, error)
	JobStatus(context.Context, client.JobStatusParams) (client.JobStatusResult, error)
	JobResult(context.Context, client.JobResultParams) (client.JobResult, error)
	JobCancel(context.Context, client.JobCancelParams) (client.JobCancelResult, error)
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
	if hello.ProtocolVersion == 0 {
		var helloErr error
		hello, helloErr = c.Hello(ctx)
		if helloErr != nil {
			_ = c.Close()
			return nil, client.HelloResult{}, helloErr
		}
	}
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
	var required []string
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
