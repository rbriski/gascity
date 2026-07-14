package effectinventory

import (
	"fmt"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

func (analysis *loadedAnalysis) unanalyzedBoundaryArgumentProblems(ref FunctionRef, call ssa.CallInstruction, callees []*ssa.Function, boundaries []resolvedBoundary) []string {
	if !analysis.callLeavesAuthoredUniverse(call, callees) {
		return nil
	}

	var problems []string
	for _, argument := range call.Common().Args {
		matches := analysis.escapedArgumentBoundaries(argument, boundaries)
		if len(matches) == 0 {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s: inventoried boundaries %s escape through an argument to an unanalyzed callee",
			ref.key(), strings.Join(matches, ", "),
		))
	}
	if call.Common().IsInvoke() {
		matches := analysis.escapedArgumentBoundaries(call.Common().Value, boundaries)
		if len(matches) != 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: inventoried boundaries %s escape through an invoke receiver to an unanalyzed callee",
				ref.key(), strings.Join(matches, ", "),
			))
		}
	}
	boundReceiverMatches := make(map[string]bool)
	for _, receiver := range boundMethodReceiverValues(call.Common().Value, make(map[ssa.Value]bool)) {
		analysis.collectExactBoundaryMatches(receiver, boundaries, boundReceiverMatches)
	}
	if matches := sortedBoundaryIDs(boundReceiverMatches); len(matches) != 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: inventoried boundaries %s escape through a bound receiver to an unanalyzed callee",
			ref.key(), strings.Join(matches, ", "),
		))
	}
	return problems
}

func (analysis *loadedAnalysis) callLeavesAuthoredUniverse(call ssa.CallInstruction, callees []*ssa.Function) bool {
	common := call.Common()
	if _, builtin := common.Value.(*ssa.Builtin); builtin {
		return false
	}
	if len(callees) == 0 {
		return true
	}
	if callHasOpenWorldSyntheticDispatch(common) {
		return true
	}
	if common.StaticCallee() == nil {
		if common.IsInvoke() {
			if hasOpenWorldDispatchReceiver(common.Value, make(map[ssa.Value]bool)) {
				return true
			}
		} else if hasOpenWorldFunctionSource(common.Value, make(map[ssa.Value]bool)) {
			return true
		}
	}
	for _, callee := range callees {
		if !analysis.calleeCoveredByAuthoredSource(callee, make(map[*ssa.Function]bool)) {
			return true
		}
	}
	return false
}

func (analysis *loadedAnalysis) calleeCoveredByAuthoredSource(function *ssa.Function, visiting map[*ssa.Function]bool) bool {
	if function == nil || visiting[function] {
		return false
	}
	if analysis.sourceFuncs[function] && len(function.Blocks) != 0 {
		return true
	}
	if origin := function.Origin(); origin != nil && analysis.sourceFuncs[origin] && len(origin.Blocks) != 0 {
		return true
	}
	if !dispatchOnlySynthetic(function.Synthetic) {
		return false
	}

	visiting[function] = true
	defer delete(visiting, function)
	node := analysis.callGraph.Nodes[function]
	if node == nil || len(node.Out) == 0 {
		return false
	}
	found := false
	for _, edge := range node.Out {
		if edge.Callee == nil || edge.Callee.Func == nil {
			return false
		}
		found = true
		if !analysis.calleeCoveredByAuthoredSource(edge.Callee.Func, visiting) {
			return false
		}
	}
	return found
}

func (analysis *loadedAnalysis) escapedArgumentBoundaries(argument ssa.Value, boundaries []resolvedBoundary) []string {
	matches := make(map[string]bool)
	analysis.collectEscapedArgumentBoundaries(argument, boundaries, matches, make(map[ssa.Value]bool))
	return sortedBoundaryIDs(matches)
}

