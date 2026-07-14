package effectinventory

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

func collectSelectOperations(roots []*packages.Package) map[token.Pos]OperationKind {
	operations := make(map[token.Pos]OperationKind)
	for _, pkg := range roots {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				selectStatement, ok := node.(*ast.SelectStmt)
				if !ok {
					return true
				}
				for _, statement := range selectStatement.Body.List {
					clause, ok := statement.(*ast.CommClause)
					if !ok || clause.Comm == nil {
						continue
					}
					switch communication := clause.Comm.(type) {
					case *ast.SendStmt:
						operations[communication.Arrow] = OperationSelectSend
					case *ast.ExprStmt:
						if receive := receiveExpression(communication.X); receive != nil {
							operations[receive.OpPos] = OperationSelectReceive
						}
					case *ast.AssignStmt:
						for _, expression := range communication.Rhs {
							if receive := receiveExpression(expression); receive != nil {
								operations[receive.OpPos] = OperationSelectReceive
							}
						}
					}
				}
				return true
			})
		}
	}
	return operations
}

func receiveExpression(expression ast.Expr) *ast.UnaryExpr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			break
		}
		expression = parenthesized.X
	}
	receive, ok := expression.(*ast.UnaryExpr)
	if !ok || receive.Op != token.ARROW {
		return nil
	}
	return receive
}

func (analysis *loadedAnalysis) observeChannelInstruction(function *ssa.Function, instruction ssa.Instruction, boundaries []resolvedBoundary) ([]observedCall, []string) {
	type operation struct {
		kind     OperationKind
		channel  ssa.Value
		position token.Pos
	}
	var operations []operation
	switch instruction := instruction.(type) {
	case *ssa.Send:
		kind := OperationChannelSend
		if selected, ok := analysis.selectOps[instruction.Pos()]; ok {
			kind = selected
		}
		operations = append(operations, operation{kind: kind, channel: instruction.Chan, position: instruction.Pos()})
	case *ssa.UnOp:
		if instruction.Op != token.ARROW {
			return nil, nil
		}
		kind := OperationChannelReceive
		if selected, ok := analysis.selectOps[instruction.Pos()]; ok {
			kind = selected
		}
		operations = append(operations, operation{kind: kind, channel: instruction.X, position: instruction.Pos()})
	case *ssa.Select:
		for _, state := range instruction.States {
			kind := analysis.selectOps[state.Pos]
			if kind == "" {
				if state.Dir == types.SendOnly {
					kind = OperationSelectSend
				} else {
					kind = OperationSelectReceive
				}
			}
			operations = append(operations, operation{kind: kind, channel: state.Chan, position: state.Pos})
		}
	default:
		return nil, nil
	}

	var observed []observedCall
	var problems []string
	for _, operation := range operations {
		if !operation.position.IsValid() {
			continue
		}
		ref, err := analysis.functionRef(function, operation.position)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		provenance := analysis.traceChannel(operation.channel, boundaries, nil, make(map[ssa.Value]bool))
		matches := provenance.sortedMatches()
		if len(matches) > 1 {
			problems = append(problems, fmt.Sprintf("%s: channel operation matches multiple boundaries: %s", ref.key(), strings.Join(matches, ", ")))
			continue
		}
		compatible := hasCompatibleChannelBoundary(operation.channel.Type(), boundaries)
		if provenance.unsafe && compatible {
			problems = append(problems, fmt.Sprintf("%s: unsafe channel provenance cannot be inventoried statically", ref.key()))
		}
		if provenance.openWorld && compatible {
			problems = append(problems, fmt.Sprintf("%s: unresolved channel operation has open-world provenance compatible with an inventoried boundary", ref.key()))
		}
		if len(matches) == 1 && !provenance.openWorld && !provenance.unsafe {
			if analysis.initReachable[function] {
				problems = append(problems, fmt.Sprintf("%s: effectful package initialization route has no injective FunctionRef", ref.key()))
				continue
			}
			observed = append(observed, observedCall{
				boundaryID: matches[0],
				function:   ref,
				operation:  operation.kind,
				position:   operation.position,
			})
		}
	}
	return observed, problems
}

type channelProvenance struct {
	matches   map[string]bool
	openWorld bool
	unsafe    bool
}

func newChannelProvenance() channelProvenance {
	return channelProvenance{matches: make(map[string]bool)}
}

func (provenance *channelProvenance) merge(other channelProvenance) {
	for boundaryID := range other.matches {
		provenance.matches[boundaryID] = true
	}
	provenance.openWorld = provenance.openWorld || other.openWorld
	provenance.unsafe = provenance.unsafe || other.unsafe
}

func (provenance channelProvenance) sortedMatches() []string {
	result := make([]string, 0, len(provenance.matches))
	for boundaryID := range provenance.matches {
		result = append(result, boundaryID)
	}
	sort.Strings(result)
	return result
}

