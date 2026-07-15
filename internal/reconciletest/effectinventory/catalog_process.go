package effectinventory

type processCatalogTargetShape string

const (
	processTargetSingleParameter   processCatalogTargetShape = "single-parameter"
	processTargetSetParameter      processCatalogTargetShape = "set-parameter"
	processTargetSingleReceiver    processCatalogTargetShape = "single-receiver"
	processTargetServerParameter   processCatalogTargetShape = "server-parameter"
	processTargetServerSetReceiver processCatalogTargetShape = "server-set-receiver"
)

type processCatalogProfileSet string

const (
	processProfileSetAll     processCatalogProfileSet = "all"
	processProfileSetUnix    processCatalogProfileSet = "unix"
	processProfileSetWindows processCatalogProfileSet = "windows"
)

type processCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
	AccessPath       AccessPath
	TargetShape      processCatalogTargetShape
	ReplacementGate  TaskRef
}

type processCatalogSiteSpec struct {
	BoundaryID                     string
	Operation                      OperationKind
	Package                        string
	Receiver                       string
	Function                       string
	File                           string
	ClosurePath                    []int
	Ordinal                        int
	ProfileSet                     processCatalogProfileSet
	Class                          catalogRouteClassID
	ExplicitManagedDoltGuardRoutes bool
}

func processInventoryRegistrations() ([]SiteRegistration, error) {
	return expandCatalogPartition(processCatalogRouteClasses(), processCatalogSiteRows())
}

func processCatalogRouteClasses() []catalogRouteClass {
	classes := make([]catalogRouteClass, 0, len(processCatalogRouteClassSpecs))
	for _, spec := range processCatalogRouteClassSpecs {
		route := Route{
			ActionFamily:     spec.ActionFamily,
			ExecutingProcess: spec.ExecutingProcess,
			Target:           processCatalogTarget(spec.TargetShape),
			Fences:           []Fence{{Kind: FenceNone}},
			CurrentGate:      GateRef{Kind: GateUnconditionalLegacy},
			AccessPath:       spec.AccessPath,
			Continuation: Continuation{
				Locus:      ContinuationInline,
				Completion: CompletionSynchronous,
			},
			OwningTests: []TestRef{{
				Package: gcCommandPackage,
				Name:    "TestReconcilerEffectInventoryOnBoundHead",
			}},
		}
		if spec.ReplacementGate == "" {
			route.Disposition = Disposition{
				Kind:   DispositionRetainBoundary,
				Reason: "retain the audited process or workspace-service boundary",
			}
		} else {
			route.Disposition = Disposition{
				Kind:   DispositionReplaceAtGate,
				Gates:  []TaskRef{spec.ReplacementGate},
				Reason: "replace direct os.Process termination with the audited process boundary",
			}
			route.Exception = &TemporaryException{
				Kind:         ExceptionLegacyBypass,
				Reason:       "direct os.Process termination bypasses the audited process effect seam",
				OwnerTask:    "P0.10",
				RemovalTasks: []TaskRef{spec.ReplacementGate},
				Anchor: VersionAnchor{
					Kind:  AnchorGitCommit,
					Value: "690285c100f96e94e7e69ac889aa7f056c527198",
				},
				Expires: "2026-12-31",
				OwningTest: TestRef{
					Package: gcCommandPackage,
					Name:    "TestReconcilerEffectInventoryOnBoundHead",
				},
			}
		}
		classes = append(classes, catalogRouteClass{ID: spec.ID, Definition: route})
	}
	return classes
}

func processCatalogSiteRows() []catalogSiteRow {
	rows := make([]catalogSiteRow, 0, len(processCatalogSiteSpecs))
	for _, spec := range processCatalogSiteSpecs {
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
			Profiles: processCatalogProfiles(spec.ProfileSet),
		}
		if spec.ExplicitManagedDoltGuardRoutes {
			row.ExplicitRoutes = processManagedDoltGuardExplicitRoutes()
		} else {
			row.Classes = []catalogRouteClassID{spec.Class}
		}
		rows = append(rows, row)
	}
	return rows
}

