package effectinventory

type wakeCatalogProfileSet string

const (
	wakeProfileSetAll  wakeCatalogProfileSet = "all"
	wakeProfileSetUnix wakeCatalogProfileSet = "unix"
)

type wakeCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
}

type wakeCatalogSiteSpec struct {
	BoundaryID                string
	Operation                 OperationKind
	Package                   string
	Receiver                  string
	Function                  string
	File                      string
	ClosurePath               []int
	Ordinal                   int
	ProfileSet                wakeCatalogProfileSet
	Classes                   []catalogRouteClassID
	ExplicitAcquireLockRoutes bool
}

func wakeInventoryRegistrations() ([]SiteRegistration, error) {
	return expandCatalogPartition(wakeCatalogRouteClasses(), wakeCatalogSiteRows())
}

func wakeCatalogRouteClasses() []catalogRouteClass {
	classes := make([]catalogRouteClass, 0, len(wakeCatalogRouteClassSpecs))
	for _, spec := range wakeCatalogRouteClassSpecs {
		classes = append(classes, catalogRouteClass{
			ID: spec.ID,
			Definition: Route{
				ActionFamily:     spec.ActionFamily,
				ExecutingProcess: spec.ExecutingProcess,
				Target: TargetRef{
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
				},
				Fences:      []Fence{{Kind: FenceNone}},
				CurrentGate: GateRef{Kind: GateUnconditionalLegacy},
				Disposition: Disposition{
					Kind:   DispositionRetainBoundary,
					Reason: "retain the canonical typed wake-source boundary",
				},
				AccessPath: AccessDirectWake,
				Continuation: Continuation{
					Locus:      ContinuationInline,
					Completion: CompletionSynchronous,
				},
				OwningTests: []TestRef{{
					Package: gcCommandPackage,
					Name:    "TestReconcilerEffectInventoryOnBoundHead",
				}},
			},
		})
	}
	return classes
}

func wakeCatalogSiteRows() []catalogSiteRow {
	rows := make([]catalogSiteRow, 0, len(wakeCatalogSiteSpecs))
	for _, spec := range wakeCatalogSiteSpecs {
		row := catalogSiteRow{
			BoundaryID: spec.BoundaryID,
			Matcher: OperationSite{
				Operation: spec.Operation,
				Enclosing: FunctionRef{
					Object: ObjectRef{
						Package:  spec.Package,
						Receiver: spec.Receiver,
						Name:     spec.Function,
					},
					File:        spec.File,
					ClosurePath: append([]int(nil), spec.ClosurePath...),
				},
				Ordinal: spec.Ordinal,
			},
			Profiles: wakeCatalogProfiles(spec.ProfileSet),
		}
		if spec.ExplicitAcquireLockRoutes {
			row.ExplicitRoutes = make([]catalogExplicitRoute, 0, len(spec.Classes))
			for _, classID := range spec.Classes {
				row.ExplicitRoutes = append(row.ExplicitRoutes, wakeAcquireLockExplicitRoute(classID))
			}
		} else {
			row.Classes = append([]catalogRouteClassID(nil), spec.Classes...)
		}
		rows = append(rows, row)
	}
	return rows
}

func wakeCatalogProfiles(set wakeCatalogProfileSet) []BuildProfileID {
	switch set {
	case wakeProfileSetAll:
		return []BuildProfileID{
			BuildDarwinDefault,
			BuildDarwinNative,
			BuildLinuxDefault,
			BuildLinuxNative,
			BuildWindowsCompile,
		}
	case wakeProfileSetUnix:
		return []BuildProfileID{
			BuildDarwinDefault,
			BuildDarwinNative,
			BuildLinuxDefault,
			BuildLinuxNative,
		}
	default:
		return nil
	}
}

func wakeAcquireLockExplicitRoute(classID catalogRouteClassID) catalogExplicitRoute {
	const productMetricsPackage = "github.com/gastownhall/gascity/internal/productmetrics"
	physicalOwner := FunctionRef{
		Object: ObjectRef{Package: productMetricsPackage, Receiver: "unixStorageDirectory", Name: "acquireLock"},
		File:   "internal/productmetrics/lock_unix.go",
	}
	storageOwner := FunctionRef{
		Object: ObjectRef{Package: productMetricsPackage, Receiver: "storageDir", Name: "acquireLock"},
		File:   "internal/productmetrics/storage.go",
	}
	var logicalOwner FunctionRef
	switch classID {
	case wakeClassControllerCLI, wakeClassTimersCLI:
		logicalOwner = FunctionRef{
			Object: ObjectRef{Package: productMetricsPackage, Receiver: "Service", Name: "activateNotice"},
			File:   "internal/productmetrics/notice.go",
		}
	case wakeClassControllerSidecar, wakeClassTimersSidecar:
		logicalOwner = FunctionRef{
			Object: ObjectRef{Package: productMetricsPackage, Receiver: "Service", Name: "lockUploader"},
			File:   "internal/productmetrics/uploader.go",
		}
	}
	return catalogExplicitRoute{
		Class:        classID,
		LogicalOwner: logicalOwner,
		Hops: []RouteHop{
			{
				Site:     OperationSite{Operation: OperationCall, Enclosing: logicalOwner, Ordinal: 1},
				Dispatch: HopDispatchExact,
				Callee:   storageOwner,
			},
			{
				Site:     OperationSite{Operation: OperationCall, Enclosing: storageOwner, Ordinal: 1},
				Dispatch: HopDispatchVTA,
				Callee:   physicalOwner,
			},
		},
	}
}
