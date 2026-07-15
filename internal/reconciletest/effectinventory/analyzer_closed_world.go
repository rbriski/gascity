package effectinventory

import (
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

type callableTargetResult struct {
	targets map[*ssa.Function]bool
	closed  bool
}

type callableTargetTracer struct {
	analysis       *loadedAnalysis
	visiting       map[ssa.Value]bool
	memo           map[ssa.Value]callableTargetResult
	resultVisiting map[callableResult]bool
	resultMemo     map[callableResult]callableTargetResult
}

func (analysis *loadedAnalysis) closedWorldCallees(function *ssa.Function, call ssa.CallInstruction) []*ssa.Function {
	callees := resolvedCallees(analysis.callGraph, function, call)
	if !analysis.config.closedWorld || call == nil || call.Common() == nil ||
		call.Common().StaticCallee() != nil || call.Common().IsInvoke() {
		return callees
	}
	result := newCallableTargetTracer(analysis).trace(call.Common().Value)
	if !result.closed {
		return callees
	}
	callees = callees[:0]
	for target := range result.targets {
		callees = append(callees, target)
	}
	sort.Slice(callees, func(i, j int) bool {
		return functionSortKey(callees[i]) < functionSortKey(callees[j])
	})
	return callees
}

func newCallableTargetTracer(analysis *loadedAnalysis) *callableTargetTracer {
	return &callableTargetTracer{
		analysis:       analysis,
		visiting:       make(map[ssa.Value]bool),
		memo:           make(map[ssa.Value]callableTargetResult),
		resultVisiting: make(map[callableResult]bool),
		resultMemo:     make(map[callableResult]callableTargetResult),
	}
}

func (tracer *callableTargetTracer) trace(value ssa.Value) callableTargetResult {
	if value == nil {
		return callableTargetResult{}
	}
	if result, ok := tracer.memo[value]; ok {
		return result
	}
	if tracer.visiting[value] {
		// Provenance-preserving cycles contribute no new source by themselves.
		// Every node on the enclosing path must still prove that all of its
		// non-cycle inputs are closed before this seed can close the result.
		return closedEmptyCallableTargets()
	}
	tracer.visiting[value] = true
	result := tracer.inspect(value)
	delete(tracer.visiting, value)
	tracer.memo[value] = result
	return result
}

func (tracer *callableTargetTracer) inspect(value ssa.Value) callableTargetResult {
	switch value := value.(type) {
	case *ssa.Function:
		return callableTargets(value)
	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)
		if function == nil {
			return callableTargetResult{}
		}
		return callableTargets(function)
	case *ssa.Builtin:
		return callableTargetResult{targets: make(map[*ssa.Function]bool), closed: true}
	case *ssa.Const:
		return callableTargetResult{targets: make(map[*ssa.Function]bool), closed: value.IsNil()}
	case *ssa.Parameter:
		return tracer.traceParameter(value)
	case *ssa.FreeVar:
		return tracer.traceAll(tracer.reachableFreeVariableBindings(value))
	case *ssa.Global:
		return tracer.traceGlobal(value)
	case *ssa.UnOp:
		if value.Op != token.MUL {
			return callableTargetResult{}
		}
		return tracer.trace(value.X)
	case *ssa.ChangeType:
		if typeUsesUnsafePointer(value.Type()) || typeUsesUnsafePointer(value.X.Type()) {
			return callableTargetResult{}
		}
		return tracer.trace(value.X)
	case *ssa.ChangeInterface:
		return tracer.trace(value.X)
	case *ssa.Convert:
		if typeUsesUnsafePointer(value.Type()) || typeUsesUnsafePointer(value.X.Type()) {
			return callableTargetResult{}
		}
		return tracer.trace(value.X)
	case *ssa.MakeInterface:
		return tracer.trace(value.X)
	case *ssa.TypeAssert:
		return tracer.trace(value.X)
	case *ssa.Extract:
		if call, ok := value.Tuple.(*ssa.Call); ok {
			return tracer.traceCallResult(call, value.Index)
		}
		return tracer.trace(value.Tuple)
	case *ssa.Call:
		return tracer.traceCallResult(value, 0)
	case *ssa.Phi:
		return tracer.traceAll(value.Edges)
	case *ssa.Alloc:
		return tracer.traceAllocation(value)
	case *ssa.Field:
		return tracer.traceField(value.X.Type(), value.Field)
	case *ssa.FieldAddr:
		return tracer.traceField(value.X.Type(), value.Field)
	case *ssa.Index:
		return tracer.trace(value.X)
	case *ssa.IndexAddr:
		return tracer.trace(value.X)
	default:
		return callableTargetResult{}
	}
}

