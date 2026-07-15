package effectinventory

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

type routeHopFunction struct {
	function  *ssa.Function
	reference FunctionRef
}

type routeHopFunctionIndex struct {
	exact             map[string][]routeHopFunction
	byObject          map[string][]routeHopFunction
	instancesByOrigin map[*ssa.Function][]*ssa.Function
}

type routeHopCall struct {
	call     ssa.CallInstruction
	position token.Pos
	dispatch HopDispatchKind
}

type routeHopDispatchStep struct {
	callee   *ssa.Function
	dispatch HopDispatchKind
}

type routeHopDispatchSearch struct {
	analysis *loadedAnalysis
	target   *ssa.Function
	matches  map[HopDispatchKind]bool
	problems []string
}

// validateRouteHopEvidenceForProfile proves the authored route chains active
// in analysis.profile against that profile's loaded SSA and VTA call graph.
// It is intentionally separate from structural registry compilation so
// canonical discovery can run it before accepting authored route claims.
func validateRouteHopEvidenceForProfile(analysis *loadedAnalysis, registry Registry) error {
	if analysis == nil {
		return fmt.Errorf("effect route-hop evidence failed: loaded analysis is required")
	}
	profile := analysis.profile.ID
	index, problems := newRouteHopFunctionIndex(analysis)

	for _, registration := range registry.Registrations {
		registrationID := deriveSiteRegistrationID(registration.BoundaryID, registration.Matcher)
		registrationScope := fmt.Sprintf("registration %q", registrationID)
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
				validateRouteHopRouteEvidence(analysis, index, registration.Matcher, route, routeScope, &problems)
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
	return fmt.Errorf("effect route-hop evidence failed for profile %q:\n- %s", profile, strings.Join(problems, "\n- "))
}

func newRouteHopFunctionIndex(analysis *loadedAnalysis) (*routeHopFunctionIndex, []string) {
	index := &routeHopFunctionIndex{
		exact:             make(map[string][]routeHopFunction),
		byObject:          make(map[string][]routeHopFunction),
		instancesByOrigin: make(map[*ssa.Function][]*ssa.Function),
	}
	if analysis == nil {
		return index, []string{"building function index: loaded analysis is required"}
	}

	var problems []string
	for function := range analysis.sourceFuncs {
		if function == nil {
			problems = append(problems, "building function index: source function is nil")
			continue
		}
		function = routeHopFunctionOrigin(function)
		if !function.Pos().IsValid() {
			// The aggregate package initializer has no authored FunctionRef.
			// Explicit init declarations and their closures retain valid positions.
			continue
		}
		reference, err := analysis.functionRef(function, function.Pos())
		if err != nil {
			problems = append(problems, "building function index: "+err.Error())
			continue
		}
		candidate := routeHopFunction{function: function, reference: reference}
		exactKey := canonicalFunctionRef(reference)
		index.exact[exactKey] = append(index.exact[exactKey], candidate)
		objectKey := canonicalObjectRef(reference.Object)
		index.byObject[objectKey] = append(index.byObject[objectKey], candidate)
	}

	for _, candidates := range index.exact {
		sortRouteHopFunctions(candidates)
	}
	for _, candidates := range index.byObject {
		sortRouteHopFunctions(candidates)
	}
	if analysis.callGraph != nil {
		for function := range analysis.callGraph.Nodes {
			if function == nil || function.Origin() == nil {
				continue
			}
			origin := routeHopFunctionOrigin(function)
			if function != origin {
				index.instancesByOrigin[origin] = append(index.instancesByOrigin[origin], function)
			}
		}
		for _, instances := range index.instancesByOrigin {
			sort.Slice(instances, func(i, j int) bool {
				return functionSortKey(instances[i]) < functionSortKey(instances[j])
			})
		}
	}
	sort.Strings(problems)
	return index, compactStrings(problems)
}

func sortRouteHopFunctions(candidates []routeHopFunction) {
	sort.Slice(candidates, func(i, j int) bool {
		left := canonicalFunctionRef(candidates[i].reference) + "|" + functionSortKey(candidates[i].function)
		right := canonicalFunctionRef(candidates[j].reference) + "|" + functionSortKey(candidates[j].function)
		return left < right
	})
}

func (index *routeHopFunctionIndex) resolve(reference FunctionRef, scope string, problems *[]string) *ssa.Function {
	if index == nil {
		*problems = append(*problems, scope+": function index is required")
		return nil
	}
	exact := index.exact[canonicalFunctionRef(reference)]
	switch len(exact) {
	case 1:
		return exact[0].function
	case 0:
		// Continue below to distinguish a missing object, a stale closure path,
		// and a source file excluded by this loaded profile.
	default:
		*problems = append(*problems, fmt.Sprintf(
			"%s: FunctionRef %s is ambiguous across %d SSA functions",
			scope,
			describeRouteHopFunction(reference),
			len(exact),
		))
		return nil
	}

	objectCandidates := index.byObject[canonicalObjectRef(reference.Object)]
	if len(objectCandidates) == 0 {
		*problems = append(*problems, fmt.Sprintf(
			"%s: FunctionRef %s is missing from loaded profile",
			scope,
			describeRouteHopFunction(reference),
		))
		return nil
	}

	var sameFile []routeHopFunction
	for _, candidate := range objectCandidates {
		if candidate.reference.File == reference.File {
			sameFile = append(sameFile, candidate)
		}
	}
	if len(sameFile) != 0 {
		*problems = append(*problems, fmt.Sprintf(
			"%s: FunctionRef %s has a stale closure path; loaded candidates: %s",
			scope,
			describeRouteHopFunction(reference),
			describeRouteHopFunctions(sameFile),
		))
		return nil
	}

	*problems = append(*problems, fmt.Sprintf(
		"%s: FunctionRef %s has a file/profile mismatch; loaded candidates: %s",
		scope,
		describeRouteHopFunction(reference),
		describeRouteHopFunctions(objectCandidates),
	))
	return nil
}

func validateRouteHopRouteEvidence(analysis *loadedAnalysis, index *routeHopFunctionIndex, matcher OperationSite, route Route, scope string, problems *[]string) {
	logicalOwner := index.resolve(route.LogicalOwner, scope+" logical owner", problems)
	physicalOwner := index.resolve(matcher.Enclosing, scope+" physical site enclosing", problems)

	enclosing := make([]*ssa.Function, len(route.Hops))
	callees := make([]*ssa.Function, len(route.Hops))
	selectedCalls := make([]ssa.CallInstruction, len(route.Hops))
	for hopIndex, hop := range route.Hops {
		hopScope := fmt.Sprintf("%s hop[%d]", scope, hopIndex)
		enclosing[hopIndex] = index.resolve(hop.Site.Enclosing, hopScope+" enclosing", problems)
		callees[hopIndex] = index.resolve(hop.Callee, hopScope+" callee", problems)
		if enclosing[hopIndex] != nil && callees[hopIndex] != nil {
			var priorCall ssa.CallInstruction
			if hopIndex > 0 && sameRouteHopFunction(callees[hopIndex-1], enclosing[hopIndex]) {
				priorCall = selectedCalls[hopIndex-1]
			}
			selectedCalls[hopIndex] = validateRouteHopCallEvidence(analysis, index, enclosing[hopIndex], callees[hopIndex], priorCall, hop, hopScope, problems)
		}
	}

	if len(route.Hops) == 0 {
		if logicalOwner != nil && physicalOwner != nil && !sameRouteHopFunction(logicalOwner, physicalOwner) {
			*problems = append(*problems, fmt.Sprintf(
				"%s: chain mismatch: route without hops has logical owner %s but physical site is enclosed by %s",
				scope,
				describeRouteHopFunction(route.LogicalOwner),
				describeRouteHopFunction(matcher.Enclosing),
			))
		}
		return
	}

	for hopIndex := 1; hopIndex < len(route.Hops); hopIndex++ {
		if callees[hopIndex-1] != nil && enclosing[hopIndex] != nil && !sameRouteHopFunction(callees[hopIndex-1], enclosing[hopIndex]) {
			*problems = append(*problems, fmt.Sprintf(
				"%s: chain mismatch: hop[%d] callee %s does not equal hop[%d] enclosing %s",
				scope,
				hopIndex-1,
				describeRouteHopFunction(route.Hops[hopIndex-1].Callee),
				hopIndex,
				describeRouteHopFunction(route.Hops[hopIndex].Site.Enclosing),
			))
		}
	}
	last := len(route.Hops) - 1
	if callees[last] != nil && physicalOwner != nil && !sameRouteHopFunction(callees[last], physicalOwner) {
		*problems = append(*problems, fmt.Sprintf(
			"%s: chain mismatch: last hop callee %s does not equal physical site enclosing %s",
			scope,
			describeRouteHopFunction(route.Hops[last].Callee),
			describeRouteHopFunction(matcher.Enclosing),
		))
	}
	if logicalOwner != nil && !resolvedRouteHopContainsFunction(enclosing, callees, logicalOwner) {
		*problems = append(*problems, fmt.Sprintf(
			"%s: chain mismatch: logical owner %s is absent from the resolved route chain",
			scope,
			describeRouteHopFunction(route.LogicalOwner),
		))
	}
}

func validateRouteHopCallEvidence(analysis *loadedAnalysis, index *routeHopFunctionIndex, enclosing, callee *ssa.Function, priorCall ssa.CallInstruction, hop RouteHop, scope string, problems *[]string) ssa.CallInstruction {
	if !oneOf(hop.Site.Operation, OperationCall, OperationGo, OperationDefer) {
		*problems = append(*problems, fmt.Sprintf("%s: operation %q is not a call, go, or defer edge", scope, hop.Site.Operation))
		return nil
	}

	matches, resolutionProblems := routeHopCallsTo(analysis, index, enclosing, callee, priorCall, hop.Site.Operation)
	for _, problem := range resolutionProblems {
		*problems = append(*problems, scope+": "+problem)
	}
	if len(resolutionProblems) != 0 {
		return nil
	}
	if len(matches) == 0 {
		*problems = append(*problems, fmt.Sprintf(
			"%s: %s has no %s edge to callee %s",
			scope,
			describeRouteHopFunction(hop.Site.Enclosing),
			hop.Site.Operation,
			describeRouteHopFunction(hop.Callee),
		))
		return nil
	}
	if hop.Site.Ordinal <= 0 || hop.Site.Ordinal > len(matches) {
		*problems = append(*problems, fmt.Sprintf(
			"%s: stale ordinal %d for %s edges to %s; loaded profile has only %d",
			scope,
			hop.Site.Ordinal,
			hop.Site.Operation,
			describeRouteHopFunction(hop.Callee),
			len(matches),
		))
		return nil
	}

	selectedIndex := hop.Site.Ordinal - 1
	selected := matches[selectedIndex]
	for candidateIndex, candidate := range matches {
		if candidateIndex != selectedIndex && candidate.position == selected.position {
			*problems = append(*problems, fmt.Sprintf(
				"%s: ordinal %d is ambiguous across multiple SSA %s edges at one source position",
				scope,
				hop.Site.Ordinal,
				hop.Site.Operation,
			))
			return nil
		}
	}
	if hop.Dispatch != selected.dispatch {
		*problems = append(*problems, fmt.Sprintf(
			"%s: dispatch %q is wrong for ordinal %d; SSA evidence requires %q",
			scope,
			hop.Dispatch,
			hop.Site.Ordinal,
			selected.dispatch,
		))
	}
	return selected.call
}

func routeHopCallsTo(analysis *loadedAnalysis, index *routeHopFunctionIndex, enclosing, callee *ssa.Function, priorCall ssa.CallInstruction, operation OperationKind) ([]routeHopCall, []string) {
	var matches []routeHopCall
	var problems []string
	for _, block := range enclosing.Blocks {
		for _, instruction := range block.Instrs {
			call, ok := instruction.(ssa.CallInstruction)
			if !ok || !call.Pos().IsValid() {
				continue
			}
			actualOperation, ok := operationForCall(call)
			if !ok || actualOperation != operation {
				continue
			}
			if !routeHopCallCanTarget(index, call, callee) {
				continue
			}
			dispatch, matchesCallee, problem := routeHopDispatchTo(analysis, index, enclosing, call, priorCall, callee)
			if problem != "" {
				problems = append(problems, problem)
				continue
			}
			if matchesCallee {
				matches = append(matches, routeHopCall{call: call, position: call.Pos(), dispatch: dispatch})
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].position < matches[j].position
	})
	sort.Strings(problems)
	return matches, compactStrings(problems)
}

func routeHopCallCanTarget(index *routeHopFunctionIndex, call ssa.CallInstruction, callee *ssa.Function) bool {
	if call == nil || call.Common() == nil || callee == nil {
		return true
	}
	common := call.Common()
	if static := common.StaticCallee(); static != nil && !dispatchOnlySynthetic(static.Synthetic) {
		return sameRouteHopFunction(static, callee)
	}
	if common.Method != nil {
		target, ok := routeHopFunctionOrigin(callee).Object().(*types.Func)
		if !ok {
			// Interface method dispatch cannot directly select an objectless
			// lexical closure, even when their callable signatures match.
			return false
		}
		if common.Method.Origin().Name() != target.Origin().Name() {
			return false
		}
	}
	return routeHopCallSignatureCanTarget(index, common.Signature(), callee)
}

func routeHopCallSignatureCanTarget(index *routeHopFunctionIndex, callSignature *types.Signature, callee *ssa.Function) bool {
	if callSignature == nil || callee == nil {
		return true
	}
	origin := routeHopFunctionOrigin(callee)
	variants := []*ssa.Function{origin}
	if index != nil {
		variants = append(variants, index.instancesByOrigin[origin]...)
	}
	for _, variant := range variants {
		if variant != nil && routeHopCallableSignaturesCompatible(callSignature, variant.Signature) {
			return true
		}
	}
	return false
}

func routeHopCallableSignaturesCompatible(left, right *types.Signature) bool {
	if left == nil || right == nil || left.Variadic() != right.Variadic() {
		return false
	}
	paramsMatch := routeHopTuplesIdentical(left.Params(), right.Params())
	if !paramsMatch && right.Recv() != nil && left.Params().Len() == right.Params().Len()+1 {
		leftReceiver := left.Params().At(0).Type()
		rightReceiver := right.Recv().Type()
		paramsMatch = types.Identical(leftReceiver, rightReceiver) ||
			types.AssignableTo(rightReceiver, leftReceiver)
		for index := 0; paramsMatch && index < right.Params().Len(); index++ {
			paramsMatch = types.Identical(left.Params().At(index+1).Type(), right.Params().At(index).Type())
		}
	}
	if !paramsMatch {
		return false
	}
	return routeHopTuplesIdentical(left.Results(), right.Results())
}

func routeHopTuplesIdentical(left, right *types.Tuple) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Len() != right.Len() {
		return false
	}
	for index := 0; index < left.Len(); index++ {
		if !types.Identical(left.At(index).Type(), right.At(index).Type()) {
			return false
		}
	}
	return true
}

