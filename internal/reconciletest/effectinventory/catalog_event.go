package effectinventory

type eventCatalogRouteClassSpec struct {
	ID               catalogRouteClassID
	ExecutingProcess ExecutingProcess
}

type eventCatalogSiteSpec struct {
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

func eventInventoryRegistrations() ([]SiteRegistration, error) {
	return expandCatalogPartition(eventCatalogRouteClasses(), eventCatalogSiteRows())
}

func eventCatalogRouteClasses() []catalogRouteClass {
	classes := make([]catalogRouteClass, 0, len(eventCatalogRouteClassSpecs))
	for _, spec := range eventCatalogRouteClassSpecs {
		classes = append(classes, catalogRouteClass{
			ID: spec.ID,
			Definition: Route{
				ActionFamily:     FamilyObservation,
				ExecutingProcess: spec.ExecutingProcess,
				Target: TargetRef{
					Kind:        TargetEventLog,
					Cardinality: TargetCardinalityOne,
					Identity:    TargetIdentityAppendRecord,
					Signature:   TargetSignatureEventAppend,
					Identities: []TargetIdentityRef{{
						Role:         TargetRolePrimary,
						BoundarySlot: ValueSlot{Kind: SlotReceiver},
						Source:       TargetSourceBoundaryValue,
					}},
					Detail: "one record offered to the selected event recorder",
				},
				Fences:      []Fence{{Kind: FenceNone}},
				CurrentGate: GateRef{Kind: GateUnconditionalLegacy},
				Disposition: Disposition{
					Kind:   DispositionRetainBoundary,
					Reason: "retain the canonical typed event-recorder boundary",
				},
				AccessPath: AccessDirectEvent,
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

func eventCatalogSiteRows() []catalogSiteRow {
	rows := make([]catalogSiteRow, 0, len(eventCatalogSiteSpecs))
	for _, spec := range eventCatalogSiteSpecs {
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
			Profiles: []BuildProfileID{
				BuildDarwinDefault,
				BuildDarwinNative,
				BuildLinuxDefault,
				BuildLinuxNative,
				BuildWindowsCompile,
			},
			Classes: []catalogRouteClassID{spec.Class},
		})
	}
	return rows
}
