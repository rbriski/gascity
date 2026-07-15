package effectinventory

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

const routeHopFixturePackage = fixtureModulePath + "/routehops"

func TestValidateRouteHopEvidenceForProfileAcceptsExactVTAClosureGoAndDeferEdges(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	genericLeaf := routeHopFixtureRef("", "GenericLeaf", "routehops.go", nil)
	closure := routeHopFixtureRef("", "ClosureOwner", "routehops.go", []int{1})
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)
	workerB := routeHopFixtureRef("WorkerB", "Run", "routehops.go", nil)

	registry := Registry{Registrations: []SiteRegistration{
		routeHopRegistration(leaf,
			routeHopRoute(leaf),
			routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
				routeHop("DuplicateOwner", OperationCall, 1, HopDispatchExact, leaf)),
			routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
				routeHop("DuplicateOwner", OperationCall, 2, HopDispatchExact, leaf)),
			routeHopRoute(routeHopFixtureRef("", "MixedDispatchOwner", "routehops.go", nil),
				routeHop("MixedDispatchOwner", OperationCall, 1, HopDispatchExact, leaf)),
			routeHopRoute(routeHopFixtureRef("", "MixedDispatchOwner", "routehops.go", nil),
				routeHop("MixedDispatchOwner", OperationCall, 2, HopDispatchVTA, leaf)),
			routeHopRoute(routeHopFixtureRef("", "ClosureOwner", "routehops.go", nil),
				routeHop("ClosureOwner", OperationCall, 1, HopDispatchExact, closure),
				RouteHop{Site: OperationSite{Operation: OperationCall, Enclosing: closure, Ordinal: 1}, Dispatch: HopDispatchExact, Callee: leaf}),
			routeHopRoute(routeHopFixtureRef("", "GoOwner", "routehops.go", nil),
				routeHop("GoOwner", OperationGo, 1, HopDispatchExact, leaf)),
			routeHopRoute(routeHopFixtureRef("", "DeferOwner", "routehops.go", nil),
				routeHop("DeferOwner", OperationDefer, 1, HopDispatchExact, leaf)),
		),
		routeHopRegistration(workerA,
			routeHopRoute(routeHopFixtureRef("", "InterfaceOwner", "routehops.go", nil),
				routeHop("InterfaceOwner", OperationCall, 1, HopDispatchVTA, workerA)),
			routeHopRoute(routeHopFixtureRef("", "GenericInterfaceOwner", "routehops.go", nil),
				routeHop("GenericInterfaceOwner", OperationCall, 1, HopDispatchVTA, workerA)),
		),
		routeHopRegistration(workerB,
			routeHopRoute(routeHopFixtureRef("", "InterfaceOwner", "routehops.go", nil),
				routeHop("InterfaceOwner", OperationCall, 1, HopDispatchVTA, workerB)),
			routeHopRoute(routeHopFixtureRef("", "GenericInterfaceOwner", "routehops.go", nil),
				routeHop("GenericInterfaceOwner", OperationCall, 1, HopDispatchVTA, workerB)),
		),
		routeHopRegistration(genericLeaf,
			routeHopRoute(routeHopFixtureRef("", "GenericOwner", "routehops.go", nil),
				routeHop("GenericOwner", OperationCall, 1, HopDispatchExact, genericLeaf)),
			routeHopRoute(routeHopFixtureRef("", "GenericDynamicOwner", "routehops.go", nil),
				routeHop("GenericDynamicOwner", OperationCall, 1, HopDispatchVTA, genericLeaf)),
		),
	}}

	if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
		t.Fatalf("validateRouteHopEvidenceForProfile() error: %v", err)
	}
}

