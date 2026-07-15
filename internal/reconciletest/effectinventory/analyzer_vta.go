package effectinventory

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// callableVTATargetSetClosed proves that VTA's targets are a complete,
// authored set for this function-value call. VTA target presence alone is not
// a proof: exported parameters, escaped storage, reflection, unsafe ancestry,
// and uncovered callees all keep the call open-world.
func (analysis *loadedAnalysis) callableVTATargetSetClosed(call ssa.CallInstruction, callees []*ssa.Function) bool {
	if call == nil || call.Common() == nil || call.Common().IsInvoke() || len(callees) == 0 {
		return false
	}
	if reflectionTarget(callees) != "" {
		return false
	}
	for _, callee := range callees {
		if !analysis.calleeCoveredByAuthoredSource(callee, make(map[*ssa.Function]bool)) {
			return false
		}
	}
	proof := callableSourceProof{
		analysis: analysis,
		states:   make(map[ssa.Value]callableProofState),
		results:  make(map[callableResult]callableProofState),
	}
	return proof.closed(call.Common().Value)
}

type callableProofState uint8

const (
	callableProofUnknown callableProofState = iota
	callableProofVisiting
	callableProofOpen
	callableProofClosed
)

type callableResult struct {
	call  *ssa.Call
	index int
}

type callableSourceProof struct {
	analysis *loadedAnalysis
	states   map[ssa.Value]callableProofState
	results  map[callableResult]callableProofState
}

func (proof *callableSourceProof) closed(value ssa.Value) bool {
	if value == nil {
		return false
	}
	switch proof.states[value] {
	case callableProofClosed:
		return true
	case callableProofOpen, callableProofVisiting:
		return false
	}
	proof.states[value] = callableProofVisiting
	closed := proof.inspect(value)
	if closed {
		proof.states[value] = callableProofClosed
	} else {
		proof.states[value] = callableProofOpen
	}
	return closed
}

func (proof *callableSourceProof) inspect(value ssa.Value) bool {
	switch value := value.(type) {
	case *ssa.Function, *ssa.Builtin:
		return true
	case *ssa.Const:
		return value.IsNil()
	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)
		return function != nil && proof.analysis.calleeCoveredByAuthoredSource(function, make(map[*ssa.Function]bool))
	case *ssa.Parameter:
		return proof.parameterClosed(value)
	case *ssa.FreeVar:
		bindings := freeVariableBindings(value)
		return proof.allClosed(bindings)
	case *ssa.Global:
		return proof.globalClosed(value)
	case *ssa.UnOp:
		return value.Op == token.MUL && proof.closed(value.X)
	case *ssa.ChangeType:
		return !typeUsesUnsafePointer(value.Type()) && !typeUsesUnsafePointer(value.X.Type()) && proof.closed(value.X)
	case *ssa.ChangeInterface:
		return proof.closed(value.X)
	case *ssa.Convert:
		return !typeUsesUnsafePointer(value.Type()) && !typeUsesUnsafePointer(value.X.Type()) && proof.closed(value.X)
	case *ssa.MakeInterface:
		return proof.closed(value.X)
	case *ssa.TypeAssert:
		return proof.closed(value.X)
	case *ssa.Extract:
		if call, ok := value.Tuple.(*ssa.Call); ok {
			return proof.callResultClosed(call, value.Index)
		}
		return proof.closed(value.Tuple)
	case *ssa.Call:
		return proof.callResultClosed(value, 0)
	case *ssa.Phi:
		return proof.allClosed(value.Edges)
	case *ssa.Alloc:
		return proof.allocationClosed(value)
	case *ssa.Field:
		return proof.fieldClosed(value.X, value.Field)
	case *ssa.FieldAddr:
		return proof.fieldClosed(value.X, value.Field)
	default:
		return false
	}
}

func (proof *callableSourceProof) allClosed(values []ssa.Value) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !proof.closed(value) {
			return false
		}
	}
	return true
}

