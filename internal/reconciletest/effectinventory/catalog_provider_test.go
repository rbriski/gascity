package effectinventory

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	providerScaffoldFingerprint = "9732281d3f084ea35253ffc1557dc6e72c3aaf3edaa82c4c06bbcd2b1bc3b3d0"
	providerSemanticFingerprint = "3ae5fee72fedf16476fe9a1a834a5633848392dd8d0bc4a2f416327eb23887a5"
	providerBoundaryDigest      = "boundaries-v1-854917b2f31cc2120382c76b579df98f1fcd38e940d55503ca7c20da4847e7d4"
)

var providerBoundaryCounts = map[string]int{
	"runtime.attachment.ClearScrollback":                  1,
	"runtime.attachment.Close":                            1,
	"runtime.attachment.Interrupt":                        1,
	"runtime.attachment.Nudge":                            1,
	"runtime.attachment.SendKeys":                         1,
	"runtime.carrier.ClearScrollback":                     3,
	"runtime.carrier.Interrupt":                           3,
	"runtime.carrier.Nudge":                               3,
	"runtime.carrier.SendKeys":                            3,
	"runtime.dialog.DismissKnownDialogs":                  4,
	"runtime.immediate-nudge.NudgeNow":                    6,
	"runtime.interaction.Respond":                         8,
	"runtime.interrupted-turn-reset.ResetInterruptedTurn": 3,
	"runtime.meta-store.RemoveMeta":                       24,
	"runtime.meta-store.SetMeta":                          23,
	"runtime.place.Stage":                                 1,
	"runtime.process-table.TerminateRuntime":              3,
	"runtime.provider.Attach":                             12,
	"runtime.provider.ClearScrollback":                    12,
	"runtime.provider.CopyTo":                             12,
	"runtime.provider.Interrupt":                          16,
	"runtime.provider.Nudge":                              24,
	"runtime.provider.RunLive":                            4,
	"runtime.provider.SendKeys":                           21,
	"runtime.provider.Start":                              17,
	"runtime.provider.Stop":                               43,
	"runtime.relaunch.Relaunch":                           11,
	"runtime.runtime.Provision":                           1,
	"runtime.runtime.Teardown":                            2,
	"runtime.server-lifecycle.ConfigureServer":            5,
	"runtime.server-lifecycle.TeardownServer":             2,
	"runtime.transport.Attach":                            1,
	"runtime.transport.Launch":                            1,
}

func TestProviderCatalogCoversScaffoldExactlyOnce(t *testing.T) {
	rows := providerCatalogSiteRows()
	if got, want := len(rows), 273; got != want {
		t.Fatalf("provider catalog rows = %d, want scaffold count %d", got, want)
	}

	providerBoundaries := make(map[string]bool)
	var providerBoundaryDefinitions []BoundaryDefinition
	for _, boundary := range CanonicalBoundaries() {
		if boundary.Kind == KindProviderMutation {
			providerBoundaries[boundary.ID] = true
			providerBoundaryDefinitions = append(providerBoundaryDefinitions, boundary)
		}
	}
	if got := deriveBoundaryDigest(providerBoundaryDefinitions); got != providerBoundaryDigest {
		t.Fatalf("provider boundary digest = %q, want %q", got, providerBoundaryDigest)
	}
	gotBoundaryCounts := make(map[string]int)
	seen := make(map[string]bool, len(rows))
	records := make([]string, 0, len(rows))
	wantProfiles := allBuildProfiles()
	for _, row := range rows {
		if !providerBoundaries[row.BoundaryID] {
			t.Errorf("non-provider boundary leaked into provider catalog: %q", row.BoundaryID)
		}
		gotBoundaryCounts[row.BoundaryID]++
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		if seen[physicalKey] {
			t.Errorf("provider physical site appears more than once: %s", describePhysicalSite(row.BoundaryID, row.Matcher))
		}
		seen[physicalKey] = true
		profiles := append([]BuildProfileID(nil), row.Profiles...)
		sort.Slice(profiles, func(i, j int) bool { return profiles[i] < profiles[j] })
		if !reflect.DeepEqual(profiles, wantProfiles) {
			t.Errorf("%s profiles = %v, want all five profiles %v", describePhysicalSite(row.BoundaryID, row.Matcher), profiles, wantProfiles)
		}
		records = append(records, providerScaffoldRecord(row, profiles))
	}
	if !reflect.DeepEqual(gotBoundaryCounts, providerBoundaryCounts) {
		t.Fatalf("provider boundary counts = %#v, want %#v", gotBoundaryCounts, providerBoundaryCounts)
	}
	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != providerScaffoldFingerprint {
		t.Fatalf("provider scaffold fingerprint = %q, want %q", fingerprint, providerScaffoldFingerprint)
	}
}