func TestValidateRouteHopEvidenceForProfileNormalizesOnlyDispatchSyntheticWrappers(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)
	workerB := routeHopFixtureRef("WorkerB", "Run", "routehops.go", nil)
	pointerWorker := routeHopFixtureRef("PointerWorker", "Run", "routehops.go", nil)
	registry := Registry{Registrations: []SiteRegistration{
		routeHopRegistration(workerA,
			routeHopRoute(routeHopFixtureRef("", "PromotedOwner", "routehops.go", nil),
				routeHop("PromotedOwner", OperationCall, 1, HopDispatchExact, workerA)),
			routeHopRoute(routeHopFixtureRef("", "BoundConcreteOwner", "routehops.go", nil),
				routeHop("BoundConcreteOwner", OperationCall, 1, HopDispatchExact, workerA)),
			routeHopRoute(routeHopFixtureRef("", "ConcreteExpressionOwner", "routehops.go", nil),
				routeHop("ConcreteExpressionOwner", OperationCall, 1, HopDispatchExact, workerA)),
			routeHopRoute(routeHopFixtureRef("", "BoundInterfaceOwner", "routehops.go", nil),
				routeHop("BoundInterfaceOwner", OperationCall, 1, HopDispatchVTA, workerA)),
			routeHopRoute(routeHopFixtureRef("", "InterfaceExpressionOwner", "routehops.go", nil),
				routeHop("InterfaceExpressionOwner", OperationCall, 1, HopDispatchVTA, workerA)),
		),
		routeHopRegistration(workerB,
			routeHopRoute(routeHopFixtureRef("", "BoundInterfaceOwner", "routehops.go", nil),
				routeHop("BoundInterfaceOwner", OperationCall, 1, HopDispatchVTA, workerB)),
			routeHopRoute(routeHopFixtureRef("", "InterfaceExpressionOwner", "routehops.go", nil),
				routeHop("InterfaceExpressionOwner", OperationCall, 1, HopDispatchVTA, workerB)),
		),
		routeHopRegistration(pointerWorker,
			routeHopRoute(routeHopFixtureRef("", "PointerAdaptOwner", "routehops.go", nil),
				routeHop("PointerAdaptOwner", OperationCall, 1, HopDispatchExact, pointerWorker)),
			routeHopRoute(routeHopFixtureRef("", "BoundPointerOwner", "routehops.go", nil),
				routeHop("BoundPointerOwner", OperationCall, 1, HopDispatchExact, pointerWorker)),
		),
	}}

	if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
		t.Fatalf("validateRouteHopEvidenceForProfile() error: %v", err)
	}
}

func TestValidateRouteHopEvidenceForProfileScopesDispatchFailuresToCandidateCalls(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)
	unseededWorker := routeHopFixtureRef("UnseededWorker", "Run", "routehops.go", nil)
	owner := routeHopFixtureRef("", "UnrelatedCycleOwner", "routehops.go", nil)
	zeroCalleeOwner := routeHopFixtureRef("", "ZeroCalleeAndPositiveOwner", "routehops.go", nil)

	t.Run("unrelated synthetic cycle does not poison exact edge", func(t *testing.T) {
		registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
			routeHopRoute(owner,
				routeHop("UnrelatedCycleOwner", OperationCall, 1, HopDispatchExact, leaf)),
		)}}
		if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
			t.Fatalf("unrelated dispatch failure poisoned exact route edge: %v", err)
		}
	})

	t.Run("positive candidate evidence is not poisoned by alternate cycle", func(t *testing.T) {
		registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(workerA,
			routeHopRoute(owner,
				routeHop("UnrelatedCycleOwner", OperationCall, 1, HopDispatchVTA, workerA)),
		)}}
		if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
			t.Fatalf("alternate dispatch failure poisoned positively proved route edge: %v", err)
		}
	})

	t.Run("unresolved candidate synthetic cycle remains fail closed", func(t *testing.T) {
		registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(unseededWorker,
			routeHopRoute(owner,
				routeHop("UnrelatedCycleOwner", OperationCall, 1, HopDispatchVTA, unseededWorker)),
		)}}
		err := validateRouteHopEvidenceForProfile(analysis, registry)
		if err == nil || !strings.Contains(err.Error(), "dispatch-only SSA cycle") {
			t.Fatalf("candidate dispatch cycle error = %v, want fail-closed synthetic-cycle diagnostic", err)
		}
	})

	t.Run("unresolved zero-callee candidate cannot hide beside positive call", func(t *testing.T) {
		registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(workerA,
			routeHopRoute(zeroCalleeOwner,
				routeHop("ZeroCalleeAndPositiveOwner", OperationCall, 1, HopDispatchExact, workerA)),
		)}}
		err := validateRouteHopEvidenceForProfile(analysis, registry)
		if err == nil || !strings.Contains(err.Error(), "shape-compatible dynamic call has no closed-world callee evidence") {
			t.Fatalf("zero-callee candidate error = %v, want fail-closed missing-callee diagnostic", err)
		}
	})
}

