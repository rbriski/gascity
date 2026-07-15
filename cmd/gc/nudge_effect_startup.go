package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
)

var errNudgeEffectStartupRefused = errors.New("keyed nudge effect ownership startup refused")

// nudgeEffectStartupCapabilities is the complete mechanical evidence required
// before a process may become the durable keyed nudge effect owner. Booleans
// are evidence supplied by the production composition, never inferred from a
// command's requester fields or from possession of store credentials.
type nudgeEffectStartupCapabilities struct {
	SupervisorDispatcher         bool
	AtomicCommandRepository      bool
	TrustedIngress               bool
	IndependentAuthority         bool
	TrustedCityPartition         bool
	TrustedCityPartitionResolver bool
	ClaimAuthorizer              bool
	ProviderEffect               bool
	CommandSecurity              nudgequeue.CommandSecurityCapabilities
}

type nudgeEffectStartupSelection struct {
	Ownership nudgeEffectOwnership
	Notice    string
}

// nudgeEffectStartupRefusal is the typed startup failure retained by both the
// standalone controller and supervisor initialization paths.
type nudgeEffectStartupRefusal struct {
	Mode    rollout.Mode
	Profile nudgequeue.CommandSecurityProfile
	Reason  string
	cause   error
}

func (e *nudgeEffectStartupRefusal) Error() string {
	if e == nil {
		return errNudgeEffectStartupRefused.Error()
	}
	return fmt.Sprintf("%s: mode=%s profile=%q: %s", errNudgeEffectStartupRefused, e.Mode, e.Profile, e.Reason)
}

func (e *nudgeEffectStartupRefusal) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{errNudgeEffectStartupRefused}
	}
	return []error{errNudgeEffectStartupRefused, e.cause}
}

// selectNudgeEffectOwnership cold-selects exactly one provider-effect owner.
// Off remains zero-cost and silent because ResolveCapability never invokes the
// supplied capability predicate for Off or ModeUnset.
func selectNudgeEffectOwnership(
	ctx context.Context,
	flags rollout.Flags,
	profileRaw string,
	loadCapabilities func(context.Context) nudgeEffectStartupCapabilities,
) (nudgeEffectStartupSelection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	profile := nudgequeue.CommandSecurityProfile(profileRaw)
	var profileStatus nudgequeue.CommandSecurityStatus
	var hardRefusal error

	decision, reason := rollout.ResolveCapability(ctx, flags.NudgeEffectOwner(), func(ctx context.Context) (bool, string) {
		var capabilities nudgeEffectStartupCapabilities
		if loadCapabilities != nil {
			capabilities = loadCapabilities(ctx)
		}

		status, err := nudgequeue.CheckCommandSecurityProfile(profile, capabilities.CommandSecurity)
		if err != nil {
			hardRefusal = err
			return false, err.Error()
		}
		profileStatus = status
		if profile != nudgequeue.CommandSecurityProfileStoreWriterIsController {
			hardRefusal = fmt.Errorf("%w: hosted keyed ownership is unavailable in this production composition", nudgequeue.ErrCommandSecurityProfile)
			return false, hardRefusal.Error()
		}
		if loadCapabilities == nil {
			return false, "startup capability evidence is unavailable"
		}
		return capabilities.complete()
	})

	if hardRefusal != nil {
		return nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(flags.NudgeEffectOwner(), profile, reason, hardRefusal)
	}
	switch decision {
	case rollout.UseLegacy:
		return nudgeEffectStartupSelection{Ownership: nudgeEffectOwnershipLegacy}, nil
	case rollout.UseNew:
		return nudgeEffectStartupSelection{
			Ownership: nudgeEffectOwnershipKeyed,
			Notice:    profileStatus.Warning,
		}, nil
	case rollout.DegradeLoud:
		return nudgeEffectStartupSelection{
			Ownership: nudgeEffectOwnershipLegacy,
			Notice:    "keyed nudge effect ownership unavailable; retaining legacy owner: " + reason,
		}, nil
	case rollout.RefuseClosed:
		return nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(flags.NudgeEffectOwner(), profile, reason, nil)
	default:
		return nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(
			flags.NudgeEffectOwner(),
			profile,
			"unknown rollout decision "+string(decision),
			nil,
		)
	}
}

func newNudgeEffectStartupRefusal(
	mode rollout.Mode,
	profile nudgequeue.CommandSecurityProfile,
	reason string,
	cause error,
) *nudgeEffectStartupRefusal {
	return &nudgeEffectStartupRefusal{
		Mode:    mode,
		Profile: profile,
		Reason:  reason,
		cause:   cause,
	}
}

func (c nudgeEffectStartupCapabilities) complete() (bool, string) {
	missing := make([]string, 0, 8)
	if !c.SupervisorDispatcher {
		missing = append(missing, "supervisor nudge dispatcher")
	}
	if !c.AtomicCommandRepository {
		missing = append(missing, "atomic command repository")
	}
	if !c.TrustedIngress {
		missing = append(missing, "trusted command ingress")
	}
	if !c.IndependentAuthority {
		missing = append(missing, "independently durable authority")
	}
	if !c.TrustedCityPartition {
		missing = append(missing, "trusted city partition")
	}
	if !c.TrustedCityPartitionResolver {
		missing = append(missing, "trusted city partition resolver")
	}
	if !c.ClaimAuthorizer {
		missing = append(missing, "claim authorizer")
	}
	if !c.ProviderEffect {
		missing = append(missing, "runtime nudge effect provider")
	}
	if len(missing) != 0 {
		return false, "missing " + strings.Join(missing, ", ")
	}
	return true, "all keyed nudge effect ownership dependencies are available"
}

// currentProductionNudgeEffectStartupCapabilities reports only capabilities
// the current production composition can prove. The protected command
// namespace, trusted ingress, claim authority, opaque partition, and
// independent authority journal are not wired yet, so they remain false. This
// makes auto retain legacy and require refuse until those dependencies land.
func currentProductionNudgeEffectStartupCapabilities(cfg *config.City, sp runtime.Provider) nudgeEffectStartupCapabilities {
	return nudgeEffectStartupCapabilities{
		SupervisorDispatcher: nudgeDispatcherIsSupervisor(cfg),
		ProviderEffect:       supportsNudgeEffectProvider(sp),
		CommandSecurity: nudgequeue.CommandSecurityCapabilities{
			ProtectedNamespace:              false,
			WorkCredentialsCanWriteCommands: true,
			TrustedIngressAvailable:         false,
			ClaimAuthorizationAvailable:     false,
		},
	}
}

func supportsNudgeEffectProvider(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	value := reflect.ValueOf(sp)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return false
		}
	}
	_, ok := sp.(runtime.NudgeEffectProvider)
	return ok
}