func hasCompatibleChannelBoundary(channelType types.Type, boundaries []resolvedBoundary) bool {
	for _, boundary := range boundaries {
		if boundary.definition.Match == ObjectMatchChannel && boundary.channel != nil && channelTypesCompatible(channelType, boundary.channel) {
			return true
		}
	}
	return false
}

func channelTypesCompatible(left, right types.Type) bool {
	left = types.Unalias(left)
	right = types.Unalias(right)
	if leftParameter, ok := left.(*types.TypeParam); ok {
		if constraint, ok := leftParameter.Constraint().Underlying().(*types.Interface); ok && types.Satisfies(right, constraint) {
			return true
		}
	}
	if rightParameter, ok := right.(*types.TypeParam); ok {
		if constraint, ok := rightParameter.Constraint().Underlying().(*types.Interface); ok && types.Satisfies(left, constraint) {
			return true
		}
	}
	if types.Identical(left, right) || types.AssignableTo(left, right) || types.AssignableTo(right, left) {
		return true
	}
	return channelLike(left) && channelLike(right) && (types.ConvertibleTo(left, right) || types.ConvertibleTo(right, left))
}

func channelLike(value types.Type) bool {
	if _, ok := value.(*types.TypeParam); ok {
		return true
	}
	_, ok := value.Underlying().(*types.Chan)
	return ok
}

func (analysis *loadedAnalysis) traceChannel(value ssa.Value, boundaries []resolvedBoundary, bindings map[*ssa.Parameter]ssa.Value, visiting map[ssa.Value]bool) channelProvenance {
	provenance := newChannelProvenance()
	if value == nil {
		return provenance
	}
	if visiting[value] {
		// Provenance is a monotone union. Returning the empty set on a
		// back-edge lets non-recursive branches provide the fixed-point seeds.
		return provenance
	}
	visiting[value] = true
	defer delete(visiting, value)

	for _, boundary := range boundaries {
		if boundary.definition.Match == ObjectMatchChannel && boundary.definition.Output.zero() && channelValueObject(value) == boundary.object {
			provenance.matches[boundary.definition.ID] = true
		}
	}
	if len(provenance.matches) != 0 {
		provenance.unsafe = channelValueHasUnsafeAncestry(value, make(map[ssa.Value]bool))
		return provenance
	}

	mergeValue := func(candidate ssa.Value) {
		provenance.merge(analysis.traceChannel(candidate, boundaries, bindings, visiting))
	}
	switch value := value.(type) {
	case *ssa.Parameter:
		if actual := bindings[value]; actual != nil {
			mergeValue(actual)
		} else {
			provenance.openWorld = true
		}
	case *ssa.FreeVar:
		provenance.merge(analysis.traceChannelFreeVar(value, boundaries, bindings, visiting))
	case *ssa.Global:
		provenance.openWorld = true
	case *ssa.MakeChan, *ssa.Const:
		// Locally created channels and nil are closed-world non-boundaries.
	case *ssa.UnOp:
		mergeValue(value.X)
	case *ssa.FieldAddr:
		provenance.merge(analysis.traceChannelField(value.X, value.Field, boundaries, bindings, visiting))
	case *ssa.Field:
		provenance.merge(analysis.traceChannelField(value.X, value.Field, boundaries, bindings, visiting))
	case *ssa.ChangeType:
		mergeValue(value.X)
	case *ssa.ChangeInterface:
		mergeValue(value.X)
	case *ssa.Convert:
		mergeValue(value.X)
		if isUnsafePointerType(value.Type()) || isUnsafePointerType(value.X.Type()) {
			provenance.unsafe = true
		}
	case *ssa.MakeInterface:
		mergeValue(value.X)
	case *ssa.TypeAssert:
		mergeValue(value.X)
	case *ssa.Extract:
		if call, ok := value.Tuple.(*ssa.Call); ok {
			provenance.merge(analysis.traceChannelCall(call, value.Index, boundaries, bindings, visiting))
		} else {
			mergeValue(value.Tuple)
		}
	case *ssa.Phi:
		for _, edge := range value.Edges {
			mergeValue(edge)
		}
	case *ssa.Alloc:
		provenance.merge(analysis.traceChannelStores(value, boundaries, bindings, visiting))
	case *ssa.Call:
		provenance.merge(analysis.traceChannelCall(value, 0, boundaries, bindings, visiting))
	case *ssa.Lookup, *ssa.Index:
		// Maps, slices, and arrays may be populated by unanalyzed callers.
		provenance.openWorld = true
	default:
		if _, isChannel := types.Unalias(value.Type()).Underlying().(*types.Chan); isChannel {
			provenance.openWorld = true
		}
	}
	return provenance
}

