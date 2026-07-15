package effectinventory

import (
	"fmt"
	"sort"
)

type targetSignaturePolicy struct {
	Cardinalities []TargetCardinality
	Identity      TargetIdentityKind
	Kinds         []TargetKind
	Effects       []EffectKind
	Roles         []TargetIdentityRole
}

type targetBoundaryRequirement struct {
	Signature   TargetSignatureKind
	Cardinality TargetCardinality
}

func cloneTarget(target TargetRef) TargetRef {
	clone := target
	clone.Identities = append([]TargetIdentityRef(nil), target.Identities...)
	sort.Slice(clone.Identities, func(i, j int) bool {
		return canonicalTargetIdentityRef(clone.Identities[i]) < canonicalTargetIdentityRef(clone.Identities[j])
	})
	return clone
}

func canonicalTargetRef(target TargetRef) string {
	identities := make([]string, len(target.Identities))
	for index, identity := range target.Identities {
		identities[index] = canonicalTargetIdentityRef(identity)
	}
	sort.Strings(identities)
	return canonicalFields(
		"target-v2",
		string(target.Kind),
		string(target.Cardinality),
		string(target.Identity),
		string(target.Signature),
		canonicalStringList("target-identities-v1", identities),
	)
}

func canonicalTargetIdentityRef(identity TargetIdentityRef) string {
	return canonicalFields(
		"target-identity-v1",
		string(identity.Role),
		canonicalValueSlot(identity.BoundarySlot),
		canonicalObjectRef(identity.Projection),
		string(identity.Source),
		canonicalObjectRef(identity.SourceObject),
		canonicalValueSlot(identity.SourceSlot),
	)
}

func validateTargetActionFamily(target TargetRef, family ActionFamily, scope string, problems *[]string) {
	if target.Signature == TargetSignatureOperatorAttach && family != FamilyOperatorAttach {
		addProblem(problems, scope, "operator-terminal-attach target requires action family %q", FamilyOperatorAttach)
	}
	if family == FamilyOperatorAttach && target.Signature != TargetSignatureOperatorAttach {
		addProblem(problems, scope, "action family %q requires an operator-terminal-attach target", FamilyOperatorAttach)
	}
}

func validateKnownBoundaryTarget(boundary BoundaryDefinition, target TargetRef, scope string, problems *[]string) {
	requirement, ok := targetRequirementForBoundary(boundary.ID)
	if !ok {
		return
	}
	if target.Signature != requirement.Signature {
		addProblem(problems, scope, "boundary %q requires target signature %q", boundary.ID, requirement.Signature)
	}
	if target.Cardinality != requirement.Cardinality {
		addProblem(problems, scope, "boundary %q requires target cardinality %q", boundary.ID, requirement.Cardinality)
	}
}

func targetRequirementForBoundary(boundaryID string) (targetBoundaryRequirement, bool) {
	switch boundaryID {
	case "beads.writer.Create", "beads.storage-create.CreateWithStorage":
		return targetBoundaryRequirement{Signature: TargetSignatureCreate, Cardinality: TargetCardinalityOne}, true
	case "beads.writer.SetMetadataBatch":
		return targetBoundaryRequirement{Signature: TargetSignatureBatch, Cardinality: TargetCardinalityOne}, true
	case "beads.writer.CloseAll", "beads.batch-delete.DeleteBatch":
		return targetBoundaryRequirement{Signature: TargetSignatureBatch, Cardinality: TargetCardinalitySet}, true
	case "beads.graph-apply.ApplyGraphPlan", "beads.storage-graph-apply.ApplyGraphPlanWithStorage":
		return targetBoundaryRequirement{Signature: TargetSignatureGraphPlan, Cardinality: TargetCardinalityPlan}, true
	case "beads.writer.DepAdd", "beads.writer.DepRemove":
		return targetBoundaryRequirement{Signature: TargetSignatureDependencyEdge, Cardinality: TargetCardinalityOne}, true
	case "beads.store.Tx":
		return targetBoundaryRequirement{Signature: TargetSignatureTransaction, Cardinality: TargetCardinalityCallback}, true
	default:
		return targetBoundaryRequirement{}, false
	}
}

