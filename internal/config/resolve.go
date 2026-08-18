package config

// DimensionResolution records a requested flag value, effective value, and the
// layer that selected it.
type DimensionResolution struct {
	Requested string `json:"requested,omitempty"`
	Effective string `json:"effective,omitempty"`
	Source    string `json:"source"`
	// Validated is false when a non-empty Agentbus advertised set did not
	// contain Effective. It is omitted for compatible values and unknown sets
	// to preserve the legacy resolution shape.
	Validated *bool `json:"validated,omitempty"`
}

// ModelEffortResolution is the independent model and effort resolution result.
type ModelEffortResolution struct {
	Model  DimensionResolution `json:"model"`
	Effort DimensionResolution `json:"effort"`
}

// ResolveModelEffort applies user defaults independently for model and effort.
// A non-overridable configuration only locks a dimension it actually defines.
func ResolveModelEffort(backend, flagModel, flagEffort string, cfg Config) ModelEffortResolution {
	defaults := cfg.DefaultsFor(backend)
	return ModelEffortResolution{
		Model:  resolveDimension(flagModel, defaults.Model, cfg.Overridable),
		Effort: resolveDimension(flagEffort, defaults.Effort, cfg.Overridable),
	}
}

func resolveDimension(flagValue, configured string, overridable bool) DimensionResolution {
	result := DimensionResolution{Requested: flagValue}
	if flagValue != "" && (overridable || configured == "") {
		result.Effective = flagValue
		result.Source = "flag"
		return result
	}
	if configured != "" {
		result.Effective = configured
		if flagValue != "" && !overridable {
			result.Source = "config-locked"
		} else {
			result.Source = "config"
		}
		return result
	}
	result.Source = "default"
	return result
}