func TestValidateRouteHopEvidenceForProfileRejectsAdversarialClaims(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	otherLeaf := routeHopFixtureRef("", "OtherLeaf", "routehops.go", nil)
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)
	closure := routeHopFixtureRef("", "ClosureOwner", "routehops.go", []int{1})

	tests := []struct {
		name     string
		registry Registry
		want     []string
	}{
		{
			name: "missing logical owner",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "MissingOwner", "routehops.go", nil)),
			)}},
			want: []string{"logical owner", "missing from loaded profile"},
		},
		{
			name: "direct route owner mismatch",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(otherLeaf),
			)}},
			want: []string{"chain mismatch", "route without hops", "OtherLeaf", "Leaf"},
		},
		{
			name: "stale closure path",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "ClosureOwner", "routehops.go", nil),
					routeHop("ClosureOwner", OperationCall, 1, HopDispatchExact,
						routeHopFixtureRef("", "ClosureOwner", "routehops.go", []int{2})),
					RouteHop{Site: OperationSite{Operation: OperationCall, Enclosing: closure, Ordinal: 1}, Dispatch: HopDispatchExact, Callee: leaf}),
			)}},
			want: []string{"hop[0] callee", "stale closure path"},
		},
		{
			name: "stale duplicate ordinal",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
					routeHop("DuplicateOwner", OperationCall, 3, HopDispatchExact, leaf)),
			)}},
			want: []string{"hop[0]", "stale ordinal 3", "only 2"},
		},
		{
			name: "wrong static dispatch claim",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
					routeHop("DuplicateOwner", OperationCall, 1, HopDispatchVTA, leaf)),
			)}},
			want: []string{"hop[0]", `dispatch "vta"`, `requires "exact"`},
		},
		{
			name: "wrong VTA dispatch claim",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(workerA,
				routeHopRoute(routeHopFixtureRef("", "InterfaceOwner", "routehops.go", nil),
					routeHop("InterfaceOwner", OperationCall, 1, HopDispatchExact, workerA)),
			)}},
			want: []string{"hop[0]", `dispatch "exact"`, `requires "vta"`},
		},
		{
			name: "wrong callee",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(otherLeaf,
				routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
					routeHop("DuplicateOwner", OperationCall, 1, HopDispatchExact, otherLeaf)),
			)}},
			want: []string{"hop[0]", "has no call edge to callee", "OtherLeaf"},
		},
		{
			name: "authored intermediate is not collapsed",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "ChainOwner", "routehops.go", nil),
					routeHop("ChainOwner", OperationCall, 1, HopDispatchExact, leaf)),
			)}},
			want: []string{"hop[0]", "has no call edge to callee", "Leaf"},
		},
		{
			name: "broken resolved chain",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
				routeHopRoute(routeHopFixtureRef("", "ChainOwner", "routehops.go", nil),
					routeHop("ChainOwner", OperationCall, 1, HopDispatchExact,
						routeHopFixtureRef("", "ChainMiddle", "routehops.go", nil)),
					routeHop("OtherOwner", OperationCall, 1, HopDispatchExact, leaf)),
			)}},
			want: []string{"chain mismatch", "hop[0] callee", "hop[1] enclosing"},
		},
		{
			name: "source profile mismatch",
			registry: Registry{Registrations: []SiteRegistration{routeHopRegistration(
				routeHopFixtureRef("", "PlatformLeaf", "platform_darwin.go", nil),
				routeHopRoute(routeHopFixtureRef("", "PlatformOwner", "routehops.go", nil),
					routeHop("PlatformOwner", OperationCall, 1, HopDispatchExact,
						routeHopFixtureRef("", "PlatformLeaf", "platform_darwin.go", nil))),
			)}},
			want: []string{"file/profile mismatch", "platform_darwin.go", "platform_linux.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRouteHopEvidenceForProfile(analysis, tt.registry)
			if err == nil {
				t.Fatalf("validateRouteHopEvidenceForProfile() error = nil, want %q", tt.want)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validateRouteHopEvidenceForProfile() error = %q, want %q", err, want)
				}
			}
		})
	}
}

