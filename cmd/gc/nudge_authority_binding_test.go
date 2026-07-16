package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBindProductionNudgeAuthorityKeepsCityIdentitySeparateFromStoreLineage(t *testing.T) {
	store := beads.NewMemStore()
	repository, err := nudgequeue.NewCommandRepository(
		store,
		nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	cityAPath := t.TempDir()
	cityBPath := t.TempDir()
	cityA, err := bindProductionNudgeAuthority(t.Context(), cityAPath, "city-a", store, repository)
	if err != nil {
		t.Fatalf("bind city-a: %v", err)
	}
	cityB, err := bindProductionNudgeAuthority(t.Context(), cityBPath, "city-b", store, repository)
	if err != nil {
		_ = cityA.Close()
		t.Fatalf("bind city-b: %v", err)
	}
	if cityA.repository != repository || cityB.repository != repository {
		t.Fatal("binding rebuilt or substituted the supplied durable repository")
	}
	if cityA.authority == nil || cityA.resolver != cityA.ingress || cityA.claimAuthorizer != cityA.authority || cityA.ingress == nil {
		t.Fatalf("city-a binding is incomplete: %#v", cityA)
	}
	if cityA.source == nil || cityA.source.repository != repository || cityA.source.partition != cityA.partition {
		t.Fatal("city-a source does not reuse the binding repository and partition")
	}
	if cityA.partition == cityB.partition {
		t.Fatal("two canonical cities sharing one store received the same trusted partition")
	}
	if _, cityScope, _ := cityA.RequesterScope(); cityScope != "city-a" {
		t.Fatalf("city-a requester scope = %q, want canonical controller identity", cityScope)
	}
	if _, cityScope, _ := cityB.RequesterScope(); cityScope != "city-b" {
		t.Fatalf("city-b requester scope = %q, want canonical controller identity", cityScope)
	}
	partitionA := cityA.partition
	if err := cityA.Close(); err != nil {
		t.Fatalf("close city-a: %v", err)
	}
	if err := cityB.Close(); err != nil {
		t.Fatalf("close city-b: %v", err)
	}

	reopened, err := bindProductionNudgeAuthority(t.Context(), cityAPath, "city-a", store, repository)
	if err != nil {
		t.Fatalf("reopen city-a: %v", err)
	}
	if reopened.partition != partitionA {
		t.Fatalf("reopened partition = %#v, want stable %#v", reopened.partition, partitionA)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened city-a: %v", err)
	}
	if mismatched, err := bindProductionNudgeAuthority(t.Context(), cityAPath, "renamed-city", store, repository); mismatched != nil || !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityConflict) {
		if mismatched != nil {
			_ = mismatched.Close()
		}
		t.Fatalf("bind renamed city = %#v, err=%v; want journal identity conflict", mismatched, err)
	}
}

func TestProductionNudgeAuthorityBindingCloseOwnsResourcesAndFailsClosed(t *testing.T) {
	store := &bindingCountingStore{MemStore: beads.NewMemStore()}
	repository, err := nudgequeue.NewCommandRepository(
		store,
		nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
	if err != nil {
		t.Fatalf("bindProductionNudgeAuthority: %v", err)
	}
	if err := binding.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := binding.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("store close count = %d, want exactly 1", got)
	}
	if _, err := binding.Admit(context.Background(), nudgequeue.NudgeIngressRequest{}); !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("Admit after Close error = %v, want unavailable", err)
	}
	if binding.live() {
		t.Fatal("closed binding still reports live")
	}
}

func TestOpenProductionNudgeAuthorityBindingClosesStoreOnPartialFailure(t *testing.T) {
	store := &bindingCountingStore{MemStore: beads.NewMemStore()}
	binding, err := openProductionNudgeAuthorityBindingFromStore(
		t.Context(),
		t.TempDir(),
		"invalid city identity",
		store,
	)
	if binding != nil || err == nil {
		t.Fatalf("open invalid identity = %#v, err=%v; want failure", binding, err)
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("store close count after partial failure = %d, want 1", got)
	}
}

func TestOpenProductionNudgeAuthorityBindingSharedStoreWithoutSecondAnchorFailsClosed(t *testing.T) {
	backing := beads.NewMemStore()
	storeA := &bindingCountingStore{MemStore: backing}
	storeB := &bindingCountingStore{MemStore: backing}
	cityA, err := openProductionNudgeAuthorityBindingFromStore(t.Context(), t.TempDir(), "city-a", storeA)
	if err != nil {
		t.Fatalf("open city-a: %v", err)
	}
	t.Cleanup(func() { _ = cityA.Close() })

	cityB, err := openProductionNudgeAuthorityBindingFromStore(t.Context(), t.TempDir(), "city-b", storeB)
	if cityB != nil || !errors.Is(err, nudgequeue.ErrRestoreAnchorAdmission) {
		if cityB != nil {
			_ = cityB.Close()
		}
		t.Fatalf("open city-b without independent anchor = %#v, err=%v; want fail-closed restore-anchor admission", cityB, err)
	}
	if got := storeB.closes.Load(); got != 1 {
		t.Fatalf("refused city-b store close count = %d, want 1", got)
	}
	if !cityA.live() {
		t.Fatal("refusing the second city invalidated the first live binding")
	}
}