func validateTarget(target TargetRef, effect EffectKind, scope string, problems *[]string) {
	if !knownTargetKind(target.Kind) {
		addProblem(problems, scope, "unknown target kind %q", target.Kind)
	}
	if !knownTargetCardinality(target.Cardinality) {
		addProblem(problems, scope, "unknown target cardinality %q", target.Cardinality)
	}
	if !knownTargetIdentity(target.Identity) {
		addProblem(problems, scope, "unknown target identity %q", target.Identity)
	}
	policy, knownSignature := targetPolicyFor(target.Signature)
	if !knownSignature {
		addProblem(problems, scope, "unknown target signature %q", target.Signature)
	} else {
		if !contains(policy.Cardinalities, target.Cardinality) {
			addProblem(problems, scope, "signature %q does not allow target cardinality %q", target.Signature, target.Cardinality)
		}
		if target.Identity != policy.Identity {
			addProblem(problems, scope, "signature %q requires target identity %q", target.Signature, policy.Identity)
		}
		if !contains(policy.Kinds, target.Kind) {
			addProblem(problems, scope, "signature %q does not allow target kind %q", target.Signature, target.Kind)
		}
		if effect != "" && !contains(policy.Effects, effect) {
			addProblem(problems, scope, "signature %q does not allow effect kind %q", target.Signature, effect)
		}
	}
	if len(target.Identities) == 0 {
		addProblem(problems, scope, "target identities are required")
		if knownSignature && len(policy.Roles) != 0 {
			addProblem(problems, scope, "signature %q requires target identity roles %v", target.Signature, policy.Roles)
		}
		return
	}

	identities := append([]TargetIdentityRef(nil), target.Identities...)
	sort.Slice(identities, func(i, j int) bool {
		return canonicalTargetIdentityRef(identities[i]) < canonicalTargetIdentityRef(identities[j])
	})
	roles := make([]TargetIdentityRole, 0, len(identities))
	roleCounts := make(map[TargetIdentityRole]int, len(identities))
	for _, identity := range identities {
		roleCounts[identity.Role]++
		if roleCounts[identity.Role] == 2 {
			addProblem(problems, scope, "duplicate target identity role %q", identity.Role)
		}
		roles = append(roles, identity.Role)
		validateTargetIdentityRef(identity, target.Signature, scope, problems)
	}
	if knownSignature && !equalTargetRoleSets(roles, policy.Roles) {
		addProblem(problems, scope, "signature %q requires target identity roles %v", target.Signature, policy.Roles)
	}
}

