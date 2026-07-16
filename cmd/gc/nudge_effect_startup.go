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

var (
	errNudgeEffectStartupRefused  = errors.New("keyed nudge effect ownership startup refused")
	errNudgeEffectStartupDegraded = errors.New("keyed nudge effect ownership startup degraded")
)

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
	CommandProducersCovered      bool
	ProviderEffect               bool
	CommandSecurity              nudgequeue.CommandSecurityCapabilities
}

type nudgeEffectStartupSelection struct {
	Ownership  nudgeEffectOwnership
	Notice     string
	Binding    *productionNudgeAuthorityBinding
	Diagnostic error
}

// nudgeEffectStartupDegradation is retained in auto mode so callers can emit
// and observe a typed reason while the legacy owner remains authoritative.
type nudgeEffectStartupDegradation struct {
	Mode    rollout.Mode
	Profile nudgequeue.CommandSecurityProfile
	Reason  string
	cause   error
}

// nudgeEffectStartupRefusal is the typed startup failure retained by both the
// standalone controller and supervisor initialization paths.
type nudgeEffectStartupRefusal struct {
	Mode    rollout.Mode
	Profile nudgequeue.CommandSecurityProfile
	Reason  string
	cause   error
}

// resolveBootRolloutFlags produces the one immutable process-lifetime gate
// snapshot shared by startup selection and controller state.
func resolveBootRolloutFlags(cfg *config.City) (rollout.Flags, error) {
	return rollout.Resolve(cfg, rollout.ResolveOptions{})
}

type productionNudgeAuthorityBindingOpener func(
	context.Context,
	string,
	string,
	rollout.Mode,
) (*productionNudgeAuthorityBinding, error)

// resolveNudgeEffectStartupForCity resolves and retains the production
// authority bundle for one canonical controller-selected city identity.
func resolveNudgeEffectStartupForCity(
	ctx context.Context,
	cfg *config.City,
	sp runtime.Provider,
	cityPath string,
	canonicalCityIdentity string,
) (rollout.Flags, nudgeEffectStartupSelection, error) {
	return resolveNudgeEffectStartupForCityWithOpener(
		ctx,
		cfg,
		sp,
		cityPath,
		canonicalCityIdentity,
		openProductionNudgeAuthorityBinding,
	)
}

func resolveNudgeEffectStartupForCityWithOpener(
	ctx context.Context,
	cfg *config.City,
	sp runtime.Provider,
	cityPath string,
	canonicalCityIdentity string,
	opener productionNudgeAuthorityBindingOpener,
) (rollout.Flags, nudgeEffectStartupSelection, error) {
	flags, err := resolveBootRolloutFlags(cfg)
	if err != nil {
		profile := nudgequeue.CommandSecurityProfile("")
		if cfg != nil {
			profile = nudgequeue.CommandSecurityProfile(cfg.Beads.CommandSecurityProfile)
		}
		return rollout.Flags{}, nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(
			rollout.ModeUnset,
			profile,
			"resolving boot rollout gates: "+err.Error(),
			err,
		)
	}
	mode := flags.NudgeEffectOwner()
	if mode == rollout.ModeUnset || mode == rollout.Off {
		return flags, nudgeEffectStartupSelection{Ownership: nudgeEffectOwnershipLegacy}, nil
	}
	profile := nudgequeue.CommandSecurityProfile("")
	if cfg != nil {
		profile = nudgequeue.CommandSecurityProfile(cfg.Beads.CommandSecurityProfile)
	}
	binding, bindingErr := loadProductionNudgeAuthorityBinding(ctx, mode, profile, func(ctx context.Context) (*productionNudgeAuthorityBinding, error) {
		if opener == nil {
			return nil, errors.New("production nudge authority binding opener is nil")
		}
		return opener(ctx, cityPath, canonicalCityIdentity, flags.BeadsConditionalWrites())
	})
	if bindingErr != nil {
		if errors.Is(bindingErr, errNudgeEffectStartupRefused) {
			return flags, nudgeEffectStartupSelection{}, bindingErr
		}
		reason := "opening production authority binding: " + bindingErr.Error()
		if mode == rollout.Auto {
			diagnostic := newNudgeEffectStartupDegradation(mode, profile, reason, bindingErr)
			return flags, nudgeEffectStartupSelection{
				Ownership:  nudgeEffectOwnershipLegacy,
				Notice:     "keyed nudge effect ownership unavailable; retaining legacy owner: " + reason,
				Diagnostic: diagnostic,
			}, nil
		}
		return flags, nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(mode, profile, reason, bindingErr)
	}
	if binding == nil || !binding.live() {
		bindingErr = errors.New("production authority opener returned an incomplete or closed binding")
		if binding != nil {
			bindingErr = errors.Join(bindingErr, binding.Close())
		}
		reason := bindingErr.Error()
		if mode == rollout.Auto {
			diagnostic := newNudgeEffectStartupDegradation(mode, profile, reason, bindingErr)
			return flags, nudgeEffectStartupSelection{
				Ownership:  nudgeEffectOwnershipLegacy,
				Notice:     "keyed nudge effect ownership unavailable; retaining legacy owner: " + reason,
				Diagnostic: diagnostic,
			}, nil
		}
		return flags, nudgeEffectStartupSelection{}, newNudgeEffectStartupRefusal(mode, profile, reason, bindingErr)
	}

	selection, selectionErr := selectNudgeEffectOwnership(
		ctx,
		flags,
		string(profile),
		func(context.Context) nudgeEffectStartupCapabilities {
			return currentProductionNudgeEffectStartupCapabilities(cfg, sp, binding)
		},
	)
	if selectionErr != nil {
		return flags, nudgeEffectStartupSelection{}, errors.Join(selectionErr, binding.Close())
	}
	if selection.Ownership != nudgeEffectOwnershipKeyed {
		closeErr := binding.Close()
		reason := selection.Notice
		if reason == "" {
			reason = "production startup evidence did not select keyed ownership"
		}
		selection.Diagnostic = newNudgeEffectStartupDegradation(mode, profile, reason, closeErr)
		return flags, selection, nil
	}
	selection.Binding = binding
	return flags, selection, nil
}

