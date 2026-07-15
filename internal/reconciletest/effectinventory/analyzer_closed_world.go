package effectinventory

import (
	"go/token"
	"sort"

	"golang.org/x/tools/go/ssa"
)

type callableTargetResult struct {
	targets map[*ssa.Function]bool
	closed  bool
}

type callableTargetTracer struct {
	analysis *loadedAnalysis
	visiting map[ssa.Value]bool
	memo     map[ssa.Value]callableTargetResult
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
		analysis: analysis,
		visiting: make(map[ssa.Value]bool),
		memo:     make(map[ssa.Value]callableTargetResult),
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
		return callableTargetResult{}
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
	case *ssa.Phi:
		return tracer.traceAll(value.Edges)
	case *ssa.Alloc:
		return tracer.traceAllocation(value)
	case *ssa.Field:
		return tracer.traceAll(localFieldStoredValues(value.X, value.Field))
	case *ssa.FieldAddr:
		return tracer.traceAll(localFieldStoredValues(value.X, value.Field))
	default:
		return callableTargetResult{}
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
	result := callableTargetResult{targets: make(map[*ssa.Function]bool), closed: true}
	for _, value := range values {
		candidate := tracer.trace(value)
		if !candidate.closed {
			result.closed = false
		}
		for target := range candidate.targets {
			result.targets[target] = true
		}
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