func validateTargetIdentityRef(identity TargetIdentityRef, signature TargetSignatureKind, scope string, problems *[]string) {
	identityScope := fmt.Sprintf("%s target identity %q", scope, identity.Role)
	if !knownTargetIdentityRole(identity.Role) {
		addProblem(problems, identityScope, "unknown target identity role %q", identity.Role)
	}
	validateTargetBoundarySlot(identity.BoundarySlot, identityScope+" boundary", problems)
	if !identity.Projection.zero() {
		validateObject(identity.Projection, identityScope+" projection", problems)
	}
	if !knownTargetSource(identity.Source) {
		addProblem(problems, identityScope, "unknown target source %q", identity.Source)
		return
	}
	requiresObject := !oneOf(identity.Source, TargetSourceBoundaryValue, TargetSourceAmbientTerminal)
	if requiresObject {
		validateObject(identity.SourceObject, identityScope+" source object", problems)
	} else if !identity.SourceObject.zero() {
		addProblem(problems, identityScope, "target source %q cannot name an object", identity.Source)
	}
	switch identity.Source {
	case TargetSourceBoundaryValue, TargetSourceObjectField, TargetSourceConfigValue, TargetSourceConstant, TargetSourceAmbientTerminal:
		if !identity.SourceSlot.zero() {
			addProblem(problems, identityScope, "target source %q cannot name a source slot", identity.Source)
		}
	case TargetSourceFunctionResult, TargetSourceStoreLiveReread, TargetSourceProcessScan:
		validateExactSlot(identity.SourceSlot, SlotResult, identityScope+" source", problems)
	case TargetSourceChannelPayload:
		validateExactSlot(identity.SourceSlot, SlotChannelElement, identityScope+" source", problems)
	}

	switch identity.Role {
	case TargetRoleGenerated:
		if identity.BoundarySlot.Kind != SlotResult {
			addProblem(problems, identityScope, "generated target identity must use a result slot")
		}
		if identity.Projection.zero() {
			addProblem(problems, identityScope, "generated target identity projection is required")
		}
		if identity.Source != TargetSourceBoundaryValue {
			addProblem(problems, identityScope, "generated target identity must use boundary-value provenance")
		}
	case TargetRoleInput, TargetRolePlan, TargetRoleFrom, TargetRoleTo, TargetRoleDestination, TargetRoleCallback:
		if identity.BoundarySlot.Kind != SlotParameter {
			addProblem(problems, identityScope, "%s target identity must use a parameter slot", identity.Role)
		}
	case TargetRoleOperatorTerminal:
		if identity.BoundarySlot.Kind != SlotAmbientTerminal {
			addProblem(problems, identityScope, "operator-terminal identity must use the ambient terminal slot")
		}
		if identity.Source != TargetSourceAmbientTerminal {
			addProblem(problems, identityScope, "operator-terminal identity must use ambient-terminal provenance")
		}
		if !identity.Projection.zero() {
			addProblem(problems, identityScope, "operator-terminal identity cannot name a projection")
		}
	}

	switch signature {
	case TargetSignatureDirect:
		if identity.Role == TargetRolePrimary && !oneOf(identity.BoundarySlot.Kind, SlotReceiver, SlotParameter) {
			addProblem(problems, identityScope, "direct target identity must use a receiver or parameter slot")
		}
	case TargetSignatureBatch:
		if identity.Role == TargetRolePrimary && identity.BoundarySlot.Kind != SlotParameter {
			addProblem(problems, identityScope, "batch target identity must use a parameter slot")
		}
	case TargetSignatureChannel:
		if identity.Role == TargetRolePrimary && identity.BoundarySlot.Kind != SlotBoundaryObject {
			addProblem(problems, identityScope, "channel target identity must use the registered boundary object")
		}
	case TargetSignatureWakeSource:
		if identity.Role == TargetRolePrimary && !oneOf(identity.BoundarySlot.Kind, SlotBoundaryObject, SlotResult) {
			addProblem(problems, identityScope, "wake-source target identity must use the registered boundary object or a result slot")
		}
		if identity.Role == TargetRolePrimary && identity.Source != TargetSourceBoundaryValue {
			addProblem(problems, identityScope, "wake-source target identity must use boundary-value provenance")
		}
		if identity.Role == TargetRolePrimary && !identity.Projection.zero() {
			addProblem(problems, identityScope, "wake-source target identity cannot name a projection")
		}
	case TargetSignatureProcess:
		if identity.Role == TargetRolePrimary && !oneOf(identity.BoundarySlot.Kind, SlotReceiver, SlotParameter) {
			addProblem(problems, identityScope, "process target identity must use a receiver or parameter slot")
		}
	case TargetSignatureEventAppend:
		if identity.Role == TargetRolePrimary && identity.BoundarySlot.Kind != SlotReceiver {
			addProblem(problems, identityScope, "event append target must identify the recorder receiver")
		}
	}
}

