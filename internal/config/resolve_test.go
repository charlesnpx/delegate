package config

import "testing"

func TestResolveModelEffortMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		cfg                                     Config
		flagModel, flagEffort                   string
		wantModel, wantModelSource              string
		wantEffort, wantEffortSource            string
		wantRequestedModel, wantRequestedEffort string
	}{
		{
			name:            "default",
			cfg:             Config{Overridable: true},
			wantModelSource: "default", wantEffortSource: "default",
		},
		{
			name:      "config",
			cfg:       Config{Overridable: true, Backend: Backends{Claude: Defaults{Model: "configured-model", Effort: "configured-effort"}}},
			wantModel: "configured-model", wantModelSource: "config",
			wantEffort: "configured-effort", wantEffortSource: "config",
		},
		{
			name:      "overridable flag wins",
			cfg:       Config{Overridable: true, Backend: Backends{Claude: Defaults{Model: "configured-model", Effort: "configured-effort"}}},
			flagModel: "flag-model", flagEffort: "flag-effort",
			wantModel: "flag-model", wantModelSource: "flag", wantRequestedModel: "flag-model",
			wantEffort: "flag-effort", wantEffortSource: "flag", wantRequestedEffort: "flag-effort",
		},
		{
			name:      "locked configured dimensions",
			cfg:       Config{Overridable: false, Backend: Backends{Claude: Defaults{Model: "configured-model", Effort: "configured-effort"}}},
			flagModel: "flag-model", flagEffort: "flag-effort",
			wantModel: "configured-model", wantModelSource: "config-locked", wantRequestedModel: "flag-model",
			wantEffort: "configured-effort", wantEffortSource: "config-locked", wantRequestedEffort: "flag-effort",
		},
		{
			name:      "locked empty model falls through independently",
			cfg:       Config{Overridable: false, Backend: Backends{Claude: Defaults{Effort: "configured-effort"}}},
			flagModel: "flag-model", flagEffort: "flag-effort",
			wantModel: "flag-model", wantModelSource: "flag", wantRequestedModel: "flag-model",
			wantEffort: "configured-effort", wantEffortSource: "config-locked", wantRequestedEffort: "flag-effort",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveModelEffort("claude", tc.flagModel, tc.flagEffort, tc.cfg)
			if got.Model.Effective != tc.wantModel || got.Model.Source != tc.wantModelSource || got.Model.Requested != tc.wantRequestedModel {
				t.Fatalf("model = %#v", got.Model)
			}
			if got.Effort.Effective != tc.wantEffort || got.Effort.Source != tc.wantEffortSource || got.Effort.Requested != tc.wantRequestedEffort {
				t.Fatalf("effort = %#v", got.Effort)
			}
		})
	}
}