func (analysis *loadedAnalysis) traceChannelFreeVar(freeVariable *ssa.FreeVar, boundaries []resolvedBoundary, bindings map[*ssa.Parameter]ssa.Value, visiting map[ssa.Value]bool) channelProvenance {
	provenance := newChannelProvenance()
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil {
		provenance.openWorld = true
		return provenance
	}
	index := -1
	for candidateIndex, candidate := range owner.FreeVars {
		if candidate == freeVariable {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		provenance.openWorld = true
		return provenance
	}
	foundBinding := false
	for _, block := range owner.Parent().Blocks {
		for _, instruction := range block.Instrs {
			closure, ok := instruction.(*ssa.MakeClosure)
			if !ok || index >= len(closure.Bindings) || closure.Fn != owner {
				continue
			}
			foundBinding = true
			provenance.merge(analysis.traceChannel(closure.Bindings[index], boundaries, bindings, visiting))
		}
	}
	if !foundBinding {
		provenance.openWorld = true
	}
	return provenance
}

func channelValueHasUnsafeAncestry(value ssa.Value, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.Field:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.FieldAddr:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.UnOp:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.ChangeType:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.ChangeInterface:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.Convert:
		return isUnsafePointerType(value.Type()) || isUnsafePointerType(value.X.Type()) || channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.MakeInterface:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.TypeAssert:
		return channelValueHasUnsafeAncestry(value.X, visiting)
	case *ssa.Extract:
		return channelValueHasUnsafeAncestry(value.Tuple, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if channelValueHasUnsafeAncestry(edge, visiting) {
				return true
			}
		}
	case *ssa.Call:
		if builtin, ok := value.Call.Value.(*ssa.Builtin); ok && isUnsafeBuiltin(builtin.Name()) {
			return true
		}
		for _, argument := range value.Call.Args {
			if channelValueHasUnsafeAncestry(argument, visiting) {
				return true
			}
		}
	case *ssa.Parameter:
		return isUnsafePointerType(value.Type())
	}
	return false
}

func (analysis *loadedAnalysis) traceChannelField(base ssa.Value, field int, boundaries []resolvedBoundary, bindings map[*ssa.Parameter]ssa.Value, visiting map[ssa.Value]bool) channelProvenance {
	provenance := newChannelProvenance()
	allocation, local := base.(*ssa.Alloc)
	if !local {
		if load, ok := base.(*ssa.UnOp); ok {
			allocation, local = load.X.(*ssa.Alloc)
		}
	}
	if !local {
		provenance.merge(analysis.traceChannel(base, boundaries, bindings, visiting))
		if len(provenance.matches) == 0 && !provenance.unsafe {
			provenance.openWorld = true
		}
		return provenance
	}

	foundStore := false
	if references := allocation.Referrers(); references != nil {
		for _, instruction := range *references {
			switch instruction := instruction.(type) {
			case *ssa.FieldAddr:
				if instruction.Field != field {
					continue
				}
				if fieldReferences := instruction.Referrers(); fieldReferences != nil {
					for _, fieldInstruction := range *fieldReferences {
						switch fieldInstruction := fieldInstruction.(type) {
						case *ssa.Store:
							if fieldInstruction.Addr == instruction {
								foundStore = true
								provenance.merge(analysis.traceChannel(fieldInstruction.Val, boundaries, bindings, visiting))
							}
						case *ssa.UnOp, *ssa.DebugRef:
							// Direct loads and debug metadata do not let the address escape.
						default:
							provenance.openWorld = true
						}
					}
				}
			case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure:
				// Local reads, metadata, and lexical captures retain analyzable identity.
			case *ssa.Store:
				// A whole-struct store can populate this field through a value the
				// field-sensitive tracer cannot project safely.
				provenance.openWorld = true
			default:
				provenance.openWorld = true
			}
		}
	}
	if !foundStore {
		// A zero-valued local field is a closed-world nil channel.
		return provenance
	}
	return provenance
}

func (analysis *loadedAnalysis) traceChannelStores(allocation *ssa.Alloc, boundaries []resolvedBoundary, bindings map[*ssa.Parameter]ssa.Value, visiting map[ssa.Value]bool) channelProvenance {
	provenance := newChannelProvenance()
	if references := allocation.Referrers(); references != nil {
		for _, instruction := range *references {
			switch instruction := instruction.(type) {
			case *ssa.Store:
				if instruction.Addr == allocation {
					provenance.merge(analysis.traceChannel(instruction.Val, boundaries, bindings, visiting))
				}
			case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure:
				// Direct loads, metadata, and lexical captures remain closed-world.
			default:
				provenance.openWorld = true
			}
		}
	}
	return provenance
}

