package effectinventory

// Inventory is the canonical P0.1 effect registry for the bound execution head.
//
// It pairs the canonical discovery boundaries (canonicalBoundaries) with the
// fully classified ownership routes that P0.1 commits to on the execution head.
// The mechanical, type-aware site census lives in the analyzer (Discover) and
// its pinned golden (testdata/inventory_golden.json); that census is what
// guarantees "every current production effect site is classified, with no
// unknown sites." This registry is the curated, human-owned classification of
// the load-bearing effect routes — starting with the one route whose weak
// exclusion already needs a tracked temporary exception. Later plan slices
// (P2.10, P3.3-P3.4, P7.14, P12.1) extend this same registry per action
// family; they do not create competing scanners or registries.
//
// The route recorded here is route recovery's live-reread/non-CAS residual
// store write: restoreCarriedWorkRoutes revalidates a snapshot bead id with a
// cache-bypassing live read and then writes metadata, without an atomic
// compare-and-swap between the read and the write. That weak fence is the
// canonical example the redesign must replace at its migration gate, so it
// carries an explicit, expiring exception anchored to the bound head.
func Inventory() Registry {
	owner := FunctionRef{
		Object: ObjectRef{
			Package: "github.com/gastownhall/gascity/cmd/gc",
			Name:    "restoreCarriedWorkRoutes",
		},
		File: "route_recovery.go",
	}
	storeGet := ObjectRef{
		Package:  "github.com/gastownhall/gascity/internal/beads",
		Receiver: "Store",
		Name:     "Get",
	}
	routeRecoveryTest := TestRef{
		Package: "github.com/gastownhall/gascity/cmd/gc",
		Name:    "TestRestoreCarriedWorkRoutesSkipsCacheStaleClaimedBead",
	}

	return Registry{
		Boundaries: canonicalBoundaries(),
		Sites: []Site{{
			ID:          "route-recovery.store-write",
			BoundaryID:  "store.SetMetadata",
			StoreDomain: StoreDomainRouteRecovery,
			Matcher: OperationSite{
				Operation: OperationCall,
				Enclosing: owner,
				Ordinal:   1,
			},
			BuildProfiles: allInventoryProfiles(),
		}},
		Routes: []Route{{
			ID:               "controller.route-recovery.store-write",
			SiteID:           "route-recovery.store-write",
			BuildProfiles:    allInventoryProfiles(),
			ActionFamily:     FamilyRouteRecovery,
			ExecutingProcess: ProcessController,
			LogicalOwner:     owner,
			Target: TargetRef{
				Kind:         TargetDurableRecord,
				Sink:         ValueSlot{Kind: SlotParameter, Index: 1},
				Source:       TargetSourceStoreLiveReread,
				SourceObject: storeGet,
				SourceSlot:   ValueSlot{Kind: SlotResult, Index: 1},
				Detail:       "snapshot bead ID revalidated by cache-bypassing live read",
			},
			Fences: []Fence{{
				Kind:   FenceLiveRereadNonCAS,
				Source: storeGet,
			}},
			CurrentGate: GateRef{Kind: GateUnconditionalLegacy},
			Disposition: Disposition{
				Kind:   DispositionReplaceAtGate,
				Gates:  []TaskRef{"P2.0", "P2.10A"},
				Reason: "move route recovery to the conditional shared writer",
			},
			AccessPath: AccessRawStoreBypass,
			Continuation: Continuation{
				Locus:      ContinuationInline,
				Completion: CompletionSynchronous,
			},
			OwningTests: []TestRef{routeRecoveryTest},
			Exception: &TemporaryException{
				Kind:         ExceptionWeakFence,
				Reason:       "live reread is not atomic with the following metadata write",
				OwnerTask:    "P0.1",
				RemovalTasks: []TaskRef{"P2.0", "P2.10A"},
				Anchor: VersionAnchor{
					Kind:  AnchorGitCommit,
					Value: "7378aa936f449566657d7a7c6e49a1ff88b29373",
				},
				Expires:    "2026-08-31",
				OwningTest: routeRecoveryTest,
			},
		}},
	}
}

// allInventoryProfiles returns the canonical build/OS profiles the registry
// classifies against, sorted to satisfy the registry validator.
func allInventoryProfiles() []BuildProfileID {
	return []BuildProfileID{
		BuildDarwinDefault,
		BuildDarwinNative,
		BuildLinuxDefault,
		BuildLinuxNative,
		BuildWindowsCompile,
	}
}