func TestProductionNudgeAuthorityBindingAdmitCloseRaceFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
	if err != nil {
		t.Fatalf("bindProductionNudgeAuthority: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = binding.Admit(t.Context(), nudgequeue.NudgeIngressRequest{})
		}()
	}
	close(start)
	if err := binding.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if _, err := binding.Admit(t.Context(), nudgequeue.NudgeIngressRequest{}); !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("Admit after raced Close error = %v, want unavailable", err)
	}
}

func TestLoadProductionNudgeAuthorityBindingOffAndUnsupportedNeverOpenStore(t *testing.T) {
	for _, test := range []struct {
		name        string
		mode        rollout.Mode
		profile     nudgequeue.CommandSecurityProfile
		wantRefusal bool
	}{
		{name: "unset", mode: rollout.ModeUnset, profile: "invalid"},
		{name: "off", mode: rollout.Off, profile: "invalid"},
		{name: "hosted auto", mode: rollout.Auto, profile: nudgequeue.CommandSecurityProfileHosted, wantRefusal: true},
		{name: "hosted require", mode: rollout.Require, profile: nudgequeue.CommandSecurityProfileHosted, wantRefusal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			binding, err := loadProductionNudgeAuthorityBinding(
				t.Context(),
				test.mode,
				test.profile,
				func(context.Context) (*productionNudgeAuthorityBinding, error) {
					called = true
					return nil, errors.New("must not be called")
				},
			)
			if called {
				t.Fatal("off or unsupported profile opened the command store")
			}
			if binding != nil {
				t.Fatalf("binding = %#v, want nil", binding)
			}
			if test.wantRefusal {
				if !errors.Is(err, errNudgeEffectStartupRefused) {
					t.Fatalf("error = %v, want typed startup refusal", err)
				}
			} else if err != nil {
				t.Fatalf("off error = %v", err)
			}
		})
	}
}

func TestResolveNudgeEffectStartupForCityRetainsOnlyCompleteLiveBinding(t *testing.T) {
	localProfile := string(nudgequeue.CommandSecurityProfileStoreWriterIsController)
	cfg := &config.City{
		Beads: config.BeadsConfig{CommandSecurityProfile: localProfile},
		Daemon: config.DaemonConfig{
			NudgeDispatcher:  "supervisor",
			NudgeEffectOwner: "auto",
		},
	}
	store := beads.NewMemStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
	if err != nil {
		t.Fatalf("bindProductionNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = binding.Close() })
	// This test exercises the future full-coverage selection. Production leaves
	// this evidence false until every CLI/API producer uses the durable ingress.
	binding.commandProducersCovered = true

	flags, selection, err := resolveNudgeEffectStartupForCityWithOpener(
		t.Context(), cfg, runtime.NewFake(), "/city/a", "city-a",
		func(context.Context, string, string, rollout.Mode) (*productionNudgeAuthorityBinding, error) {
			return binding, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveNudgeEffectStartupForCityWithOpener: %v", err)
	}
	if flags.NudgeEffectOwner() != rollout.Auto || selection.Ownership != nudgeEffectOwnershipKeyed {
		t.Fatalf("selection = flags %q, %#v; want auto keyed", flags.NudgeEffectOwner(), selection)
	}
	if selection.Binding != binding || !selection.Binding.live() {
		t.Fatal("startup did not retain the exact live authority binding")
	}
	capabilities := currentProductionNudgeEffectStartupCapabilities(cfg, runtime.NewFake(), binding)
	if ok, reason := capabilities.complete(); !ok {
		t.Fatalf("live binding capability evidence incomplete: %s", reason)
	}
}

func TestProductionNudgeAuthorityBindingDoesNotClaimUnwiredCommandProducers(t *testing.T) {
	cfg := &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}}
	store := beads.NewMemStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
	if err != nil {
		t.Fatalf("bindProductionNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = binding.Close() })

	capabilities := currentProductionNudgeEffectStartupCapabilities(cfg, runtime.NewFake(), binding)
	if capabilities.CommandProducersCovered {
		t.Fatal("production binding claimed unwired CLI/API command producers")
	}
	if ok, reason := capabilities.complete(); ok || !strings.Contains(reason, "canonical CLI/API command ingress") {
		t.Fatalf("capability completeness = %t, %q; want precise producer-coverage refusal", ok, reason)
	}
}

func TestResolveNudgeEffectStartupForCityUnwiredProducersKeepLegacyOrRefuse(t *testing.T) {
	newBinding := func(t *testing.T) *productionNudgeAuthorityBinding {
		t.Helper()
		store := beads.NewMemStore()
		repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()))
		if err != nil {
			t.Fatalf("NewCommandRepository: %v", err)
		}
		if _, err := repository.Provision(t.Context()); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
		if err != nil {
			t.Fatalf("bindProductionNudgeAuthority: %v", err)
		}
		return binding
	}
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			binding := newBinding(t)
			t.Cleanup(func() { _ = binding.Close() })
			cfg := &config.City{
				Beads: config.BeadsConfig{CommandSecurityProfile: string(nudgequeue.CommandSecurityProfileStoreWriterIsController)},
				Daemon: config.DaemonConfig{
					NudgeDispatcher:  "supervisor",
					NudgeEffectOwner: string(mode),
				},
			}
			_, selection, err := resolveNudgeEffectStartupForCityWithOpener(
				t.Context(), cfg, runtime.NewFake(), "/city/a", "city-a",
				func(context.Context, string, string, rollout.Mode) (*productionNudgeAuthorityBinding, error) {
					return binding, nil
				},
			)
			if mode == rollout.Auto {
				if err != nil || selection.Ownership != nudgeEffectOwnershipLegacy ||
					!errors.Is(selection.Diagnostic, errNudgeEffectStartupDegraded) ||
					!strings.Contains(selection.Notice, "canonical CLI/API command ingress") {
					t.Fatalf("auto result = selection %#v, err=%v; want precise loud legacy degradation", selection, err)
				}
			} else if !errors.Is(err, errNudgeEffectStartupRefused) || !strings.Contains(err.Error(), "canonical CLI/API command ingress") {
				t.Fatalf("require error = %v, want precise fail-closed producer refusal", err)
			}
			if binding.live() {
				t.Fatal("non-keyed startup retained a live authority binding")
			}
		})
	}
}