func processManagedDoltGuardExplicitRoutes() []catalogExplicitRoute {
	guarded := FunctionRef{
		Object: ObjectRef{Package: gcCommandPackage, Name: "terminateManagedDoltPIDGuarded"},
		File:   "cmd/gc/dolt_start_managed.go",
	}
	legacy := FunctionRef{
		Object: ObjectRef{Package: gcCommandPackage, Name: "terminateManagedDoltPID"},
		File:   "cmd/gc/dolt_start_managed.go",
	}
	direct := func(classID catalogRouteClassID, owner FunctionRef, ordinal int) catalogExplicitRoute {
		return catalogExplicitRoute{
			Class:        classID,
			LogicalOwner: owner,
			Hops: []RouteHop{{
				Site:     OperationSite{Operation: OperationCall, Enclosing: owner, Ordinal: ordinal},
				Dispatch: HopDispatchExact,
				Callee:   guarded,
			}},
		}
	}
	viaLegacy := func(classID catalogRouteClassID, owner FunctionRef, ordinal int) catalogExplicitRoute {
		return catalogExplicitRoute{
			Class:        classID,
			LogicalOwner: owner,
			Hops: []RouteHop{
				{
					Site:     OperationSite{Operation: OperationCall, Enclosing: owner, Ordinal: ordinal},
					Dispatch: HopDispatchExact,
					Callee:   legacy,
				},
				{
					Site:     OperationSite{Operation: OperationCall, Enclosing: legacy, Ordinal: 1},
					Dispatch: HopDispatchExact,
					Callee:   guarded,
				},
			},
		}
	}
	function := func(name, file string) FunctionRef {
		return FunctionRef{Object: ObjectRef{Package: gcCommandPackage, Name: name}, File: file}
	}
	startedProcess := function("terminateManagedDoltStartedProcess", "cmd/gc/dolt_start_managed.go")
	testWatchdogStart := function("startManagedDoltSQLServerWithTestWatchdog", "cmd/gc/dolt_start_managed.go")
	scopeWatchdogStart := function("startManagedDoltSQLServerWithScopeWatchdog", "cmd/gc/dolt_scope_watchdog.go")
	recoveryCleanup := function("cleanupFailedManagedDoltRecovery", "cmd/gc/dolt_recover_managed.go")
	testWatchdog := function("runManagedDoltTestWatchdog", "cmd/gc/dolt_start_managed.go")
	scopeWatchdogChild := function("terminateManagedDoltScopeWatchdogChild", "cmd/gc/dolt_scope_watchdog.go")
	return []catalogExplicitRoute{
		direct(processClassBoundarySignalCLIOne, startedProcess, 1),
		direct(processClassBoundarySignalCLIOne, startedProcess, 2),
		viaLegacy(processClassBoundarySignalCLIOne, testWatchdogStart, 1),
		viaLegacy(processClassBoundarySignalCLIOne, scopeWatchdogStart, 1),
		viaLegacy(processClassBoundarySignalCLIOne, recoveryCleanup, 1),
		viaLegacy(processClassBoundarySignalProviderChildOne, testWatchdog, 1),
		viaLegacy(processClassBoundarySignalProviderChildOne, testWatchdog, 2),
		viaLegacy(processClassBoundarySignalProviderChildOne, testWatchdog, 3),
		direct(processClassBoundarySignalProviderChildOne, scopeWatchdogChild, 1),
	}
}

func processCatalogProfiles(set processCatalogProfileSet) []BuildProfileID {
	switch set {
	case processProfileSetAll:
		return []BuildProfileID{
			BuildDarwinDefault,
			BuildDarwinNative,
			BuildLinuxDefault,
			BuildLinuxNative,
			BuildWindowsCompile,
		}
	case processProfileSetUnix:
		return []BuildProfileID{
			BuildDarwinDefault,
			BuildDarwinNative,
			BuildLinuxDefault,
			BuildLinuxNative,
		}
	case processProfileSetWindows:
		return []BuildProfileID{BuildWindowsCompile}
	default:
		return nil
	}
}

func processCatalogTarget(shape processCatalogTargetShape) TargetRef {
	identity := func(kind ValueSlotKind, index int) TargetIdentityRef {
		return TargetIdentityRef{
			Role:         TargetRolePrimary,
			BoundarySlot: ValueSlot{Kind: kind, Index: index},
			Source:       TargetSourceBoundaryValue,
		}
	}
	target := TargetRef{
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityExisting,
		Signature:   TargetSignatureProcess,
	}
	switch shape {
	case processTargetSingleParameter:
		target.Kind = TargetProcessIdentity
		target.Identities = []TargetIdentityRef{identity(SlotParameter, 1)}
		target.Detail = "one existing process identified by the boundary parameter"
	case processTargetSetParameter:
		target.Kind = TargetProcessIdentity
		target.Cardinality = TargetCardinalitySet
		target.Identities = []TargetIdentityRef{identity(SlotParameter, 1)}
		target.Detail = "one process group or managed process tree rooted by the boundary parameter"
	case processTargetSingleReceiver:
		target.Kind = TargetProcessIdentity
		target.Identities = []TargetIdentityRef{identity(SlotReceiver, 0)}
		target.Detail = "one process identified by the os.Process receiver"
	case processTargetServerParameter:
		target.Kind = TargetProviderServer
		target.Identities = []TargetIdentityRef{identity(SlotParameter, 1)}
		target.Detail = "one named workspace service selected for restart"
	case processTargetServerSetReceiver:
		target.Kind = TargetProviderServer
		target.Cardinality = TargetCardinalitySet
		target.Identities = []TargetIdentityRef{identity(SlotReceiver, 0)}
		target.Detail = "the workspace service set owned by this manager"
	default:
		return TargetRef{}
	}
	return target
}