func TestValidateRouteHopEvidenceForProfileBindsParameterCallbackToPriorCallsite(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	invokeCallback := routeHopFixtureRef("", "InvokeCallback", "routehops.go", nil)

	valid := Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
		routeHopRoute(routeHopFixtureRef("", "CallLeafThroughCallback", "routehops.go", nil),
			routeHop("CallLeafThroughCallback", OperationCall, 1, HopDispatchExact, invokeCallback),
			routeHop("InvokeCallback", OperationCall, 1, HopDispatchVTA, leaf)),
	)}}
	if err := validateRouteHopEvidenceForProfile(analysis, valid); err != nil {
		t.Fatalf("matching callback callsite rejected: %v", err)
	}
	otherLeaf := routeHopFixtureRef("", "OtherLeaf", "routehops.go", nil)
	otherValid := Registry{Registrations: []SiteRegistration{routeHopRegistration(otherLeaf,
		routeHopRoute(routeHopFixtureRef("", "CallOtherLeafThroughCallback", "routehops.go", nil),
			routeHop("CallOtherLeafThroughCallback", OperationCall, 1, HopDispatchExact, invokeCallback),
			routeHop("InvokeCallback", OperationCall, 1, HopDispatchVTA, otherLeaf)),
	)}}
	if err := validateRouteHopEvidenceForProfile(analysis, otherValid); err != nil {
		t.Fatalf("symmetric matching callback callsite rejected: %v", err)
	}

	invalid := Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
		routeHopRoute(routeHopFixtureRef("", "CallOtherLeafThroughCallback", "routehops.go", nil),
			routeHop("CallOtherLeafThroughCallback", OperationCall, 1, HopDispatchExact, invokeCallback),
			routeHop("InvokeCallback", OperationCall, 1, HopDispatchVTA, leaf)),
	)}}
	err := validateRouteHopEvidenceForProfile(analysis, invalid)
	if err == nil || !strings.Contains(err.Error(), "InvokeCallback") || !strings.Contains(err.Error(), "has no call edge to callee") {
		t.Fatalf("non-matching callback callsite error = %v, want callsite-bound rejection", err)
	}
}

func TestValidateRouteHopEvidenceForProfileDoesNotConfuseInterfaceMethodWithBoundClosure(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	owner := routeHopFixtureRef("", "CallClosureThroughCallbackBesideUnresolvedMethod", "routehops.go", nil)
	bridge := routeHopFixtureRef("", "InvokeCallbackBesideUnresolvedMethod", "routehops.go", nil)
	closure := routeHopFixtureRef("", "CallClosureThroughCallbackBesideUnresolvedMethod", "routehops.go", []int{1})
	registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(closure,
		routeHopRoute(owner,
			routeHop("CallClosureThroughCallbackBesideUnresolvedMethod", OperationCall, 1, HopDispatchExact, bridge),
			routeHop("InvokeCallbackBesideUnresolvedMethod", OperationCall, 1, HopDispatchVTA, closure)),
	)}}

	if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
		t.Fatalf("same-signature interface method contaminated bound closure evidence: %v", err)
	}
}

func TestValidateRouteHopEvidenceForProfileUsesOnlyOneUnambiguousActiveCase(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	valid := routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
		routeHop("DuplicateOwner", OperationCall, 1, HopDispatchExact, leaf))
	invalidInactive := routeHopRoute(routeHopFixtureRef("", "MissingDarwinOwner", "routehops.go", nil))
	registration := routeHopRegistration(leaf, valid)
	registration.Cases = append(registration.Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildDarwinDefault},
		Routes:        []Route{invalidInactive},
	})

	if err := validateRouteHopEvidenceForProfile(analysis, Registry{Registrations: []SiteRegistration{registration}}); err != nil {
		t.Fatalf("inactive route affected Linux evidence: %v", err)
	}

	registration.Cases = append(registration.Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildLinuxDefault},
		Routes:        []Route{valid},
	})
	err := validateRouteHopEvidenceForProfile(analysis, Registry{Registrations: []SiteRegistration{registration}})
	if err == nil || !strings.Contains(err.Error(), "profile has 2 active classification cases") {
		t.Fatalf("overlapping profile cases error = %v, want deterministic profile ambiguity", err)
	}
}

