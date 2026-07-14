package effectinventory

type providerCatalogTargetShape string

const (
	providerTargetSessionP1       providerCatalogTargetShape = "session-p1"
	providerTargetSessionP2       providerCatalogTargetShape = "session-p2"
	providerTargetSessionReceiver providerCatalogTargetShape = "session-receiver"
	providerTargetRuntimeP2       providerCatalogTargetShape = "runtime-p2"
	providerTargetRuntimeReceiver providerCatalogTargetShape = "runtime-receiver"
	providerTargetServerReceiver  providerCatalogTargetShape = "server-receiver"
	providerTargetProcessRuntime  providerCatalogTargetShape = "process-runtime-p1"
	providerTargetAttachP1        providerCatalogTargetShape = "operator-attach-p1"
	providerTargetAttachP3        providerCatalogTargetShape = "operator-attach-p3"
)

type providerCatalogFenceShape string

const (
	providerFenceNone                   providerCatalogFenceShape = "none"
	providerFenceSessionMutationLock    providerCatalogFenceShape = "session-mutation-lock"
	providerFenceControllerSingleWriter providerCatalogFenceShape = "controller-single-writer"
	providerFenceProcessScanReread      providerCatalogFenceShape = "process-scan-live-reread"
)

type providerCatalogContinuationShape string

const (
	providerContinuationInline providerCatalogContinuationShape = "inline"
	providerContinuationChild  providerCatalogContinuationShape = "provider-child"
)

type providerCatalogDisposition string

const (
	providerDispositionRetainNative  providerCatalogDisposition = "retain-native"
	providerDispositionRetainWorker  providerCatalogDisposition = "retain-worker"
	providerDispositionReplaceLegacy providerCatalogDisposition = "replace-legacy"
)

type providerCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
	AccessPath       AccessPath
	TargetShape      providerCatalogTargetShape
	FenceShape       providerCatalogFenceShape
	Continuation     providerCatalogContinuationShape
	Disposition      providerCatalogDisposition
}

type providerCatalogSiteSpec struct {
	BoundaryID  string
	Operation   OperationKind
	Package     string
	Receiver    string
	Function    string
	File        string
	ClosurePath []int
	Ordinal     int
	Class       catalogRouteClassID
}

func providerInventoryRegistrations() ([]SiteRegistration, error) {
	return expandCatalogPartition(providerCatalogRouteClasses(), providerCatalogSiteRows())
}

func providerCatalogRouteClasses() []catalogRouteClass {
	classes := make([]catalogRouteClass, 0, len(providerCatalogRouteClassSpecs))
	for _, spec := range providerCatalogRouteClassSpecs {
		route := Route{
			ActionFamily:     spec.ActionFamily,
			ExecutingProcess: spec.ExecutingProcess,
			Target:           providerCatalogTarget(spec.TargetShape),
			Fences:           providerCatalogFences(spec.FenceShape),
			CurrentGate:      GateRef{Kind: GateUnconditionalLegacy},
			AccessPath:       spec.AccessPath,
			Continuation:     providerCatalogContinuation(spec.Continuation),
			OwningTests: []TestRef{{
				Package: gcCommandPackage,
				Name:    "TestReconcilerEffectInventoryOnBoundHead",
			}},
		}
		providerCatalogApplyDisposition(&route, spec.Disposition)
		classes = append(classes, catalogRouteClass{ID: spec.ID, Definition: route})
	}
	return classes
}