func (tracer *callableTargetTracer) traceCallResult(call *ssa.Call, resultIndex int) callableTargetResult {
	if call == nil || resultIndex < 0 {
		return callableTargetResult{}
	}
	key := callableResult{call: call, index: resultIndex}
	if result, ok := tracer.resultMemo[key]; ok {
		return result
	}
	if tracer.resultVisiting[key] {
		return closedEmptyCallableTargets()
	}
	tracer.resultVisiting[key] = true
	result := tracer.inspectCallResult(call, resultIndex)
	delete(tracer.resultVisiting, key)
	tracer.resultMemo[key] = result
	return result
}

func (tracer *callableTargetTracer) inspectCallResult(call *ssa.Call, resultIndex int) callableTargetResult {
	common := call.Common()
	if common == nil {
		return callableTargetResult{}
	}
	if builtin, ok := common.Value.(*ssa.Builtin); ok {
		if builtin.Name() != "append" || resultIndex != 0 || len(common.Args) == 0 {
			return callableTargetResult{}
		}
		return tracer.traceAppend(call)
	}
	parent := call.Parent()
	if parent == nil {
		return callableTargetResult{}
	}
	callees := tracer.analysis.closedWorldCallees(parent, call)
	if len(callees) == 0 || reflectionTarget(callees) != "" {
		return callableTargetResult{}
	}
	result := closedEmptyCallableTargets()
	for _, callee := range callees {
		if !tracer.analysis.calleeCoveredByAuthoredSource(callee, make(map[*ssa.Function]bool)) {
			result.closed = false
			continue
		}
		values, ok := callableCalleeResultValues(callee, resultIndex)
		if !ok {
			result.closed = false
			continue
		}
		result.merge(tracer.traceAll(values))
	}
	return result
}

func callableCalleeResultValues(callee *ssa.Function, resultIndex int) ([]ssa.Value, bool) {
	if callee == nil || resultIndex < 0 {
		return nil, false
	}
	function := callee
	if len(function.Blocks) == 0 {
		if origin := function.Origin(); origin != nil {
			function = origin
		}
	}
	if len(function.Blocks) == 0 {
		return nil, false
	}
	found := false
	var values []ssa.Value
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			returned, ok := instruction.(*ssa.Return)
			if !ok {
				continue
			}
			if resultIndex >= len(returned.Results) {
				return nil, false
			}
			found = true
			values = append(values, returned.Results[resultIndex])
		}
	}
	return values, found
}

func (tracer *callableTargetTracer) traceAppend(call *ssa.Call) callableTargetResult {
	common := call.Common()
	if common == nil || len(common.Args) == 0 {
		return callableTargetResult{}
	}
	result := closedEmptyCallableTargets()
	result.merge(tracer.trace(common.Args[0]))
	for _, argument := range common.Args[1:] {
		result.merge(tracer.traceAppendArgument(call, argument))
	}
	return result
}

func (tracer *callableTargetTracer) traceAppendArgument(call *ssa.Call, argument ssa.Value) callableTargetResult {
	if constant, ok := argument.(*ssa.Const); ok && constant.IsNil() {
		return closedEmptyCallableTargets()
	}
	view, ok := argument.(*ssa.Slice)
	if !ok {
		return callableTargetResult{}
	}
	values, closed := callableVarargsValues(call, view)
	if !closed {
		return callableTargetResult{}
	}
	if len(values) == 0 {
		return closedEmptyCallableTargets()
	}
	return tracer.traceAll(values)
}

