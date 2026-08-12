package main

import (
	"encoding/json"
	"time"

	"github.com/charlesnpx/delegate/internal/config"
)

// daemonTimeoutResolution reads the additive daemon response through JSON so
// this client remains compatible with older Agentbus libraries that do not yet
// declare the Timeout field. Once the client library declares it, its normal
// JSON decoding preserves the field and this adapter reads the same wire shape.
func daemonTimeoutResolution(response any) (config.DimensionResolution, bool) {
	raw, err := json.Marshal(response)
	if err != nil {
		return config.DimensionResolution{}, false
	}
	var envelope struct {
		Timeout *struct {
			Requested *int64 `json:"requested,omitempty"`
			Effective int64  `json:"effective"`
			Source    string `json:"source"`
		} `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Timeout == nil || envelope.Timeout.Source == "" {
		return config.DimensionResolution{}, false
	}
	resolution := config.DimensionResolution{
		Effective: timeoutDurationString(envelope.Timeout.Effective),
		Source:    timeoutEnvelopeSource(envelope.Timeout.Source),
	}
	if envelope.Timeout.Requested != nil {
		resolution.Requested = timeoutDurationString(*envelope.Timeout.Requested)
	}
	return resolution, true
}

func timeoutResolutionForSubmission(requested time.Duration, requestedSet bool, submitted any) config.DimensionResolution {
	if resolution, ok := daemonTimeoutResolution(submitted); ok {
		if requestedSet {
			resolution.Requested = requested.String()
		}
		return resolution
	}
	if requestedSet && requested > 0 {
		value := requested.String()
		return config.DimensionResolution{Requested: value, Effective: value, Source: "flag"}
	}
	return config.DimensionResolution{Requested: requestedTimeoutValue(requested, requestedSet), Source: "unknown"}
}

func timeoutEnvelopeSource(source string) string {
	switch source {
	case "client", "flag":
		return "flag"
	case "daemon_default", "daemon":
		return "daemon"
	default:
		return source
	}
}

func timeoutDurationString(millis int64) string {
	return (time.Duration(millis) * time.Millisecond).String()
}