func providerCatalogApplyDisposition(route *Route, disposition providerCatalogDisposition) {
	switch disposition {
	case providerDispositionRetainNative:
		route.Disposition = Disposition{
			Kind:   DispositionRetainBoundary,
			Reason: "retain the provider-native mutation entry as the auditable effect leaf",
		}
	case providerDispositionRetainWorker:
		route.Disposition = Disposition{
			Kind:   DispositionRetainBoundary,
			Reason: "retain the mandatory worker effect boundary",
		}
	case providerDispositionReplaceLegacy:
		gates := []TaskRef{"P3.4", "P5.4A"}
		route.Disposition = Disposition{
			Kind:   DispositionReplaceAtGate,
			Gates:  append([]TaskRef(nil), gates...),
			Reason: "replace the legacy direct call with the mandatory worker effect boundary and shared per-session executor",
		}
		route.Exception = &TemporaryException{
			Kind:         ExceptionLegacyBypass,
			Reason:       "legacy provider mutation bypasses the mandatory worker effect boundary",
			OwnerTask:    "P0.1",
			RemovalTasks: append([]TaskRef(nil), gates...),
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
}

func providerCatalogSiteRows() []catalogSiteRow {
	rows := make([]catalogSiteRow, 0, len(providerCatalogSiteSpecs))
	for _, spec := range providerCatalogSiteSpecs {
		rows = append(rows, catalogSiteRow{
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
			Profiles: providerCatalogProfiles(),
			Classes:  []catalogRouteClassID{spec.Class},
		})
	}
	return rows
}

func providerCatalogProfiles() []BuildProfileID {
	return []BuildProfileID{
		BuildDarwinDefault,
		BuildDarwinNative,
		BuildLinuxDefault,
		BuildLinuxNative,
		BuildWindowsCompile,
	}
}

func providerCatalogTarget(shape providerCatalogTargetShape) TargetRef {
	direct := func(kind TargetKind, slot ValueSlot) TargetRef {
		return TargetRef{
			Kind:        kind,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityExisting,
			Signature:   TargetSignatureDirect,
			Identities: []TargetIdentityRef{{
				Role:         TargetRolePrimary,
				BoundarySlot: slot,
				Source:       TargetSourceBoundaryValue,
			}},
		}
	}
	switch shape {
	case providerTargetSessionP1:
		target := direct(TargetSessionIdentity, ValueSlot{Kind: SlotParameter, Index: 1})
		target.Detail = "one named session addressed by the first provider parameter"
		return target
	case providerTargetSessionP2:
		target := direct(TargetSessionIdentity, ValueSlot{Kind: SlotParameter, Index: 2})
		target.Detail = "one named session addressed after the context parameter"
		return target
	case providerTargetSessionReceiver:
		target := direct(TargetSessionIdentity, ValueSlot{Kind: SlotReceiver})
		target.Detail = "one live session carried by its attachment receiver"
		return target
	case providerTargetRuntimeP2:
		target := direct(TargetRuntimeIdentity, ValueSlot{Kind: SlotParameter, Index: 2})
		target.Detail = "one runtime environment addressed by name or Place after the context parameter"
		return target
	case providerTargetRuntimeReceiver:
		target := direct(TargetRuntimeIdentity, ValueSlot{Kind: SlotReceiver})
		target.Detail = "one provisioned runtime environment carried by its Place receiver"
		return target
	case providerTargetServerReceiver:
		target := direct(TargetProviderServer, ValueSlot{Kind: SlotReceiver})
		target.Detail = "the provider-global server carried by the lifecycle receiver"
		return target
	case providerTargetProcessRuntime:
		target := direct(TargetRuntimeIdentity, ValueSlot{Kind: SlotParameter, Index: 1})
		target.Identities[0].Projection = ObjectRef{Package: runtimePackage, Receiver: "LiveRuntime", Name: "SessionID"}
		target.Identities[0].Source = TargetSourceProcessScan
		target.Identities[0].SourceObject = ObjectRef{Package: runtimePackage, Receiver: "ProcessTableScanner", Name: "FindRuntimesBySessionID"}
		target.Identities[0].SourceSlot = ValueSlot{Kind: SlotResult, Index: 1}
		target.Detail = "one scanned live runtime projected by stable session identity"
		return target
	case providerTargetAttachP1:
		return providerCatalogAttachTarget(1)
	case providerTargetAttachP3:
		return providerCatalogAttachTarget(3)
	default:
		return TargetRef{}
	}
}

func providerCatalogAttachTarget(destinationParameter int) TargetRef {
	return TargetRef{
		Kind:        TargetOperatorTerminal,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityComposite,
		Signature:   TargetSignatureOperatorAttach,
		Identities: []TargetIdentityRef{
			{
				Role:         TargetRoleOperatorTerminal,
				BoundarySlot: ValueSlot{Kind: SlotAmbientTerminal},
				Source:       TargetSourceAmbientTerminal,
			},
			{
				Role:         TargetRoleDestination,
				BoundarySlot: ValueSlot{Kind: SlotParameter, Index: destinationParameter},
				Source:       TargetSourceBoundaryValue,
			},
		},
		Detail: "the calling operator terminal attached to one named runtime destination",
	}
}

func providerCatalogFences(shape providerCatalogFenceShape) []Fence {
	switch shape {
	case providerFenceNone:
		return []Fence{{Kind: FenceNone}}
	case providerFenceSessionMutationLock:
		return []Fence{{
			Kind: FenceProcessLocalNonExclusive,
			Source: ObjectRef{
				Package: "github.com/gastownhall/gascity/internal/session",
				Name:    "WithSessionMutationLock",
			},
		}}
	case providerFenceControllerSingleWriter:
		source := ObjectRef{Package: gcCommandPackage, Receiver: "CityRuntime", Name: "run"}
		return []Fence{
			{Kind: FenceProcessLocalNonExclusive, Source: source},
			{Kind: FenceSingleWriterAssumption, Source: source},
		}
	case providerFenceProcessScanReread:
		return []Fence{{
			Kind: FenceLiveRereadNonCAS,
			Source: ObjectRef{
				Package:  runtimePackage,
				Receiver: "ProcessTableScanner",
				Name:     "FindRuntimesBySessionID",
			},
		}}
	default:
		return nil
	}
}

func providerCatalogContinuation(shape providerCatalogContinuationShape) Continuation {
	switch shape {
	case providerContinuationInline:
		return Continuation{Locus: ContinuationInline, Completion: CompletionSynchronous}
	case providerContinuationChild:
		return Continuation{Locus: ContinuationProviderChild, Completion: CompletionDetached}
	default:
		return Continuation{}
	}
}
