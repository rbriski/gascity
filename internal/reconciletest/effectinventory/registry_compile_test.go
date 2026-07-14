package effectinventory

import (
	"reflect"
	"strings"
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
		_, err := CompileRegistry(registry, discoveryWithSites(registry, candidate), validationDate())
		assertErrorContains(t, err, "discovered site", "has no registration")
		if repetition == 0 {
			baseline = err.Error()
		} else if err.Error() != baseline {
			t.Fatalf("CompileRegistry() diagnostics changed with discovery order\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
}

func TestCompileRegistryRejectsDiscoveredProfileWithoutClassificationCase(t *testing.T) {
	registry := compileRegistryFixture()
	discovery := discoveryForRegistry(registry)
	additional := observedForRegistry(registry)[0]
	additional.Profile = BuildLinuxDefault
	for index := range discovery.Profiles {
		if discovery.Profiles[index].Profile == BuildLinuxDefault {
			discovery.Profiles[index].Sites = append(discovery.Profiles[index].Sites, additional)
			break
		}
	}

	_, err := CompileRegistry(registry, discovery, validationDate())
	assertErrorContains(t, err, "discovered site", "has no classification case")
}

func TestCompileRegistryRequiresEveryCanonicalDiscoveryProfileExactlyOnce(t *testing.T) {
	registry := compileRegistryFixture()
	allDiscovery := discoveryForRegistry(registry)

	t.Run("missing profile", func(t *testing.T) {
		missing := allDiscovery
		missing.Profiles = missing.Profiles[:len(missing.Profiles)-1]
		_, err := CompileRegistry(registry, missing, validationDate())
		assertErrorContains(t, err, "missing discovery profile")
	})

	t.Run("duplicate profile", func(t *testing.T) {
		duplicate := allDiscovery
		duplicate.Profiles = append([]ProfileDiscovery(nil), allDiscovery.Profiles...)
		duplicate.Profiles = append(duplicate.Profiles, allDiscovery.Profiles[0])
		_, err := CompileRegistry(registry, duplicate, validationDate())
		assertErrorContains(t, err, "duplicate discovery profile")
	})

	t.Run("site profile mismatch", func(t *testing.T) {
		mismatch := discoveryForRegistry(registry)
		for index := range mismatch.Profiles {
			if len(mismatch.Profiles[index].Sites) == 0 {
				continue
			}
			mismatch.Profiles[index].Sites[0].Profile = BuildLinuxDefault
			break
		}
		_, err := CompileRegistry(registry, mismatch, validationDate())
		assertErrorContains(t, err, "contains site labeled with build profile")
	})
}

func TestCompileRegistryBindsDiscoveryToExactBoundaryVocabulary(t *testing.T) {
	registry := compileRegistryFixture()
	discovery := discoveryForRegistry(registry)
	registry.Boundaries[0].Object.Name = "Update"

	_, err := CompileRegistry(registry, discovery, validationDate())
	assertErrorContains(t, err, "discovery boundary digest", "does not match registry digest")
}

func TestCompileRegistryRejectsStaleRegistrationProfile(t *testing.T) {
	registry := compileRegistryFixture()
	registry.Registrations[0].Cases = append(registry.Registrations[0].Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildLinuxDefault},
		Routes:        []Route{registry.Registrations[0].Cases[0].Routes[0]},
	})

	_, err := CompileRegistry(registry, discoveryForRegistry(Registry{
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

	_, err := CompileRegistry(registry, discoveryForRegistry(compileRegistryFixture()), validationDate())
	assertErrorContains(t, err, `build profile "darwin/default" has multiple classification cases`)
}

func TestCompileRegistryRejectsDuplicateAnalyzerObservation(t *testing.T) {
	registry := compileRegistryFixture()
	discovery := discoveryForRegistry(registry)
	for index := range discovery.Profiles {
		if len(discovery.Profiles[index].Sites) == 0 {
			continue
		}
		discovery.Profiles[index].Sites = append(
			discovery.Profiles[index].Sites,
			discovery.Profiles[index].Sites[0],
		)
		break
	}

	_, err := CompileRegistry(registry, discovery, validationDate())
	assertErrorContains(t, err, "was reported more than once by discovery")
}

func TestCompileRegistryRejectsConflictingClassificationsForOneLogicalOrigin(t *testing.T) {
	registry := compileRegistryFixture()
	conflicting := registry.Registrations[0].Cases[0].Routes[0]
	conflicting.ActionFamily = FamilyMaintenance
	registry.Registrations[0].Cases[0].Routes = append(
		registry.Registrations[0].Cases[0].Routes,
		conflicting,
	)

	_, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate())
	assertErrorContains(t, err, "logical origin", "has multiple classifications")
}

func TestCompileRegistryAcceptsMultipleLogicalRoutesForOneSite(t *testing.T) {
	registry := compileRegistryFixture()
	registry.Registrations[0].Cases[0].Routes = append(
		registry.Registrations[0].Cases[0].Routes,
		secondLogicalRoute(registry),
	)

	compiled, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate())
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

	discovery := discoveryForRegistry(registry)
	compiled, err := CompileRegistry(registry, discovery, validationDate())
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

func TestCompileRegistryRouteIDsCoverProfileAndSafetyClassification(t *testing.T) {
	base := compileRegistryFixture()
	baseID := compiledFixtureRouteID(t, base)
	if value := string(baseID); !strings.HasPrefix(value, "route-v2-") || len(value) != len("route-v2-")+64 {
		t.Fatalf("derived route ID = %q, want route-v2- plus a full SHA-256 digest", value)
	}

	tests := []struct {
		name   string
		mutate func(*ProfileCase, *Route)
	}{
		{"target safety signature", func(_ *ProfileCase, route *Route) { route.Target = batchTarget(TargetCardinalitySet) }},
		{"target identity projection", func(_ *ProfileCase, route *Route) {
			route.Target.Identities[0].Projection = objectRef(beadsPackage, "Bead", "ID")
		}},
		{"gate", func(_ *ProfileCase, route *Route) {
			route.CurrentGate = GateRef{
				Kind:      GatePredicate,
				Predicate: objectRef("github.com/gastownhall/gascity/cmd/gc", "CityRuntime", "routeRecoveryEnabled"),
				Expected:  "true",
			}
		}},
		{"disposition reason", func(_ *ProfileCase, route *Route) { route.Disposition.Reason += " after rollout" }},
		{"owning test", func(_ *ProfileCase, route *Route) {
			route.OwningTests[0].Name = "TestRestoreCarriedWorkRoutesRejectsStaleGeneration"
		}},
		{"exception expiry", func(_ *ProfileCase, route *Route) { route.Exception.Expires = "2026-09-30" }},
		{"profile case", func(profileCase *ProfileCase, _ *Route) {
			profileCase.BuildProfiles = []BuildProfileID{BuildLinuxDefault}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := compileRegistryFixture()
			profileCase := &registry.Registrations[0].Cases[0]
			tt.mutate(profileCase, &profileCase.Routes[0])
			if got := compiledFixtureRouteID(t, registry); got == baseID {
				t.Fatalf("route ID stayed %q after %s changed", got, tt.name)
			}
		})
	}
}

func TestCanonicalRouteCoversEveryAuthoredRouteField(t *testing.T) {
	base := compileRegistryFixture().Registrations[0].Cases[0].Routes[0]
	wantDifferent := []struct {
		name   string
		mutate func(*Route)
	}{
		{"StoreDomain", func(route *Route) { route.StoreDomain = StoreDomainMaintenance }},
		{"ActionFamily", func(route *Route) { route.ActionFamily = FamilyMaintenance }},
		{"ExecutingProcess", func(route *Route) { route.ExecutingProcess = ProcessForegroundCLI }},
		{"LogicalOwner", func(route *Route) { route.LogicalOwner.Object.Name = "otherOwner" }},
		{"Target", func(route *Route) { route.Target.Detail += " changed" }},
		{"Fences", func(route *Route) { route.Fences[0].Source.Name = "OtherGet" }},
		{"CurrentGate", func(route *Route) { route.CurrentGate.Expected = "changed" }},
		{"Disposition", func(route *Route) { route.Disposition.Reason += " changed" }},
		{"AccessPath", func(route *Route) { route.AccessPath = AccessSessionStoreFrontDoor }},
		{"Continuation", func(route *Route) { route.Continuation.Completion = CompletionDetached }},
		{"Hops", func(route *Route) {
			route.Hops = []RouteHop{{
				Site:     OperationSite{Operation: OperationCall, Enclosing: route.LogicalOwner, Ordinal: 1},
				Dispatch: HopDispatchExact,
				Callee:   route.LogicalOwner,
			}}
		}},
		{"OwningTests", func(route *Route) { route.OwningTests[0].Name = "TestOther" }},
		{"Exception", func(route *Route) { route.Exception.Expires = "2026-09-30" }},
	}

	baseline := canonicalRoute(base)
	for _, tt := range wantDifferent {
		t.Run(tt.name, func(t *testing.T) {
			candidate := cloneRoute(base)
			tt.mutate(&candidate)
			if got := canonicalRoute(candidate); got == baseline {
				t.Fatalf("canonicalRoute() did not cover Route.%s", tt.name)
			}
		})
	}
}

func TestCanonicalTargetCoversEverySemanticTargetField(t *testing.T) {
	base := compileFixtureTarget()
	wantDifferent := []struct {
		name   string
		mutate func(*TargetRef)
	}{
		{"Kind", func(target *TargetRef) { target.Kind = TargetSessionIdentity }},
		{"Cardinality", func(target *TargetRef) { target.Cardinality = TargetCardinalitySet }},
		{"Identity", func(target *TargetRef) { target.Identity = TargetIdentityGenerated }},
		{"Signature", func(target *TargetRef) { target.Signature = TargetSignatureBatch }},
		{"Identities", func(target *TargetRef) { target.Identities[0].Projection = objectRef(beadsPackage, "Bead", "ID") }},
	}

	baseline := canonicalTargetRef(base)
	for _, tt := range wantDifferent {
		t.Run(tt.name, func(t *testing.T) {
			candidate := cloneRoute(Route{Target: base}).Target
			tt.mutate(&candidate)
			if got := canonicalTargetRef(candidate); got == baseline {
				t.Fatalf("canonicalTargetRef() did not cover TargetRef.%s", tt.name)
			}
		})
	}
}

func TestCanonicalTargetIdentityCoversEveryAuthoredField(t *testing.T) {
	base := compileFixtureTarget().Identities[0]
	wantDifferent := []struct {
		name   string
		mutate func(*TargetIdentityRef)
	}{
		{"Role", func(identity *TargetIdentityRef) { identity.Role = TargetRoleInput }},
		{"BoundarySlot", func(identity *TargetIdentityRef) { identity.BoundarySlot.Index++ }},
		{"Projection", func(identity *TargetIdentityRef) { identity.Projection = objectRef(beadsPackage, "Bead", "ID") }},
		{"Source", func(identity *TargetIdentityRef) { identity.Source = TargetSourceFunctionResult }},
		{"SourceObject", func(identity *TargetIdentityRef) { identity.SourceObject.Name = "OtherGet" }},
		{"SourceSlot", func(identity *TargetIdentityRef) { identity.SourceSlot.Index++ }},
	}

	baseline := canonicalTargetIdentityRef(base)
	for _, tt := range wantDifferent {
		t.Run(tt.name, func(t *testing.T) {
			candidate := base
			tt.mutate(&candidate)
			if got := canonicalTargetIdentityRef(candidate); got == baseline {
				t.Fatalf("canonicalTargetIdentityRef() did not cover TargetIdentityRef.%s", tt.name)
			}
		})
	}
}

func TestCanonicalIdentityStructFieldsArePinned(t *testing.T) {
	assertStructFields(t, reflect.TypeOf(BoundaryDefinition{}), "ID", "Kind", "Object", "Match", "Input", "Output")
	assertStructFields(t, reflect.TypeOf(Route{}), "StoreDomain", "ActionFamily", "ExecutingProcess", "LogicalOwner", "Target", "Fences", "CurrentGate", "Disposition", "AccessPath", "Continuation", "Hops", "OwningTests", "Exception")
	assertStructFields(t, reflect.TypeOf(TargetRef{}), "Kind", "Cardinality", "Identity", "Signature", "Identities", "Detail")
	assertStructFields(t, reflect.TypeOf(TargetIdentityRef{}), "Role", "BoundarySlot", "Projection", "Source", "SourceObject", "SourceSlot")
	assertStructFields(t, reflect.TypeOf(Fence{}), "Kind", "Source", "Token")
	assertStructFields(t, reflect.TypeOf(GateRef{}), "Kind", "Predicate", "Expected", "Conditions")
	assertStructFields(t, reflect.TypeOf(GateCondition{}), "Kind", "Predicate", "Parameter", "Capability", "Expected")
	assertStructFields(t, reflect.TypeOf(GateParameterRef{}), "Function", "Slot")
	assertStructFields(t, reflect.TypeOf(Disposition{}), "Kind", "Gates", "Reason")
	assertStructFields(t, reflect.TypeOf(Continuation{}), "Locus", "Completion")
	assertStructFields(t, reflect.TypeOf(RouteHop{}), "Site", "Dispatch", "Callee")
	assertStructFields(t, reflect.TypeOf(TemporaryException{}), "Kind", "Reason", "OwnerTask", "RemovalTasks", "Anchor", "Expires", "OwningTest")
	assertStructFields(t, reflect.TypeOf(VersionAnchor{}), "Kind", "Value")
	assertStructFields(t, reflect.TypeOf(TestRef{}), "Package", "Name")
	assertStructFields(t, reflect.TypeOf(OperationSite{}), "Operation", "Enclosing", "Ordinal")
	assertStructFields(t, reflect.TypeOf(FunctionRef{}), "Object", "File", "ClosurePath")
	assertStructFields(t, reflect.TypeOf(ObjectRef{}), "Package", "Receiver", "Name")
	assertStructFields(t, reflect.TypeOf(ValueSlot{}), "Kind", "Index")
}

func TestRegistryCanonicalKeysDoNotAliasDelimiterShapedReferences(t *testing.T) {
	left := ObjectRef{Package: "p", Receiver: "r", Name: "n.x"}
	right := ObjectRef{Package: "p", Receiver: "r.n", Name: "x"}
	if left.key() != right.key() {
		t.Fatalf("test precondition failed: legacy delimiter keys no longer alias: %q != %q", left.key(), right.key())
	}
	if canonicalObjectRef(left) == canonicalObjectRef(right) {
		t.Fatalf("canonical object references alias for distinct values: %#v and %#v", left, right)
	}

	leftSite := OperationSite{Operation: OperationCall, Enclosing: FunctionRef{Object: left, File: "left.go"}, Ordinal: 1}
	rightSite := OperationSite{Operation: OperationCall, Enclosing: FunctionRef{Object: right, File: "left.go"}, Ordinal: 1}
	if registrationPhysicalKey("boundary", leftSite) == registrationPhysicalKey("boundary", rightSite) {
		t.Fatal("physical registration keys alias for distinct structured references")
	}
}

func TestCompileRegistryCanonicalizesInputAndDoesNotAliasIt(t *testing.T) {
	registry := compileRegistryFixture()
	extraBoundary := registry.Boundaries[0]
	extraBoundary.ID = "beads.store.update"
	extraBoundary.Object.Name = "Update"
	registry.Boundaries = append(registry.Boundaries, extraBoundary)
	registry.Registrations[0].Cases[0].Routes = append(
		registry.Registrations[0].Cases[0].Routes,
		secondLogicalRoute(registry),
	)
	secondRegistration := registry.Registrations[0]
	secondRegistration.Matcher.Ordinal = 2
	registry.Registrations = append(registry.Registrations, secondRegistration)
	targetRegistrationID := deriveSiteRegistrationID(
		registry.Registrations[0].BoundaryID,
		registry.Registrations[0].Matcher,
	)
	targetRouteID := deriveRouteID(
		targetRegistrationID,
		registry.Registrations[0].Cases[0].BuildProfiles,
		registry.Registrations[0].Cases[0].Routes[0],
	)

	compiled, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry() failed: %v", err)
	}

	permuted := registry
	permuted.Boundaries = append([]BoundaryDefinition(nil), registry.Boundaries...)
	reverse(permuted.Boundaries)
	permuted.Registrations = append([]SiteRegistration(nil), registry.Registrations...)
	for index := range permuted.Registrations {
		permuted.Registrations[index].Cases = append([]ProfileCase(nil), permuted.Registrations[index].Cases...)
		for caseIndex := range permuted.Registrations[index].Cases {
			permuted.Registrations[index].Cases[caseIndex].Routes = append(
				[]Route(nil),
				permuted.Registrations[index].Cases[caseIndex].Routes...,
			)
			reverse(permuted.Registrations[index].Cases[caseIndex].Routes)
		}
		reverse(permuted.Registrations[index].Cases)
	}
	reverse(permuted.Registrations)
	permutedDiscovery := discoveryForRegistry(permuted)
	reverse(permutedDiscovery.Profiles)
	for index := range permutedDiscovery.Profiles {
		reverse(permutedDiscovery.Profiles[index].Sites)
	}
	permutedCompiled, err := CompileRegistry(permuted, permutedDiscovery, validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry(permuted) failed: %v", err)
	}
	if !reflect.DeepEqual(permutedCompiled, compiled) {
		t.Fatalf("compiled output changed with input order\n got: %#v\nwant: %#v", permutedCompiled, compiled)
	}

	registry.Boundaries[0].Object.Name = "Changed"
	registry.Registrations[0].Cases[0].BuildProfiles[0] = BuildLinuxDefault
	registry.Registrations[0].Cases[0].Routes[0].Fences[0].Kind = FenceNone
	registry.Registrations[0].Cases[0].Routes[0].Disposition.Gates[0] = "P9.changed"
	registry.Registrations[0].Cases[0].Routes[0].OwningTests[0].Name = "TestChanged"
	registry.Registrations[0].Cases[0].Routes[0].Exception.RemovalTasks[0] = "P9.changed"
	compiledRegistration := compiledRegistrationByID(t, compiled, targetRegistrationID)
	compiledRoute := compiledRouteByID(t, compiledRegistration.Cases[0], targetRouteID)
	if compiledBoundaryByID(t, compiled, "beads.store.set-metadata").Object.Name == "Changed" ||
		compiledRegistration.Cases[0].BuildProfiles[0] == BuildLinuxDefault ||
		compiledRoute.Definition.Fences[0].Kind == FenceNone ||
		compiledRoute.Definition.Disposition.Gates[0] == "P9.changed" ||
		compiledRoute.Definition.OwningTests[0].Name == "TestChanged" ||
		compiledRoute.Definition.Exception.RemovalTasks[0] == "P9.changed" {
		t.Fatal("compiled registry retained mutable input slices")
	}
}

func TestCompileRegistryBoundaryDiagnosticsIgnoreDefinitionOrder(t *testing.T) {
	registry := compileRegistryFixture()
	duplicateID := registry.Boundaries[0]
	duplicateID.Object.Name = "Update"
	registry.Boundaries = append(registry.Boundaries, duplicateID)
	discovery := discoveryForRegistry(registry)

	_, firstErr := CompileRegistry(registry, discovery, validationDate())
	if firstErr == nil {
		t.Fatal("CompileRegistry() error = nil, want duplicate boundary diagnostic")
	}
	reverse(registry.Boundaries)
	_, secondErr := CompileRegistry(registry, discovery, validationDate())
	if secondErr == nil {
		t.Fatal("CompileRegistry(reversed) error = nil, want duplicate boundary diagnostic")
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("boundary diagnostics changed with definition order\n first:\n%s\nsecond:\n%s", firstErr, secondErr)
	}
}

func compiledFixtureRouteID(t *testing.T, registry Registry) RouteID {
	t.Helper()
	compiled, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry() failed: %v", err)
	}
	return compiled.Registrations[0].Cases[0].Routes[0].ID
}

