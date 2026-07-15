package effectinventory

type wakeCatalogProfileSet string

const (
	wakeProfileSetAll  wakeCatalogProfileSet = "all"
	wakeProfileSetUnix wakeCatalogProfileSet = "unix"
)

type wakeCatalogTargetShape string

const (
	wakeTargetControllerChannel wakeCatalogTargetShape = "controller-channel"
	wakeTargetBoundarySource    wakeCatalogTargetShape = "boundary-source"
	wakeTargetResultSource      wakeCatalogTargetShape = "result-source"
)

type wakeCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
	TargetShape      wakeCatalogTargetShape
}

type wakeCatalogSiteSpec struct {
	BoundaryID     string
	Operation      OperationKind
	Package        string
	Receiver       string
	Function       string
	File           string
	ClosurePath    []int
	Ordinal        int
	ProfileSet     wakeCatalogProfileSet
	Classes        []catalogRouteClassID
	ExplicitRoutes []catalogExplicitRoute
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
				Target:           wakeCatalogTarget(spec.TargetShape),
				Fences:           []Fence{{Kind: FenceNone}},
				CurrentGate:      GateRef{Kind: GateUnconditionalLegacy},
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

func knownWakeCatalogTargetShape(shape wakeCatalogTargetShape) bool {
	switch shape {
	case wakeTargetControllerChannel, wakeTargetBoundarySource, wakeTargetResultSource:
		return true
	default:
		return false
	}
}

func wakeCatalogTarget(shape wakeCatalogTargetShape) TargetRef {
	target := TargetRef{
		Cardinality: TargetCardinalityOne,
		Identities: []TargetIdentityRef{{
			Role:   TargetRolePrimary,
			Source: TargetSourceBoundaryValue,
		}},
	}
	switch shape {
	case wakeTargetControllerChannel:
		target.Kind = TargetControllerChannel
		target.Identity = TargetIdentitySingleton
		target.Signature = TargetSignatureChannel
		target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotBoundaryObject}
		target.Detail = "one controller-owned channel identified by its registered field"
	case wakeTargetBoundarySource:
		target.Kind = TargetWakeSource
		target.Identity = TargetIdentityExisting
		target.Signature = TargetSignatureWakeSource
		target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotBoundaryObject}
		target.Detail = "one invocation-local timer, cancellation, or signal channel"
	case wakeTargetResultSource:
		target.Kind = TargetWakeSource
		target.Identity = TargetIdentityExisting
		target.Signature = TargetSignatureWakeSource
		target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotResult, Index: 1}
		target.Detail = "one invocation-local wake source returned by the registered boundary"
	}
	return target
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
		if explicitRoutes, ok := wakeCatalogReviewedExplicitRoutes[registrationPhysicalKey(row.BoundaryID, row.Matcher)]; ok {
			row.ExplicitRoutes = cloneCatalogExplicitRoutes(explicitRoutes)
		} else if len(spec.ExplicitRoutes) != 0 {
			row.ExplicitRoutes = cloneCatalogExplicitRoutes(spec.ExplicitRoutes)
		} else {
			row.Classes = append([]catalogRouteClassID(nil), spec.Classes...)
		}
		rows = append(rows, row)
	}
	return rows
}

func cloneCatalogExplicitRoutes(routes []catalogExplicitRoute) []catalogExplicitRoute {
	cloned := make([]catalogExplicitRoute, len(routes))
	for index, route := range routes {
		definition := cloneRoute(Route{
			LogicalOwner: route.LogicalOwner,
			Hops:         route.Hops,
		})
		cloned[index] = catalogExplicitRoute{
			Class:        route.Class,
			LogicalOwner: definition.LogicalOwner,
			Hops:         definition.Hops,
		}
	}
	return cloned
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

func wakeAcquireLockExplicitRoutes(classIDs ...catalogRouteClassID) []catalogExplicitRoute {
	routes := make([]catalogExplicitRoute, 0, len(classIDs))
	for _, classID := range classIDs {
		routes = append(routes, wakeAcquireLockExplicitRoute(classID))
	}
	return routes
}