func (e *nudgeEffectStartupDegradation) Error() string {
	if e == nil {
		return errNudgeEffectStartupDegraded.Error()
	}
	return fmt.Sprintf("%s: mode=%s profile=%q: %s", errNudgeEffectStartupDegraded, e.Mode, e.Profile, e.Reason)
}

func (e *nudgeEffectStartupDegradation) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{errNudgeEffectStartupDegraded}
	}
	return []error{errNudgeEffectStartupDegraded, e.cause}
}

func newNudgeEffectStartupDegradation(
	mode rollout.Mode,
	profile nudgequeue.CommandSecurityProfile,
	reason string,
	cause error,
) *nudgeEffectStartupDegradation {
	return &nudgeEffectStartupDegradation{Mode: mode, Profile: profile, Reason: reason, cause: cause}
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
	missing := make([]string, 0, 9)
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
	if !c.CommandProducersCovered {
		missing = append(missing, "canonical CLI/API command ingress")
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
// the current production composition can prove. Local authority fields become
// true only for one exact, complete, live binding; absent, duplicated,
// incomplete, or closed evidence leaves them false.
func currentProductionNudgeEffectStartupCapabilities(
	cfg *config.City,
	sp runtime.Provider,
	bindings ...*productionNudgeAuthorityBinding,
) nudgeEffectStartupCapabilities {
	capabilities := nudgeEffectStartupCapabilities{
		SupervisorDispatcher: nudgeDispatcherIsSupervisor(cfg),
		ProviderEffect:       supportsNudgeEffectProvider(sp),
		CommandSecurity: nudgequeue.CommandSecurityCapabilities{
			ProtectedNamespace:              false,
			WorkCredentialsCanWriteCommands: true,
			TrustedIngressAvailable:         false,
			ClaimAuthorizationAvailable:     false,
		},
	}
	if len(bindings) != 1 {
		return capabilities
	}
	completeBinding, commandProducersCovered := bindings[0].startupEvidence()
	if !completeBinding {
		return capabilities
	}
	capabilities.AtomicCommandRepository = true
	capabilities.TrustedIngress = true
	capabilities.IndependentAuthority = true
	capabilities.TrustedCityPartition = true
	capabilities.TrustedCityPartitionResolver = true
	capabilities.ClaimAuthorizer = true
	capabilities.CommandProducersCovered = commandProducersCovered
	capabilities.CommandSecurity.TrustedIngressAvailable = true
	capabilities.CommandSecurity.ClaimAuthorizationAvailable = true
	return capabilities
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