func (analysis *loadedAnalysis) traceChannelCall(call *ssa.Call, resultIndex int, boundaries []resolvedBoundary, bindings map[*ssa.Parameter]ssa.Value, visiting map[ssa.Value]bool) channelProvenance {
	provenance := newChannelProvenance()
	authoritativeMatch := false
	for _, boundary := range boundaries {
		if analysis.callProducesChannel(call, resultIndex, boundary) {
			provenance.matches[boundary.definition.ID] = true
			authoritativeMatch = authoritativeMatch || callDirectlyProducesChannel(call, resultIndex, boundary)
		}
	}
	if authoritativeMatch {
		return provenance
	}
	if builtin, ok := call.Call.Value.(*ssa.Builtin); ok && isUnsafeBuiltin(builtin.Name()) {
		provenance.unsafe = true
		return provenance
	}

	callees := resolvedCallees(analysis.callGraph, call.Parent(), call)
	if call.Call.StaticCallee() == nil {
		provenance.openWorld = true
	}
	if len(callees) == 0 {
		provenance.openWorld = true
		return provenance
	}
	for _, callee := range callees {
		if callee == nil || len(callee.Blocks) == 0 {
			provenance.openWorld = true
			continue
		}
		actuals := append([]ssa.Value(nil), call.Call.Args...)
		if call.Call.IsInvoke() && callee.Signature.Recv() != nil {
			actuals = append([]ssa.Value{call.Call.Value}, actuals...)
		}
		calleeBindings := make(map[*ssa.Parameter]ssa.Value, len(callee.Params))
		for index, parameter := range callee.Params {
			if index < len(actuals) {
				calleeBindings[parameter] = resolveChannelActual(actuals[index], bindings)
			}
		}
		foundReturn := false
		for _, block := range callee.Blocks {
			for _, instruction := range block.Instrs {
				returned, ok := instruction.(*ssa.Return)
				if !ok || resultIndex < 0 || resultIndex >= len(returned.Results) {
					continue
				}
				foundReturn = true
				provenance.merge(analysis.traceChannel(returned.Results[resultIndex], boundaries, calleeBindings, visiting))
			}
		}
		if !foundReturn {
			provenance.openWorld = true
		}
	}
	return provenance
}

func callDirectlyProducesChannel(call *ssa.Call, resultIndex int, boundary resolvedBoundary) bool {
	if boundary.definition.Output.zero() || boundary.function == nil || boundary.definition.Output.Index != resultIndex+1 {
		return false
	}
	if static := call.Call.StaticCallee(); static != nil {
		if object, ok := static.Object().(*types.Func); ok && object.Origin() == boundary.function.Origin() {
			return true
		}
	}
	return call.Call.Method != nil && call.Call.Method.Origin() == boundary.function.Origin()
}

func resolveChannelActual(value ssa.Value, bindings map[*ssa.Parameter]ssa.Value) ssa.Value {
	seen := make(map[*ssa.Parameter]bool)
	for {
		parameter, ok := value.(*ssa.Parameter)
		if !ok || seen[parameter] || bindings[parameter] == nil {
			return value
		}
		seen[parameter] = true
		value = bindings[parameter]
	}
}

func isUnsafePointerType(value types.Type) bool {
	basic, ok := types.Unalias(value).Underlying().(*types.Basic)
	return ok && basic.Kind() == types.UnsafePointer
}

func isUnsafeBuiltin(name string) bool {
	switch name {
	case "Add", "Slice", "SliceData", "String", "StringData":
		return true
	default:
		return false
	}
}

func channelValueObject(value ssa.Value) types.Object {
	switch value := value.(type) {
	case *ssa.Global:
		return value.Object()
	case *ssa.Field:
		return fieldObject(value.X.Type(), value.Field)
	case *ssa.FieldAddr:
		return fieldObject(value.X.Type(), value.Field)
	default:
		return nil
	}
}

func fieldObject(container types.Type, index int) *types.Var {
	container = types.Unalias(container)
	if pointer, ok := container.(*types.Pointer); ok {
		container = types.Unalias(pointer.Elem())
	}
	structure, ok := container.Underlying().(*types.Struct)
	if !ok || index < 0 || index >= structure.NumFields() {
		return nil
	}
	return structure.Field(index)
}

func (analysis *loadedAnalysis) callProducesChannel(call *ssa.Call, resultIndex int, boundary resolvedBoundary) bool {
	if boundary.definition.Output.zero() || boundary.function == nil || boundary.definition.Output.Index != resultIndex+1 {
		return false
	}
	for _, callee := range resolvedCallees(analysis.callGraph, call.Parent(), call) {
		if object, ok := callee.Object().(*types.Func); ok && object.Origin() == boundary.function.Origin() {
			return true
		}
	}
	return call.Common().Method != nil && call.Common().Method.Origin() == boundary.function.Origin()
}