func routeHopDispatchTo(analysis *loadedAnalysis, index *routeHopFunctionIndex, enclosing *ssa.Function, call, priorCall ssa.CallInstruction, callee *ssa.Function) (HopDispatchKind, bool, string) {
	search := routeHopDispatchSearch{
		analysis: analysis,
		target:   callee,
		matches:  make(map[HopDispatchKind]bool),
	}
	steps := routeHopAuthoredDispatchSteps(analysis, index, enclosing, call, priorCall)
	if len(steps) == 0 {
		return "", false, "shape-compatible dynamic call has no closed-world callee evidence"
	}
	for _, step := range steps {
		search.visit(step.callee, step.dispatch, make(map[*ssa.Function]bool))
	}
	if len(search.matches) == 0 {
		if len(search.problems) != 0 {
			sort.Strings(search.problems)
			return "", false, strings.Join(compactStrings(search.problems), "; ")
		}
		return "", false, ""
	}
	// Route evidence is existential: one positively resolved target edge proves
	// this authored call can reach the requested callee. Cycles in alternative
	// dispatch branches do not negate that edge. A shape-compatible call with
	// no positive target remains fail closed above.
	if len(search.matches) != 1 {
		return "", false, fmt.Sprintf(
			"callee %s has ambiguous exact and VTA dispatch evidence",
			describeRouteHopFunctionRefFromSSA(analysis, callee),
		)
	}
	for dispatch := range search.matches {
		return dispatch, true, ""
	}
	return "", false, ""
}

