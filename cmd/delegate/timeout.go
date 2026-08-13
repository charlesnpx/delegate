package main

import (
	"encoding/json"
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/delegate/internal/config"
)

// daemonTimeoutResolution validates a timeout field that is present on a
// delegate-side response shape. Older typed Agentbus response types omit that
// field, so production callers obtain it from timeoutCapturingClient instead.
func daemonTimeoutResolution(response any) (config.DimensionResolution, bool) {
	raw, err := json.Marshal(response)
	if err != nil {
		return config.DimensionResolution{}, false
	}
	var envelope struct {
		Timeout json.RawMessage `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return config.DimensionResolution{}, false
	}
	return timeoutResolutionFromWire(envelope.Timeout)
}

func timeoutResolutionFromWire(raw json.RawMessage) (config.DimensionResolution, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return config.DimensionResolution{}, false
	}
	var timeout struct {
		Requested *int64  `json:"requested,omitempty"`
		Effective *int64  `json:"effective"`
		Source    *string `json:"source"`
	}
	if err := json.Unmarshal(raw, &timeout); err != nil || timeout.Effective == nil || timeout.Source == nil {
		return config.DimensionResolution{}, false
	}
	if *timeout.Effective < 0 || (timeout.Requested != nil && *timeout.Requested < 0) {
		return config.DimensionResolution{}, false
	}
	source, valid := timeoutEnvelopeSource(*timeout.Source)
	if !valid {
		return config.DimensionResolution{}, false
	}
	if (source == "flag") != (timeout.Requested != nil) {
		return config.DimensionResolution{}, false
	}
	resolution := config.DimensionResolution{
		Effective: timeoutDurationString(*timeout.Effective),
		Source:    source,
	}
	if timeout.Requested != nil {
		resolution.Requested = timeoutDurationString(*timeout.Requested)
	}
	return resolution, true
}

type daemonTimeoutResolutionProvider interface {
	submittedTimeoutResolution(jobID string) (config.DimensionResolution, bool)
	resultTimeoutResolution(jobID string) (config.DimensionResolution, bool)
	statusTimeoutResolution(jobID string) (config.DimensionResolution, bool)
}

func timeoutResolutionForSubmission(requested time.Duration, requestedSet bool, submitted any, clients ...agentbusClient) config.DimensionResolution {
	var resolution config.DimensionResolution
	ok := false
	if len(clients) > 0 {
		if provider, supported := clients[0].(daemonTimeoutResolutionProvider); supported {
			switch result := submitted.(type) {
			case client.JobSubmitResult:
				resolution, ok = provider.submittedTimeoutResolution(result.JobID)
			}
		}
	}
	if !ok {
		resolution, ok = daemonTimeoutResolution(submitted)
	}
	if ok {
		if requestedSet {
			resolution.Requested = requested.String()
		}
		return resolution
	}
	return config.DimensionResolution{Requested: requestedTimeoutValue(requested, requestedSet), Source: "unknown"}
}

func timeoutEnvelopeSource(source string) (string, bool) {
	switch source {
	case "client", "flag":
		return "flag", true
	case "daemon_default", "daemon":
		return "daemon", true
	default:
		return "", false
	}
}

func timeoutDurationString(millis int64) string {
	return (time.Duration(millis) * time.Millisecond).String()
}

func timeoutResolutionIsResolved(resolution config.DimensionResolution) bool {
	return resolution.Effective != "" && resolution.Source != "" && resolution.Source != "unknown"
}
