package effectinventory

type storeCatalogTargetShape string

const (
	storeTargetDirect         storeCatalogTargetShape = "direct"
	storeTargetCreate         storeCatalogTargetShape = "create"
	storeTargetBatchOne       storeCatalogTargetShape = "batch-one"
	storeTargetBatchSet       storeCatalogTargetShape = "batch-set"
	storeTargetGraph          storeCatalogTargetShape = "graph"
	storeTargetDependencyEdge storeCatalogTargetShape = "dependency-edge"
	storeTargetTransaction    storeCatalogTargetShape = "transaction"
)

type storeCatalogProfileSet string

const (
	storeProfileSetAll    storeCatalogProfileSet = "all"
	storeProfileSetNative storeCatalogProfileSet = "native"
)

type storeCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	StoreDomain      StoreDomain
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
	AccessPath       AccessPath
	TargetShape      storeCatalogTargetShape
	ReplacementGate  TaskRef
}

type storeCatalogSiteSpec struct {
	BoundaryID  string
	Operation   OperationKind
	Package     string
	Receiver    string
	Function    string
	File        string
	ClosurePath []int
	Ordinal     int
	ProfileSet  storeCatalogProfileSet
	Class       catalogRouteClassID
}

func storeInventoryRegistrations() ([]SiteRegistration, error) {
	return expandCatalogPartition(storeCatalogRouteClasses(), storeCatalogSiteRows())
}

func storeCatalogRouteClasses() []catalogRouteClass {
	classes := make([]catalogRouteClass, 0, len(storeCatalogRouteClassSpecs))
	for _, spec := range storeCatalogRouteClassSpecs {
		route := Route{
			StoreDomain:      spec.StoreDomain,
			ActionFamily:     spec.ActionFamily,
			ExecutingProcess: spec.ExecutingProcess,
			Target:           storeCatalogTarget(spec.TargetShape),
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
				Reason: storeRetainedDispositionReason(spec.AccessPath),
			}
		} else {
			route.Disposition = Disposition{
				Kind:   DispositionReplaceAtGate,
				Gates:  []TaskRef{spec.ReplacementGate},
				Reason: "replace the legacy direct writer with its domain owner",
			}
			route.Exception = &TemporaryException{
				Kind:         ExceptionLegacyBypass,
				Reason:       "legacy mutation bypasses the typed or keyed domain writer",
				OwnerTask:    "P0.1",
				RemovalTasks: []TaskRef{spec.ReplacementGate},
				Anchor: VersionAnchor{
					Kind:  AnchorGitCommit,
					Value: "914c87067b4646c3a3358d8c95acb93a64e8066f",
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

func storeRetainedDispositionReason(access AccessPath) string {
	if access == AccessSessionStoreFrontDoor {
		return "retain the typed session persistence front door"
	}
	return "retain the durable domain or backend store adapter"
}

func storeCatalogSiteRows() []catalogSiteRow {
	rows := make([]catalogSiteRow, 0, len(storeCatalogSiteSpecs))
	for _, spec := range storeCatalogSiteSpecs {
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
			Profiles: storeCatalogProfiles(spec.ProfileSet),
			Classes:  []catalogRouteClassID{spec.Class},
		})
	}
	return rows
}

func storeCatalogProfiles(set storeCatalogProfileSet) []BuildProfileID {
	switch set {
	case storeProfileSetAll:
		return []BuildProfileID{
			BuildDarwinDefault,
			BuildDarwinNative,
			BuildLinuxDefault,
			BuildLinuxNative,
			BuildWindowsCompile,
		}
	case storeProfileSetNative:
		return []BuildProfileID{BuildDarwinNative, BuildLinuxNative}
	default:
		return nil
	}
}

func storeCatalogTarget(shape storeCatalogTargetShape) TargetRef {
	boundaryValue := func(role TargetIdentityRole, kind ValueSlotKind, index int) TargetIdentityRef {
		return TargetIdentityRef{
			Role:         role,
			BoundarySlot: ValueSlot{Kind: kind, Index: index},
			Source:       TargetSourceBoundaryValue,
		}
	}
	switch shape {
	case storeTargetDirect:
		return TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityExisting,
			Signature:   TargetSignatureDirect,
			Identities:  []TargetIdentityRef{boundaryValue(TargetRolePrimary, SlotParameter, 1)},
			Detail:      "one existing durable record addressed by ID",
		}
	case storeTargetCreate:
		generated := boundaryValue(TargetRoleGenerated, SlotResult, 1)
		generated.Projection = ObjectRef{Package: beadsPackage, Receiver: "Bead", Name: "ID"}
		return TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityGenerated,
			Signature:   TargetSignatureCreate,
			Identities: []TargetIdentityRef{
				boundaryValue(TargetRoleInput, SlotParameter, 1),
				generated,
			},
			Detail: "one durable record whose ID is assigned by the store",
		}
	case storeTargetBatchOne:
		return TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityExisting,
			Signature:   TargetSignatureBatch,
			Identities:  []TargetIdentityRef{boundaryValue(TargetRolePrimary, SlotParameter, 1)},
			Detail:      "one existing durable record receives a metadata batch",
		}
	case storeTargetBatchSet:
		return TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalitySet,
			Identity:    TargetIdentityExisting,
			Signature:   TargetSignatureBatch,
			Identities:  []TargetIdentityRef{boundaryValue(TargetRolePrimary, SlotParameter, 1)},
			Detail:      "an explicit set of durable record IDs",
		}
	case storeTargetGraph:
		generated := boundaryValue(TargetRoleGenerated, SlotResult, 1)
		generated.Projection = ObjectRef{Package: beadsPackage, Receiver: "GraphApplyResult", Name: "IDs"}
		return TargetRef{
			Kind:        TargetDurableGraph,
			Cardinality: TargetCardinalityPlan,
			Identity:    TargetIdentitySymbolicGenerated,
			Signature:   TargetSignatureGraphPlan,
			Identities: []TargetIdentityRef{
				boundaryValue(TargetRolePlan, SlotParameter, 2),
				generated,
			},
			Detail: "graph plan keys map to store-generated durable IDs",
		}
	case storeTargetDependencyEdge:
		return TargetRef{
			Kind:        TargetDurableDependencyEdge,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityComposite,
			Signature:   TargetSignatureDependencyEdge,
			Identities: []TargetIdentityRef{
				boundaryValue(TargetRoleFrom, SlotParameter, 1),
				boundaryValue(TargetRoleTo, SlotParameter, 2),
			},
			Detail: "one ordered dependency edge identified by both durable endpoints",
		}
	case storeTargetTransaction:
		return TargetRef{
			Kind:        TargetDurableTransaction,
			Cardinality: TargetCardinalityCallback,
			Identity:    TargetIdentityCallbackEffects,
			Signature:   TargetSignatureTransaction,
			Identities:  []TargetIdentityRef{boundaryValue(TargetRoleCallback, SlotParameter, 2)},
			Detail:      "durable effects selected inside the transaction callback",
		}
	default:
		return TargetRef{}
	}
}