func callableVarargsValues(call *ssa.Call, view *ssa.Slice) ([]ssa.Value, bool) {
	if call == nil || view == nil || view.Referrers() == nil {
		return nil, false
	}
	for _, referrer := range *view.Referrers() {
		switch referrer := referrer.(type) {
		case *ssa.Call:
			if referrer != call {
				return nil, false
			}
		case *ssa.DebugRef:
		default:
			return nil, false
		}
	}
	allocation, ok := view.X.(*ssa.Alloc)
	if !ok || allocation.Referrers() == nil || !callableArrayType(allocation.Type()) {
		return nil, false
	}
	var values []ssa.Value
	for _, referrer := range *allocation.Referrers() {
		switch referrer := referrer.(type) {
		case *ssa.IndexAddr:
			if referrer.Referrers() == nil {
				return nil, false
			}
			for _, elementReferrer := range *referrer.Referrers() {
				switch elementReferrer := elementReferrer.(type) {
				case *ssa.Store:
					if elementReferrer.Addr != referrer {
						return nil, false
					}
					values = append(values, elementReferrer.Val)
				case *ssa.UnOp, *ssa.DebugRef:
				default:
					return nil, false
				}
			}
		case *ssa.Slice:
			if referrer != view {
				return nil, false
			}
		case *ssa.DebugRef:
		default:
			return nil, false
		}
	}
	return values, true
}

func callableArrayType(candidate types.Type) bool {
	pointer, ok := types.Unalias(candidate).Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	array, ok := types.Unalias(pointer.Elem()).Underlying().(*types.Array)
	if !ok {
		return false
	}
	_, ok = types.Unalias(array.Elem()).Underlying().(*types.Signature)
	return ok
}

func (tracer *callableTargetTracer) callableFieldUsesClosed(object *types.Var) bool {
	if object == nil || !callableFieldType(object.Type()) {
		return false
	}
	addresses := tracer.analysis.fieldAddresses[object]
	if !fieldAddressesHaveDirectUsesOnly(addresses) {
		return false
	}
	_, isCallableSlice := types.Unalias(object.Type()).Underlying().(*types.Slice)
	for _, address := range addresses {
		for _, referrer := range *address.Referrers() {
			load, ok := referrer.(*ssa.UnOp)
			if ok && isCallableSlice && !callableSliceUsesClosed(load, object, make(map[ssa.Value]bool)) {
				return false
			}
		}
	}
	return true
}

func fieldAddressesHaveDirectUsesOnly(addresses []*ssa.FieldAddr) bool {
	for _, address := range addresses {
		if address == nil || address.Referrers() == nil {
			return false
		}
		for _, referrer := range *address.Referrers() {
			switch referrer := referrer.(type) {
			case *ssa.Store:
				if referrer.Addr != address {
					return false
				}
			case *ssa.UnOp:
				if referrer.X != address || referrer.Op != token.MUL {
					return false
				}
			case *ssa.DebugRef:
			default:
				return false
			}
		}
	}
	return true
}

func callableFieldType(candidate types.Type) bool {
	if candidate == nil {
		return false
	}
	underlying := types.Unalias(candidate).Underlying()
	if _, ok := underlying.(*types.Signature); ok {
		return true
	}
	slice, ok := underlying.(*types.Slice)
	if !ok {
		return false
	}
	_, ok = types.Unalias(slice.Elem()).Underlying().(*types.Signature)
	return ok
}

