package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestAuthenticatedNudgeRequesterFromVerifiedGrantMapsOnlyTrustedEvidence(t *testing.T) {
	grant := citywriteauth.Grant{
		Kid:  "write-key-7",
		Aud:  citywriteauth.AudienceCityWrite,
		City: "city-a",
		CID:  "verified-route-cid",
		JTI:  "grant-evidence-11",
	}

	got, err := authenticatedNudgeRequesterFromVerifiedGrant(
		grant,
		"city-a",
		"binding-tenant",
		"store/4c71",
		"city-write-grant",
	)
	if err != nil {
		t.Fatalf("authenticatedNudgeRequesterFromVerifiedGrant: %v", err)
	}
	want := nudgequeue.AuthenticatedNudgeRequester{
		PrincipalID:     "write-key-7",
		TenantScope:     "binding-tenant",
		CityScope:       "store/4c71",
		CredentialClass: "city-write-grant",
		EvidenceID:      "grant-evidence-11",
	}
	if got != want {
		t.Fatalf("requester = %+v, want %+v", got, want)
	}
}

func TestAuthenticatedNudgeRequesterFromVerifiedGrantRejectsUntrustedOrIncompleteEvidence(t *testing.T) {
	valid := citywriteauth.Grant{Kid: "write-key", City: "city-a", JTI: "grant-1"}
	tests := []struct {
		name            string
		grant           citywriteauth.Grant
		routeCity       string
		tenantScope     string
		cityScope       string
		credentialClass string
		want            error
	}{
		{name: "cross city replay", grant: valid, routeCity: "city-b", tenantScope: "tenant", cityScope: "store/1", credentialClass: "grant", want: nudgequeue.ErrNudgeAuthorizationDenied},
		{name: "missing key principal", grant: citywriteauth.Grant{City: "city-a", JTI: "grant-1"}, routeCity: "city-a", tenantScope: "tenant", cityScope: "store/1", credentialClass: "grant", want: nudgequeue.ErrNudgeAuthorizationDenied},
		{name: "missing evidence id", grant: citywriteauth.Grant{Kid: "write-key", City: "city-a"}, routeCity: "city-a", tenantScope: "tenant", cityScope: "store/1", credentialClass: "grant", want: nudgequeue.ErrNudgeAuthorizationDenied},
		{name: "missing binding tenant", grant: valid, routeCity: "city-a", cityScope: "store/1", credentialClass: "grant", want: nudgequeue.ErrLocalNudgeAuthorityUnavailable},
		{name: "missing binding city", grant: valid, routeCity: "city-a", tenantScope: "tenant", credentialClass: "grant", want: nudgequeue.ErrLocalNudgeAuthorityUnavailable},
		{name: "missing binding credential class", grant: valid, routeCity: "city-a", tenantScope: "tenant", cityScope: "store/1", want: nudgequeue.ErrLocalNudgeAuthorityUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := authenticatedNudgeRequesterFromVerifiedGrant(
				test.grant,
				test.routeCity,
				test.tenantScope,
				test.cityScope,
				test.credentialClass,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestProductionSessionNudgeAdmissionMissingVerifiedGrantFailsBeforeResolution(t *testing.T) {
	resolverCalls := 0
	admission := newProductionSessionNudgeAdmission(func(string) (productionSessionNudgeAuthority, bool) {
		resolverCalls++
		return &recordingProductionSessionNudgeAuthority{}, true
	})

	_, err := admission.AdmitSessionNudge(t.Context(), "city-a", validProductionNudgeRequest(time.Now().UTC()))
	if !errors.Is(err, nudgequeue.ErrNudgeAuthorizationDenied) {
		t.Fatalf("AdmitSessionNudge error = %v, want ErrNudgeAuthorizationDenied", err)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0 before authenticated evidence", resolverCalls)
	}
}

func TestProductionSessionNudgeAdmissionRoutesVerifiedGrantToLiveBinding(t *testing.T) {
	binding := &recordingProductionSessionNudgeAuthority{
		tenantScope:     "local-tenant",
		cityScope:       "store/verified-uuid",
		credentialClass: "city-write-grant",
		result: nudgequeue.NudgeIngressResult{Entry: nudgequeue.CommandIndexEntry{Command: &nudgequeue.Command{
			ID:    "nudge-command-1",
			State: nudgequeue.CommandStatePending,
		}}, Created: true},
	}
	resolverCalls := 0
	harness := newProductionNudgeAdmissionHTTPHarness(t, func(city string) (productionSessionNudgeAuthority, bool) {
		resolverCalls++
		if city != "city-a" {
			t.Fatalf("resolver city = %q, want city-a", city)
		}
		return binding, true
	}, true)

	response := harness.serve(t, "city-a", "write-key", "verified-grant-1")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want 202", response.Code, response.Body.String())
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
	if got := binding.admissionCount(); got != 1 {
		t.Fatalf("binding admission calls = %d, want 1", got)
	}
	if binding.request.Source != nudgequeue.CommandSourceSession || binding.request.Target.SessionID != "session-1" {
		t.Fatalf("binding request = %+v, want typed session nudge", binding.request)
	}
}

func TestProductionSessionNudgeAdmissionUnavailableProfilesFailBeforeDurableAdmission(t *testing.T) {
	var typedNil *recordingProductionSessionNudgeAuthority
	tests := []struct {
		name    string
		resolve productionSessionNudgeAuthorityResolver
	}{
		{name: "off", resolve: nil},
		{name: "auto unavailable", resolve: func(string) (productionSessionNudgeAuthority, bool) { return nil, false }},
		{name: "unsupported", resolve: func(string) (productionSessionNudgeAuthority, bool) { return nil, false }},
		{name: "typed nil", resolve: func(string) (productionSessionNudgeAuthority, bool) { return typedNil, true }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newProductionNudgeAdmissionHTTPHarness(t, test.resolve, true)
			response := harness.serve(t, "city-a", "write-key", "grant-"+test.name)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d body=%s, want 503", response.Code, response.Body.String())
			}
		})
	}
}

func TestProductionSessionNudgeAdmissionWithoutWriteAuthFailsClosed(t *testing.T) {
	binding := &recordingProductionSessionNudgeAuthority{
		tenantScope: "tenant", cityScope: "store/uuid", credentialClass: "grant",
	}
	harness := newProductionNudgeAdmissionHTTPHarness(t, func(string) (productionSessionNudgeAuthority, bool) {
		return binding, true
	}, false)

	response := harness.serveWithoutGrant(t)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", response.Code, response.Body.String())
	}
	if got := binding.admissionCount(); got != 0 {
		t.Fatalf("binding admission calls = %d, want 0", got)
	}
}

type recordingProductionSessionNudgeAuthority struct {
	mu              sync.Mutex
	tenantScope     string
	cityScope       string
	credentialClass string
	request         nudgequeue.NudgeIngressRequest
	result          nudgequeue.NudgeIngressResult
	err             error
	admissions      int
}

func (a *recordingProductionSessionNudgeAuthority) RequesterScope() (string, string, string) {
	if a == nil {
		return "", "", ""
	}
	return a.tenantScope, a.cityScope, a.credentialClass
}

func (a *recordingProductionSessionNudgeAuthority) Admit(
	_ context.Context,
	request nudgequeue.NudgeIngressRequest,
) (nudgequeue.NudgeIngressResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.admissions++
	a.request = request
	return a.result, a.err
}

func (a *recordingProductionSessionNudgeAuthority) admissionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admissions
}