func (analysis *loadedAnalysis) collectEscapedArgumentBoundaries(argument ssa.Value, boundaries []resolvedBoundary, matches map[string]bool, visiting map[ssa.Value]bool) {
	if argument == nil || visiting[argument] {
		return
	}
	visiting[argument] = true
	defer delete(visiting, argument)

	provenance := analysis.traceChannel(argument, boundaries, nil, make(map[ssa.Value]bool))
	for boundaryID := range provenance.matches {
		matches[boundaryID] = true
	}
	if (provenance.openWorld || provenance.unsafe) && hasCompatibleChannelCarrierBoundary(argument.Type(), boundaries) {
		for _, boundary := range boundaries {
			if boundary.definition.Match == ObjectMatchChannel && boundary.channel != nil && channelCarrierTypeCompatible(argument.Type(), boundary.channel) {
				matches[boundary.definition.ID] = true
			}
		}
	}

	openWorld := hasOpenWorldFunctionSource(argument, make(map[ssa.Value]bool))
	for _, boundary := range boundaries {
		if boundary.definition.Match == ObjectMatchChannel || !analysis.callableValueCompatibleWithBoundary(argument, boundary, make(map[ssa.Value]bool)) {
			continue
		}
		if openWorld || boundClosureOpenForBoundary(argument, boundary, make(map[ssa.Value]bool)) || analysis.callableValueMatchesBoundary(argument, boundary, make(map[ssa.Value]bool)) {
			matches[boundary.definition.ID] = true
		}
	}

	if values, closed, isSlice := sliceContentValues(argument); isSlice {
		for _, value := range values {
			analysis.collectEscapedArgumentBoundaries(value, boundaries, matches, visiting)
		}
		if !closed {
			addOpenSliceElementBoundaries(sliceCarrierType(argument), boundaries, matches)
		}
	}
}

func (analysis *loadedAnalysis) collectExactBoundaryMatches(value ssa.Value, boundaries []resolvedBoundary, matches map[string]bool) {
	provenance := analysis.traceChannel(value, boundaries, nil, make(map[ssa.Value]bool))
	for boundaryID := range provenance.matches {
		matches[boundaryID] = true
	}
	for _, boundary := range boundaries {
		if boundary.definition.Match != ObjectMatchChannel && analysis.callableValueMatchesBoundary(value, boundary, make(map[ssa.Value]bool)) {
			matches[boundary.definition.ID] = true
		}
	}
}

func sortedBoundaryIDs(matches map[string]bool) []string {
	result := make([]string, 0, len(matches))
	for boundaryID := range matches {
		result = append(result, boundaryID)
	}
	sort.Strings(result)
	return result
}

func (analysis *loadedAnalysis) callableValueCompatibleWithBoundary(value ssa.Value, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)
	if signature, ok := types.Unalias(value.Type()).Underlying().(*types.Signature); ok && callableSignatureCompatible(signature, boundary) {
		return true
	}
	switch value := value.(type) {
	case *ssa.MakeInterface:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.ChangeInterface:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.TypeAssert:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.ChangeType:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.Convert:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.UnOp:
		return analysis.callableValueCompatibleWithBoundary(value.X, boundary, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if analysis.callableValueCompatibleWithBoundary(edge, boundary, visiting) {
				return true
			}
		}
	}
	return false
}

func sliceContentValues(value ssa.Value) ([]ssa.Value, bool, bool) {
	state := sliceContentTraversal{
		visiting: make(map[ssa.Value]bool),
		memo:     make(map[ssa.Value]sliceContentResult),
	}
	result := state.visit(value)
	return result.values, result.closed, result.isSlice
}

type sliceContentResult struct {
	values  []ssa.Value
	closed  bool
	isSlice bool
}

type sliceContentTraversal struct {
	visiting map[ssa.Value]bool
	memo     map[ssa.Value]sliceContentResult
}

func (state *sliceContentTraversal) visit(value ssa.Value) sliceContentResult {
	if value == nil {
		return sliceContentResult{}
	}
	if result, ok := state.memo[value]; ok {
		return result
	}
	if state.visiting[value] {
		// A Phi back edge cannot prove closed contents. Preserve its slice
		// shape so callers remain fail-closed while sibling edges still
		// contribute every exact value they contain.
		_, isSlice := types.Unalias(value.Type()).Underlying().(*types.Slice)
		return sliceContentResult{isSlice: isSlice}
	}
	state.visiting[value] = true
	defer delete(state.visiting, value)

	result := state.inspect(value)
	state.memo[value] = result
	return result
}