func TestProviderCatalogPinsSemanticAssignmentsAndAuditedCensus(t *testing.T) {
	classes := make(map[catalogRouteClassID]providerCatalogRouteClassSpec, len(providerCatalogRouteClassSpecs))
	for _, class := range providerCatalogRouteClassSpecs {
		classes[class.ID] = class
	}

	accessCounts := make(map[AccessPath]int)
	actionCounts := make(map[ActionFamily]int)
	processCounts := make(map[ExecutingProcess]int)
	targetCounts := make(map[providerCatalogTargetShape]int)
	fenceCounts := make(map[providerCatalogFenceShape]int)
	continuationCounts := make(map[providerCatalogContinuationShape]int)
	records := make([]string, 0, len(providerCatalogSiteSpecs))
	for _, row := range providerCatalogSiteRows() {
		if got := len(row.Classes); got != 1 {
			t.Fatalf("%s selects %d semantic classes, want exactly one", describePhysicalSite(row.BoundaryID, row.Matcher), got)
		}
		class, ok := classes[row.Classes[0]]
		if !ok {
			t.Fatalf("%s selects missing semantic class %q", describePhysicalSite(row.BoundaryID, row.Matcher), row.Classes[0])
		}
		accessCounts[class.AccessPath]++
		actionCounts[class.ActionFamily]++
		processCounts[class.ExecutingProcess]++
		targetCounts[class.TargetShape]++
		fenceCounts[class.FenceShape]++
		continuationCounts[class.Continuation]++
		records = append(records, canonicalFields(
			"provider-semantic-site-v1",
			registrationPhysicalKey(row.BoundaryID, row.Matcher),
			string(row.Classes[0]),
		))
	}

	providerAssertCountMap(t, "access", accessCounts, map[AccessPath]int{
		AccessProviderNative: 182,
		AccessProviderBypass: 52,
		AccessManagerBypass:  27,
		AccessWorkerBoundary: 12,
	})
	providerAssertCountMap(t, "action family", actionCounts, map[ActionFamily]int{
		FamilyDrainAckCompletion:        9,
		FamilyDrainBeginCancel:          6,
		FamilyIdentityHealRetirement:    3,
		FamilyInterruptStopTurn:         20,
		FamilyLiveConfig:                4,
		FamilyNudge:                     86,
		FamilyOperatorAttach:            13,
		FamilyProcessSignal:             3,
		FamilyRestartGeneration:         19,
		FamilyRuntimeLaunch:             1,
		FamilyRuntimeProvision:          17,
		FamilyServerTeardown:            2,
		FamilyStartConfirmationAdoption: 1,
		FamilyStartInitiation:           23,
		FamilyStatusHeal:                24,
		FamilyStop:                      42,
	})
	providerAssertCountMap(t, "executing process", processCounts, map[ExecutingProcess]int{
		ProcessController:      240,
		ProcessAPIInController: 1,
		ProcessForegroundCLI:   32,
	})
	providerAssertCountMap(t, "target shape", targetCounts, map[providerCatalogTargetShape]int{
		providerTargetSessionP1:       193,
		providerTargetSessionP2:       47,
		providerTargetSessionReceiver: 5,
		providerTargetRuntimeP2:       4,
		providerTargetRuntimeReceiver: 1,
		providerTargetServerReceiver:  7,
		providerTargetProcessRuntime:  3,
		providerTargetAttachP1:        12,
		providerTargetAttachP3:        1,
	})
	providerAssertCountMap(t, "fence shape", fenceCounts, map[providerCatalogFenceShape]int{
		providerFenceNone:                   235,
		providerFenceSessionMutationLock:    27,
		providerFenceControllerSingleWriter: 9,
		providerFenceProcessScanReread:      2,
	})
	providerAssertCountMap(t, "continuation", continuationCounts, map[providerCatalogContinuationShape]int{
		providerContinuationInline: 238,
		providerContinuationChild:  35,
	})

	sort.Strings(records)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if fingerprint != providerSemanticFingerprint {
		t.Fatalf("provider semantic fingerprint = %q, want %q", fingerprint, providerSemanticFingerprint)
	}
}

