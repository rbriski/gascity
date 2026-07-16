package main

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestRunSupervisorInstallsProductionNudgeAdmissionBeforeServingMux(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	fn := findProductionFunc(t, files["cmd_supervisor.go"], "runSupervisor")
	var constructorPos, installPos, servePos token.Pos
	constructorCalls := 0
	installCalls := 0
	serveCalls := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch controllerStopCalledIdent(call.Fun) {
		case "NewSupervisorMux":
			constructorCalls++
			constructorPos = call.Pos()
		case "installSupervisorProductionNudgeAdmission":
			installCalls++
			installPos = call.Pos()
			if len(call.Args) != 2 || controllerStopCalledIdent(call.Args[0]) != "apiMux" || controllerStopCalledIdent(call.Args[1]) != "registry" {
				t.Fatalf("production nudge admission installer args = %#v, want (apiMux, registry)", call.Args)
			}
		case "Serve":
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ownerOK := selector.X.(*ast.Ident)
			if ownerOK && owner.Name == "apiMux" {
				serveCalls++
				servePos = call.Pos()
			}
		}
		return true
	})
	if constructorCalls != 1 || installCalls != 1 || serveCalls != 1 {
		t.Fatalf("production supervisor mux calls = constructor:%d installer:%d serve:%d, want 1/1/1", constructorCalls, installCalls, serveCalls)
	}
	if constructorPos == token.NoPos || installPos == token.NoPos || servePos == token.NoPos || constructorPos >= installPos || installPos >= servePos {
		t.Fatalf("production supervisor mux order = constructor:%d installer:%d serve:%d, want constructor < installer < serve", constructorPos, installPos, servePos)
	}
}

func TestCityRegistryProductionNudgeAuthorityPublishesOnlyStartedRuntime(t *testing.T) {
	binding, _ := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name: "city-a",
		cr:   &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
	})

	if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
		t.Fatalf("startup authority = (%#v, %t), want unavailable", authority, live)
	}

	registry.UpdateCallback("/city/a", func(city *managedCity) {
		city.started = true
	})
	authority, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || authority != binding {
		t.Fatalf("started authority = (%#v, %t), want exact binding %#v", authority, live, binding)
	}
	if authority, live := registry.resolveProductionNudgeAuthority("city-b"); live || authority != nil {
		t.Fatalf("foreign city authority = (%#v, %t), want unavailable", authority, live)
	}
}

func TestCityRegistryProductionNudgeAuthorityRejectsTombstoneMissingAndClosedBinding(t *testing.T) {
	t.Run("tombstoned", func(t *testing.T) {
		binding, _ := newRegistryProductionNudgeBinding(t)
		registry := newCityRegistry()
		registry.Add("/city/a", &managedCity{
			name:    "city-a",
			started: true,
			cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
		})
		registry.UpdateCallback("/city/a", func(city *managedCity) {
			city.tombstoned.Store(true)
		})
		if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
			t.Fatalf("tombstoned authority = (%#v, %t), want unavailable", authority, live)
		}
	})

	t.Run("missing binding", func(t *testing.T) {
		registry := newCityRegistry()
		registry.Add("/city/a", &managedCity{
			name:    "city-a",
			started: true,
			cr:      &CityRuntime{cityName: "city-a"},
		})
		if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
			t.Fatalf("missing authority = (%#v, %t), want unavailable", authority, live)
		}
	})

	t.Run("closed binding", func(t *testing.T) {
		binding, _ := newRegistryProductionNudgeBinding(t)
		if err := binding.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		registry := newCityRegistry()
		registry.Add("/city/a", &managedCity{
			name:    "city-a",
			started: true,
			cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
		})
		if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
			t.Fatalf("closed authority = (%#v, %t), want unavailable", authority, live)
		}
	})
}

func TestCityRegistryProductionNudgeAuthorityFailsClosedForDuplicateCanonicalName(t *testing.T) {
	first, _ := newRegistryProductionNudgeBinding(t)
	second, _ := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/first", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: first},
	})
	registry.Add("/city/second", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: second},
	})

	if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
		t.Fatalf("ambiguous authority = (%#v, %t), want fail-closed", authority, live)
	}
}

func TestCityRegistryProductionNudgeAuthorityReplacementPublishesOnlyNewRuntime(t *testing.T) {
	oldBinding, oldRepository := newRegistryProductionNudgeBinding(t)
	newBinding, _ := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: oldBinding},
	})

	retained, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || retained != oldBinding {
		t.Fatalf("old authority = (%#v, %t), want exact old binding", retained, live)
	}
	oldRequestContext := productionNudgeBindingRequestContext(t, oldBinding, "replacement-old-grant")
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: newBinding},
	})
	current, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || current != newBinding {
		t.Fatalf("replacement authority = (%#v, %t), want exact new binding", current, live)
	}

	if err := oldBinding.Close(); err != nil {
		t.Fatalf("close replaced binding: %v", err)
	}
	if _, err := retained.Admit(oldRequestContext, validProductionNudgeRequest(time.Now().UTC())); !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("retained old authority admission error = %v, want unavailable", err)
	}
	assertProductionNudgeRepositoryEntries(t, oldRepository, 0)
	current, live = registry.resolveProductionNudgeAuthority("city-a")
	if !live || current != newBinding {
		t.Fatalf("authority after old shutdown = (%#v, %t), want new binding", current, live)
	}
}