func (state *sliceContentTraversal) inspect(value ssa.Value) sliceContentResult {
	switch value := value.(type) {
	case *ssa.MakeInterface:
		return state.visit(value.X)
	case *ssa.ChangeInterface:
		return state.visit(value.X)
	case *ssa.TypeAssert:
		return state.visit(value.X)
	case *ssa.Extract:
		return state.visit(value.Tuple)
	case *ssa.Phi:
		closed := true
		found := false
		var values []ssa.Value
		for _, edge := range value.Edges {
			edgeResult := state.visit(edge)
			if !edgeResult.isSlice {
				closed = false
				continue
			}
			found = true
			closed = closed && edgeResult.closed
			values = append(values, edgeResult.values...)
		}
		return sliceContentResult{values: values, closed: closed, isSlice: found}
	}
	if _, isSlice := types.Unalias(value.Type()).Underlying().(*types.Slice); !isSlice {
		return sliceContentResult{}
	}
	switch value := value.(type) {
	case *ssa.Slice:
		values, closed := indexedContainerValues(value.X)
		return sliceContentResult{values: values, closed: closed, isSlice: true}
	case *ssa.MakeSlice, *ssa.Alloc:
		values, closed := indexedContainerValues(value)
		return sliceContentResult{values: values, closed: closed, isSlice: true}
	case *ssa.ChangeType:
		result := state.visit(value.X)
		result.isSlice = true
		return result
	case *ssa.Convert:
		result := state.visit(value.X)
		result.isSlice = true
		return result
	default:
		return sliceContentResult{isSlice: true}
	}
}

func indexedContainerValues(container ssa.Value) ([]ssa.Value, bool) {
	if container == nil || container.Referrers() == nil {
		return nil, false
	}
	closed := true
	var values []ssa.Value
	for _, instruction := range *container.Referrers() {
		switch instruction := instruction.(type) {
		case *ssa.IndexAddr:
			if instruction.Referrers() == nil {
				closed = false
				continue
			}
			for _, indexedInstruction := range *instruction.Referrers() {
				switch indexedInstruction := indexedInstruction.(type) {
				case *ssa.Store:
					if indexedInstruction.Addr == instruction {
						values = append(values, indexedInstruction.Val)
					}
				case *ssa.DebugRef:
				default:
					closed = false
				}
			}
		case *ssa.Slice, *ssa.DebugRef, *ssa.ChangeType, *ssa.Convert, ssa.CallInstruction:
			// Reads, view conversions, and the consuming call do not mutate
			// the locally proven element stores before the handoff.
		default:
			closed = false
		}
	}
	return values, closed
}

func addOpenSliceElementBoundaries(sliceType types.Type, boundaries []resolvedBoundary, matches map[string]bool) {
	if sliceType == nil {
		return
	}
	slice, ok := types.Unalias(sliceType).Underlying().(*types.Slice)
	if !ok {
		return
	}
	element := slice.Elem()
	for _, boundary := range boundaries {
		if boundary.definition.Match == ObjectMatchChannel {
			if boundary.channel != nil && channelCarrierTypeCompatible(element, boundary.channel) {
				matches[boundary.definition.ID] = true
			}
			continue
		}
		signature, ok := types.Unalias(element).Underlying().(*types.Signature)
		if ok && callableSignatureCompatible(signature, boundary) {
			matches[boundary.definition.ID] = true
			continue
		}
		if _, isInterface := types.Unalias(element).Underlying().(*types.Interface); isInterface && boundary.function != nil && types.AssignableTo(boundary.function.Type(), element) {
			matches[boundary.definition.ID] = true
		}
	}
}

func sliceCarrierType(value ssa.Value) types.Type {
	return sliceCarrierTypeAlong(value, make(map[ssa.Value]bool))
}

func sliceCarrierTypeAlong(value ssa.Value, visiting map[ssa.Value]bool) types.Type {
	if value == nil || visiting[value] {
		return nil
	}
	if _, isSlice := types.Unalias(value.Type()).Underlying().(*types.Slice); isSlice {
		return value.Type()
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.MakeInterface:
		return sliceCarrierTypeAlong(value.X, visiting)
	case *ssa.ChangeInterface:
		return sliceCarrierTypeAlong(value.X, visiting)
	case *ssa.TypeAssert:
		return sliceCarrierTypeAlong(value.X, visiting)
	case *ssa.Extract:
		return sliceCarrierTypeAlong(value.Tuple, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if candidate := sliceCarrierTypeAlong(edge, visiting); candidate != nil {
				return candidate
			}
		}
	}
	return nil
}