func providerAssertCountMap[K comparable](t *testing.T, name string, got, want map[K]int) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider %s census = %#v, want %#v", name, got, want)
	}
}

func providerScaffoldRecord(row catalogSiteRow, profiles []BuildProfileID) string {
	profileNames := make([]string, len(profiles))
	for index, profile := range profiles {
		profileNames[index] = string(profile)
	}
	closure := make([]string, len(row.Matcher.Enclosing.ClosurePath))
	for index, item := range row.Matcher.Enclosing.ClosurePath {
		closure[index] = strconv.Itoa(item)
	}
	return strings.Join([]string{
		row.BoundaryID,
		string(row.Matcher.Operation),
		row.Matcher.Enclosing.Object.Package,
		row.Matcher.Enclosing.Object.Receiver,
		row.Matcher.Enclosing.Object.Name,
		row.Matcher.Enclosing.File,
		strings.Join(closure, ","),
		strconv.Itoa(row.Matcher.Ordinal),
		strings.Join(profileNames, ","),
	}, "\t")
}

func TestProviderCatalogUsesOnlyClosedKnownSemanticClasses(t *testing.T) {
	classes := providerCatalogRouteClasses()
	if got, want := len(classes), 63; got != want {
		t.Fatalf("provider semantic classes = %d, want %d", got, want)
	}
	known := make(map[catalogRouteClassID]bool, len(classes))
	for _, class := range classes {
		if !knownProviderCatalogClassID(class.ID) {
			t.Errorf("route class definition has non-closed ID %q", class.ID)
		}
		if known[class.ID] {
			t.Errorf("route class %q is defined more than once", class.ID)
		}
		known[class.ID] = true
	}
	used := make(map[catalogRouteClassID]bool, len(classes))
	for _, row := range providerCatalogSiteRows() {
		if got := len(row.Classes); got != 1 {
			t.Errorf("%s selects %d classes, want exactly one explicit semantic class", describePhysicalSite(row.BoundaryID, row.Matcher), got)
		}
		for _, classID := range row.Classes {
			if !knownProviderCatalogClassID(classID) || !known[classID] {
				t.Errorf("%s selects unknown class %q", describePhysicalSite(row.BoundaryID, row.Matcher), classID)
			}
			used[classID] = true
		}
	}
	for classID := range known {
		if !used[classID] {
			t.Errorf("route class %q has no physical provider site", classID)
		}
	}
}

func TestProviderCatalogExpandsDeterministicallyAndCompilesStructurally(t *testing.T) {
	forward, err := providerInventoryRegistrations()
	if err != nil {
		t.Fatalf("providerInventoryRegistrations() failed: %v", err)
	}
	reverseClasses := providerCatalogRouteClasses()
	reverseRows := providerCatalogSiteRows()
	reverseCatalogTest(reverseClasses)
	reverseCatalogTest(reverseRows)
	reversed, err := expandCatalogPartition(reverseClasses, reverseRows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(reversed provider catalog) failed: %v", err)
	}
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatal("provider catalog expansion depends on authored row or class order")
	}

	registry := Registry{Boundaries: CanonicalBoundaries(), Registrations: forward}
	if _, err := CompileRegistry(registry, discoveryForRegistry(registry), validationDate()); err != nil {
		t.Fatalf("provider catalog failed structural compilation: %v", err)
	}

	baseline := cloneProviderRegistrations(forward)
	forward[0].Matcher.Enclosing.ClosurePath = append(forward[0].Matcher.Enclosing.ClosurePath, 99)
	forward[0].Cases[0].BuildProfiles[0] = BuildWindowsCompile
	forward[0].Cases[0].Routes[0].Target.Identities[0].BoundarySlot.Index = 99
	forward[0].Cases[0].Routes[0].Fences[0].Kind = FenceLeaseEpoch
	forward[0].Cases[0].Routes[0].OwningTests[0].Name = "TestMutated"
	if forward[0].Cases[0].Routes[0].Exception != nil {
		forward[0].Cases[0].Routes[0].Exception.RemovalTasks[0] = "P9.9"
	}
	again, err := providerInventoryRegistrations()
	if err != nil {
		t.Fatalf("providerInventoryRegistrations(after mutation) failed: %v", err)
	}
	if !reflect.DeepEqual(again, baseline) {
		t.Fatal("provider catalog expansion aliases authored classes or site rows")
	}
}

func cloneProviderRegistrations(registrations []SiteRegistration) []SiteRegistration {
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