type productionNudgeAdmissionHTTPHarness struct {
	handler http.Handler
	priv    ed25519.PrivateKey
	body    []byte
}

func newProductionNudgeAdmissionHTTPHarness(
	t *testing.T,
	resolve productionSessionNudgeAuthorityResolver,
	installWriteAuth bool,
) productionNudgeAdmissionHTTPHarness {
	return newProductionNudgeAdmissionHTTPHarnessWithMuxSetup(t, func(mux *api.SupervisorMux) {
		mux.WithSessionNudgeAdmission(newProductionSessionNudgeAdmission(resolve))
	}, installWriteAuth)
}

func newProductionNudgeAdmissionHTTPHarnessWithMuxSetup(
	t *testing.T,
	setup func(*api.SupervisorMux),
	installWriteAuth bool,
) productionNudgeAdmissionHTTPHarness {
	t.Helper()
	const cityName = "city-a"
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: cityName}}
	state := newControllerStateWithMemoryStore(t, cfg, cityName, cityPath)
	mux := api.NewSupervisorMux(&singleCityStateResolver{state: state}, nil, false, "controller", "test", time.Now())
	mux.WithAnyHostAllowed()
	setup(mux)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate write-auth key: %v", err)
	}
	if installWriteAuth {
		keyConfig := "write-key:" + base64.StdEncoding.EncodeToString(pub)
		if err := api.InstallWriteAuth(mux, keyConfig, false, api.WriteAuthBindContext{}); err != nil {
			t.Fatalf("InstallWriteAuth: %v", err)
		}
	}

	body, err := json.Marshal(api.SessionNudgeAdmissionRequestBody{
		RequestID:            "request-1",
		Mode:                 nudgequeue.DeliveryModeQueue,
		IntentGeneration:     1,
		ContinuationIdentity: "continuation-1",
		TargetPolicy:         nudgequeue.TargetPolicyContinuation,
		Message:              "new work is ready",
		ExpiresAt:            time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("marshal nudge body: %v", err)
	}
	return productionNudgeAdmissionHTTPHarness{handler: mux.Handler(), priv: priv, body: body}
}

func newControllerStateWithMemoryStore(t *testing.T, cfg *config.City, cityName, cityPath string) *controllerState {
	t.Helper()
	previous := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	defer func() { newControllerStateOpenCityStore = previous }()
	return newControllerState(t.Context(), cfg, runtime.NewFake(), events.NewFake(), cityName, cityPath)
}

func (h productionNudgeAdmissionHTTPHarness) serve(t *testing.T, city, kid, evidenceID string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v0/city/" + city + "/session/session-1/nudges"
	now := time.Now().UTC()
	grant := citywriteauth.Grant{
		Kid:  kid,
		Aud:  citywriteauth.AudienceCityWrite,
		City: city,
		IAT:  now.Unix(),
		Exp:  now.Add(time.Minute).Unix(),
		JTI:  evidenceID,
		Req:  citywriteauth.ReqDigest(http.MethodPost, path, "", h.body),
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	signature := ed25519.Sign(h.priv, payload)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)

	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(h.body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GC-Request", "test")
	request.Header.Set("X-GC-City-Write", token)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func (h productionNudgeAdmissionHTTPHarness) serveWithoutGrant(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v0/city/city-a/session/session-1/nudges"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(h.body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GC-Request", "test")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func validProductionNudgeRequest(now time.Time) nudgequeue.NudgeIngressRequest {
	return nudgequeue.NudgeIngressRequest{
		RequestID: "request-1",
		Mode:      nudgequeue.DeliveryModeQueue,
		Target: nudgequeue.CommandTarget{
			SessionID:            "session-1",
			IntentGeneration:     1,
			ContinuationIdentity: "continuation-1",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Source:    nudgequeue.CommandSourceSession,
		Message:   "new work is ready",
		ExpiresAt: now.Add(time.Minute),
	}
}