func routeHopAuthoredDispatchSteps(analysis *loadedAnalysis, index *routeHopFunctionIndex, enclosing *ssa.Function, call, priorCall ssa.CallInstruction) []routeHopDispatchStep {
	if static := call.Common().StaticCallee(); static != nil {
		return []routeHopDispatchStep{{callee: static, dispatch: HopDispatchExact}}
	}
	if argument, bound := routeHopParameterCallArgument(enclosing, call, priorCall); bound {
		result := newCallableTargetTracer(analysis).trace(argument)
		if !result.closed {
			return nil
		}
		steps := make([]routeHopDispatchStep, 0, len(result.targets))
		for target := range result.targets {
			steps = append(steps, routeHopDispatchStep{callee: target, dispatch: HopDispatchVTA})
		}
		sort.Slice(steps, func(i, j int) bool {
			return functionSortKey(steps[i].callee) < functionSortKey(steps[j].callee)
		})
		return steps
	}

	type callVariant struct {
		enclosing *ssa.Function
		call      ssa.CallInstruction
	}
	variants := []callVariant{{enclosing: enclosing, call: call}}
	seenCalls := map[ssa.CallInstruction]bool{call: true}
	operation, _ := operationForCall(call)
	for _, function := range index.instancesByOrigin[routeHopFunctionOrigin(enclosing)] {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				candidate, ok := instruction.(ssa.CallInstruction)
				if !ok || seenCalls[candidate] || candidate.Pos() != call.Pos() {
					continue
				}
				candidateOperation, candidateOK := operationForCall(candidate)
				if !candidateOK || candidateOperation != operation {
					continue
				}
				seenCalls[candidate] = true
				variants = append(variants, callVariant{enclosing: function, call: candidate})
			}
		}
	}

	var steps []routeHopDispatchStep
	for _, variant := range variants {
		if static := variant.call.Common().StaticCallee(); static != nil {
			// The authored origin was dynamic. Even if instantiation makes one
			// body exact, resolving that body still required VTA/instance evidence.
			steps = append(steps, routeHopDispatchStep{callee: static, dispatch: HopDispatchVTA})
			continue
		}
		for _, candidate := range analysis.closedWorldCallees(variant.enclosing, variant.call) {
			steps = append(steps, routeHopDispatchStep{callee: candidate, dispatch: HopDispatchVTA})
		}
	}
	sort.Slice(steps, func(i, j int) bool {
		return functionSortKey(steps[i].callee) < functionSortKey(steps[j].callee)
	})
	return steps
}