func TestRouteHopFunctionIndexRejectsAmbiguousDerivedIdentity(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	index, problems := newRouteHopFunctionIndex(analysis)
	if len(problems) != 0 {
		t.Fatalf("newRouteHopFunctionIndex() problems: %v", problems)
	}
	ref := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	key := canonicalFunctionRef(ref)
	index.exact[key] = append(index.exact[key], index.exact[key][0])

	var got []string
	if function := index.resolve(ref, "route logical owner", &got); function != nil {
		t.Fatalf("resolve() = %v, want nil for ambiguous evidence", function)
	}
	if len(got) != 1 || !strings.Contains(got[0], "ambiguous") || !strings.Contains(got[0], "2 SSA functions") {
		t.Fatalf("resolve() problems = %q, want ambiguous identity", got)
	}
}

func TestValidateRouteHopEvidenceForProfileDiagnosticsAreDeterministic(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)
	registrations := []SiteRegistration{
		routeHopRegistration(leaf,
			routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
				routeHop("DuplicateOwner", OperationCall, 9, HopDispatchVTA, leaf))),
		routeHopRegistration(workerA,
			routeHopRoute(routeHopFixtureRef("", "InterfaceOwner", "routehops.go", nil),
				routeHop("InterfaceOwner", OperationCall, 1, HopDispatchExact, workerA))),
	}

	var baseline string
	for repetition := 0; repetition < 4; repetition++ {
		permuted := append([]SiteRegistration(nil), registrations...)
		if repetition%2 == 1 {
			permuted[0], permuted[1] = permuted[1], permuted[0]
		}
		err := validateRouteHopEvidenceForProfile(analysis, Registry{Registrations: permuted})
		if err == nil {
			t.Fatal("validateRouteHopEvidenceForProfile() error = nil")
		}
		if baseline == "" {
			baseline = err.Error()
			continue
		}
		if err.Error() != baseline {
			t.Fatalf("diagnostics changed\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
	lines := strings.Split(strings.TrimPrefix(baseline, "effect route-hop evidence failed for profile \"linux/default\":\n- "), "\n- ")
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(lines, sorted) {
		t.Fatalf("diagnostics are not sorted:\n%s", baseline)
	}
}

func TestValidateRouteHopEvidenceForProfileIsConcurrentAndReadOnly(t *testing.T) {
	analysis := loadRouteHopFixture(t)
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
		routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
			routeHop("DuplicateOwner", OperationCall, 2, HopDispatchExact, leaf)),
	)}}

	const workers = 8
	errors := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			errors <- validateRouteHopEvidenceForProfile(analysis, registry)
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("validateRouteHopEvidenceForProfile() error: %v", err)
		}
	}
}

func loadRouteHopFixture(t *testing.T) *loadedAnalysis {
	t.Helper()
	analysis, err := loadAnalysis(context.Background(), fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/routehops",
	}), fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	return analysis
}

func routeHopRegistration(endpoint FunctionRef, routes ...Route) SiteRegistration {
	return SiteRegistration{
		BoundaryID: "route-hop.fixture",
		Matcher: OperationSite{
			Operation: OperationCall,
			Enclosing: endpoint,
			Ordinal:   1,
		},
		Cases: []ProfileCase{{
			BuildProfiles: []BuildProfileID{BuildLinuxDefault},
			Routes:        routes,
		}},
	}
}

func routeHopRoute(owner FunctionRef, hops ...RouteHop) Route {
	return Route{LogicalOwner: owner, Hops: hops}
}

func routeHop(enclosingName string, operation OperationKind, ordinal int, dispatch HopDispatchKind, callee FunctionRef) RouteHop {
	return RouteHop{
		Site: OperationSite{
			Operation: operation,
			Enclosing: routeHopFixtureRef("", enclosingName, "routehops.go", nil),
			Ordinal:   ordinal,
		},
		Dispatch: dispatch,
		Callee:   callee,
	}
}

func routeHopFixtureRef(receiver, name, file string, closure []int) FunctionRef {
	return FunctionRef{
		Object: ObjectRef{
			Package:  routeHopFixturePackage,
			Receiver: receiver,
			Name:     name,
		},
		File:        "internal/reconciletest/effectinventory/testdata/analyzerfixture/routehops/" + file,
		ClosurePath: append([]int(nil), closure...),
	}
}
