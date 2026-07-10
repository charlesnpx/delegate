package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/charlesnpx/agentbus/client"
)

const stopReviewGateLine = "stop-review-gate: not available (planned v0.2)"

type setupJSON struct {
	Schema         int           `json:"schema"`
	Delegate       string        `json:"delegate"`
	Agentbus       setupAgentbus `json:"agentbus"`
	StopReviewGate string        `json:"stop_review_gate"`
}

type setupAgentbus struct {
	Path            string          `json:"path"`
	Version         string          `json:"version,omitempty"`
	ProtocolVersion int             `json:"protocolVersion"`
	Backends        []string        `json:"backends"`
	Capabilities    map[string]bool `json:"capabilities"`
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
	if *jsonOut {
		return 0, writeJSONLine(stdout, setupJSON{
			Schema:   envelopeSchema,
			Delegate: versionLine(),
			Agentbus: setupAgentbus{
				Path:            path,
				Version:         version,
				ProtocolVersion: hello.ProtocolVersion,
				Backends:        hello.Backends,
				Capabilities:    hello.Capabilities,
			},
			StopReviewGate: "not available (planned v0.2)",
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
	if _, err := fmt.Fprintf(stdout, "agentbus protocol: %d\ncapabilities: ok\n%s\n", hello.ProtocolVersion, stopReviewGateLine); err != nil {
		return 0, err
	}
	return 0, nil
}