func routeHopParameterCallArgument(enclosing *ssa.Function, call, priorCall ssa.CallInstruction) (ssa.Value, bool) {
	if enclosing == nil || call == nil || call.Common() == nil || priorCall == nil {
		return nil, false
	}
	parameter, ok := call.Common().Value.(*ssa.Parameter)
	if !ok || parameter.Parent() == nil || !sameRouteHopFunction(parameter.Parent(), enclosing) {
		return nil, false
	}
	for index, candidate := range enclosing.Params {
		if candidate == parameter {
			argument, ok := callArgumentForParameter(priorCall, enclosing, index)
			if !ok {
				// The current call is definitely a parameter invocation in a
				// chain-bound context. Failing to recover its actual must remain
				// fail closed rather than falling back to context-insensitive VTA.
				return nil, true
			}
			return argument, true
		}
	}
	// The parameter belongs to this function, so an SSA identity mismatch is
	// also an unresolved binding, not permission to use all global callers.
	return nil, true
}

func routeHopDispatchSteps(analysis *loadedAnalysis, enclosing *ssa.Function, call ssa.CallInstruction) []routeHopDispatchStep {
	if static := call.Common().StaticCallee(); static != nil {
		return []routeHopDispatchStep{{callee: static, dispatch: HopDispatchExact}}
	}
	callees := analysis.closedWorldCallees(enclosing, call)
	steps := make([]routeHopDispatchStep, len(callees))
	for index, callee := range callees {
		steps[index] = routeHopDispatchStep{callee: callee, dispatch: HopDispatchVTA}
	}
	return steps
}