func (proof *callableSourceProof) parameterClosed(parameter *ssa.Parameter) bool {
	parent := parameter.Parent()
	if parent == nil || functionExternallyNameable(parent) || functionValueEscapesAuthoredUniverse(parent, proof.analysis) {
		return false
	}
	index := -1
	for candidateIndex, candidate := range parent.Params {
		if candidate == parameter {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return false
	}
	node := proof.analysis.callGraph.Nodes[parent]
	if node == nil || len(node.In) == 0 {
		return false
	}
	found := false
	for _, edge := range node.In {
		if edge != nil && edge.Caller != nil && edge.Caller.Func != nil && proof.analysis.config.closedWorld && !proof.analysis.executionFunction(edge.Caller.Func) {
			continue
		}
		if edge == nil || edge.Site == nil || edge.Caller == nil || edge.Caller.Func == nil {
			return false
		}
		if !proof.analysis.calleeCoveredByAuthoredSource(edge.Caller.Func, make(map[*ssa.Function]bool)) {
			return false
		}
		argument, ok := callArgumentForParameter(edge.Site, parent, index)
		if !ok || !proof.closed(argument) {
			return false
		}
		found = true
	}
	return found
}

func functionExternallyNameable(function *ssa.Function) bool {
	if function == nil {
		return true
	}
	if origin := function.Origin(); origin != nil {
		function = origin
	}
	object, _ := function.Object().(*types.Func)
	return object != nil && object.Exported()
}

func functionValueEscapesAuthoredUniverse(function *ssa.Function, analysis *loadedAnalysis) bool {
	if function == nil {
		return true
	}
	referrers := function.Referrers()
	if referrers == nil {
		return false
	}
	for _, instruction := range *referrers {
		switch instruction := instruction.(type) {
		case ssa.CallInstruction:
			common := instruction.Common()
			if common == nil || (common.Value != function && common.StaticCallee() != function) {
				return true
			}
			caller := instruction.Parent()
			if caller == nil || !analysis.calleeCoveredByAuthoredSource(caller, make(map[*ssa.Function]bool)) {
				return true
			}
		case *ssa.MakeClosure:
			if instruction.Fn != function {
				return true
			}
			if callableValueEscapesAuthoredUniverse(instruction, analysis) {
				return true
			}
		case *ssa.DebugRef:
		default:
			return true
		}
	}
	return false
}

func callableValueEscapesAuthoredUniverse(value ssa.Value, analysis *loadedAnalysis) bool {
	if value == nil || value.Referrers() == nil {
		return true
	}
	foundUse := false
	for _, instruction := range *value.Referrers() {
		switch instruction := instruction.(type) {
		case ssa.CallInstruction:
			common := instruction.Common()
			caller := instruction.Parent()
			if common == nil || common.Value != value || caller == nil ||
				!analysis.calleeCoveredByAuthoredSource(caller, make(map[*ssa.Function]bool)) {
				return true
			}
			foundUse = true
		case *ssa.DebugRef:
		default:
			return true
		}
	}
	return !foundUse
}

func callArgumentForParameter(site ssa.CallInstruction, callee *ssa.Function, parameterIndex int) (ssa.Value, bool) {
	if site == nil || callee == nil || parameterIndex < 0 || parameterIndex >= len(callee.Params) {
		return nil, false
	}
	common := site.Common()
	if common == nil {
		return nil, false
	}
	if len(common.Args) == len(callee.Params) {
		return common.Args[parameterIndex], true
	}
	if common.IsInvoke() && len(common.Args)+1 == len(callee.Params) {
		if parameterIndex == 0 {
			return common.Value, true
		}
		return common.Args[parameterIndex-1], true
	}
	return nil, false
}

func (proof *callableSourceProof) globalClosed(global *ssa.Global) bool {
	if global == nil || global.Object() == nil || global.Object().Exported() {
		return false
	}
	uses := proof.analysis.globalUses[global]
	if len(uses) == 0 {
		return false
	}
	foundStore := false
	for _, instruction := range uses {
		switch instruction := instruction.(type) {
		case *ssa.Store:
			if instruction.Addr != global || !proof.closed(instruction.Val) {
				return false
			}
			foundStore = true
		case *ssa.UnOp:
			if instruction.X != global || instruction.Op != token.MUL {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return foundStore
}

func collectSourceGlobalUses(functions map[*ssa.Function]bool) map[*ssa.Global][]ssa.Instruction {
	uses := make(map[*ssa.Global][]ssa.Instruction)
	for function := range functions {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				seen := make(map[*ssa.Global]bool)
				for _, operand := range instruction.Operands(nil) {
					if operand == nil {
						continue
					}
					global, ok := (*operand).(*ssa.Global)
					if !ok || seen[global] {
						continue
					}
					seen[global] = true
					uses[global] = append(uses[global], instruction)
				}
			}
		}
	}
	return uses
}

func (proof *callableSourceProof) allocationClosed(allocation *ssa.Alloc) bool {
	if allocation == nil || allocation.Referrers() == nil {
		return false
	}
	foundStore := false
	for _, instruction := range *allocation.Referrers() {
		switch instruction := instruction.(type) {
		case *ssa.Store:
			if instruction.Addr != allocation || !proof.closed(instruction.Val) {
				return false
			}
			foundStore = true
		case *ssa.UnOp:
			if instruction.X != allocation || instruction.Op != token.MUL {
				return false
			}
		case *ssa.MakeClosure:
			if !proof.capturedSlotClosed(instruction, allocation) {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return foundStore
}

func (proof *callableSourceProof) capturedSlotClosed(closure *ssa.MakeClosure, binding ssa.Value) bool {
	if closure == nil || binding == nil {
		return false
	}
	function, _ := closure.Fn.(*ssa.Function)
	if function == nil || !proof.analysis.calleeCoveredByAuthoredSource(function, make(map[*ssa.Function]bool)) {
		return false
	}
	foundBinding := false
	for index, candidate := range closure.Bindings {
		if candidate != binding {
			continue
		}
		if index >= len(function.FreeVars) || !proof.capturedFreeVarClosed(function.FreeVars[index]) {
			return false
		}
		foundBinding = true
	}
	return foundBinding
}

func (proof *callableSourceProof) capturedFreeVarClosed(freeVariable *ssa.FreeVar) bool {
	if freeVariable == nil || freeVariable.Referrers() == nil {
		return false
	}
	for _, instruction := range *freeVariable.Referrers() {
		switch instruction := instruction.(type) {
		case *ssa.Store:
			if instruction.Addr != freeVariable || !proof.closed(instruction.Val) {
				return false
			}
		case *ssa.UnOp:
			if instruction.X != freeVariable || instruction.Op != token.MUL {
				return false
			}
		case *ssa.MakeClosure:
			if !proof.capturedSlotClosed(instruction, freeVariable) {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}

func (proof *callableSourceProof) fieldClosed(base ssa.Value, field int) bool {
	if openWorldFunctionField(base, field, make(map[ssa.Value]bool)) {
		return false
	}
	return proof.allClosed(localFieldStoredValues(base, field))
}

func (proof *callableSourceProof) callResultClosed(call *ssa.Call, resultIndex int) bool {
	if call == nil || resultIndex < 0 {
		return false
	}
	key := callableResult{call: call, index: resultIndex}
	switch proof.results[key] {
	case callableProofClosed:
		return true
	case callableProofOpen, callableProofVisiting:
		return false
	}
	proof.results[key] = callableProofVisiting
	closed := proof.inspectCallResult(call, resultIndex)
	if closed {
		proof.results[key] = callableProofClosed
	} else {
		proof.results[key] = callableProofOpen
	}
	return closed
}

func (proof *callableSourceProof) inspectCallResult(call *ssa.Call, resultIndex int) bool {
	parent := call.Parent()
	if parent == nil {
		return false
	}
	callees := proof.analysis.closedWorldCallees(parent, call)
	if len(callees) == 0 || reflectionTarget(callees) != "" {
		return false
	}
	for _, callee := range callees {
		if !proof.analysis.calleeCoveredByAuthoredSource(callee, make(map[*ssa.Function]bool)) || !proof.calleeResultClosed(callee, resultIndex) {
			return false
		}
	}
	return true
}

func (proof *callableSourceProof) calleeResultClosed(callee *ssa.Function, resultIndex int) bool {
	if callee == nil {
		return false
	}
	function := callee
	if len(function.Blocks) == 0 {
		if origin := function.Origin(); origin != nil {
			function = origin
		}
	}
	if len(function.Blocks) == 0 {
		return false
	}
	found := false
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			returned, ok := instruction.(*ssa.Return)
			if !ok {
				continue
			}
			if resultIndex >= len(returned.Results) || !proof.closed(returned.Results[resultIndex]) {
				return false
			}
			found = true
		}
	}
	return found
}

func typeUsesUnsafePointer(candidate types.Type) bool {
	if candidate == nil {
		return false
	}
	basic, ok := types.Unalias(candidate).Underlying().(*types.Basic)
	return ok && basic.Kind() == types.UnsafePointer
}
