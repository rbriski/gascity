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
	closure := routeHopFixtureRef("", "ClosureOwner", "routehops.go", []int{1})
	workerA := routeHopFixtureRef("WorkerA", "Run", "routehops.go", nil)

	registry := Registry{Registrations: []SiteRegistration{
		routeHopRegistration(leaf,
			routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
				routeHop("DuplicateOwner", OperationCall, 1, HopDispatchExact, leaf)),
			routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
				routeHop("DuplicateOwner", OperationCall, 2, HopDispatchExact, leaf)),
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
		),
	}}

	if err := validateRouteHopEvidenceForProfile(analysis, registry); err != nil {
		t.Fatalf("validateRouteHopEvidenceForProfile() error: %v", err)
	}
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
	lines := strings.Split(strings.TrimPrefix(baseline, `effect route-hop evidence failed for profile "linux/default":\n- `), "\n- ")
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
