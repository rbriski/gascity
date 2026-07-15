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
	wakeScaffoldFingerprint      = "a12c326a68cbbd1fd33088b0ccca306ec53042595ad5c9c8d99a4477f7559958"
	wakeSemanticFingerprint      = "83dee76c3904d814c5abc3fee833b7224a0d9ebf53997f89af462f1a0a4a130e"
	wakeExplicitRouteFingerprint = "7601613241a309de90bd47ee4e524d5cf078380e20ec46fa21c7a27364b1b060"
)

func TestWakeCatalogCoversTypedScaffoldExactlyOnce(t *testing.T) {
	assertProcessEventCatalogScaffold(t, KindWakeSource, wakeCatalogSiteRows(), 292, "wake-scaffold-site-v1", wakeScaffoldFingerprint)
}

func TestWakeCatalogPinsEveryPhysicalSiteSemanticClassSelection(t *testing.T) {
	rows := wakeCatalogSiteRows()
	records := make([]string, 0, 327)
	explicitRouteRecords := make([]string, 0, 62)
	for _, row := range rows {
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		classes := append([]catalogRouteClassID(nil), row.Classes...)
		for _, explicit := range row.ExplicitRoutes {
			classes = append(classes, explicit.Class)
			if _, reviewed := wakeCatalogReviewedExplicitRoutes[physicalKey]; reviewed {
				hops := make([]string, len(explicit.Hops))
				for index, hop := range explicit.Hops {
					hops[index] = canonicalRouteHop(hop)
				}
				explicitRouteRecords = append(explicitRouteRecords, canonicalFields(
					"wake-explicit-route-v1",
					physicalKey,
					string(explicit.Class),
					canonicalFunctionRef(explicit.LogicalOwner),
					canonicalStringList("wake-explicit-route-hops-v1", hops),
				))
			}
		}
		for _, classID := range classes {
			records = append(records, canonicalFields(
				"wake-semantic-route-v1",
				physicalKey,
				string(classID),
			))
		}
	}
	if got := len(records); got != 327 {
		t.Fatalf("wake semantic selections = %d, want 327 physical-route pairs", got)
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != wakeSemanticFingerprint {
		t.Fatalf("wake semantic fingerprint = %q, want exact class-selection fingerprint %q", fingerprint, wakeSemanticFingerprint)
	}

	if got := len(explicitRouteRecords); got != 62 {
		t.Fatalf("wake reviewed explicit-route evidence records = %d, want 62", got)
	}
	sort.Strings(explicitRouteRecords)
	explicitRouteFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(explicitRouteRecords, "\n"))))
	if explicitRouteFingerprint != wakeExplicitRouteFingerprint {
		t.Fatalf("wake explicit-route evidence fingerprint = %q, want exact owner/hop fingerprint %q", explicitRouteFingerprint, wakeExplicitRouteFingerprint)
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

func TestWakeCatalogTargetShapesDistinguishOwnedChannelsFromInvocationSources(t *testing.T) {
	tests := []struct {
		name  string
		shape wakeCatalogTargetShape
		want  TargetRef
	}{
		{
			name:  "controller-owned channel",
			shape: wakeTargetControllerChannel,
			want: TargetRef{
				Kind:        TargetControllerChannel,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentitySingleton,
				Signature:   TargetSignatureChannel,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotBoundaryObject},
					Source:       TargetSourceBoundaryValue,
				}},
				Detail: "one controller-owned channel identified by its registered field",
			},
		},
		{
			name:  "invocation-local channel source",
			shape: wakeTargetBoundarySource,
			want: TargetRef{
				Kind:        TargetWakeSource,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityExisting,
				Signature:   TargetSignatureWakeSource,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotBoundaryObject},
					Source:       TargetSourceBoundaryValue,
				}},
				Detail: "one invocation-local timer, cancellation, or signal channel",
			},
		},
		{
			name:  "invocation-local result source",
			shape: wakeTargetResultSource,
			want: TargetRef{
				Kind:        TargetWakeSource,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityExisting,
				Signature:   TargetSignatureWakeSource,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
					Source:       TargetSourceBoundaryValue,
				}},
				Detail: "one invocation-local wake source returned by the registered boundary",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wakeCatalogTarget(tt.shape); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("wakeCatalogTarget(%q) = %#v, want %#v", tt.shape, got, tt.want)
			}
		})
	}
}

