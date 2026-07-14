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
	processScaffoldFingerprint = "cd18b9dc760e2084bfe21bd19a7e6efc50d589df5aeff0e53b59681173228a2b"
	eventScaffoldFingerprint   = "86d98ee3d27144db965f87dd15bdbb80614937d10b56ea7437a50f57fd9b3869"
)

func TestProcessCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindProcessMutation, processCatalogSiteRows(), 52, "process-scaffold-site-v1", processScaffoldFingerprint)
}

func TestEventCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindEventEmission, eventCatalogSiteRows(), 93, "event-scaffold-site-v1", eventScaffoldFingerprint)
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
		route := registration.Cases[0].Routes[0]
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
		if got := len(row.Classes); got != 1 {
			t.Errorf("%s selects %d classes, want exactly one explicit semantic class", describePhysicalSite(row.BoundaryID, row.Matcher), got)
			continue
		}
		classID := row.Classes[0]
		if !knownClass(classID) || !known[classID] {
			t.Errorf("%s selects unknown class %q", describePhysicalSite(row.BoundaryID, row.Matcher), classID)
		}
		used[classID] = true
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