func secondLogicalRoute(registry Registry) Route {
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
	return second
}

func reverse[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func compiledBoundaryByID(t *testing.T, compiled CompiledRegistry, id string) BoundaryDefinition {
	t.Helper()
	for _, boundary := range compiled.Boundaries {
		if boundary.ID == id {
			return boundary
		}
	}
	t.Fatalf("compiled boundary %q not found", id)
	return BoundaryDefinition{}
}

func compiledRegistrationByID(t *testing.T, compiled CompiledRegistry, id SiteRegistrationID) CompiledSiteRegistration {
	t.Helper()
	for _, registration := range compiled.Registrations {
		if registration.ID == id {
			return registration
		}
	}
	t.Fatalf("compiled registration %q not found", id)
	return CompiledSiteRegistration{}
}

func compiledRouteByID(t *testing.T, profileCase CompiledProfileCase, id RouteID) CompiledRoute {
	t.Helper()
	for _, route := range profileCase.Routes {
		if route.ID == id {
			return route
		}
	}
	t.Fatalf("compiled route %q not found", id)
	return CompiledRoute{}
}

func assertStructFields(t *testing.T, structType reflect.Type, want ...string) {
	t.Helper()
	got := make([]string, structType.NumField())
	for index := range got {
		got[index] = structType.Field(index).Name
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s fields = %q, want %q; update the canonical identity schema deliberately", structType.Name(), got, want)
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
		Kind:        TargetDurableRecord,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityExisting,
		Signature:   TargetSignatureDirect,
		Identities: []TargetIdentityRef{{
			Role:         TargetRolePrimary,
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
			Source:       TargetSourceStoreLiveReread,
			SourceObject: ObjectRef{
				Package:  "github.com/gastownhall/gascity/internal/beads",
				Receiver: "Store",
				Name:     "Get",
			},
			SourceSlot: ValueSlot{Kind: SlotResult, Index: 1},
		}},
		Detail: "snapshot bead ID revalidated by cache-bypassing live read",
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

func discoveryForRegistry(registry Registry) DiscoveryResult {
	return discoveryWithSites(registry, observedForRegistry(registry))
}

func discoveryWithSites(registry Registry, sites []ObservedSite) DiscoveryResult {
	profiles := canonicalAnalysisProfiles()
	discovery := DiscoveryResult{
		BoundaryDigest: deriveBoundaryDigest(registry.Boundaries),
		Profiles:       make([]ProfileDiscovery, len(profiles)),
	}
	byProfile := make(map[BuildProfileID]int, len(profiles))
	for index, profile := range profiles {
		discovery.Profiles[index].Profile = profile.ID
		byProfile[profile.ID] = index
	}
	for _, site := range sites {
		index, ok := byProfile[site.Profile]
		if !ok {
			discovery.Profiles = append(discovery.Profiles, ProfileDiscovery{
				Profile: site.Profile,
				Sites:   []ObservedSite{site},
			})
			continue
		}
		discovery.Profiles[index].Sites = append(discovery.Profiles[index].Sites, site)
	}
	return discovery
}