func TestCityRegistryProductionNudgeAuthorityReplacementFencesRetainedAdmissions(t *testing.T) {
	oldStore := newBlockingProductionNudgeAtomicStore()
	oldBinding, oldRepository := newRegistryProductionNudgeBindingFromStore(t, oldStore)
	newBinding, newRepository := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: oldBinding},
	})

	retained, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || retained != oldBinding {
		t.Fatalf("old authority = (%#v, %t), want exact old binding", retained, live)
	}
	entered, release := oldStore.blockNextAtomicWrite()
	firstRequest := validProductionNudgeRequest(time.Now().UTC())
	firstRequest.RequestID = "replacement-in-flight-old"
	firstContext := productionNudgeBindingRequestContext(t, oldBinding, "replacement-in-flight-old-grant")
	lateContext := productionNudgeBindingRequestContext(t, oldBinding, "replacement-retained-late-grant")
	firstDone := make(chan error, 1)
	go func() {
		_, err := retained.Admit(firstContext, firstRequest)
		firstDone <- err
	}()
	waitForProductionNudgeAdmissionBlock(t, entered)

	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: newBinding},
	})
	current, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || current != newBinding {
		t.Fatalf("replacement authority = (%#v, %t), want exact new binding", current, live)
	}
	newRequest := validProductionNudgeRequest(time.Now().UTC())
	newRequest.RequestID = "replacement-current-new"
	if _, err := current.Admit(productionNudgeBindingRequestContext(t, newBinding, "replacement-current-new-grant"), newRequest); err != nil {
		t.Fatalf("new authority admission: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- oldBinding.Close() }()
	waitForProductionNudgeCloseWriter(t, oldBinding)
	lateRequest := validProductionNudgeRequest(time.Now().UTC())
	lateRequest.RequestID = "replacement-retained-late"
	lateDone := make(chan error, 1)
	go func() {
		_, err := retained.Admit(lateContext, lateRequest)
		lateDone <- err
	}()

	close(release)
	if err := waitForProductionNudgeOperation(t, "in-flight old admission", firstDone); err != nil {
		t.Fatalf("in-flight old admission: %v", err)
	}
	if err := waitForProductionNudgeOperation(t, "old binding close", closeDone); err != nil {
		t.Fatalf("old binding close: %v", err)
	}
	if err := waitForProductionNudgeOperation(t, "late retained admission", lateDone); !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("late retained admission error = %v, want unavailable", err)
	}
	assertProductionNudgeRepositoryEntries(t, oldRepository, 1)
	assertProductionNudgeRepositoryEntries(t, newRepository, 1)
	current, live = registry.resolveProductionNudgeAuthority("city-a")
	if !live || current != newBinding {
		t.Fatalf("post-race authority = (%#v, %t), want exact new binding", current, live)
	}
}

func TestSupervisorProductionNudgeAdmissionTombstonePreventsDurableCommand(t *testing.T) {
	binding, repository := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
	})
	registry.UpdateCallback("/city/a", func(city *managedCity) {
		city.tombstoned.Store(true)
	})

	harness := newProductionNudgeAdmissionHTTPHarnessWithMuxSetup(t, func(mux *api.SupervisorMux) {
		installSupervisorProductionNudgeAdmission(mux, registry)
	}, true)
	response := harness.serve(t, "city-a", "write-key", "tombstone-grant")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503", response.Code, response.Body.String())
	}
	assertProductionNudgeRepositoryEntries(t, repository, 0)
}

func TestCityRegistryProductionNudgeAuthorityAdmissionCompletesBeforeCloseReturns(t *testing.T) {
	store := newBlockingProductionNudgeAtomicStore()
	binding, repository := newRegistryProductionNudgeBindingFromStore(t, store)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
	})
	authority, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || authority != binding {
		t.Fatalf("authority = (%#v, %t), want exact live binding", authority, live)
	}

	entered, release := store.blockNextAtomicWrite()
	admissionDone := make(chan error, 1)
	request := validProductionNudgeRequest(time.Now().UTC())
	request.RequestID = "admission-before-close"
	requestContext := productionNudgeBindingRequestContext(t, binding, "admission-before-close-grant")
	go func() {
		_, err := authority.Admit(requestContext, request)
		admissionDone <- err
	}()
	waitForProductionNudgeAdmissionBlock(t, entered)

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- binding.Close()
	}()
	<-closeStarted
	waitForProductionNudgeCloseWriter(t, binding)
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the admitted durable write was released: %v", err)
	default:
	}

	close(release)
	if err := <-admissionDone; err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertProductionNudgeRepositoryEntries(t, repository, 1)
	if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
		t.Fatalf("post-close authority = (%#v, %t), want unavailable", authority, live)
	}
}