func (search *routeHopDispatchSearch) visit(function *ssa.Function, dispatch HopDispatchKind, visiting map[*ssa.Function]bool) {
	if function == nil {
		return
	}
	if sameRouteHopFunction(function, search.target) {
		search.matches[dispatch] = true
		return
	}
	if !dispatchOnlySynthetic(function.Synthetic) {
		return
	}
	if visiting[function] {
		search.problems = append(search.problems, fmt.Sprintf(
			"dispatch-only SSA cycle through %s",
			functionSortKey(function),
		))
		return
	}
	visiting[function] = true
	defer delete(visiting, function)

	steps := routeHopSyntheticDispatchSteps(search.analysis, function)
	if len(steps) == 0 {
		search.problems = append(search.problems, fmt.Sprintf(
			"dispatch-only SSA function %s has no resolvable call edge",
			functionSortKey(function),
		))
		return
	}
	for _, step := range steps {
		search.visit(step.callee, combineRouteHopDispatch(dispatch, step.dispatch), visiting)
	}
}

func routeHopSyntheticDispatchSteps(analysis *loadedAnalysis, function *ssa.Function) []routeHopDispatchStep {
	var steps []routeHopDispatchStep
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			call, ok := instruction.(ssa.CallInstruction)
			if !ok {
				continue
			}
			steps = append(steps, routeHopDispatchSteps(analysis, function, call)...)
		}
	}
	sort.Slice(steps, func(i, j int) bool {
		left := string(steps[i].dispatch) + "|" + functionSortKey(steps[i].callee)
		right := string(steps[j].dispatch) + "|" + functionSortKey(steps[j].callee)
		return left < right
	})
	return steps
}