func TestWakeCatalogUsesOnlyClosedTruthfulTargetShapes(t *testing.T) {
	classes := wakeCatalogRouteClasses()
	if len(classes) != len(wakeCatalogRouteClassSpecs) {
		t.Fatalf("wake route classes = %d, want %d specs", len(classes), len(wakeCatalogRouteClassSpecs))
	}
	for index, spec := range wakeCatalogRouteClassSpecs {
		if !knownWakeCatalogTargetShape(spec.TargetShape) {
			t.Errorf("wake route class %q has unknown target shape %q", spec.ID, spec.TargetShape)
			continue
		}
		if got, want := classes[index].Definition.Target, wakeCatalogTarget(spec.TargetShape); !reflect.DeepEqual(got, want) {
			t.Errorf("wake route class %q target = %#v, want %#v", spec.ID, got, want)
		}
	}

	registrations, err := wakeInventoryRegistrations()
	if err != nil {
		t.Fatalf("wakeInventoryRegistrations() failed: %v", err)
	}
	for _, registration := range registrations {
		for _, route := range registration.Cases[0].Routes {
			physical := describePhysicalSite(registration.BoundaryID, registration.Matcher)
			if route.ActionFamily != FamilyControllerWake && route.ActionFamily != FamilyTimersWake {
				t.Errorf("%s family = %q, want controller-wake or timers-wake", physical, route.ActionFamily)
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

func TestWakeCatalogAuthorsEveryReviewedProductionOwnershipRouteExplicitly(t *testing.T) {
	if got := len(wakeCatalogReviewedExplicitRoutes); got != 30 {
		t.Fatalf("reviewed production explicit-route keys = %d, want 30", got)
	}
	for _, spec := range wakeCatalogSiteSpecs {
		physicalKey := wakeCatalogReviewedSiteKey(spec.BoundaryID, spec.Operation, FunctionRef{
			Object: ObjectRef{Package: spec.Package, Receiver: spec.Receiver, Name: spec.Function},
			File:   spec.File, ClosurePath: spec.ClosurePath,
		}, spec.Ordinal)
		if _, reviewed := wakeCatalogReviewedExplicitRoutes[physicalKey]; reviewed && len(spec.Classes) != 0 {
			t.Errorf("reviewed production spec %q retains %d fallback leaf classes", physicalKey, len(spec.Classes))
		}
	}
	matchedKeys := make(map[string]bool, len(wakeCatalogReviewedExplicitRoutes))
	var explicitSites, explicitRoutes, singletonSites, sharedSites int
	for _, row := range wakeCatalogSiteRows() {
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		reviewedRoutes, reviewed := wakeCatalogReviewedExplicitRoutes[physicalKey]
		if !reviewed {
			continue
		}
		matchedKeys[physicalKey] = true
		if len(row.ExplicitRoutes) != len(reviewedRoutes) {
			t.Errorf("%s expanded explicit routes = %d, want %d reviewed routes", describePhysicalSite(row.BoundaryID, row.Matcher), len(row.ExplicitRoutes), len(reviewedRoutes))
		}
		explicitSites++
		explicitRoutes += len(row.ExplicitRoutes)
		switch len(row.ExplicitRoutes) {
		case 1:
			singletonSites++
		default:
			sharedSites++
		}
		if len(row.Classes) != 0 {
			t.Errorf("%s retains %d leaf classes alongside explicit provenance", describePhysicalSite(row.BoundaryID, row.Matcher), len(row.Classes))
		}

		seen := make(map[string]bool, len(row.ExplicitRoutes))
		for _, explicit := range row.ExplicitRoutes {
			if len(explicit.Hops) == 0 {
				t.Errorf("%s explicit class %q has no authored route hops", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
				continue
			}
			if !explicit.Hops[0].Site.Enclosing.equal(explicit.LogicalOwner) {
				t.Errorf("%s explicit class %q route does not start at its logical owner", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
			}
			if !explicit.Hops[len(explicit.Hops)-1].Callee.equal(row.Matcher.Enclosing) {
				t.Errorf("%s explicit class %q route does not terminate at its physical owner", describePhysicalSite(row.BoundaryID, row.Matcher), explicit.Class)
			}
			parts := []string{string(explicit.Class), canonicalFunctionRef(explicit.LogicalOwner)}
			for _, hop := range explicit.Hops {
				parts = append(parts, canonicalRouteHop(hop))
			}
			key := strings.Join(parts, "|")
			if seen[key] {
				t.Errorf("%s duplicates explicit class/origin/hop route %q", describePhysicalSite(row.BoundaryID, row.Matcher), key)
			}
			seen[key] = true
		}
	}

	if explicitSites != 30 || explicitRoutes != 62 {
		t.Fatalf("reviewed production explicit sites/routes = %d/%d, want 30/62", explicitSites, explicitRoutes)
	}
	if singletonSites != 5 || sharedSites != 25 {
		t.Fatalf("reviewed production singleton/shared explicit sites = %d/%d, want 5/25", singletonSites, sharedSites)
	}
	for physicalKey := range wakeCatalogReviewedExplicitRoutes {
		if !matchedKeys[physicalKey] {
			t.Errorf("reviewed production explicit-route key %q has no physical catalog site", physicalKey)
		}
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
