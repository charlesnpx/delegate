package policy

import (
	_ "embed"
	"encoding/json"

	"github.com/charlesnpx/agentbus/engine"
)

const (
	// DelegateReportContractName is the immutable cache name for the embedded v1 report shape.
	DelegateReportContractName = "delegate/delegate-report@1"

	// NoContractFlagReason is stamped by envelope code when CLI policy enforcement is disabled.
	NoContractFlagReason = "no_contract_flag"
)

// CorrectiveRetryTemplate is validated by agentbus before a retry-capable policy is used.
const CorrectiveRetryTemplate = `The previous report missed these delegate-report requirements: {{missing}}.

Make no further changes. Emit the corrected report only.`

//go:embed delegate_report_spec.json
var delegateReportSpecJSON []byte

//go:embed digest/delegate-contract.md
var delegateContractDigest string

// Flags are the CLI switches that determine delegate's task policy tier.
type Flags struct {
	Write          bool
	StrictContract bool
	NoContract     bool
}

// DelegateReportSpec returns a fresh copy of the embedded concrete report contract.
func DelegateReportSpec() (engine.ContractSpec, error) {
	var spec engine.ContractSpec
	if err := json.Unmarshal(delegateReportSpecJSON, &spec); err != nil {
		return engine.ContractSpec{}, err
	}
	return spec, nil
}

// DelegateReportSpecJSON returns a copy of the embedded declarative contract data.
func DelegateReportSpecJSON() []byte {
	return append([]byte(nil), delegateReportSpecJSON...)
}

// DelegateContractDigest returns the embedded delegate-contract prompt digest.
func DelegateContractDigest() string {
	return delegateContractDigest
}

// RegisterDelegateReport stores the embedded delegate-report shape under its immutable name.
func RegisterDelegateReport(registry *engine.PolicyRegistry) (string, error) {
	spec, err := DelegateReportSpec()
	if err != nil {
		return "", err
	}
	return registry.Register(DelegateReportContractName, spec)
}

// ResolveTurnPolicy converts CLI policy flags into the engine policy for a task turn.
func ResolveTurnPolicy(flags Flags) (*engine.TurnPolicy, error) {
	if flags.NoContract {
		return nil, nil
	}
	spec, err := DelegateReportSpec()
	if err != nil {
		return nil, err
	}
	policy := &engine.TurnPolicy{
		Prologue: DelegateContractDigest(),
		Contract: &spec,
	}
	if flags.Write || flags.StrictContract {
		policy.Retry = &engine.RetryPolicy{
			Max:      1,
			Template: CorrectiveRetryTemplate,
		}
	}
	return policy, nil
}

// DisabledStamp returns the engine-compatible contract stamp for --no-contract envelopes.
func DisabledStamp() engine.ContractStamp {
	stamp := engine.DisabledContractStamp()
	stamp.Reason = NoContractFlagReason
	return stamp
}
