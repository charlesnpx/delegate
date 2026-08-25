package main

import (
	"time"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/delegate/internal/config"
)

// timeoutResolutionFromAgentbus validates the pinned typed Agentbus timeout
// response and converts it to delegate's stable JSON envelope shape.
func timeoutResolutionFromAgentbus(timeout *engine.TimeoutResolution) (config.DimensionResolution, bool) {
	if timeout == nil || !timeout.Valid() {
		return config.DimensionResolution{}, false
	}
	source, valid := timeoutEnvelopeSource(timeout.Source)
	if !valid {
		return config.DimensionResolution{}, false
	}
	resolution := config.DimensionResolution{
		Effective: timeoutDurationString(timeout.Effective),
		Source:    source,
	}
	if timeout.Requested != nil {
		resolution.Requested = timeoutDurationString(*timeout.Requested)
	}
	return resolution, true
}

func timeoutResolutionForSubmission(requested time.Duration, requestedSet bool, submitted client.JobSubmitResult) config.DimensionResolution {
	resolution, ok := timeoutResolutionFromAgentbus(submitted.Timeout)
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
