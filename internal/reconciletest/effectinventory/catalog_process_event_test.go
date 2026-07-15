package effectinventory

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	processScaffoldFingerprint                 = "41f7034b7d9916f42c3831c12d24eee7f45891ea673440a8e4e5e001b8d42c24"
	processManagedDoltExplicitRouteFingerprint = "77d375c5e58dd2de0672a8427293a348da5ff68383287ba57359c7894c3db11b"
	eventScaffoldFingerprint                   = "6343a7f98fdd77721801ae14dd7ce39b825df878421e88fe304fdc8791e48c5a"
)

func TestProcessCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindProcessMutation, processCatalogSiteRows(), 54, "process-scaffold-site-v1", processScaffoldFingerprint)
}

func TestEventCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindEventEmission, eventCatalogSiteRows(), 90, "event-scaffold-site-v1", eventScaffoldFingerprint)
}

func assertProcessEventCatalogScaffold(
	t *testing.T,
	kind EffectKind,
	rows []catalogSiteRow,
	wantCount int,
	fingerprintKind string,
	wantFingerprint string,
) {
	t.Helper()
	if got := len(rows); got != wantCount {
		t.Fatalf("%s catalog rows = %d, want scaffold count %d", kind, got, wantCount)
	}

	typedBoundaries := make(map[string]bool)
	for _, boundary := range CanonicalBoundaries() {
		if boundary.Kind == kind {
			typedBoundaries[boundary.ID] = true
		}
	}
	seen := make(map[string]bool, len(rows))
	records := make([]string, 0, len(rows))
	for _, row := range rows {
		if !typedBoundaries[row.BoundaryID] {
			t.Errorf("boundary from another typed partition leaked into %s catalog: %q", kind, row.BoundaryID)
		}
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		if seen[physicalKey] {
			t.Errorf("%s physical site appears more than once: %s", kind, describePhysicalSite(row.BoundaryID, row.Matcher))
		}
		seen[physicalKey] = true
		profiles := append([]BuildProfileID(nil), row.Profiles...)
		sort.Slice(profiles, func(i, j int) bool { return profiles[i] < profiles[j] })
		records = append(records, canonicalFields(
			fingerprintKind,
			row.BoundaryID,
			canonicalOperationSite(row.Matcher),
			canonicalBuildProfiles(profiles),
		))
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != wantFingerprint {
		t.Fatalf("%s scaffold fingerprint = %q, want exact matcher/profile fingerprint %q", kind, fingerprint, wantFingerprint)
	}
}

func TestProcessCatalogUsesOnlyClosedKnownSemanticClasses(t *testing.T) {
	assertProcessEventCatalogClasses(t, processCatalogRouteClasses(), processCatalogSiteRows(), knownProcessCatalogClassID)
}

func TestEventCatalogUsesOnlyClosedKnownSemanticClasses(t *testing.T) {
	assertProcessEventCatalogClasses(t, eventCatalogRouteClasses(), eventCatalogSiteRows(), knownEventCatalogClassID)
}

func TestProcessCatalogUsesExactTargetSlotsAndBoundsDirectBypasses(t *testing.T) {
	registrations, err := processInventoryRegistrations()
	if err != nil {
		t.Fatalf("processInventoryRegistrations() failed: %v", err)
	}
	for _, registration := range registrations {
		for _, route := range registration.Cases[0].Routes {
			if route.Target.Signature != TargetSignatureProcess {
				t.Errorf("%s target signature = %q, want %q", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Target.Signature, TargetSignatureProcess)
			}
			if got := len(route.Target.Identities); got != 1 {
				t.Errorf("%s target identities = %d, want one primary identity", describePhysicalSite(registration.BoundaryID, registration.Matcher), got)
				continue
			}
			wantSlot := ValueSlot{Kind: SlotParameter, Index: 1}
			switch registration.BoundaryID {
			case "os.process.Kill", "workspacesvc.manager.Reload", "workspacesvc.manager.Tick", "workspacesvc.manager.Close":
				wantSlot = ValueSlot{Kind: SlotReceiver}
			}
			if got := route.Target.Identities[0].BoundarySlot; got != wantSlot {
				t.Errorf("%s target slot = %#v, want exact boundary slot %#v", describePhysicalSite(registration.BoundaryID, registration.Matcher), got, wantSlot)
			}

			if registration.BoundaryID == "os.process.Kill" {
				if route.AccessPath != AccessDirectProcessBypass {
					t.Errorf("%s access = %q, want %q", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.AccessPath, AccessDirectProcessBypass)
				}
				if route.Disposition.Kind != DispositionReplaceAtGate || !reflect.DeepEqual(route.Disposition.Gates, []TaskRef{"P3.3"}) {
					t.Errorf("%s disposition = %#v, want replacement at P3.3", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Disposition)
				}
				if route.Exception == nil || route.Exception.Kind != ExceptionLegacyBypass || !reflect.DeepEqual(route.Exception.RemovalTasks, []TaskRef{"P3.3"}) {
					t.Errorf("%s exception = %#v, want bounded P3.3 legacy bypass", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Exception)
				}
			} else {
				if route.AccessPath != AccessProcessBoundary {
					t.Errorf("%s access = %q, want %q", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.AccessPath, AccessProcessBoundary)
				}
				if route.Exception != nil {
					t.Errorf("%s unexpectedly has temporary exception %#v", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Exception)
				}
			}
		}
	}
}

func TestProcessCatalogPinsEveryManagedDoltGuardExecutionOrigin(t *testing.T) {
	rows := processCatalogSiteRows()
	records := make([]string, 0, 18)
	guardRows := 0
	for _, row := range rows {
		if row.BoundaryID != "pidutil.SignalProcess" || row.Matcher.Enclosing.Object.Name != "terminateManagedDoltPIDGuarded" {
			continue
		}
		guardRows++
		if len(row.Classes) != 0 || len(row.ExplicitRoutes) != 9 {
			t.Fatalf("%s leaf/explicit routes = %d/%d, want zero leaf and nine explicit", describePhysicalSite(row.BoundaryID, row.Matcher), len(row.Classes), len(row.ExplicitRoutes))
		}
		processCounts := map[ExecutingProcess]int{}
		classByID := make(map[catalogRouteClassID]ExecutingProcess)
		for _, class := range processCatalogRouteClassSpecs {
			classByID[class.ID] = class.ExecutingProcess
		}
		for _, explicit := range row.ExplicitRoutes {
			processCounts[classByID[explicit.Class]]++
			if len(explicit.Hops) == 0 || !explicit.Hops[0].Site.Enclosing.equal(explicit.LogicalOwner) || !explicit.Hops[len(explicit.Hops)-1].Callee.equal(row.Matcher.Enclosing) {
				t.Errorf("%s explicit class %q does not connect its logical owner to the guarded signal helper", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
			}
			for _, hop := range explicit.Hops {
				if hop.Dispatch != HopDispatchExact {
					t.Errorf("%s explicit class %q dispatch = %q, want exact managed-Dolt route", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class, hop.Dispatch)
				}
			}
			records = append(records, canonicalFields(
				"process-explicit-route-selection-v1",
				registrationPhysicalKey(row.BoundaryID, row.Matcher),
				catalogExplicitRouteKey(explicit),
			))
		}
		if !reflect.DeepEqual(processCounts, map[ExecutingProcess]int{ProcessForegroundCLI: 5, ProcessProviderChild: 4}) {
			t.Errorf("%s process route counts = %#v, want five CLI and four provider-child", describePhysicalSite(row.BoundaryID, row.Matcher), processCounts)
		}
	}
	if guardRows != 2 || len(records) != 18 {
		t.Fatalf("managed-Dolt guarded signal rows/routes = %d/%d, want 2/18", guardRows, len(records))
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != processManagedDoltExplicitRouteFingerprint {
		t.Fatalf("managed-Dolt guarded route fingerprint = %q, want exact 18-origin fingerprint %q", fingerprint, processManagedDoltExplicitRouteFingerprint)
	}
}

func TestEventCatalogClaimsAppendIntentAtRecorderReceiverOnly(t *testing.T) {
	registrations, err := eventInventoryRegistrations()
	if err != nil {
		t.Fatalf("eventInventoryRegistrations() failed: %v", err)
	}
	for _, registration := range registrations {
		route := registration.Cases[0].Routes[0]
		wantTarget := TargetRef{
			Kind:        TargetEventLog,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityAppendRecord,
			Signature:   TargetSignatureEventAppend,
		}
		if route.Target.Kind != wantTarget.Kind || route.Target.Cardinality != wantTarget.Cardinality || route.Target.Identity != wantTarget.Identity || route.Target.Signature != wantTarget.Signature {
			t.Errorf("%s target = %#v, want event-log append identity", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Target)
		}
		if got, want := route.Target.Identities, []TargetIdentityRef{{Role: TargetRolePrimary, BoundarySlot: ValueSlot{Kind: SlotReceiver}, Source: TargetSourceBoundaryValue}}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s target identities = %#v, want recorder receiver %#v", describePhysicalSite(registration.BoundaryID, registration.Matcher), got, want)
		}
		if route.ActionFamily != FamilyObservation || route.AccessPath != AccessDirectEvent {
			t.Errorf("%s family/access = %q/%q, want observation/direct-event", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.ActionFamily, route.AccessPath)
		}
		if !reflect.DeepEqual(route.Fences, []Fence{{Kind: FenceNone}}) || route.CurrentGate.Kind != GateUnconditionalLegacy {
			t.Errorf("%s fence/gate = %#v/%#v, want conservative none/unconditional-legacy", describePhysicalSite(registration.BoundaryID, registration.Matcher), route.Fences, route.CurrentGate)
		}
	}
}

func assertProcessEventCatalogClasses(
	t *testing.T,
	classes []catalogRouteClass,
	rows []catalogSiteRow,
	knownClass func(catalogRouteClassID) bool,
) {
	t.Helper()
	known := make(map[catalogRouteClassID]bool, len(classes))
	for _, class := range classes {
		if !knownClass(class.ID) {
			t.Errorf("route class definition has non-closed ID %q", class.ID)
		}
		if known[class.ID] {
			t.Errorf("route class %q is defined more than once", class.ID)
		}
		known[class.ID] = true
	}
	used := make(map[catalogRouteClassID]bool, len(classes))
	for _, row := range rows {
		if len(row.Classes) != 0 && len(row.ExplicitRoutes) != 0 {
			t.Errorf("%s mixes leaf classes and explicit routes", describePhysicalSite(row.BoundaryID, row.Matcher))
			continue
		}
		selected := append([]catalogRouteClassID(nil), row.Classes...)
		for _, explicit := range row.ExplicitRoutes {
			selected = append(selected, explicit.Class)
		}
		if len(selected) == 0 {
			t.Errorf("%s selects no semantic class", describePhysicalSite(row.BoundaryID, row.Matcher))
			continue
		}
		for _, classID := range selected {
			if !knownClass(classID) || !known[classID] {
				t.Errorf("%s selects unknown class %q", describePhysicalSite(row.BoundaryID, row.Matcher), classID)
			}
			used[classID] = true
		}
	}
	for classID := range known {
		if !used[classID] {
			t.Errorf("route class %q has no physical site", classID)
		}
	}
}

func TestProcessCatalogExpandsDeterministicallyWithoutAliasingAndCompiles(t *testing.T) {
	assertProcessEventCatalogExpansion(
		t,
		processCatalogRouteClasses,
		processCatalogSiteRows,
		processInventoryRegistrations,
	)
}

func TestEventCatalogExpandsDeterministicallyWithoutAliasingAndCompiles(t *testing.T) {
	assertProcessEventCatalogExpansion(
		t,
		eventCatalogRouteClasses,
		eventCatalogSiteRows,
		eventInventoryRegistrations,
	)
}

func assertProcessEventCatalogExpansion(
	t *testing.T,
	classes func() []catalogRouteClass,
	rows func() []catalogSiteRow,
	registrations func() ([]SiteRegistration, error),
) {
	t.Helper()
	forward, err := registrations()
	if err != nil {
		t.Fatalf("inventory registrations failed: %v", err)
	}
	reverseClasses := classes()
	reverseRows := rows()
	reverseCatalogTest(reverseClasses)
	reverseCatalogTest(reverseRows)
	reversed, err := expandCatalogPartition(reverseClasses, reverseRows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(reversed catalog) failed: %v", err)
	}
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatal("catalog expansion depends on authored row or class order")
	}

	registry := Registry{Boundaries: CanonicalBoundaries(), Registrations: forward}
	if _, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate()); err != nil {
		t.Fatalf("catalog failed structural compilation: %v", err)
	}

	baseline := cloneProcessEventRegistrationsForTest(forward)
	forward[0].Matcher.Enclosing.ClosurePath = append(forward[0].Matcher.Enclosing.ClosurePath, 99)
	forward[0].Cases[0].BuildProfiles[0] = BuildWindowsCompile
	forward[0].Cases[0].Routes[0].Target.Identities[0].BoundarySlot.Index = 99
	forward[0].Cases[0].Routes[0].Fences[0].Kind = FenceLeaseEpoch
	forward[0].Cases[0].Routes[0].OwningTests[0].Name = "TestMutated"
	for registrationIndex := range forward {
		exception := forward[registrationIndex].Cases[0].Routes[0].Exception
		if exception != nil {
			exception.RemovalTasks[0] = "P9.9"
			break
		}
	}
	again, err := registrations()
	if err != nil {
		t.Fatalf("inventory registrations after mutation failed: %v", err)
	}
	if !reflect.DeepEqual(again, baseline) {
		t.Fatal("catalog expansion aliases authored classes or physical rows")
	}
}

func cloneProcessEventRegistrationsForTest(registrations []SiteRegistration) []SiteRegistration {
	clone := make([]SiteRegistration, len(registrations))
	for index, registration := range registrations {
		clone[index] = SiteRegistration{
			BoundaryID: registration.BoundaryID,
			Matcher:    cloneOperationSite(registration.Matcher),
			Cases:      make([]ProfileCase, len(registration.Cases)),
		}
		for caseIndex, profileCase := range registration.Cases {
			clone[index].Cases[caseIndex] = ProfileCase{
				BuildProfiles: append([]BuildProfileID(nil), profileCase.BuildProfiles...),
				Routes:        make([]Route, len(profileCase.Routes)),
			}
			for routeIndex, route := range profileCase.Routes {
				clone[index].Cases[caseIndex].Routes[routeIndex] = cloneRoute(route)
			}
		}
	}
	return clone
}