func hasCompatibleChannelCarrierBoundary(valueType types.Type, boundaries []resolvedBoundary) bool {
	for _, boundary := range boundaries {
		if boundary.definition.Match == ObjectMatchChannel && boundary.channel != nil && channelCarrierTypeCompatible(valueType, boundary.channel) {
			return true
		}
	}
	return false
}

func channelCarrierTypeCompatible(carrier, channel types.Type) bool {
	if channelTypesCompatible(carrier, channel) {
		return true
	}
	carrier = types.Unalias(carrier)
	if pointer, ok := carrier.(*types.Pointer); ok {
		return channelTypesCompatible(pointer.Elem(), channel)
	}
	return false
}

func callHasOpenWorldSyntheticDispatch(common *ssa.CallCommon) bool {
	if boundMethodReceiverOpenWorld(common.Value, make(map[ssa.Value]bool)) {
		return true
	}
	static := common.StaticCallee()
	if static == nil || len(common.Args) == 0 || strings.HasPrefix(static.Synthetic, "bound method wrapper for ") {
		return false
	}
	return (strings.HasPrefix(static.Synthetic, "wrapper for ") || strings.HasPrefix(static.Synthetic, "thunk for ")) && hasOpenWorldDispatchReceiver(common.Args[0], make(map[ssa.Value]bool))
}

func boundMethodReceiverOpenWorld(value ssa.Value, visiting map[ssa.Value]bool) bool {
	for _, receiver := range boundMethodReceiverValues(value, visiting) {
		if hasOpenWorldDispatchReceiver(receiver, make(map[ssa.Value]bool)) {
			return true
		}
	}
	return false
}

func boundMethodReceiverValues(value ssa.Value, visiting map[ssa.Value]bool) []ssa.Value {
	if value == nil || visiting[value] {
		return nil
	}
	visiting[value] = true
	defer delete(visiting, value)
	var values []ssa.Value
	merge := func(candidate ssa.Value) {
		values = append(values, boundMethodReceiverValues(candidate, visiting)...)
	}
	switch value := value.(type) {
	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)
		if function != nil && strings.HasPrefix(function.Synthetic, "bound method wrapper for ") {
			return append(values, value.Bindings...)
		}
	case *ssa.MakeInterface:
		merge(value.X)
	case *ssa.ChangeInterface:
		merge(value.X)
	case *ssa.TypeAssert:
		merge(value.X)
	case *ssa.Extract:
		merge(value.Tuple)
	case *ssa.UnOp:
		merge(value.X)
	case *ssa.ChangeType:
		merge(value.X)
	case *ssa.Convert:
		merge(value.X)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			merge(edge)
		}
	case *ssa.FreeVar:
		for _, binding := range freeVariableBindings(value) {
			merge(binding)
		}
	case *ssa.FieldAddr:
		for _, stored := range localFieldStoredValues(value.X, value.Field) {
			merge(stored)
		}
	case *ssa.Field:
		for _, stored := range localFieldStoredValues(value.X, value.Field) {
			merge(stored)
		}
	case *ssa.Alloc:
		if references := value.Referrers(); references != nil {
			for _, instruction := range *references {
				store, ok := instruction.(*ssa.Store)
				if ok && store.Addr == value {
					merge(store.Val)
				}
			}
		}
	}
	return values
}

