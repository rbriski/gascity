package nudgequeue

import (
	"errors"
	"fmt"
)

// ErrCommandSecurityProfile reports that a deployment profile cannot safely
// admit protected command effects.
var ErrCommandSecurityProfile = errors.New("durable command security profile is not effect-safe")

// CommandSecurityProfile names the closed set of durable-command trust modes.
// The zero value is invalid so store writers never gain control implicitly.
type CommandSecurityProfile string

const (
	// CommandSecurityProfileHosted requires an independently protected command
	// namespace, separated work credentials, and both authorization boundaries.
	CommandSecurityProfileHosted CommandSecurityProfile = "hosted"
	// CommandSecurityProfileStoreWriterIsController is the explicit local-only
	// compatibility mode in which every store writer has command authority.
	CommandSecurityProfileStoreWriterIsController CommandSecurityProfile = "store_writer_is_controller"
)

// CommandSecurityCapabilities is startup evidence about the concrete store and
// authorization wiring. It contains no tenant or city identity and therefore
// cannot mint a trusted command partition.
type CommandSecurityCapabilities struct {
	ProtectedNamespace              bool
	WorkCredentialsCanWriteCommands bool
	TrustedIngressAvailable         bool
	ClaimAuthorizationAvailable     bool
}

// CommandSecurityStatus is the accepted profile result surfaced to operators.
type CommandSecurityStatus struct {
	Warning string
}

// CheckCommandSecurityProfile fails closed unless the selected profile's
// concrete credential and authorization capabilities are proven. It never
// enables an effect; callers use the result only as one admission prerequisite.
func CheckCommandSecurityProfile(profile CommandSecurityProfile, capabilities CommandSecurityCapabilities) (CommandSecurityStatus, error) {
	switch profile {
	case CommandSecurityProfileHosted:
		if !capabilities.ProtectedNamespace {
			return CommandSecurityStatus{}, fmt.Errorf("%w: hosted profile has no protected command namespace", ErrCommandSecurityProfile)
		}
		if capabilities.WorkCredentialsCanWriteCommands {
			return CommandSecurityStatus{}, fmt.Errorf("%w: hosted work credentials can write protected commands", ErrCommandSecurityProfile)
		}
		if !capabilities.TrustedIngressAvailable {
			return CommandSecurityStatus{}, fmt.Errorf("%w: hosted trusted ingress is unavailable", ErrCommandSecurityProfile)
		}
		if !capabilities.ClaimAuthorizationAvailable {
			return CommandSecurityStatus{}, fmt.Errorf("%w: hosted claim authorization is unavailable", ErrCommandSecurityProfile)
		}
		return CommandSecurityStatus{}, nil
	case CommandSecurityProfileStoreWriterIsController:
		return CommandSecurityStatus{Warning: "local single-tenant profile: every store credential holder has full session-control authority"}, nil
	default:
		return CommandSecurityStatus{}, fmt.Errorf("%w: profile %q is not an explicit supported mode", ErrCommandSecurityProfile, profile)
	}
}