func combineRouteHopDispatch(left, right HopDispatchKind) HopDispatchKind {
	if left == HopDispatchVTA || right == HopDispatchVTA {
		return HopDispatchVTA
	}
	return HopDispatchExact
}

func describeRouteHopFunctionRefFromSSA(analysis *loadedAnalysis, function *ssa.Function) string {
	reference, err := analysis.functionRef(function, function.Pos())
	if err != nil {
		return functionSortKey(function)
	}
	return describeRouteHopFunction(reference)
}

func routeHopFunctionOrigin(function *ssa.Function) *ssa.Function {
	if function != nil {
		if origin := function.Origin(); origin != nil {
			return origin
		}
	}
	return function
}

func sameRouteHopFunction(left, right *ssa.Function) bool {
	return left != nil && right != nil && routeHopFunctionOrigin(left) == routeHopFunctionOrigin(right)
}

func resolvedRouteHopContainsFunction(enclosing, callees []*ssa.Function, target *ssa.Function) bool {
	for index := range enclosing {
		if sameRouteHopFunction(enclosing[index], target) || sameRouteHopFunction(callees[index], target) {
			return true
		}
	}
	return false
}

func routeHopProfileMemberships(profiles []BuildProfileID, target BuildProfileID) int {
	count := 0
	for _, profile := range profiles {
		if profile == target {
			count++
		}
	}
	return count
}

func describeRouteHopFunctions(candidates []routeHopFunction) string {
	descriptions := make([]string, len(candidates))
	for index, candidate := range candidates {
		descriptions[index] = describeRouteHopFunction(candidate.reference)
	}
	sort.Strings(descriptions)
	descriptions = compactStrings(descriptions)
	return strings.Join(descriptions, ", ")
}

func describeRouteHopFunction(reference FunctionRef) string {
	closure := "root"
	if len(reference.ClosurePath) != 0 {
		parts := make([]string, len(reference.ClosurePath))
		for index, item := range reference.ClosurePath {
			parts[index] = fmt.Sprint(item)
		}
		closure = strings.Join(parts, ".")
	}
	return fmt.Sprintf("%s@%s#%s", reference.Object.key(), reference.File, closure)
}