func freeVariableBindings(freeVariable *ssa.FreeVar) []ssa.Value {
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil {
		return nil
	}
	index := -1
	for candidateIndex, candidate := range owner.FreeVars {
		if candidate == freeVariable {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return nil
	}
	var bindings []ssa.Value
	for _, block := range owner.Parent().Blocks {
		for _, instruction := range block.Instrs {
			closure, ok := instruction.(*ssa.MakeClosure)
			if ok && closure.Fn == owner && index < len(closure.Bindings) {
				bindings = append(bindings, closure.Bindings[index])
			}
		}
	}
	return bindings
}

func localFieldStoredValues(base ssa.Value, field int) []ssa.Value {
	allocation, local := base.(*ssa.Alloc)
	if !local {
		if load, ok := base.(*ssa.UnOp); ok {
			allocation, local = load.X.(*ssa.Alloc)
		}
	}
	if !local || allocation.Referrers() == nil {
		return nil
	}
	var values []ssa.Value
	for _, instruction := range *allocation.Referrers() {
		fieldAddress, ok := instruction.(*ssa.FieldAddr)
		if !ok || fieldAddress.Field != field || fieldAddress.Referrers() == nil {
			continue
		}
		for _, fieldInstruction := range *fieldAddress.Referrers() {
			store, ok := fieldInstruction.(*ssa.Store)
			if ok && store.Addr == fieldAddress {
				values = append(values, store.Val)
			}
		}
	}
	return values
}

func (analysis *loadedAnalysis) callableValueMatchesBoundary(value ssa.Value, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)

	matchFunction := func(function *ssa.Function) bool {
		if function == nil {
			return false
		}
		if calleeMatchesBoundary(function, boundary, analysis.callGraph, nil, make(map[*ssa.Function]bool)) {
			return true
		}
		object, _ := function.Object().(*types.Func)
		if object == nil || !strings.HasPrefix(function.Synthetic, "thunk for ") {
			return false
		}
		objectSignature, _ := object.Type().(*types.Signature)
		return objectSignature != nil && objectSignature.Recv() != nil && object.Name() == boundary.function.Name() && callableSignatureCompatible(function.Signature, boundary)
	}

	switch value := value.(type) {
	case *ssa.Function:
		return matchFunction(value)
	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)
		return matchFunction(function)
	case *ssa.FreeVar:
		return analysis.callableFreeVarMatchesBoundary(value, boundary, visiting)
	case *ssa.UnOp:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.FieldAddr:
		return analysis.callableFieldMatchesBoundary(value.X, value.Field, boundary, visiting)
	case *ssa.Field:
		return analysis.callableFieldMatchesBoundary(value.X, value.Field, boundary, visiting)
	case *ssa.ChangeType:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.ChangeInterface:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.Convert:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.MakeInterface:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.TypeAssert:
		return analysis.callableValueMatchesBoundary(value.X, boundary, visiting)
	case *ssa.Extract:
		return analysis.callableValueMatchesBoundary(value.Tuple, boundary, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if analysis.callableValueMatchesBoundary(edge, boundary, visiting) {
				return true
			}
		}
	case *ssa.Alloc:
		if references := value.Referrers(); references != nil {
			for _, instruction := range *references {
				store, ok := instruction.(*ssa.Store)
				if ok && store.Addr == value && analysis.callableValueMatchesBoundary(store.Val, boundary, visiting) {
					return true
				}
			}
		}
	}
	return false
}

func (analysis *loadedAnalysis) callableFreeVarMatchesBoundary(freeVariable *ssa.FreeVar, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil {
		return false
	}
	index := -1
	for candidateIndex, candidate := range owner.FreeVars {
		if candidate == freeVariable {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return false
	}
	for _, block := range owner.Parent().Blocks {
		for _, instruction := range block.Instrs {
			closure, ok := instruction.(*ssa.MakeClosure)
			if !ok || closure.Fn != owner || index >= len(closure.Bindings) {
				continue
			}
			if analysis.callableValueMatchesBoundary(closure.Bindings[index], boundary, visiting) {
				return true
			}
		}
	}
	return false
}

func (analysis *loadedAnalysis) callableFieldMatchesBoundary(base ssa.Value, field int, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	allocation, local := base.(*ssa.Alloc)
	if !local {
		if load, ok := base.(*ssa.UnOp); ok {
			allocation, local = load.X.(*ssa.Alloc)
		}
	}
	if !local {
		return false
	}
	if references := allocation.Referrers(); references != nil {
		for _, instruction := range *references {
			fieldAddress, ok := instruction.(*ssa.FieldAddr)
			if !ok || fieldAddress.Field != field || fieldAddress.Referrers() == nil {
				continue
			}
			for _, fieldInstruction := range *fieldAddress.Referrers() {
				store, ok := fieldInstruction.(*ssa.Store)
				if ok && store.Addr == fieldAddress && analysis.callableValueMatchesBoundary(store.Val, boundary, visiting) {
					return true
				}
			}
		}
	}
	return false
}
