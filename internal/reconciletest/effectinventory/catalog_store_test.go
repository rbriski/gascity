package effectinventory

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const storeScaffoldFingerprint = "79b0c4c97f8e8f68897aaa50aac6f56874ece2d8a4f35b1987e61873261e3a55"

func TestStoreCatalogCoversScaffoldExactlyOnce(t *testing.T) {
	rows := storeCatalogSiteRows()
	if got, want := len(rows), 414; got != want {
		t.Fatalf("store catalog rows = %d, want scaffold count %d", got, want)
	}

	storeBoundaries := make(map[string]bool)
	for _, boundary := range CanonicalBoundaries() {
		if boundary.Kind == KindStoreMutation {
			storeBoundaries[boundary.ID] = true
		}
	}
	seen := make(map[string]bool, len(rows))
	records := make([]string, 0, len(rows))
	for _, row := range rows {
		if !storeBoundaries[row.BoundaryID] {
			t.Errorf("non-store boundary leaked into store catalog: %q", row.BoundaryID)
		}
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		if seen[physicalKey] {
			t.Errorf("store physical site appears more than once: %s", describePhysicalSite(row.BoundaryID, row.Matcher))
		}
		seen[physicalKey] = true
		profiles := append([]BuildProfileID(nil), row.Profiles...)
		sort.Slice(profiles, func(i, j int) bool { return profiles[i] < profiles[j] })
		records = append(records, canonicalFields(
			"store-scaffold-site-v1",
			row.BoundaryID,
			canonicalOperationSite(row.Matcher),
			canonicalBuildProfiles(profiles),
		))
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != storeScaffoldFingerprint {
		t.Fatalf("store scaffold fingerprint = %q, want %q", fingerprint, storeScaffoldFingerprint)
	}
}

func TestStoreCatalogUsesOnlyClosedKnownSemanticClasses(t *testing.T) {
	classes := storeCatalogRouteClasses()
	known := make(map[catalogRouteClassID]bool, len(classes))
	for _, class := range classes {
		if !knownStoreCatalogClassID(class.ID) {
			t.Errorf("route class definition has non-closed ID %q", class.ID)
		}
		if known[class.ID] {
			t.Errorf("route class %q is defined more than once", class.ID)
		}
		known[class.ID] = true
	}
	used := make(map[catalogRouteClassID]bool, len(classes))
	for _, row := range storeCatalogSiteRows() {
		if got := len(row.Classes); got != 1 {
			t.Errorf("%s selects %d classes, want exactly one explicit semantic class", describePhysicalSite(row.BoundaryID, row.Matcher), got)
			continue
		}
		classID := row.Classes[0]
		if !knownStoreCatalogClassID(classID) || !known[classID] {
			t.Errorf("%s selects unknown class %q", describePhysicalSite(row.BoundaryID, row.Matcher), classID)
		}
		used[classID] = true
	}
	for classID := range known {
		if !used[classID] {
			t.Errorf("route class %q has no physical store site", classID)
		}
	}
}

func TestStoreCatalogExpandsDeterministicallyAndCompilesStructurally(t *testing.T) {
	forward, err := storeInventoryRegistrations()
	if err != nil {
		t.Fatalf("storeInventoryRegistrations() failed: %v", err)
	}
	reverseClasses := storeCatalogRouteClasses()
	reverseRows := storeCatalogSiteRows()
	reverseCatalogTest(reverseClasses)
	reverseCatalogTest(reverseRows)
	reversed, err := expandCatalogPartition(reverseClasses, reverseRows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(reversed store catalog) failed: %v", err)
	}
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatal("store catalog expansion depends on authored row or class order")
	}

	registry := Registry{
		Boundaries:    CanonicalBoundaries(),
		Registrations: forward,
	}
	if _, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate()); err != nil {
		t.Fatalf("store catalog failed structural compilation: %v", err)
	}

	baseline := cloneSiteRegistrationsForTest(forward)
	forward[0].Matcher.Enclosing.ClosurePath = append(forward[0].Matcher.Enclosing.ClosurePath, 99)
	forward[0].Cases[0].BuildProfiles[0] = BuildWindowsCompile
	forward[0].Cases[0].Routes[0].Target.Identities[0].BoundarySlot.Index = 99
	forward[0].Cases[0].Routes[0].Fences[0].Kind = FenceLeaseEpoch
	forward[0].Cases[0].Routes[0].OwningTests[0].Name = "TestMutated"
	if forward[0].Cases[0].Routes[0].Exception != nil {
		forward[0].Cases[0].Routes[0].Exception.RemovalTasks[0] = "P9.9"
	}
	again, err := storeInventoryRegistrations()
	if err != nil {
		t.Fatalf("storeInventoryRegistrations(after mutation) failed: %v", err)
	}
	if !reflect.DeepEqual(again, baseline) {
		t.Fatal("store catalog expansion aliases authored classes or site rows")
	}
}

func cloneSiteRegistrationsForTest(registrations []SiteRegistration) []SiteRegistration {
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
