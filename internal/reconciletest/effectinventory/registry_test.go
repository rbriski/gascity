package effectinventory

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestValidateRegistryAcceptsCompleteDirectRoute(t *testing.T) {
	if err := validateRegistry(validRegistry(), validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected a complete registry: %v", err)
	}
}

func TestValidateRegistryAcceptsProfileSpecificRoutesToSharedSite(t *testing.T) {
	registry := validRegistry()
	controller := registry.Registrations[0].Cases[0].Routes[0]
	controllerCase := ProfileCase{BuildProfiles: []BuildProfileID{
		BuildDarwinDefault,
		BuildDarwinNative,
	}, Routes: []Route{controller}}
	api := controller
	apiCase := ProfileCase{BuildProfiles: []BuildProfileID{
		BuildLinuxDefault,
		BuildLinuxNative,
		BuildWindowsCompile,
	}, Routes: []Route{api}}
	api.ExecutingProcess = ProcessAPIInController
	api.LogicalOwner = functionRef("github.com/gastownhall/gascity/internal/api", "internal/api/routes.go", "recoverRoute")
	api.Hops = []RouteHop{{
		Site: OperationSite{
			Operation: OperationCall,
			Enclosing: api.LogicalOwner,
			Ordinal:   1,
		},
		Dispatch: HopDispatchExact,
		Callee:   registry.Registrations[0].Matcher.Enclosing,
	}}
	apiCase.Routes[0] = api
	registry.Registrations[0].Cases = []ProfileCase{controllerCase, apiCase}

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected profile-specific routes to one site: %v", err)
	}
}

func TestValidateRegistryAcceptsDisjointProfileSafetyClassifications(t *testing.T) {
	registry := validRegistry()
	darwin := registry.Registrations[0].Cases[0].Routes[0]
	darwinCase := ProfileCase{BuildProfiles: []BuildProfileID{
		BuildDarwinDefault,
		BuildDarwinNative,
	}, Routes: []Route{darwin}}
	linux := darwin
	linuxCase := ProfileCase{BuildProfiles: []BuildProfileID{
		BuildLinuxDefault,
		BuildLinuxNative,
		BuildWindowsCompile,
	}, Routes: []Route{linux}}
	linux.Fences = []Fence{{Kind: FenceNone}}
	linux.CurrentGate = GateRef{
		Kind:      GatePredicate,
		Predicate: objectRef("github.com/gastownhall/gascity/cmd/gc", "CityRuntime", "routeRecoveryEnabled"),
		Expected:  "true",
	}
	linux.Disposition = Disposition{
		Kind:   DispositionRemoveAtGate,
		Gates:  []TaskRef{"P2.0", "P2.10A"},
		Reason: "remove the legacy route after the conditional writer is live",
	}
	linuxCase.Routes[0] = linux
	registry.Registrations[0].Cases = []ProfileCase{darwinCase, linuxCase}

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected disjoint profile safety classifications: %v", err)
	}
}

func TestValidateRegistryRejectsBoundaryDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry)
		want   string
	}{
		{"unknown site boundary", func(r *Registry) { r.Registrations[0].BoundaryID = "missing" }, `references unknown boundary "missing"`},
		{"duplicate boundary id", func(r *Registry) { r.Boundaries = append(r.Boundaries, r.Boundaries[0]) }, "duplicate boundary id"},
		{"duplicate boundary object", func(r *Registry) {
			extra := r.Boundaries[0]
			extra.ID = "duplicate-object"
			r.Boundaries = append(r.Boundaries, extra)
		}, "duplicates boundary object"},
		{"interface dispatch without receiver", func(r *Registry) { r.Boundaries[0].Object.Receiver = "" }, "interface boundary requires a receiver"},
		{"invalid object name", func(r *Registry) { r.Boundaries[0].Object.Name = "not.a.name" }, "must be a Go identifier"},
		{"non-channel input", func(r *Registry) { r.Boundaries[0].Input = ValueSlot{Kind: SlotParameter, Index: 1} }, "non-channel boundary cannot name an input slot"},
		{"non-channel output", func(r *Registry) { r.Boundaries[0].Output = ValueSlot{Kind: SlotResult, Index: 1} }, "non-channel boundary cannot name an output slot"},
		{"channel input and output", func(r *Registry) {
			r.Boundaries[0].Kind = KindWakeSource
			r.Boundaries[0].Match = ObjectMatchChannel
			r.Boundaries[0].Input = ValueSlot{Kind: SlotParameter, Index: 1}
			r.Boundaries[0].Output = ValueSlot{Kind: SlotResult, Index: 1}
		}, "channel boundary cannot name both input and output slots"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(&registry)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryAcceptsUnusedDiscoveryBoundary(t *testing.T) {
	registry := validRegistry()
	extra := registry.Boundaries[0]
	extra.ID = "beads.store.update"
	extra.Object.Name = "Update"
	registry.Boundaries = append(registry.Boundaries, extra)

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected an unused discovery seed: %v", err)
	}
}

func TestValidateRegistryRejectsDuplicateSiteAndRouteIdentity(t *testing.T) {
	registry := validRegistry()
	registry.Registrations = append(registry.Registrations, registry.Registrations[0])
	duplicateRoute := registry.Registrations[0].Cases[0].Routes[0]
	registry.Registrations[0].Cases[0].Routes = append(registry.Registrations[0].Cases[0].Routes, duplicateRoute)

	err := validateRegistry(registry, validationDate())
	assertErrorContains(t, err, "duplicates physical site registration", "logical origin", "has multiple classifications")
}

func TestValidateRegistryRejectsRouteProfileMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry)
		want   string
	}{
		{"empty", func(r *Registry) { r.Registrations[0].Cases[0].BuildProfiles = nil }, "case build profiles are required"},
		{"unknown", func(r *Registry) { r.Registrations[0].Cases[0].BuildProfiles = []BuildProfileID{"plan9/amd64/default"} }, `unknown build profile "plan9/amd64/default"`},
		{"overlap", func(r *Registry) {
			r.Registrations[0].Cases = append(r.Registrations[0].Cases, r.Registrations[0].Cases[0])
		}, `has multiple classification cases`},
		{"missing routes", func(r *Registry) { r.Registrations[0].Cases[0].Routes = nil }, "at least one logical route is required"},
		{"unsorted", func(r *Registry) {
			r.Registrations[0].Cases[0].BuildProfiles = []BuildProfileID{BuildWindowsCompile, BuildLinuxDefault}
		}, "build profiles must be sorted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(&registry)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestFencePolicyDerivesScopeAndExclusion(t *testing.T) {
	tests := []struct {
		kind                FenceKind
		scope               FenceScope
		serializesIdentity  bool
		rejectsStaleTarget  bool
		suppressesDuplicate bool
	}{
		{FenceNone, FenceScopeNone, false, false, false},
		{FenceProcessLocalNonExclusive, FenceScopeProcess, true, false, false},
		{FenceSingleWriterAssumption, FenceScopeDeployment, false, false, false},
		{FenceIdentifierFlock, FenceScopeLockIdentity, true, false, false},
		{FenceLiveRereadNonCAS, FenceScopeStore, false, false, false},
		{FenceRevisionCAS, FenceScopeTarget, false, true, false},
		{FenceProviderAtomic, FenceScopeTarget, false, true, false},
		{FenceCommandDedup, FenceScopeProvider, false, false, true},
	}

	for _, tt := range tests {
		policy, ok := fencePolicyFor(tt.kind)
		if !ok {
			t.Fatalf("fencePolicyFor(%q) was not found", tt.kind)
		}
		if policy.Scope != tt.scope ||
			policy.SerializesSameIdentity != tt.serializesIdentity ||
			policy.RejectsStaleTarget != tt.rejectsStaleTarget ||
			policy.SuppressesDuplicate != tt.suppressesDuplicate {
			t.Errorf("fencePolicyFor(%q) = %+v, want scope=%q serializes=%t stale=%t dedup=%t", tt.kind, policy, tt.scope, tt.serializesIdentity, tt.rejectsStaleTarget, tt.suppressesDuplicate)
		}
	}
}

func TestValidateRegistryRejectsProviderBypassOnNonProviderBoundary(t *testing.T) {
	registry := validRegistry()
	firstRoute(&registry).AccessPath = AccessProviderBypass

	assertErrorContains(t, validateRegistry(registry, validationDate()), "provider-bypass requires a provider-mutation boundary")
}

func TestValidateRegistryClassifiesStoreDomainOnEachLogicalRoute(t *testing.T) {
	t.Run("missing on store route", func(t *testing.T) {
		registry := validRegistry()
		firstRoute(&registry).StoreDomain = ""
		assertErrorContains(t, validateRegistry(registry, validationDate()), "store domain is required")
	})

	t.Run("present on non-store route", func(t *testing.T) {
		registry := validRegistry()
		registry.Boundaries[0].Kind = KindProviderMutation
		firstRoute(&registry).AccessPath = AccessProviderNative
		assertErrorContains(t, validateRegistry(registry, validationDate()), "store domain is only valid for store mutations")
	})
}

func TestValidateRegistryRejectsFenceEvidenceThatDoesNotMatchMechanism(t *testing.T) {
	tests := []struct {
		name  string
		fence Fence
		want  string
	}{
		{"none with source", Fence{Kind: FenceNone, Source: objectRef("sync", "Mutex", "Lock")}, "fence none cannot name a source"},
		{"process lock without source", Fence{Kind: FenceProcessLocalNonExclusive}, "fence source is required"},
		{"live reread with token", Fence{Kind: FenceLiveRereadNonCAS, Source: objectRef("example/store", "Store", "Get"), Token: objectRef("example", "", "Generation")}, "does not accept a token"},
		{"token reread without token", Fence{Kind: FenceTokenRereadNonCAS, Source: objectRef("example/store", "Store", "Get")}, "fence token is required"},
		{"duplicate", Fence{Kind: FenceLiveRereadNonCAS, Source: objectRef("example/store", "Store", "Get")}, "duplicate fence"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			if tt.name == "duplicate" {
				firstRoute(&registry).Fences = []Fence{tt.fence, tt.fence}
			} else {
				firstRoute(&registry).Fences = []Fence{tt.fence}
			}
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsUntypedTargetSource(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TargetRef)
		want   string
	}{
		{"unknown", func(target *TargetRef) { target.Source = TargetSourceKind("derived") }, `unknown target source "derived"`},
		{"bad sink slot", func(target *TargetRef) { target.Sink = ValueSlot{Kind: SlotParameter} }, "parameter slot index must be positive"},
		{"missing source object", func(target *TargetRef) { target.SourceObject = ObjectRef{} }, "target source object package is required"},
		{"bad result slot", func(target *TargetRef) { target.SourceSlot = ValueSlot{Kind: SlotResult} }, "result slot index must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(&firstRoute(&registry).Target)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsUnverifiableCurrentGate(t *testing.T) {
	tests := []struct {
		name string
		gate GateRef
		want string
	}{
		{"unknown", GateRef{Kind: GateKind("usually")}, `unknown current gate "usually"`},
		{"unconditional with object", GateRef{Kind: GateUnconditionalLegacy, Predicate: objectRef("example", "Config", "Enabled")}, "unconditional gate cannot name a predicate"},
		{"predicate without object", GateRef{Kind: GatePredicate, Expected: "true"}, "gate predicate package is required"},
		{"predicate without expected value", GateRef{Kind: GatePredicate, Predicate: objectRef("example", "Config", "Enabled")}, "gate expected value is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).CurrentGate = tt.gate
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsInvalidRemovalDisposition(t *testing.T) {
	tests := []struct {
		name        string
		disposition Disposition
		want        string
	}{
		{"unknown", Disposition{Kind: DispositionKind("later"), Reason: "unknown"}, `unknown disposition "later"`},
		{"replace without gate", Disposition{Kind: DispositionReplaceAtGate, Reason: "move to conditional writer"}, "replacement gates are required"},
		{"retain with gate", Disposition{Kind: DispositionRetainBoundary, Gates: []TaskRef{"P2.0"}, Reason: "permanent leaf"}, "retained boundary cannot name replacement gates"},
		{"missing reason", Disposition{Kind: DispositionRetainBoundary}, "disposition reason is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).Disposition = tt.disposition
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsImpossibleContinuation(t *testing.T) {
	tests := []struct {
		name         string
		continuation Continuation
		want         string
	}{
		{"inline detached", Continuation{Locus: ContinuationInline, Completion: CompletionDetached}, "inline continuation must complete synchronously"},
		{"goroutine synchronous", Continuation{Locus: ContinuationGoroutine, Completion: CompletionSynchronous}, "goroutine continuation must be joined or detached"},
		{"channel synchronous", Continuation{Locus: ContinuationChannel, Completion: CompletionSynchronous}, "channel continuation must be request-reply or detached"},
		{"child request reply", Continuation{Locus: ContinuationProviderChild, Completion: CompletionRequestReply}, "provider-child continuation must be joined or detached"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).Continuation = tt.continuation
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsBrokenExactRouteChain(t *testing.T) {
	registry := validRegistry()
	origin := functionRef("example/controller", "controller.go", "run")
	firstRoute(&registry).Hops = []RouteHop{{
		Site: OperationSite{
			Operation: OperationCall,
			Enclosing: origin,
			Ordinal:   1,
		},
		Dispatch: HopDispatchExact,
		Callee:   functionRef("example/controller", "controller.go", "wrongLeaf"),
	}}

	assertErrorContains(t, validateRegistry(registry, validationDate()), "last route hop must reach the physical site enclosing function")
}

func TestValidateRegistryRejectsInventedLogicalOwner(t *testing.T) {
	registry := validRegistry()
	firstRoute(&registry).LogicalOwner = functionRef("example/unrelated", "unrelated.go", "owner")

	assertErrorContains(t, validateRegistry(registry, validationDate()), "logical owner is not present in the exact route chain")
}

func TestValidateRegistryRequiresBoundedExceptionForDirectBypasses(t *testing.T) {
	registry := validRegistry()
	firstRoute(&registry).Exception = nil
	assertErrorContains(t, validateRegistry(registry, validationDate()), "raw-store-bypass requires a temporary exception")

	tests := []struct {
		name   string
		mutate func(*TemporaryException)
		want   string
	}{
		{"unknown kind", func(exception *TemporaryException) { exception.Kind = ExceptionKind("ignore") }, `unknown exception kind "ignore"`},
		{"invalid anchor", func(exception *TemporaryException) { exception.Anchor.Value = "current" }, "git commit anchor must be 40 lowercase hexadecimal characters"},
		{"expired", func(exception *TemporaryException) { exception.Expires = "2026-07-13" }, "exception expired on 2026-07-13"},
		{"missing seam test", func(exception *TemporaryException) { exception.OwningTest = TestRef{} }, "owning test package is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(firstRoute(&registry).Exception)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsPermanentOrMismatchedBypassException(t *testing.T) {
	t.Run("retained bypass", func(t *testing.T) {
		registry := validRegistry()
		firstRoute(&registry).Disposition = Disposition{
			Kind:   DispositionRetainBoundary,
			Reason: "incorrectly retain a bypass",
		}
		assertErrorContains(t, validateRegistry(registry, validationDate()), "expiring bypass cannot retain its ownership route")
	})

	t.Run("removal tasks differ", func(t *testing.T) {
		registry := validRegistry()
		firstRoute(&registry).Exception.RemovalTasks = []TaskRef{"P2.0"}
		assertErrorContains(t, validateRegistry(registry, validationDate()), "exception removal tasks must equal disposition gates")
	})
}

func TestValidateRegistryRejectsExceptionKindWithoutRequiredSeam(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Route)
		want   string
	}{
		{"best effort on store", func(route *Route) { route.Exception.Kind = ExceptionBestEffortEvent }, "best-effort-event exception requires an event boundary"},
		{"detached on inline", func(route *Route) { route.Exception.Kind = ExceptionDetachedContinuation }, "detached-continuation exception requires detached completion"},
		{"collision on store", func(route *Route) { route.Exception.Kind = ExceptionDestructiveCollision }, "destructive-collision exception requires a provider/process boundary"},
		{"legacy bypass on canonical path", func(route *Route) {
			route.AccessPath = AccessSessionStoreFrontDoor
			route.Exception.Kind = ExceptionLegacyBypass
		}, "legacy-bypass exception requires a bypass access path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(firstRoute(&registry))
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsZeroValidationDate(t *testing.T) {
	assertErrorContains(t, validateRegistry(validRegistry(), time.Time{}), "validation date is required")
}

func TestValidateRegistryRejectsEmptyRegistry(t *testing.T) {
	err := validateRegistry(Registry{}, validationDate())
	assertErrorContains(t, err, "registry has no boundaries", "registry has no site registrations", "registry has no routes")
}

func TestValidateRegistryReturnsProblemsInDeterministicOrder(t *testing.T) {
	registry := validRegistry()
	registry.Registrations[0].Matcher.Ordinal = 0
	firstRoute(&registry).ActionFamily = ""

	err := validateRegistry(registry, validationDate())
	if err == nil {
		t.Fatal("ValidateRegistry() returned nil")
	}
	lines := strings.Split(strings.TrimPrefix(err.Error(), "effect registry validation failed:\n- "), "\n- ")
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(lines, sorted) {
		t.Fatalf("ValidateRegistry() error is not sorted:\n%s", err)
	}
}

func validRegistry() Registry {
	registry := compileRegistryFixture()
	registry.Registrations[0].Cases[0].BuildProfiles = allBuildProfiles()
	return registry
}

func allBuildProfiles() []BuildProfileID {
	profiles := canonicalAnalysisProfiles()
	result := make([]BuildProfileID, len(profiles))
	for index, profile := range profiles {
		result[index] = profile.ID
	}
	return result
}

func functionRef(packagePath, file, name string) FunctionRef {
	return FunctionRef{
		Object: ObjectRef{Package: packagePath, Name: name},
		File:   file,
	}
}

func objectRef(packagePath, receiver, name string) ObjectRef {
	return ObjectRef{Package: packagePath, Receiver: receiver, Name: name}
}

func firstRoute(registry *Registry) *Route {
	return &registry.Registrations[0].Cases[0].Routes[0]
}

func validateRegistry(registry Registry, asOf time.Time) error {
	return ValidateRegistry(registry, discoveryForRegistry(registry), asOf)
}

func validationDate() time.Time {
	return time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC)
}

func assertErrorContains(t *testing.T, err error, substrings ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ValidateRegistry() returned nil, want error containing %q", substrings)
	}
	for _, substring := range substrings {
		if !strings.Contains(err.Error(), substring) {
			t.Errorf("ValidateRegistry() error = %q, want substring %q", err, substring)
		}
	}
}