func TestResolveNudgeEffectStartupForCityFailureHonorsOffAutoAndRequire(t *testing.T) {
	localProfile := string(nudgequeue.CommandSecurityProfileStoreWriterIsController)
	for _, test := range []struct {
		name           string
		mode           string
		profile        string
		wantCalls      int
		wantErr        bool
		wantDiagnostic bool
	}{
		{name: "off", mode: "off", profile: "invalid"},
		{name: "hosted auto", mode: "auto", profile: string(nudgequeue.CommandSecurityProfileHosted), wantErr: true},
		{name: "local auto", mode: "auto", profile: localProfile, wantCalls: 1, wantDiagnostic: true},
		{name: "local require", mode: "require", profile: localProfile, wantCalls: 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.City{
				Beads:  config.BeadsConfig{CommandSecurityProfile: test.profile},
				Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor", NudgeEffectOwner: test.mode},
			}
			calls := 0
			_, selection, err := resolveNudgeEffectStartupForCityWithOpener(
				t.Context(), cfg, runtime.NewFake(), "/city/a", "city-a",
				func(context.Context, string, string, rollout.Mode) (*productionNudgeAuthorityBinding, error) {
					calls++
					return nil, errors.New("authority store unavailable")
				},
			)
			if calls != test.wantCalls {
				t.Fatalf("opener calls = %d, want %d", calls, test.wantCalls)
			}
			if test.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr=%t", err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, errNudgeEffectStartupRefused) {
				t.Fatalf("error = %v, want typed startup refusal", err)
			}
			if test.wantDiagnostic {
				if selection.Ownership != nudgeEffectOwnershipLegacy || !errors.Is(selection.Diagnostic, errNudgeEffectStartupDegraded) {
					t.Fatalf("auto selection = %#v, want typed loud legacy degradation", selection)
				}
			} else if selection.Diagnostic != nil {
				t.Fatalf("diagnostic = %v, want nil", selection.Diagnostic)
			}
		})
	}
}

func TestCityRuntimeRetainsAndClosesProductionNudgeAuthorityBinding(t *testing.T) {
	store := &bindingCountingStore{MemStore: beads.NewMemStore()}
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(t.TempDir()))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	binding, err := bindProductionNudgeAuthority(t.Context(), t.TempDir(), "city-a", store, repository)
	if err != nil {
		t.Fatalf("bindProductionNudgeAuthority: %v", err)
	}
	cr := &CityRuntime{
		nudgeAuthorityBinding: binding,
		stdout:                io.Discard,
		stderr:                io.Discard,
	}
	cr.preserveSessionsOnShutdown()
	if got := cr.liveNudgeAuthorityBinding(); got != binding {
		t.Fatalf("live binding = %#v, want exact %#v", got, binding)
	}
	cr.shutdown()
	if got := cr.liveNudgeAuthorityBinding(); got != nil {
		t.Fatalf("live binding after shutdown = %#v, want nil", got)
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("store close count after runtime shutdown = %d, want 1", got)
	}
}

type bindingCountingStore struct {
	*beads.MemStore
	closes atomic.Int32
}

func (s *bindingCountingStore) CloseStore() error {
	s.closes.Add(1)
	return nil
}
