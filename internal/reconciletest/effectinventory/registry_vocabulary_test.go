package effectinventory

import (
	"reflect"
	"testing"
)

func TestValidateRegistryAcceptsBoundedTargetSignatures(t *testing.T) {
	tests := []struct {
		name        string
		boundary    EffectKind
		access      AccessPath
		storeDomain StoreDomain
		target      TargetRef
	}{
		{
			name:        "single generated create",
			boundary:    KindStoreMutation,
			access:      AccessStoreAdapter,
			storeDomain: StoreDomainSessionLifecycle,
			target: TargetRef{
				Kind:        TargetDurableRecord,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityGenerated,
				Signature:   TargetSignatureCreate,
				Identities: []TargetIdentityRef{
					{
						Role:         TargetRoleInput,
						BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
						Source:       TargetSourceBoundaryValue,
					},
					{
						Role:         TargetRoleGenerated,
						BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
						Projection:   objectRef(beadsPackage, "Bead", "ID"),
						Source:       TargetSourceBoundaryValue,
					},
				},
				Detail: "one durable record whose ID is assigned by the store",
			},
		},
		{
			name:        "one-record metadata batch",
			boundary:    KindStoreMutation,
			access:      AccessStoreAdapter,
			storeDomain: StoreDomainSessionLifecycle,
			target:      batchTarget(TargetCardinalityOne),
		},
		{
			name:        "record set batch",
			boundary:    KindStoreMutation,
			access:      AccessStoreAdapter,
			storeDomain: StoreDomainMaintenance,
			target:      batchTarget(TargetCardinalitySet),
		},
		{
			name:        "symbolic graph plan with generated IDs",
			boundary:    KindStoreMutation,
			access:      AccessStoreAdapter,
			storeDomain: StoreDomainControlDispatch,
			target: TargetRef{
				Kind:        TargetDurableGraph,
				Cardinality: TargetCardinalityPlan,
				Identity:    TargetIdentitySymbolicGenerated,
				Signature:   TargetSignatureGraphPlan,
				Identities: []TargetIdentityRef{
					{
						Role:         TargetRolePlan,
						BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 2},
						Source:       TargetSourceBoundaryValue,
					},
					{
						Role:         TargetRoleGenerated,
						BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
						Projection:   objectRef(beadsPackage, "GraphApplyResult", "IDs"),
						Source:       TargetSourceBoundaryValue,
					},
				},
				Detail: "plan node keys map to store-generated durable IDs",
			},
		},
		{
			name:        "dependency edge endpoints",
			boundary:    KindStoreMutation,
			access:      AccessStoreAdapter,
			storeDomain: StoreDomainControlDispatch,
			target: TargetRef{
				Kind:        TargetDurableDependencyEdge,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityComposite,
				Signature:   TargetSignatureDependencyEdge,
				Identities: []TargetIdentityRef{
					{Role: TargetRoleFrom, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue},
					{Role: TargetRoleTo, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 2}, Source: TargetSourceBoundaryValue},
				},
				Detail: "one ordered dependency edge identified by both durable endpoints",
			},
		},
		{
			name:     "controller channel",
			boundary: KindWakeSource,
			access:   AccessDirectWake,
			target: TargetRef{
				Kind:        TargetControllerChannel,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentitySingleton,
				Signature:   TargetSignatureChannel,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotBoundaryObject},
					Source:       TargetSourceBoundaryValue,
				}},
				Detail: "the exact registered controller-owned channel object",
			},
		},
		{
			name:     "process identity",
			boundary: KindProcessMutation,
			access:   AccessProcessBoundary,
			target: TargetRef{
				Kind:        TargetProcessIdentity,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityExisting,
				Signature:   TargetSignatureProcess,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
					Source:       TargetSourceProcessScan,
					SourceObject: objectRef("github.com/gastownhall/gascity/internal/runtime/proctable", "Scanner", "Find"),
					SourceSlot:   ValueSlot{Kind: SlotResult, Index: 1},
				}},
				Detail: "one live process identity returned by the complete process scan",
			},
		},
		{
			name:     "event append",
			boundary: KindEventEmission,
			access:   AccessDirectEvent,
			target: TargetRef{
				Kind:        TargetEventLog,
				Cardinality: TargetCardinalityOne,
				Identity:    TargetIdentityAppendRecord,
				Signature:   TargetSignatureEventAppend,
				Identities: []TargetIdentityRef{{
					Role:         TargetRolePrimary,
					BoundarySlot: ValueSlot{Kind: SlotReceiver},
					Source:       TargetSourceBoundaryValue,
				}},
				Detail: "one typed record appended to the selected event log",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := registryForTarget(tt.boundary, tt.access, tt.storeDomain, tt.target)
			if err := validateRegistry(registry, validationDate()); err != nil {
				t.Fatalf("ValidateRegistry() rejected %s target: %v", tt.name, err)
			}
		})
	}
}

