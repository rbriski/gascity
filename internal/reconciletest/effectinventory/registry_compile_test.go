package effectinventory

import (
	"reflect"
	"testing"
)

func TestCompileRegistryRejectsDiscoveredSitesWithoutRegistrationsDeterministically(t *testing.T) {
	registry := compileRegistryFixture()
	observed := observedForRegistry(registry)
	unknownA := observed[0]
	unknownA.Matcher.Ordinal = 2
	unknownB := observed[0]
	unknownB.Matcher.Enclosing = functionRef(
		"github.com/gastownhall/gascity/cmd/gc",
		"cmd/gc/other_recovery.go",
		"restoreOtherRoutes",
	)
	observed = append(observed, unknownA, unknownB)

	var baseline string
	for repetition := 0; repetition < 4; repetition++ {
		candidate := append([]ObservedSite(nil), observed...)
		if repetition%2 == 1 {
			for left, right := 0, len(candidate)-1; left < right; left, right = left+1, right-1 {
				candidate[left], candidate[right] = candidate[right], candidate[left]
			}
		}
		_, err := CompileRegistry(registry, candidate, validationDate())
		assertErrorContains(t, err, "discovered site", "has no registration")
		if repetition == 0 {
			baseline = err.Error()
		} else if err.Error() != baseline {
			t.Fatalf("CompileRegistry() diagnostics changed with discovery order\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
}

func TestCompileRegistryRejectsStaleRegistrationProfile(t *testing.T) {
	registry := compileRegistryFixture()
	registry.Registrations[0].Cases = append(registry.Registrations[0].Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildLinuxDefault},
		Routes:        []Route{registry.Registrations[0].Cases[0].Routes[0]},
	})

	_, err := CompileRegistry(registry, observedForRegistry(Registry{
		Boundaries: registry.Boundaries,
		Registrations: []SiteRegistration{{
			BoundaryID: registry.Registrations[0].BoundaryID,
			Matcher:    registry.Registrations[0].Matcher,
			Cases:      registry.Registrations[0].Cases[:1],
		}},
	}), validationDate())
	assertErrorContains(t, err, "stale registration", `build profile "linux/default"`)
}

func TestCompileRegistryRejectsDuplicateClassificationForSiteProfile(t *testing.T) {
	registry := compileRegistryFixture()
	duplicate := registry.Registrations[0].Cases[0]
	registry.Registrations[0].Cases = append(registry.Registrations[0].Cases, duplicate)

	_, err := CompileRegistry(registry, observedForRegistry(compileRegistryFixture()), validationDate())
	assertErrorContains(t, err, `build profile "darwin/default" has multiple classification cases`)
}

func TestCompileRegistryAcceptsMultipleLogicalRoutesForOneSite(t *testing.T) {
	registry := compileRegistryFixture()
	second := registry.Registrations[0].Cases[0].Routes[0]
	second.ExecutingProcess = ProcessAPIInController
	second.LogicalOwner = functionRef(
		"github.com/gastownhall/gascity/internal/api",
		"internal/api/routes.go",
		"recoverRoute",
	)
	second.Hops = []RouteHop{{
		Site: OperationSite{
			Operation: OperationCall,
			Enclosing: second.LogicalOwner,
			Ordinal:   1,
		},
		Dispatch: HopDispatchExact,
		Callee:   registry.Registrations[0].Matcher.Enclosing,
	}}
	registry.Registrations[0].Cases[0].Routes = append(
		registry.Registrations[0].Cases[0].Routes,
		second,
	)

	compiled, err := CompileRegistry(registry, observedForRegistry(registry), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry() rejected multiple routes to one physical site: %v", err)
	}
	got := compiled.Registrations[0].Cases[0].Routes
	if len(got) != 2 {
		t.Fatalf("compiled routes = %d, want 2", len(got))
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("derived route IDs collided: %q", got[0].ID)
	}
}

func TestCompileRegistryAcceptsDisjointProfileCasesAndCanonicalizesOutput(t *testing.T) {
	registry := compileRegistryFixture()
	darwin := registry.Registrations[0].Cases[0]
	linux := darwin
	linux.BuildProfiles = []BuildProfileID{BuildLinuxDefault}
	linux.Routes = append([]Route(nil), darwin.Routes...)
	linux.Routes[0].ExecutingProcess = ProcessForegroundCLI
	registry.Registrations[0].Cases = []ProfileCase{linux, darwin}

	observed := observedForRegistry(registry)
	compiled, err := CompileRegistry(registry, observed, validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry() rejected disjoint profile cases: %v", err)
	}
	if got := compiled.Registrations[0].Cases[0].BuildProfiles; !reflect.DeepEqual(got, []BuildProfileID{BuildDarwinDefault}) {
		t.Fatalf("first compiled profile case = %q, want canonical Darwin case", got)
	}
	if got := compiled.Registrations[0].Cases[1].BuildProfiles; !reflect.DeepEqual(got, []BuildProfileID{BuildLinuxDefault}) {
		t.Fatalf("second compiled profile case = %q, want canonical Linux case", got)
	}
}

func compileRegistryFixture() Registry {
	leaf := functionRef(
		"github.com/gastownhall/gascity/cmd/gc",
		"cmd/gc/route_recovery.go",
		"restoreCarriedWorkRoutes",
	)
	return Registry{
		Boundaries: []BoundaryDefinition{{
			ID:   "beads.store.set-metadata",
			Kind: KindStoreMutation,
			Object: ObjectRef{
				Package:  "github.com/gastownhall/gascity/internal/beads",
				Receiver: "Store",
				Name:     "SetMetadata",
			},
			Match: ObjectMatchInterfaceImplementors,
		}},
		Registrations: []SiteRegistration{{
			BoundaryID: "beads.store.set-metadata",
			Matcher: OperationSite{
				Operation: OperationCall,
				Enclosing: leaf,
				Ordinal:   1,
			},
			Cases: []ProfileCase{{
				BuildProfiles: []BuildProfileID{BuildDarwinDefault},
				Routes: []Route{{
					StoreDomain:      StoreDomainRouteRecovery,
					ActionFamily:     FamilyRouteRecovery,
					ExecutingProcess: ProcessController,
					LogicalOwner:     leaf,
					Target:           compileFixtureTarget(),
					Fences:           compileFixtureFences(),
					CurrentGate:      GateRef{Kind: GateUnconditionalLegacy},
					Disposition:      compileFixtureDisposition(),
					AccessPath:       AccessRawStoreBypass,
					Continuation:     Continuation{Locus: ContinuationInline, Completion: CompletionSynchronous},
					OwningTests:      []TestRef{{Package: "github.com/gastownhall/gascity/cmd/gc", Name: "TestRestoreCarriedWorkRoutesSkipsCacheStaleClaimedBead"}},
					Exception:        compileFixtureException(),
				}},
			}},
		}},
	}
}

func compileFixtureTarget() TargetRef {
	return TargetRef{
		Kind:   TargetDurableRecord,
		Sink:   ValueSlot{Kind: SlotParameter, Index: 1},
		Source: TargetSourceStoreLiveReread,
		SourceObject: ObjectRef{
			Package:  "github.com/gastownhall/gascity/internal/beads",
			Receiver: "Store",
			Name:     "Get",
		},
		SourceSlot: ValueSlot{Kind: SlotResult, Index: 1},
		Detail:     "snapshot bead ID revalidated by cache-bypassing live read",
	}
}

func compileFixtureFences() []Fence {
	return []Fence{{
		Kind: FenceLiveRereadNonCAS,
		Source: ObjectRef{
			Package:  "github.com/gastownhall/gascity/internal/beads",
			Receiver: "Store",
			Name:     "Get",
		},
	}}
}

func compileFixtureDisposition() Disposition {
	return Disposition{
		Kind:   DispositionReplaceAtGate,
		Gates:  []TaskRef{"P2.0", "P2.10A"},
		Reason: "move route recovery to the conditional shared writer",
	}
}

func compileFixtureException() *TemporaryException {
	return &TemporaryException{
		Kind:         ExceptionWeakFence,
		Reason:       "live reread is not atomic with the following metadata write",
		OwnerTask:    "P0.1",
		RemovalTasks: []TaskRef{"P2.0", "P2.10A"},
		Anchor: VersionAnchor{
			Kind:  AnchorGitCommit,
			Value: "7378aa936f449566657d7a7c6e49a1ff88b29373",
		},
		Expires: "2026-08-31",
		OwningTest: TestRef{
			Package: "github.com/gastownhall/gascity/cmd/gc",
			Name:    "TestRestoreCarriedWorkRoutesSkipsCacheStaleClaimedBead",
		},
	}
}

func observedForRegistry(registry Registry) []ObservedSite {
	var observed []ObservedSite
	for _, registration := range registry.Registrations {
		for _, profileCase := range registration.Cases {
			for _, profile := range profileCase.BuildProfiles {
				observed = append(observed, ObservedSite{
					BoundaryID: registration.BoundaryID,
					Matcher:    registration.Matcher,
					Profile:    profile,
				})
			}
		}
	}
	return observed
}