func callableSliceUsesClosed(value ssa.Value, field *types.Var, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return true
	}
	visiting[value] = true
	defer delete(visiting, value)
	if value.Referrers() == nil {
		return true
	}
	for _, referrer := range *value.Referrers() {
		switch referrer := referrer.(type) {
		case *ssa.Index:
		case *ssa.IndexAddr:
			if !callableIndexAddressReadOnly(referrer) {
				return false
			}
		case *ssa.BinOp:
			if referrer.Op != token.EQL && referrer.Op != token.NEQ {
				return false
			}
		case *ssa.Call:
			builtin, ok := referrer.Common().Value.(*ssa.Builtin)
			if !ok {
				return false
			}
			switch builtin.Name() {
			case "len", "cap":
			case "append":
				if !callableAppendStoredBackInField(referrer, field) {
					return false
				}
			default:
				return false
			}
		case *ssa.ChangeType:
			if typeUsesUnsafePointer(referrer.Type()) || typeUsesUnsafePointer(referrer.X.Type()) ||
				!callableSliceUsesClosed(referrer, field, visiting) {
				return false
			}
		case *ssa.Convert:
			if typeUsesUnsafePointer(referrer.Type()) || typeUsesUnsafePointer(referrer.X.Type()) ||
				!callableSliceUsesClosed(referrer, field, visiting) {
				return false
			}
		case *ssa.Slice, *ssa.Phi, *ssa.ChangeInterface, *ssa.MakeInterface, *ssa.TypeAssert:
			projected, ok := referrer.(ssa.Value)
			if !ok || !callableSliceUsesClosed(projected, field, visiting) {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}

func callableIndexAddressReadOnly(address *ssa.IndexAddr) bool {
	if address == nil || address.Referrers() == nil {
		return false
	}
	for _, referrer := range *address.Referrers() {
		switch referrer := referrer.(type) {
		case *ssa.UnOp:
			if referrer.X != address || referrer.Op != token.MUL {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}

func callableAppendStoredBackInField(call *ssa.Call, field *types.Var) bool {
	if call == nil || field == nil || call.Referrers() == nil {
		return false
	}
	found := false
	for _, referrer := range *call.Referrers() {
		switch referrer := referrer.(type) {
		case *ssa.Store:
			address, ok := referrer.Addr.(*ssa.FieldAddr)
			if !ok || fieldObject(address.X.Type(), address.Field) != field {
				return false
			}
			found = true
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return found
}

func (tracer *callableTargetTracer) traceField(containerType types.Type, field int) callableTargetResult {
	object := tracer.analysis.privateModuleField(containerType, field)
	if object == nil {
		return callableTargetResult{}
	}
	if !tracer.callableFieldUsesClosed(object) {
		return callableTargetResult{}
	}
	stores := tracer.analysis.fieldStores[object]
	if len(stores) == 0 {
		return closedEmptyCallableTargets()
	}
	return tracer.traceAll(stores)
}

func (analysis *loadedAnalysis) privateModuleField(containerType types.Type, field int) *types.Var {
	object := fieldObject(containerType, field)
	if object == nil || object.Exported() || object.Pkg() == nil ||
		(object.Pkg().Path() != analysis.config.ModulePath &&
			!strings.HasPrefix(object.Pkg().Path(), analysis.config.ModulePath+"/")) {
		return nil
	}
	return object
}

func closedEmptyCallableTargets() callableTargetResult {
	return callableTargetResult{targets: make(map[*ssa.Function]bool), closed: true}
}

func (result *callableTargetResult) merge(candidate callableTargetResult) {
	if result.targets == nil {
		result.targets = make(map[*ssa.Function]bool)
	}
	if !candidate.closed {
		result.closed = false
	}
	for target := range candidate.targets {
		result.targets[target] = true
	}
}

func callableTargets(function *ssa.Function) callableTargetResult {
	return callableTargetResult{
		targets: map[*ssa.Function]bool{function: true},
		closed:  true,
	}
}

func (tracer *callableTargetTracer) traceAll(values []ssa.Value) callableTargetResult {
	if len(values) == 0 {
		return callableTargetResult{}
	}
	result := closedEmptyCallableTargets()
	for _, value := range values {
		result.merge(tracer.trace(value))
	}
	return result
}

func (tracer *callableTargetTracer) traceParameter(parameter *ssa.Parameter) callableTargetResult {
	parent := parameter.Parent()
	if parent == nil {
		return callableTargetResult{}
	}
	index := -1
	for candidateIndex, candidate := range parent.Params {
		if candidate == parameter {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return callableTargetResult{}
	}
	node := tracer.analysis.callGraph.Nodes[parent]
	if node == nil {
		return callableTargetResult{}
	}
	var actuals []ssa.Value
	for _, edge := range node.In {
		if edge == nil || edge.Caller == nil || edge.Caller.Func == nil ||
			!tracer.analysis.executionFunction(edge.Caller.Func) {
			continue
		}
		if edge.Site == nil {
			return callableTargetResult{}
		}
		actual, ok := callArgumentForParameter(edge.Site, parent, index)
		if !ok {
			return callableTargetResult{}
		}
		actuals = append(actuals, actual)
	}
	return tracer.traceAll(actuals)
}

func (tracer *callableTargetTracer) traceGlobal(global *ssa.Global) callableTargetResult {
	if global == nil {
		return callableTargetResult{}
	}
	var stores []ssa.Value
	for _, instruction := range tracer.analysis.globalUses[global] {
		switch instruction := instruction.(type) {
		case *ssa.Store:
			if instruction.Addr != global {
				return callableTargetResult{}
			}
			stores = append(stores, instruction.Val)
		case *ssa.UnOp:
			if instruction.X != global || instruction.Op != token.MUL {
				return callableTargetResult{}
			}
		case *ssa.DebugRef:
		default:
			return callableTargetResult{}
		}
	}
	return tracer.traceAll(stores)
}

func (tracer *callableTargetTracer) traceAllocation(allocation *ssa.Alloc) callableTargetResult {
	if allocation == nil || allocation.Referrers() == nil {
		return callableTargetResult{}
	}
	var stores []ssa.Value
	for _, instruction := range *allocation.Referrers() {
		switch instruction := instruction.(type) {
		case *ssa.Store:
			if instruction.Addr != allocation {
				return callableTargetResult{}
			}
			stores = append(stores, instruction.Val)
		case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure:
		default:
			return callableTargetResult{}
		}
	}
	return tracer.traceAll(stores)
}

func (tracer *callableTargetTracer) reachableFreeVariableBindings(freeVariable *ssa.FreeVar) []ssa.Value {
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil || !tracer.analysis.executionFunction(owner.Parent()) {
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

func refineClosedWorldExecution(analysis *loadedAnalysis, roots []*ssa.Package) {
	for {
		analysis.effectFuncs = sourceFunctionsInSet(analysis.executionFuncs, analysis.sourceFuncs)
		analysis.globalUses = collectSourceGlobalUses(analysis.effectFuncs)
		fieldEvidence := collectSourceFieldEvidence(analysis.effectFuncs)
		analysis.fieldStores = fieldEvidence.stores
		analysis.fieldAddresses = fieldEvidence.addresses
		next := functionsReachableFromEntries(analysis, rootEntryFunctions(roots, true))
		if sameFunctionSet(next, analysis.executionFuncs) {
			return
		}
		analysis.executionFuncs = next
	}
}

func functionsReachableFromEntries(analysis *loadedAnalysis, entries []*ssa.Function) map[*ssa.Function]bool {
	reachable := make(map[*ssa.Function]bool)
	var visit func(*ssa.Function)
	visit = func(function *ssa.Function) {
		if function == nil || reachable[function] {
			return
		}
		reachable[function] = true
		if origin := function.Origin(); origin != nil {
			reachable[origin] = true
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok {
					continue
				}
				for _, callee := range analysis.closedWorldCallees(function, call) {
					visit(callee)
				}
			}
		}
	}
	for _, entry := range entries {
		visit(entry)
	}
	return reachable
}

func rootEntryFunctions(packages []*ssa.Package, includeMain bool) []*ssa.Function {
	var entries []*ssa.Function
	for _, pkg := range packages {
		if pkg == nil {
			continue
		}
		entries = append(entries, pkg.Func("init"))
		if includeMain {
			entries = append(entries, pkg.Func("main"))
		}
	}
	return entries
}

func sameFunctionSet(left, right map[*ssa.Function]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for function := range left {
		if !right[function] {
			return false
		}
	}
	return true
}