func TestValidateRegistryRejectsImpossibleOrIncompleteTargetSignatures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TargetRef)
		want   string
	}{
		{"unknown cardinality", func(target *TargetRef) { target.Cardinality = TargetCardinality("some") }, `unknown target cardinality "some"`},
		{"unknown identity", func(target *TargetRef) { target.Identity = TargetIdentityKind("guess") }, `unknown target identity "guess"`},
		{"unknown signature", func(target *TargetRef) { target.Signature = TargetSignatureKind("opaque") }, `unknown target signature "opaque"`},
		{"missing identities", func(target *TargetRef) { target.Identities = nil }, "target identities are required"},
		{"wrong cardinality", func(target *TargetRef) { target.Cardinality = TargetCardinalitySet }, `signature "direct" does not allow target cardinality "set"`},
		{"wrong identity kind", func(target *TargetRef) { target.Identity = TargetIdentityGenerated }, `signature "direct" requires target identity "existing"`},
		{"wrong target kind", func(target *TargetRef) { target.Kind = TargetControllerChannel }, `signature "direct" does not allow target kind "controller-channel"`},
		{"duplicate identity role", func(target *TargetRef) { target.Identities = append(target.Identities, target.Identities[0]) }, `duplicate target identity role "primary"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			tt.mutate(&firstRoute(&registry).Target)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsMalformedGeneratedAndSpecialTargets(t *testing.T) {
	t.Run("generated identity without result projection", func(t *testing.T) {
		registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainSessionLifecycle, TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityGenerated,
			Signature:   TargetSignatureCreate,
			Identities: []TargetIdentityRef{
				{Role: TargetRoleInput, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue},
				{Role: TargetRoleGenerated, BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1}, Source: TargetSourceBoundaryValue},
			},
			Detail: "generated record",
		})
		assertErrorContains(t, validateRegistry(registry, validationDate()), "generated target identity projection is required")
	})

	t.Run("dependency edge missing destination", func(t *testing.T) {
		registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainControlDispatch, TargetRef{
			Kind:        TargetDurableDependencyEdge,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityComposite,
			Signature:   TargetSignatureDependencyEdge,
			Identities: []TargetIdentityRef{{
				Role: TargetRoleFrom, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue,
			}},
			Detail: "incomplete edge",
		})
		assertErrorContains(t, validateRegistry(registry, validationDate()), `signature "dependency-edge" requires target identity roles [from to]`)
	})

	t.Run("channel uses payload instead of channel identity", func(t *testing.T) {
		registry := registryForTarget(KindWakeSource, AccessDirectWake, "", TargetRef{
			Kind:        TargetControllerChannel,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentitySingleton,
			Signature:   TargetSignatureChannel,
			Identities: []TargetIdentityRef{{
				Role: TargetRolePrimary, BoundarySlot: ValueSlot{Kind: SlotChannelElement}, Source: TargetSourceBoundaryValue,
			}},
			Detail: "payload is not the channel identity",
		})
		assertErrorContains(t, validateRegistry(registry, validationDate()), "channel target identity must use the registered boundary object")
	})
}

func TestValidateRegistryRejectsImpossibleDirectAndBatchTargetSlots(t *testing.T) {
	t.Run("direct result cannot identify a pre-existing mutation target", func(t *testing.T) {
		registry := validRegistry()
		firstRoute(&registry).Target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotResult, Index: 1}
		assertErrorContains(
			t,
			validateRegistry(registry, validationDate()),
			"direct target identity must use a receiver or parameter slot",
		)
	})

	t.Run("batch receiver cannot identify the addressed record collection", func(t *testing.T) {
		target := batchTarget(TargetCardinalitySet)
		target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotReceiver}
		registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainMaintenance, target)
		assertErrorContains(
			t,
			validateRegistry(registry, validationDate()),
			"batch target identity must use a parameter slot",
		)
	})
}

func TestValidateRegistryAcceptsPermanentStoreAdapterWithoutException(t *testing.T) {
	registry := validRegistry()
	route := firstRoute(&registry)
	route.AccessPath = AccessStoreAdapter
	route.Fences = []Fence{{Kind: FenceNone}}
	route.Disposition = Disposition{Kind: DispositionRetainBoundary, Reason: "permanent policy or capability adapter"}
	route.Exception = nil

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected permanent store adapter: %v", err)
	}

	registry.Boundaries[0].Kind = KindProviderMutation
	route.StoreDomain = ""
	assertErrorContains(t, validateRegistry(registry, validationDate()), "store-adapter requires a store-mutation boundary")
}

func TestValidateRegistryAcceptsOperatorTerminalAttachTarget(t *testing.T) {
	target := TargetRef{
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
				BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
				Source:       TargetSourceBoundaryValue,
			},
		},
		Detail: "the calling operator terminal attached to one named runtime destination",
	}
	registry := registryForTarget(KindProviderMutation, AccessProviderNative, "", target)
	registry.Boundaries[0].Object = objectRef(runtimePackage, "Provider", "Attach")
	registry.Boundaries[0].Match = ObjectMatchInterfaceImplementors
	firstRoute(&registry).ActionFamily = FamilyOperatorAttach

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected operator terminal attach: %v", err)
	}
}

func TestValidateRegistryRejectsImpreciseOperatorTerminalAttach(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TargetIdentityRef)
		want   string
	}{
		{"explicit parameter masquerading as terminal", func(identity *TargetIdentityRef) {
			identity.BoundarySlot = ValueSlot{Kind: SlotParameter, Index: 1}
		}, "operator-terminal identity must use the ambient terminal slot"},
		{"ordinary boundary value masquerading as terminal", func(identity *TargetIdentityRef) {
			identity.Source = TargetSourceBoundaryValue
		}, "operator-terminal identity must use ambient-terminal provenance"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := operatorAttachTarget()
			tt.mutate(&target.Identities[0])
			registry := registryForTarget(KindProviderMutation, AccessProviderNative, "", target)
			firstRoute(&registry).ActionFamily = FamilyOperatorAttach
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryBindsOperatorAttachFamilyToItsTarget(t *testing.T) {
	t.Run("attach target with another family", func(t *testing.T) {
		registry := registryForTarget(KindProviderMutation, AccessProviderNative, "", operatorAttachTarget())
		assertErrorContains(t, validateRegistry(registry, validationDate()), `operator-terminal-attach target requires action family "operator-terminal-attach"`)
	})

	t.Run("attach family with another target", func(t *testing.T) {
		registry := validRegistry()
		firstRoute(&registry).ActionFamily = FamilyOperatorAttach
		assertErrorContains(t, validateRegistry(registry, validationDate()), `action family "operator-terminal-attach" requires an operator-terminal-attach target`)
	})
}

func TestTargetDiagnosticsAndCompilationIgnoreIdentityInputOrder(t *testing.T) {
	makeRegistry := func() Registry {
		registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainSessionLifecycle, TargetRef{
			Kind:        TargetDurableRecord,
			Cardinality: TargetCardinalityOne,
			Identity:    TargetIdentityGenerated,
			Signature:   TargetSignatureCreate,
			Identities: []TargetIdentityRef{
				{
					Role:         TargetRoleInput,
					BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
					Source:       TargetSourceBoundaryValue,
				},
				{
					Role:         TargetRoleGenerated,
					BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
					Projection:   objectRef(beadsPackage, "Bead", "ID"),
					Source:       TargetSourceBoundaryValue,
				},
			},
			Detail: "one generated durable record",
		})
		return registry
	}

	forward := makeRegistry()
	reversed := makeRegistry()
	reverse(firstRoute(&reversed).Target.Identities)
	forwardCompiled, err := CompileRegistry(forward, discoveryForRegistry(forward), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry(forward) failed: %v", err)
	}
	reversedCompiled, err := CompileRegistry(reversed, discoveryForRegistry(reversed), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry(reversed) failed: %v", err)
	}
	if !reflect.DeepEqual(forwardCompiled, reversedCompiled) {
		t.Fatalf("compiled registry changed with target identity order\nforward: %#v\nreverse: %#v", forwardCompiled, reversedCompiled)
	}

	firstRoute(&forward).Target.Identities[0].Role = TargetRoleFrom
	got := forwardCompiled.Registrations[0].Cases[0].Routes[0].Definition.Target.Identities
	if got[0].Role == TargetRoleFrom || got[1].Role == TargetRoleFrom {
		t.Fatal("compiled registry retained mutable target identity input")
	}

	invalidForward := makeRegistry()
	invalidForwardTarget := &firstRoute(&invalidForward).Target
	invalidForwardTarget.Identities[0].BoundarySlot = ValueSlot{Kind: SlotParameter}
	invalidForwardTarget.Identities[1].Projection = ObjectRef{}
	invalidReverse := makeRegistry()
	invalidReverseTarget := &firstRoute(&invalidReverse).Target
	invalidReverseTarget.Identities[0].BoundarySlot = ValueSlot{Kind: SlotParameter}
	invalidReverseTarget.Identities[1].Projection = ObjectRef{}
	reverse(invalidReverseTarget.Identities)
	forwardErr := validateRegistry(invalidForward, validationDate())
	reverseErr := validateRegistry(invalidReverse, validationDate())
	if forwardErr == nil || reverseErr == nil {
		t.Fatalf("invalid target validation errors = (%v, %v), want both non-nil", forwardErr, reverseErr)
	}
	if forwardErr.Error() != reverseErr.Error() {
		t.Fatalf("target diagnostics changed with identity order\nforward:\n%s\nreverse:\n%s", forwardErr, reverseErr)
	}
}

func TestStoreDomainIsAuthoredOnlyOnLogicalRoute(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(BoundaryDefinition{}),
		reflect.TypeOf(SiteRegistration{}),
		reflect.TypeOf(ProfileCase{}),
		reflect.TypeOf(Route{}),
		reflect.TypeOf(TargetRef{}),
	}
	for _, typ := range types {
		_, found := typ.FieldByName("StoreDomain")
		if found != (typ == reflect.TypeOf(Route{})) {
			t.Fatalf("%s StoreDomain presence = %t, want %t", typ.Name(), found, typ == reflect.TypeOf(Route{}))
		}
	}
}

func TestTargetDetailIsOptionalAndExcludedFromRouteIdentity(t *testing.T) {
	withDetail := validRegistry()
	withDetailID := compiledFixtureRouteID(t, withDetail)

	withoutDetail := validRegistry()
	firstRoute(&withoutDetail).Target.Detail = ""
	withoutDetailID := compiledFixtureRouteID(t, withoutDetail)
	if withoutDetailID != withDetailID {
		t.Fatalf("route ID changed with non-semantic target detail: with=%q without=%q", withDetailID, withoutDetailID)
	}
}

func TestValidateRegistryAcceptsCallbackDefinedStoreTransactionTarget(t *testing.T) {
	registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainMaintenance, transactionTarget())
	setStoreBoundary(&registry, "beads.store.Tx", "Tx")

	if err := validateRegistry(registry, validationDate()); err != nil {
		t.Fatalf("ValidateRegistry() rejected callback-defined transaction target: %v", err)
	}
}

func TestKnownStoreBoundariesRequireHonestTargetShape(t *testing.T) {
	tests := []struct {
		name        string
		boundaryID  string
		method      string
		target      TargetRef
		wrongTarget TargetRef
		want        string
	}{
		{
			name:        "Create",
			boundaryID:  "beads.writer.Create",
			method:      "Create",
			target:      singleCreateTarget(),
			wrongTarget: batchTarget(TargetCardinalityOne),
			want:        `boundary "beads.writer.Create" requires target signature "single-create"`,
		},
		{
			name:        "CreateWithStorage",
			boundaryID:  "beads.storage-create.CreateWithStorage",
			method:      "CreateWithStorage",
			target:      singleCreateTarget(),
			wrongTarget: batchTarget(TargetCardinalityOne),
			want:        `boundary "beads.storage-create.CreateWithStorage" requires target signature "single-create"`,
		},
		{
			name:        "SetMetadataBatch has one target",
			boundaryID:  "beads.writer.SetMetadataBatch",
			method:      "SetMetadataBatch",
			target:      batchTarget(TargetCardinalityOne),
			wrongTarget: batchTarget(TargetCardinalitySet),
			want:        `boundary "beads.writer.SetMetadataBatch" requires target cardinality "one"`,
		},
		{
			name:        "CloseAll has a target set",
			boundaryID:  "beads.writer.CloseAll",
			method:      "CloseAll",
			target:      batchTarget(TargetCardinalitySet),
			wrongTarget: batchTarget(TargetCardinalityOne),
			want:        `boundary "beads.writer.CloseAll" requires target cardinality "set"`,
		},
		{
			name:        "DeleteBatch has a target set",
			boundaryID:  "beads.batch-delete.DeleteBatch",
			method:      "DeleteBatch",
			target:      batchTarget(TargetCardinalitySet),
			wrongTarget: batchTarget(TargetCardinalityOne),
			want:        `boundary "beads.batch-delete.DeleteBatch" requires target cardinality "set"`,
		},
		{
			name:        "ApplyGraphPlan",
			boundaryID:  "beads.graph-apply.ApplyGraphPlan",
			method:      "ApplyGraphPlan",
			target:      graphPlanTarget(),
			wrongTarget: singleCreateTarget(),
			want:        `boundary "beads.graph-apply.ApplyGraphPlan" requires target signature "graph-plan"`,
		},
		{
			name:        "ApplyGraphPlanWithStorage",
			boundaryID:  "beads.storage-graph-apply.ApplyGraphPlanWithStorage",
			method:      "ApplyGraphPlanWithStorage",
			target:      graphPlanTarget(),
			wrongTarget: singleCreateTarget(),
			want:        `boundary "beads.storage-graph-apply.ApplyGraphPlanWithStorage" requires target signature "graph-plan"`,
		},
		{
			name:        "DepAdd",
			boundaryID:  "beads.writer.DepAdd",
			method:      "DepAdd",
			target:      dependencyEdgeTarget(),
			wrongTarget: compileFixtureTarget(),
			want:        `boundary "beads.writer.DepAdd" requires target signature "dependency-edge"`,
		},
		{
			name:        "DepRemove",
			boundaryID:  "beads.writer.DepRemove",
			method:      "DepRemove",
			target:      dependencyEdgeTarget(),
			wrongTarget: compileFixtureTarget(),
			want:        `boundary "beads.writer.DepRemove" requires target signature "dependency-edge"`,
		},
		{
			name:        "Tx",
			boundaryID:  "beads.store.Tx",
			method:      "Tx",
			target:      transactionTarget(),
			wrongTarget: batchTarget(TargetCardinalitySet),
			want:        `boundary "beads.store.Tx" requires target signature "transaction"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" accepts exact shape", func(t *testing.T) {
			registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainMaintenance, tt.target)
			setStoreBoundary(&registry, tt.boundaryID, tt.method)
			if err := validateRegistry(registry, validationDate()); err != nil {
				t.Fatalf("ValidateRegistry() rejected exact target shape: %v", err)
			}
		})
		t.Run(tt.name+" rejects false shape", func(t *testing.T) {
			registry := registryForTarget(KindStoreMutation, AccessStoreAdapter, StoreDomainMaintenance, tt.wrongTarget)
			setStoreBoundary(&registry, tt.boundaryID, tt.method)
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func batchTarget(cardinality TargetCardinality) TargetRef {
	return TargetRef{
		Kind:        TargetDurableRecord,
		Cardinality: cardinality,
		Identity:    TargetIdentityExisting,
		Signature:   TargetSignatureBatch,
		Identities: []TargetIdentityRef{{
			Role:         TargetRolePrimary,
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
			Source:       TargetSourceBoundaryValue,
		}},
		Detail: "an explicitly addressed record collection mutated by one boundary call",
	}
}

func operatorAttachTarget() TargetRef {
	return TargetRef{
		Kind:        TargetOperatorTerminal,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityComposite,
		Signature:   TargetSignatureOperatorAttach,
		Identities: []TargetIdentityRef{
			{Role: TargetRoleOperatorTerminal, BoundarySlot: ValueSlot{Kind: SlotAmbientTerminal}, Source: TargetSourceAmbientTerminal},
			{Role: TargetRoleDestination, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue},
		},
		Detail: "the calling operator terminal attached to one named runtime destination",
	}
}

func singleCreateTarget() TargetRef {
	return TargetRef{
		Kind:        TargetDurableRecord,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityGenerated,
		Signature:   TargetSignatureCreate,
		Identities: []TargetIdentityRef{
			{Role: TargetRoleInput, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue},
			{
				Role:         TargetRoleGenerated,
				BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
				Projection:   objectRef(beadsPackage, "Bead", "ID"),
				Source:       TargetSourceBoundaryValue,
			},
		},
		Detail: "one durable record whose ID is assigned by the store",
	}
}

func graphPlanTarget() TargetRef {
	return TargetRef{
		Kind:        TargetDurableGraph,
		Cardinality: TargetCardinalityPlan,
		Identity:    TargetIdentitySymbolicGenerated,
		Signature:   TargetSignatureGraphPlan,
		Identities: []TargetIdentityRef{
			{Role: TargetRolePlan, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 2}, Source: TargetSourceBoundaryValue},
			{
				Role:         TargetRoleGenerated,
				BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
				Projection:   objectRef(beadsPackage, "GraphApplyResult", "IDs"),
				Source:       TargetSourceBoundaryValue,
			},
		},
		Detail: "plan node keys map to store-generated durable IDs",
	}
}

func dependencyEdgeTarget() TargetRef {
	return TargetRef{
		Kind:        TargetDurableDependencyEdge,
		Cardinality: TargetCardinalityOne,
		Identity:    TargetIdentityComposite,
		Signature:   TargetSignatureDependencyEdge,
		Identities: []TargetIdentityRef{
			{Role: TargetRoleFrom, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1}, Source: TargetSourceBoundaryValue},
			{Role: TargetRoleTo, BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 2}, Source: TargetSourceBoundaryValue},
		},
		Detail: "one ordered dependency edge identified by both durable endpoints",
	}
}

func transactionTarget() TargetRef {
	return TargetRef{
		Kind:        TargetDurableTransaction,
		Cardinality: TargetCardinalityCallback,
		Identity:    TargetIdentityCallbackEffects,
		Signature:   TargetSignatureTransaction,
		Identities: []TargetIdentityRef{{
			Role:         TargetRoleCallback,
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 2},
			Source:       TargetSourceBoundaryValue,
		}},
		Detail: "the durable effects selected by the transaction callback",
	}
}

func setStoreBoundary(registry *Registry, id, method string) {
	registry.Boundaries[0].ID = id
	registry.Boundaries[0].Object.Name = method
	registry.Registrations[0].BoundaryID = id
}

func registryForTarget(kind EffectKind, access AccessPath, domain StoreDomain, target TargetRef) Registry {
	registry := validRegistry()
	registry.Boundaries[0].Kind = kind
	route := firstRoute(&registry)
	route.StoreDomain = domain
	route.Target = target
	route.AccessPath = access
	route.Fences = []Fence{{Kind: FenceNone}}
	route.Disposition = Disposition{Kind: DispositionRetainBoundary, Reason: "permanent classified boundary"}
	route.Exception = nil
	return registry
}