func targetPolicyFor(signature TargetSignatureKind) (targetSignaturePolicy, bool) {
	switch signature {
	case TargetSignatureDirect:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityExisting,
			Kinds:         []TargetKind{TargetDurableRecord, TargetSessionIdentity, TargetRuntimeIdentity, TargetProviderServer},
			Effects:       []EffectKind{KindStoreMutation, KindProviderMutation},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureCreate:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityGenerated,
			Kinds:         []TargetKind{TargetDurableRecord},
			Effects:       []EffectKind{KindStoreMutation},
			Roles:         []TargetIdentityRole{TargetRoleInput, TargetRoleGenerated},
		}, true
	case TargetSignatureBatch:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne, TargetCardinalitySet},
			Identity:      TargetIdentityExisting,
			Kinds:         []TargetKind{TargetDurableRecord},
			Effects:       []EffectKind{KindStoreMutation},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureGraphPlan:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityPlan},
			Identity:      TargetIdentitySymbolicGenerated,
			Kinds:         []TargetKind{TargetDurableGraph},
			Effects:       []EffectKind{KindStoreMutation},
			Roles:         []TargetIdentityRole{TargetRolePlan, TargetRoleGenerated},
		}, true
	case TargetSignatureDependencyEdge:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityComposite,
			Kinds:         []TargetKind{TargetDurableDependencyEdge},
			Effects:       []EffectKind{KindStoreMutation},
			Roles:         []TargetIdentityRole{TargetRoleFrom, TargetRoleTo},
		}, true
	case TargetSignatureChannel:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentitySingleton,
			Kinds:         []TargetKind{TargetControllerChannel},
			Effects:       []EffectKind{KindWakeSource},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureWakeSource:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityExisting,
			Kinds:         []TargetKind{TargetWakeSource},
			Effects:       []EffectKind{KindWakeSource},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureProcess:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne, TargetCardinalitySet},
			Identity:      TargetIdentityExisting,
			Kinds:         []TargetKind{TargetProcessIdentity, TargetProviderServer},
			Effects:       []EffectKind{KindProcessMutation},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureEventAppend:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityAppendRecord,
			Kinds:         []TargetKind{TargetEventLog},
			Effects:       []EffectKind{KindEventEmission},
			Roles:         []TargetIdentityRole{TargetRolePrimary},
		}, true
	case TargetSignatureOperatorAttach:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityOne},
			Identity:      TargetIdentityComposite,
			Kinds:         []TargetKind{TargetOperatorTerminal},
			Effects:       []EffectKind{KindProviderMutation},
			Roles:         []TargetIdentityRole{TargetRoleOperatorTerminal, TargetRoleDestination},
		}, true
	case TargetSignatureTransaction:
		return targetSignaturePolicy{
			Cardinalities: []TargetCardinality{TargetCardinalityCallback},
			Identity:      TargetIdentityCallbackEffects,
			Kinds:         []TargetKind{TargetDurableTransaction},
			Effects:       []EffectKind{KindStoreMutation},
			Roles:         []TargetIdentityRole{TargetRoleCallback},
		}, true
	default:
		return targetSignaturePolicy{}, false
	}
}

func validateTargetBoundarySlot(slot ValueSlot, scope string, problems *[]string) {
	if !oneOf(slot.Kind, SlotReceiver, SlotParameter, SlotResult, SlotChannelElement, SlotBoundaryObject, SlotAmbientTerminal) {
		addProblem(problems, scope, "invalid target boundary slot %q", slot.Kind)
		return
	}
	validateSlotIndex(slot, scope, problems)
}

func knownTargetKind(value TargetKind) bool {
	return oneOf(value,
		TargetDurableRecord, TargetDurableGraph, TargetDurableDependencyEdge,
		TargetDurableTransaction,
		TargetSessionIdentity, TargetRuntimeIdentity, TargetProcessIdentity,
		TargetProviderServer, TargetEventLog, TargetControllerChannel, TargetWakeSource,
		TargetOperatorTerminal,
	)
}

func knownTargetCardinality(value TargetCardinality) bool {
	return oneOf(value, TargetCardinalityOne, TargetCardinalitySet, TargetCardinalityPlan, TargetCardinalityCallback)
}

func knownTargetIdentity(value TargetIdentityKind) bool {
	return oneOf(value,
		TargetIdentityExisting, TargetIdentityGenerated,
		TargetIdentitySymbolicGenerated, TargetIdentityComposite,
		TargetIdentitySingleton, TargetIdentityAppendRecord,
		TargetIdentityCallbackEffects,
	)
}

func knownTargetIdentityRole(value TargetIdentityRole) bool {
	return oneOf(value,
		TargetRolePrimary, TargetRoleInput, TargetRoleGenerated, TargetRolePlan,
		TargetRoleFrom, TargetRoleTo, TargetRoleOperatorTerminal,
		TargetRoleDestination, TargetRoleCallback,
	)
}

func knownTargetSource(value TargetSourceKind) bool {
	return oneOf(value,
		TargetSourceBoundaryValue, TargetSourceObjectField,
		TargetSourceFunctionResult, TargetSourceStoreLiveReread,
		TargetSourceProcessScan, TargetSourceChannelPayload,
		TargetSourceConfigValue, TargetSourceConstant,
		TargetSourceAmbientTerminal,
	)
}

func equalTargetRoleSets(left, right []TargetIdentityRole) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[TargetIdentityRole]int, len(left))
	for _, role := range left {
		want[role]++
	}
	for _, role := range right {
		want[role]--
		if want[role] < 0 {
			return false
		}
	}
	return true
}
