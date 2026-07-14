package effectinventory

import (
	"fmt"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

type targetGateRouteChain struct {
	functions map[*ssa.Function]bool
	connected bool
}

// validateTargetGateEvidenceForProfile proves the authored target and gate
// claims active in analysis.profile against that profile's loaded go/types and
// SSA identities. Structural registry policy remains in CompileRegistry; this
// validator binds the structurally valid references to real program objects.
func validateTargetGateEvidenceForProfile(analysis *loadedAnalysis, registry Registry) error {
	if analysis == nil {
		return fmt.Errorf("effect target/gate evidence failed: loaded analysis is required")
	}
	profile := analysis.profile.ID
	resolved, err := resolveBoundaries(analysis.packages, registry.Boundaries)
	if err != nil {
		return fmt.Errorf("effect target/gate evidence failed for profile %q: %w", profile, err)
	}
	boundaries := make(map[string]resolvedBoundary, len(resolved))
	for _, boundary := range resolved {
		boundaries[boundary.definition.ID] = boundary
	}
	functionIndex, problems := newRouteHopFunctionIndex(analysis)

	for _, registration := range registry.Registrations {
		registrationID := deriveSiteRegistrationID(registration.BoundaryID, registration.Matcher)
		registrationScope := fmt.Sprintf("registration %q", registrationID)
		boundary, boundaryExists := boundaries[registration.BoundaryID]
		if !boundaryExists {
			problems = append(problems, fmt.Sprintf(
				"%s: boundary %q is missing from the loaded registry",
				registrationScope,
				registration.BoundaryID,
			))
		}

		activeCases := 0
		for _, profileCase := range registration.Cases {
			memberships := routeHopProfileMemberships(profileCase.BuildProfiles, profile)
			if memberships == 0 {
				continue
			}
			activeCases++
			caseScope := fmt.Sprintf("%s case[%s]", registrationScope, canonicalProfileKey(profileCase.BuildProfiles))
			if memberships != 1 {
				problems = append(problems, fmt.Sprintf("%s: profile %q appears %d times", caseScope, profile, memberships))
			}
			for _, route := range profileCase.Routes {
				routeID := deriveRouteID(registrationID, profileCase.BuildProfiles, route)
				routeScope := fmt.Sprintf("%s route %q", caseScope, routeID)
				chain := resolveTargetGateRouteChain(functionIndex, registration.Matcher, route, routeScope, &problems)
				if boundaryExists {
					validateTargetEvidence(analysis, boundary, route.Target, routeScope, &problems)
				}
				validateGateEvidence(analysis, functionIndex, chain, route.CurrentGate, routeScope, &problems)
			}
		}
		if activeCases > 1 {
			problems = append(problems, fmt.Sprintf(
				"%s: profile has %d active classification cases for %q",
				registrationScope,
				activeCases,
				profile,
			))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	problems = compactStrings(problems)
	return fmt.Errorf("effect target/gate evidence failed for profile %q:\n- %s", profile, strings.Join(problems, "\n- "))
}

func resolveTargetGateRouteChain(index *routeHopFunctionIndex, matcher OperationSite, route Route, scope string, problems *[]string) targetGateRouteChain {
	// This establishes exact SSA identity connectivity for gate membership.
	// validateRouteHopEvidenceForProfile separately proves that every authored
	// adjacency is a real call/go/defer edge in the same loaded profile.
	chain := targetGateRouteChain{functions: make(map[*ssa.Function]bool)}
	logical := index.resolve(route.LogicalOwner, scope+" route chain logical owner", problems)
	physical := index.resolve(matcher.Enclosing, scope+" route chain physical owner", problems)

	if len(route.Hops) == 0 {
		chain.connected = logical != nil && physical != nil && sameRouteHopFunction(logical, physical)
		if chain.connected {
			chain.add(logical)
		}
		return chain
	}

	enclosing := make([]*ssa.Function, len(route.Hops))
	callees := make([]*ssa.Function, len(route.Hops))
	complete := logical != nil && physical != nil
	for hopIndex, hop := range route.Hops {
		hopScope := fmt.Sprintf("%s route chain hop[%d]", scope, hopIndex)
		enclosing[hopIndex] = resolveTargetGateFunction(index, hop.Site.Enclosing, hopScope+" enclosing", problems)
		callees[hopIndex] = resolveTargetGateFunction(index, hop.Callee, hopScope+" callee", problems)
		chain.add(enclosing[hopIndex])
		chain.add(callees[hopIndex])
		complete = complete && enclosing[hopIndex] != nil && callees[hopIndex] != nil
	}
	if !complete {
		return chain
	}
	connected := true
	for hopIndex := 1; hopIndex < len(route.Hops); hopIndex++ {
		connected = connected && sameRouteHopFunction(callees[hopIndex-1], enclosing[hopIndex])
	}
	connected = connected && sameRouteHopFunction(callees[len(callees)-1], physical)
	connected = connected && resolvedRouteHopContainsFunction(enclosing, callees, logical)
	chain.connected = connected
	return chain
}

func resolveTargetGateFunction(index *routeHopFunctionIndex, reference FunctionRef, scope string, problems *[]string) *ssa.Function {
	if index == nil {
		*problems = append(*problems, scope+": function index is required")
		return nil
	}
	return index.resolve(reference, scope, problems)
}

func (chain *targetGateRouteChain) add(function *ssa.Function) {
	if function != nil {
		chain.functions[routeHopFunctionOrigin(function)] = true
	}
}

func (chain targetGateRouteChain) contains(function *ssa.Function) bool {
	return function != nil && chain.functions[routeHopFunctionOrigin(function)]
}

func validateTargetEvidence(analysis *loadedAnalysis, boundary resolvedBoundary, target TargetRef, scope string, problems *[]string) {
	identities := append([]TargetIdentityRef(nil), target.Identities...)
	sort.Slice(identities, func(i, j int) bool {
		return canonicalTargetIdentityRef(identities[i]) < canonicalTargetIdentityRef(identities[j])
	})
	for _, identity := range identities {
		identityScope := fmt.Sprintf("%s target identity %q", scope, identity.Role)
		selectedType, typed := targetBoundarySlotType(boundary, identity.BoundarySlot, identityScope+" boundary", problems)
		if !identity.Projection.zero() {
			validateTargetProjection(analysis, selectedType, typed, identity.Projection, identityScope+" projection", problems)
		}
		validateTargetSourceEvidence(analysis, identity, identityScope, problems)
	}
}

func targetBoundarySlotType(boundary resolvedBoundary, slot ValueSlot, scope string, problems *[]string) (types.Type, bool) {
	var signature *types.Signature
	if boundary.function != nil {
		signature, _ = boundary.function.Type().(*types.Signature)
	}
	switch slot.Kind {
	case SlotReceiver:
		if slot.Index != 0 {
			addProblem(problems, scope, "boundary receiver slot cannot have an index")
			return nil, false
		}
		if signature == nil || signature.Recv() == nil {
			addProblem(problems, scope, "boundary receiver has no receiver slot")
			return nil, false
		}
		return signature.Recv().Type(), true
	case SlotParameter:
		if signature == nil {
			addProblem(problems, scope, "boundary object has no function signature")
			return nil, false
		}
		index := slot.Index - 1
		if slot.Index <= 0 || index >= signature.Params().Len() {
			if signature.Params().Len() == 0 {
				addProblem(problems, scope, "boundary parameter slot %d is stale; boundary has no parameters", slot.Index)
			} else {
				addProblem(problems, scope, "boundary parameter slot %d is stale; boundary has only %d parameter%s", slot.Index, signature.Params().Len(), targetGatePluralSuffix(signature.Params().Len()))
			}
			return nil, false
		}
		return signature.Params().At(index).Type(), true
	case SlotResult:
		if signature == nil {
			addProblem(problems, scope, "boundary object has no function signature")
			return nil, false
		}
		index := slot.Index - 1
		if slot.Index <= 0 || index >= signature.Results().Len() {
			if signature.Results().Len() == 0 {
				addProblem(problems, scope, "boundary result slot %d is stale; boundary has no results", slot.Index)
			} else {
				addProblem(problems, scope, "boundary result slot %d is stale; boundary has only %d result%s", slot.Index, signature.Results().Len(), targetGatePluralSuffix(signature.Results().Len()))
			}
			return nil, false
		}
		return signature.Results().At(index).Type(), true
	case SlotChannelElement:
		if slot.Index != 0 {
			addProblem(problems, scope, "boundary channel-element slot cannot have an index")
			return nil, false
		}
		channel := targetGateChannelType(boundary.channel)
		if channel == nil {
			addProblem(problems, scope, "boundary channel-element is not a channel boundary")
			return nil, false
		}
		return channel.Elem(), true
	case SlotBoundaryObject:
		if slot.Index != 0 {
			addProblem(problems, scope, "boundary object slot cannot have an index")
			return nil, false
		}
		if boundary.channel != nil {
			return boundary.channel, true
		}
		if boundary.object == nil {
			addProblem(problems, scope, "registered boundary object is missing")
			return nil, false
		}
		return boundary.object.Type(), true
	case SlotAmbientTerminal:
		if slot.Index != 0 {
			addProblem(problems, scope, "ambient terminal slot cannot have an index")
		}
		return nil, false
	default:
		addProblem(problems, scope, "boundary slot kind %q is not typed evidence", slot.Kind)
		return nil, false
	}
}

func validateTargetProjection(analysis *loadedAnalysis, selectedType types.Type, typed bool, reference ObjectRef, scope string, problems *[]string) {
	object, err := resolveTargetGateObject(analysis, reference)
	if err != nil {
		addProblem(problems, scope, "%v", err)
		return
	}
	field, ok := object.(*types.Var)
	if !ok || !field.IsField() {
		addProblem(problems, scope, "object %s is not a field", reference.key())
		return
	}
	if !typed || selectedType == nil {
		addProblem(problems, scope, "field %s cannot project an untyped boundary slot", reference.key())
		return
	}
	pkg := analysis.packages[reference.Package]
	selected, _, _ := types.LookupFieldOrMethod(selectedType, true, pkg.Types, reference.Name)
	selectedField, selectedIsField := selected.(*types.Var)
	// Origin equality admits an unambiguous promoted field and instantiated
	// generic field while still rejecting a same-named field from another type.
	if !selectedIsField || selectedField.Origin() != field.Origin() {
		addProblem(
			problems,
			scope,
			"field %s does not belong to selected slot type %s",
			reference.key(),
			targetGateTypeString(selectedType),
		)
	}
}

func validateTargetSourceEvidence(analysis *loadedAnalysis, identity TargetIdentityRef, scope string, problems *[]string) {
	sourceScope := scope + " source object"
	switch identity.Source {
	case TargetSourceBoundaryValue, TargetSourceAmbientTerminal:
		return
	case TargetSourceObjectField:
		object, err := resolveTargetGateObject(analysis, identity.SourceObject)
		if err != nil {
			addProblem(problems, sourceScope, "%v", err)
			return
		}
		field, ok := object.(*types.Var)
		if !ok || !field.IsField() {
			addProblem(problems, sourceScope, "object %s is not a field", identity.SourceObject.key())
		}
	case TargetSourceConfigValue:
		object, err := resolveTargetGateObject(analysis, identity.SourceObject)
		if err != nil {
			addProblem(problems, sourceScope, "%v", err)
			return
		}
		switch object.(type) {
		case *types.Var, *types.Const:
		default:
			addProblem(problems, sourceScope, "object %s is not a variable, field, or constant", identity.SourceObject.key())
		}
	case TargetSourceConstant:
		object, err := resolveTargetGateObject(analysis, identity.SourceObject)
		if err != nil {
			addProblem(problems, sourceScope, "%v", err)
			return
		}
		if _, ok := object.(*types.Const); !ok {
			addProblem(problems, sourceScope, "object %s is not a constant", identity.SourceObject.key())
		}
	case TargetSourceFunctionResult, TargetSourceStoreLiveReread, TargetSourceProcessScan:
		validateTargetSourceResult(analysis, identity.SourceObject, identity.SourceSlot, sourceScope, problems)
	case TargetSourceChannelPayload:
		if identity.SourceSlot.Kind != SlotChannelElement || identity.SourceSlot.Index != 0 {
			addProblem(problems, sourceScope, "source slot must be channel-element")
			return
		}
		object, err := resolveTargetGateObject(analysis, identity.SourceObject)
		if err != nil {
			addProblem(problems, sourceScope, "%v", err)
			return
		}
		if targetGateChannelType(object.Type()) == nil {
			addProblem(problems, sourceScope, "object %s does not have channel type", identity.SourceObject.key())
		}
	default:
		addProblem(problems, scope, "target source %q has no typed evidence rule", identity.Source)
	}
}

func validateTargetSourceResult(analysis *loadedAnalysis, reference ObjectRef, slot ValueSlot, scope string, problems *[]string) {
	if slot.Kind != SlotResult {
		addProblem(problems, scope, "source slot must be result")
		return
	}
	object, err := resolveTargetGateObject(analysis, reference)
	if err != nil {
		addProblem(problems, scope, "%v", err)
		return
	}
	function, ok := object.(*types.Func)
	if !ok {
		addProblem(problems, scope, "object %s is not a function or method", reference.key())
		return
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil {
		addProblem(problems, scope, "function %s has no signature", reference.key())
		return
	}
	index := slot.Index - 1
	if slot.Index <= 0 || index >= signature.Results().Len() {
		if signature.Results().Len() == 0 {
			addProblem(problems, scope, "source result slot %d is stale; function has no results", slot.Index)
		} else {
			addProblem(problems, scope, "source result slot %d is stale; function has only %d result%s", slot.Index, signature.Results().Len(), targetGatePluralSuffix(signature.Results().Len()))
		}
	}
}

func validateGateEvidence(analysis *loadedAnalysis, functions *routeHopFunctionIndex, chain targetGateRouteChain, gate GateRef, scope string, problems *[]string) {
	switch gate.Kind {
	case GateUnconditionalLegacy:
		return
	case GatePredicate:
		validateGatePredicateEvidence(analysis, gate.Predicate, gate.Expected, scope+" gate predicate", problems)
	case GateAll, GateAny:
		conditions := append([]GateCondition(nil), gate.Conditions...)
		sort.Slice(conditions, func(i, j int) bool {
			return canonicalGateCondition(conditions[i]) < canonicalGateCondition(conditions[j])
		})
		for _, condition := range conditions {
			conditionID := deriveContentID("condition-v1-", canonicalGateCondition(condition))
			conditionScope := fmt.Sprintf("%s gate condition %q", scope, conditionID)
			switch condition.Kind {
			case GateConditionPredicate:
				validateGatePredicateEvidence(analysis, condition.Predicate, condition.Expected, conditionScope+" gate predicate", problems)
			case GateConditionParameter:
				validateGateParameterEvidence(functions, chain, condition.Parameter, conditionScope, problems)
			case GateConditionCapability:
				parameterType := validateGateParameterEvidence(functions, chain, condition.Parameter, conditionScope, problems)
				validateGateCapabilityEvidence(analysis, parameterType, condition.Capability, conditionScope, problems)
			default:
				addProblem(problems, conditionScope, "gate condition %q has no typed evidence rule", condition.Kind)
			}
		}
	default:
		addProblem(problems, scope, "gate kind %q has no typed evidence rule", gate.Kind)
	}
}

func validateGateParameterEvidence(functions *routeHopFunctionIndex, chain targetGateRouteChain, parameter GateParameterRef, scope string, problems *[]string) types.Type {
	function := resolveTargetGateFunction(functions, parameter.Function, scope+" gate parameter function", problems)
	if function == nil {
		return nil
	}
	if !chain.connected {
		addProblem(problems, scope, "gate parameter function %s cannot be admitted because the authored route chain is not connected", describeRouteHopFunction(parameter.Function))
	} else if !chain.contains(function) {
		addProblem(problems, scope, "gate parameter function %s is outside the connected route chain", describeRouteHopFunction(parameter.Function))
	}
	if parameter.Slot.Kind != SlotParameter {
		addProblem(problems, scope, "gate parameter slot must be parameter")
		return nil
	}
	signature := function.Signature
	if signature == nil {
		addProblem(problems, scope, "gate parameter function %s has no signature", describeRouteHopFunction(parameter.Function))
		return nil
	}
	index := parameter.Slot.Index - 1
	if parameter.Slot.Index <= 0 || index >= signature.Params().Len() {
		if signature.Params().Len() == 0 {
			addProblem(problems, scope, "gate parameter slot %d is stale; function has no parameters", parameter.Slot.Index)
		} else {
			addProblem(problems, scope, "gate parameter slot %d is stale; function has only %d parameter%s", parameter.Slot.Index, signature.Params().Len(), targetGatePluralSuffix(signature.Params().Len()))
		}
		return nil
	}
	return signature.Params().At(index).Type()
}

func validateGateCapabilityEvidence(analysis *loadedAnalysis, parameterType types.Type, reference ObjectRef, scope string, problems *[]string) {
	object, err := resolveTargetGateObject(analysis, reference)
	if err != nil {
		addProblem(problems, scope+" gate capability", "%v", err)
		return
	}
	typeName, ok := object.(*types.TypeName)
	if !ok {
		addProblem(problems, scope+" gate capability", "object %s is not a declared type", reference.key())
		return
	}
	if typeName.IsAlias() {
		addProblem(problems, scope+" gate capability", "type %s is an alias; capability evidence requires the exact declared interface", reference.key())
		return
	}
	if named, ok := typeName.Type().(*types.Named); ok && named.TypeParams().Len() != 0 {
		addProblem(problems, scope+" gate capability", "type %s is an uninstantiated generic type", reference.key())
		return
	}
	capability, ok := types.Unalias(typeName.Type()).Underlying().(*types.Interface)
	if !ok {
		addProblem(problems, scope+" gate capability", "type %s is not an interface type", reference.key())
		return
	}
	capability = capability.Complete()
	if parameterType == nil {
		return
	}
	parameterInterface, ok := types.Unalias(parameterType).Underlying().(*types.Interface)
	if !ok {
		addProblem(problems, scope+" gate capability parameter", "type %s must have interface type", targetGateTypeString(parameterType))
		return
	}
	parameterInterface = parameterInterface.Complete()
	if !targetGateInterfacesCompatible(parameterInterface, capability) || !types.AssertableTo(parameterInterface, typeName.Type()) {
		addProblem(
			problems,
			scope+" gate capability",
			"interface %s cannot be asserted from parameter type %s",
			reference.key(),
			targetGateTypeString(parameterType),
		)
	}
}

func targetGateInterfacesCompatible(left, right *types.Interface) bool {
	if left == nil || right == nil || !left.IsMethodSet() || !right.IsMethodSet() {
		return false
	}
	leftMethods := make(map[string]*types.Signature, left.NumMethods())
	for index := 0; index < left.NumMethods(); index++ {
		method := left.Method(index)
		signature, _ := method.Type().(*types.Signature)
		leftMethods[types.Id(method.Pkg(), method.Name())] = signature
	}
	for index := 0; index < right.NumMethods(); index++ {
		method := right.Method(index)
		leftSignature, exists := leftMethods[types.Id(method.Pkg(), method.Name())]
		if !exists {
			continue
		}
		rightSignature, _ := method.Type().(*types.Signature)
		if !targetGateMethodSignaturesIdentical(leftSignature, rightSignature) {
			return false
		}
	}
	return true
}

func targetGateMethodSignaturesIdentical(left, right *types.Signature) bool {
	if left == nil || right == nil || left.Variadic() != right.Variadic() {
		return false
	}
	if left.Params().Len() != right.Params().Len() || left.Results().Len() != right.Results().Len() {
		return false
	}
	for index := 0; index < left.Params().Len(); index++ {
		if !types.Identical(left.Params().At(index).Type(), right.Params().At(index).Type()) {
			return false
		}
	}
	for index := 0; index < left.Results().Len(); index++ {
		if !types.Identical(left.Results().At(index).Type(), right.Results().At(index).Type()) {
			return false
		}
	}
	return true
}

func validateGatePredicateEvidence(analysis *loadedAnalysis, reference ObjectRef, expected, scope string, problems *[]string) {
	object, err := resolveTargetGateObject(analysis, reference)
	if err != nil {
		addProblem(problems, scope, "%v", err)
		return
	}
	var valueType types.Type
	switch object := object.(type) {
	case *types.Func:
		signature, _ := object.Type().(*types.Signature)
		if signature == nil {
			addProblem(problems, scope, "function %s has no signature", reference.key())
			return
		}
		if signature.TypeParams().Len() != 0 {
			addProblem(problems, scope, "function %s is an uninstantiated generic function", reference.key())
			return
		}
		if signature.Params().Len() != 0 {
			addProblem(problems, scope, "function %s has %d parameter%s; predicate must be closed", reference.key(), signature.Params().Len(), targetGatePluralSuffix(signature.Params().Len()))
		}
		if signature.Results().Len() != 1 {
			addProblem(problems, scope, "function %s has %d results; predicate requires exactly 1", reference.key(), signature.Results().Len())
			return
		}
		valueType = signature.Results().At(0).Type()
	case *types.Var, *types.Const:
		valueType = object.Type()
	default:
		addProblem(problems, scope, "object %s is not a function, variable, field, or constant predicate", reference.key())
		return
	}
	if !targetGateBooleanType(valueType) {
		addProblem(problems, scope, "predicate must produce one boolean value; got %s", targetGateTypeString(valueType))
	}
	if expected != "true" && expected != "false" {
		addProblem(problems, scope, "predicate expectation %q must be %q or %q", expected, "true", "false")
	}
}

func resolveTargetGateObject(analysis *loadedAnalysis, reference ObjectRef) (types.Object, error) {
	if analysis == nil {
		return nil, fmt.Errorf("loaded analysis is required")
	}
	pkg := analysis.packages[reference.Package]
	if pkg == nil || pkg.Types == nil {
		return nil, fmt.Errorf("package %q was not loaded", reference.Package)
	}
	return resolveObject(pkg.Types, reference)
}

func targetGateChannelType(candidate types.Type) *types.Chan {
	if candidate == nil {
		return nil
	}
	channel, _ := types.Unalias(candidate).Underlying().(*types.Chan)
	return channel
}

func targetGateBooleanType(candidate types.Type) bool {
	if candidate == nil {
		return false
	}
	basic, ok := types.Unalias(candidate).Underlying().(*types.Basic)
	return ok && (basic.Kind() == types.Bool || basic.Kind() == types.UntypedBool)
}

func targetGateTypeString(candidate types.Type) string {
	if candidate == nil {
		return "<missing>"
	}
	return types.TypeString(candidate, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	})
}

func targetGatePluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