func TestCityRegistryProductionNudgeAuthorityCloseCompletionRejectsRetainedAdmission(t *testing.T) {
	binding, repository := newRegistryProductionNudgeBinding(t)
	registry := newCityRegistry()
	registry.Add("/city/a", &managedCity{
		name:    "city-a",
		started: true,
		cr:      &CityRuntime{cityName: "city-a", nudgeAuthorityBinding: binding},
	})
	retained, live := registry.resolveProductionNudgeAuthority("city-a")
	if !live || retained != binding {
		t.Fatalf("authority = (%#v, %t), want exact live binding", retained, live)
	}
	requestContext := productionNudgeBindingRequestContext(t, binding, "close-before-admission-grant")

	if err := binding.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := retained.Admit(requestContext, validProductionNudgeRequest(time.Now().UTC())); !errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("retained admission after Close error = %v, want unavailable", err)
	}
	assertProductionNudgeRepositoryEntries(t, repository, 0)
	if authority, live := registry.resolveProductionNudgeAuthority("city-a"); live || authority != nil {
		t.Fatalf("post-close authority = (%#v, %t), want unavailable", authority, live)
	}
}

func newRegistryProductionNudgeBinding(t *testing.T) (*productionNudgeAuthorityBinding, *nudgequeue.CommandRepository) {
	t.Helper()
	return newRegistryProductionNudgeBindingFromStore(t, newNudgeCommandSourceAtomicStore())
}

func newRegistryProductionNudgeBindingFromStore(
	t *testing.T,
	store beads.Store,
) (*productionNudgeAuthorityBinding, *nudgequeue.CommandRepository) {
	t.Helper()
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
	t.Cleanup(func() { _ = binding.Close() })
	return binding, repository
}

func productionNudgeBindingRequestContext(
	t *testing.T,
	binding *productionNudgeAuthorityBinding,
	evidenceID string,
) context.Context {
	t.Helper()
	tenantScope, cityScope, credentialClass := binding.RequesterScope()
	if tenantScope == "" || cityScope == "" || credentialClass == "" {
		t.Fatal("live production binding returned an incomplete requester scope")
	}
	return nudgequeue.WithAuthenticatedNudgeRequester(t.Context(), nudgequeue.AuthenticatedNudgeRequester{
		PrincipalID:     "write-key",
		TenantScope:     tenantScope,
		CityScope:       cityScope,
		CredentialClass: credentialClass,
		EvidenceID:      evidenceID,
	})
}

type blockingProductionNudgeAtomicStore struct {
	*nudgeCommandSourceAtomicStore

	mu        sync.Mutex
	blockNext bool
	entered   chan struct{}
	release   chan struct{}
}

func newBlockingProductionNudgeAtomicStore() *blockingProductionNudgeAtomicStore {
	return &blockingProductionNudgeAtomicStore{nudgeCommandSourceAtomicStore: newNudgeCommandSourceAtomicStore()}
}

func (s *blockingProductionNudgeAtomicStore) blockNextAtomicWrite() (<-chan struct{}, chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockNext = true
	s.entered = make(chan struct{})
	s.release = make(chan struct{})
	return s.entered, s.release
}

func (s *blockingProductionNudgeAtomicStore) AtomicReadWrite(
	ctx context.Context,
	operation string,
	fn func(beads.AtomicReadWriteTx) error,
) error {
	s.mu.Lock()
	block := s.blockNext
	entered := s.entered
	release := s.release
	if block {
		s.blockNext = false
	}
	s.mu.Unlock()
	if block {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.nudgeCommandSourceAtomicStore.AtomicReadWrite(ctx, operation, fn)
}

func waitForProductionNudgeAdmissionBlock(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for admission to enter the durable store")
	}
}

func waitForProductionNudgeCloseWriter(t *testing.T, binding *productionNudgeAuthorityBinding) {
	t.Helper()
	deadline := time.Now().Add(testutil.GoroutineRaceTimeout)
	for {
		if binding.mu.TryRLock() {
			binding.mu.RUnlock()
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for Close to contend with the admitted read section")
			}
			runtime.Gosched()
			continue
		}
		return
	}
}

func waitForProductionNudgeOperation(t *testing.T, operation string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}

func assertProductionNudgeRepositoryEntries(t *testing.T, repository *nudgequeue.CommandRepository, want int) {
	t.Helper()
	snapshot, err := repository.Snapshot(t.Context(), nudgequeue.MaxCommandRepositorySnapshotCommands)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := len(snapshot.Entries); got != want {
		t.Fatalf("durable command entries = %d, want %d", got, want)
	}
}
