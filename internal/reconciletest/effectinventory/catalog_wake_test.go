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
	wakeScaffoldFingerprint = "f97eb191572ade2adec0ad6194a9c6fd448d898f3fe399ba1cb06713487e43b9"
	wakeSemanticFingerprint = "c728032b6acc487bee4dba77b462cd39adecee3d7e9662fb69c919da6550de02"
)

func TestWakeCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindWakeSource, wakeCatalogSiteRows(), 11, "wake-scaffold-site-v1", wakeScaffoldFingerprint)
}

func TestWakeCatalogPinsEveryPhysicalSiteSemanticClassSelection(t *testing.T) {
	rows := wakeCatalogSiteRows()
	records := make([]string, 0, 14)
	for _, row := range rows {
		classes := append([]catalogRouteClassID(nil), row.Classes...)
		for _, explicit := range row.ExplicitRoutes {
			classes = append(classes, explicit.Class)
		}
		for _, classID := range classes {
			records = append(records, canonicalFields(
				"wake-semantic-route-v1",
				registrationPhysicalKey(row.BoundaryID, row.Matcher),
				string(classID),
			))
		}
	}
	if got := len(records); got != 14 {
		t.Fatalf("wake semantic selections = %d, want 14 physical-route pairs", got)
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != wakeSemanticFingerprint {
		t.Fatalf("wake semantic fingerprint = %q, want exact class-selection fingerprint %q", fingerprint, wakeSemanticFingerprint)
	}
}

func TestWakeCatalogUsesOnlyClosedKnownSemanticClasses(t *testing.T) {
	classes := wakeCatalogRouteClasses()
	known := make(map[catalogRouteClassID]bool, len(classes))
	for _, class := range classes {
		if !knownWakeCatalogClassID(class.ID) {
			t.Errorf("wake route class definition has non-closed ID %q", class.ID)
		}
		if known[class.ID] {
			t.Errorf("wake route class %q is defined more than once", class.ID)
		}
		known[class.ID] = true
	}
	used := make(map[catalogRouteClassID]bool, len(classes))
	for _, row := range wakeCatalogSiteRows() {
		for _, classID := range row.Classes {
			if !knownWakeCatalogClassID(classID) || !known[classID] {
				t.Errorf("%s selects unknown leaf class %q", describePhysicalSite(row.BoundaryID, row.Matcher), classID)
			}
			used[classID] = true
		}
		for _, explicit := range row.ExplicitRoutes {
			if !knownWakeCatalogClassID(explicit.Class) || !known[explicit.Class] {
				t.Errorf("%s selects unknown explicit class %q", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
			}
			used[explicit.Class] = true
		}
	}
	for classID := range known {
		if !used[classID] {
			t.Errorf("wake route class %q has no physical site", classID)
		}
	}
}

func TestWakeCatalogUsesConservativeDirectWakeSafetyShape(t *testing.T) {
	registrations, err := wakeInventoryRegistrations()
	if err != nil {
		t.Fatalf("wakeInventoryRegistrations() failed: %v", err)
	}
	wantTarget := TargetRef{
		Kind:        TargetControllerChannel,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentitySingleton,
		Signature:   TargetSignatureChannel,
		Identities: []TargetIdentityRef{{
			Role:         TargetRolePrimary,
			BoundarySlot: ValueSlot{Kind: SlotBoundaryObject},
			Source:       TargetSourceBoundaryValue,
		}},
		Detail: "one registered wake source identified by its canonical boundary object",
	}
	for _, registration := range registrations {
		for _, route := range registration.Cases[0].Routes {
			physical := describePhysicalSite(registration.BoundaryID, registration.Matcher)
			if !reflect.DeepEqual(route.Target, wantTarget) {
				t.Errorf("%s target = %#v, want conservative wake target %#v", physical, route.Target, wantTarget)
			}
			if route.ActionFamily != FamilyControllerWake && route.ActionFamily != FamilyTimersWake {
				t.Errorf("%s family = %q, want controller-wake or timers-wake", physical, route.ActionFamily)
			}
			if route.ExecutingProcess != ProcessForegroundCLI && route.ExecutingProcess != ProcessProviderChild && route.ExecutingProcess != ProcessSidecarPoller {
				t.Errorf("%s executing process = %q, want an exact current process class", physical, route.ExecutingProcess)
			}
			if route.AccessPath != AccessDirectWake || !reflect.DeepEqual(route.Fences, []Fence{{Kind: FenceNone}}) || route.CurrentGate.Kind != GateUnconditionalLegacy {
				t.Errorf("%s access/fence/gate = %q/%#v/%#v, want direct-wake/none/unconditional-legacy", physical, route.AccessPath, route.Fences, route.CurrentGate)
			}
			if route.Disposition.Kind != DispositionRetainBoundary || route.Exception != nil {
				t.Errorf("%s disposition/exception = %#v/%#v, want retained canonical boundary without exception", physical, route.Disposition, route.Exception)
			}
			if route.Continuation != (Continuation{Locus: ContinuationInline, Completion: CompletionSynchronous}) {
				t.Errorf("%s continuation = %#v, want inline synchronous", physical, route.Continuation)
			}
		}
	}
}

func TestWakeCatalogAuthorsDistinctCLIAndSidecarOriginsForSharedProductMetricsWaits(t *testing.T) {
	rows := wakeCatalogSiteRows()
	sharedRows := 0
	for _, row := range rows {
		if row.Matcher.Enclosing.Object.Package != "github.com/gastownhall/gascity/internal/productmetrics" ||
			row.Matcher.Enclosing.Object.Receiver != "unixStorageDirectory" ||
			row.Matcher.Enclosing.Object.Name != "acquireLock" {
			continue
		}
		sharedRows++
		if len(row.Classes) != 0 || len(row.ExplicitRoutes) != 2 {
			t.Fatalf("%s leaf/explicit routes = %d/%d, want zero leaf and two explicit", describePhysicalSite(row.BoundaryID, row.Matcher), len(row.Classes), len(row.ExplicitRoutes))
		}
		owners := make(map[string]bool, 2)
		for _, explicit := range row.ExplicitRoutes {
			owners[canonicalFunctionRef(explicit.LogicalOwner)] = true
			if got := len(explicit.Hops); got != 2 {
				t.Fatalf("%s explicit class %q hops = %d, want exact/VTA two-hop route", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class, got)
			}
			if explicit.Hops[0].Dispatch != HopDispatchExact || explicit.Hops[1].Dispatch != HopDispatchVTA {
				t.Errorf("%s explicit class %q dispatches = %q/%q, want exact/VTA", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class, explicit.Hops[0].Dispatch, explicit.Hops[1].Dispatch)
			}
			if !explicit.Hops[0].Site.Enclosing.equal(explicit.LogicalOwner) || !explicit.Hops[1].Callee.equal(row.Matcher.Enclosing) {
				t.Errorf("%s explicit class %q does not connect logical owner to physical owner", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
			}
		}
		wantOwners := []FunctionRef{
			{
				Object: ObjectRef{Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "Service", Name: "activateNotice"},
				File:   "internal/productmetrics/notice.go",
			},
			{
				Object: ObjectRef{Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "Service", Name: "lockUploader"},
				File:   "internal/productmetrics/uploader.go",
			},
		}
		for _, owner := range wantOwners {
			if !owners[canonicalFunctionRef(owner)] {
				t.Errorf("%s missing explicit logical owner %s", describePhysicalSite(row.BoundaryID, row.Matcher), canonicalFunctionRef(owner))
			}
		}
	}
	if sharedRows != 3 {
		t.Fatalf("shared product-metrics wake rows = %d, want context plus two timer receives", sharedRows)
	}
}

func TestWakeCatalogExpandsDeterministicallyWithoutAliasingAndCompiles(t *testing.T) {
	assertProcessEventCatalogExpansion(
		t,
		wakeCatalogRouteClasses,
		wakeCatalogSiteRows,
		wakeInventoryRegistrations,
	)
}
