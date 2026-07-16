package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// productionSessionNudgeAuthority is the lifecycle-safe capability presented
// by one fully recovered city authority binding. The API adapter deliberately
// cannot construct, recover, close, or reach effects through this interface.
type productionSessionNudgeAuthority interface {
	RequesterScope() (tenantScope, cityScope, credentialClass string)
	Admit(context.Context, nudgequeue.NudgeIngressRequest) (nudgequeue.NudgeIngressResult, error)
}

type productionSessionNudgeAuthorityResolver func(string) (productionSessionNudgeAuthority, bool)

type productionSessionNudgeAdmission struct {
	resolve productionSessionNudgeAuthorityResolver
}

var _ api.SessionNudgeAdmission = (*productionSessionNudgeAdmission)(nil)

func newProductionSessionNudgeAdmission(resolve productionSessionNudgeAuthorityResolver) *productionSessionNudgeAdmission {
	return &productionSessionNudgeAdmission{resolve: resolve}
}

func installSupervisorProductionNudgeAdmission(mux *api.SupervisorMux, registry *cityRegistry) {
	mux.WithSessionNudgeAdmission(newProductionSessionNudgeAdmission(registry.resolveProductionNudgeAuthority))
}

func (a *productionSessionNudgeAdmission) AdmitSessionNudge(
	ctx context.Context,
	city string,
	request nudgequeue.NudgeIngressRequest,
) (nudgequeue.NudgeIngressResult, error) {
	grant, ok := api.VerifiedCityWriteGrantFromContext(ctx)
	if !ok {
		return nudgequeue.NudgeIngressResult{}, fmt.Errorf("%w: verified city write grant is required", nudgequeue.ErrNudgeAuthorizationDenied)
	}
	if err := validateVerifiedNudgeGrantCity(grant, city); err != nil {
		return nudgequeue.NudgeIngressResult{}, err
	}
	if a == nil || a.resolve == nil {
		return nudgequeue.NudgeIngressResult{}, fmt.Errorf("%w: production nudge authority is not configured", nudgequeue.ErrLocalNudgeAuthorityUnavailable)
	}
	authority, live := a.resolve(city)
	if !live || isNilProductionSessionNudgeAuthority(authority) {
		return nudgequeue.NudgeIngressResult{}, fmt.Errorf("%w: production nudge authority is not live", nudgequeue.ErrLocalNudgeAuthorityUnavailable)
	}
	tenantScope, cityScope, credentialClass := authority.RequesterScope()
	requester, err := authenticatedNudgeRequesterFromVerifiedGrant(
		grant,
		city,
		tenantScope,
		cityScope,
		credentialClass,
	)
	if err != nil {
		return nudgequeue.NudgeIngressResult{}, err
	}
	return authority.Admit(nudgequeue.WithAuthenticatedNudgeRequester(ctx, requester), request)
}

func authenticatedNudgeRequesterFromVerifiedGrant(
	grant citywriteauth.Grant,
	routeCity string,
	tenantScope string,
	cityScope string,
	credentialClass string,
) (nudgequeue.AuthenticatedNudgeRequester, error) {
	if err := validateVerifiedNudgeGrantCity(grant, routeCity); err != nil {
		return nudgequeue.AuthenticatedNudgeRequester{}, err
	}
	if tenantScope == "" || cityScope == "" || credentialClass == "" {
		return nudgequeue.AuthenticatedNudgeRequester{}, fmt.Errorf("%w: production nudge requester scope is incomplete", nudgequeue.ErrLocalNudgeAuthorityUnavailable)
	}
	return nudgequeue.AuthenticatedNudgeRequester{
		PrincipalID:     grant.Kid,
		TenantScope:     tenantScope,
		CityScope:       cityScope,
		CredentialClass: credentialClass,
		EvidenceID:      grant.JTI,
	}, nil
}

func validateVerifiedNudgeGrantCity(grant citywriteauth.Grant, routeCity string) error {
	if routeCity == "" || grant.City != routeCity || grant.Kid == "" || grant.JTI == "" {
		return fmt.Errorf("%w: verified city write grant does not cover the requested city", nudgequeue.ErrNudgeAuthorizationDenied)
	}
	return nil
}

func isNilProductionSessionNudgeAuthority(authority productionSessionNudgeAuthority) bool {
	if authority == nil {
		return true
	}
	value := reflect.ValueOf(authority)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
