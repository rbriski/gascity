package effectinventory

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

func resolveBoundaries(packageIndex map[string]*packages.Package, definitions []BoundaryDefinition) ([]resolvedBoundary, error) {
	resolved := make([]resolvedBoundary, 0, len(definitions))
	var problems []string
	seenIDs := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		if seenIDs[definition.ID] {
			problems = append(problems, fmt.Sprintf("boundary %q is defined more than once", definition.ID))
			continue
		}
		seenIDs[definition.ID] = true
		pkg := packageIndex[definition.Object.Package]
		if pkg == nil || pkg.Types == nil {
			problems = append(problems, fmt.Sprintf("boundary %q: package %q was not loaded", definition.ID, definition.Object.Package))
			continue
		}
		boundary := resolvedBoundary{definition: definition}
		if definition.Match == ObjectMatchChannel {
			object, objectErr := resolveObject(pkg.Types, definition.Object)
			if objectErr != nil {
				problems = append(problems, fmt.Sprintf("boundary %q: %v", definition.ID, objectErr))
				continue
			}
			boundary.object = object
			if function, ok := object.(*types.Func); ok {
				boundary.function = function
			}
			if channelErr := validateChannelBoundaryObject(&boundary); channelErr != nil {
				problems = append(problems, fmt.Sprintf("boundary %q: %v", definition.ID, channelErr))
				continue
			}
			resolved = append(resolved, boundary)
			continue
		}

		object, objectErr := resolveFunctionObject(pkg.Types, definition.Object)
		if objectErr != nil {
			problems = append(problems, fmt.Sprintf("boundary %q: %v", definition.ID, objectErr))
			continue
		}
		boundary.object = object
		boundary.function = object
		if definition.Match == ObjectMatchInterfaceImplementors {
			receiverObject, ok := pkg.Types.Scope().Lookup(definition.Object.Receiver).(*types.TypeName)
			if !ok {
				problems = append(problems, fmt.Sprintf("boundary %q: receiver %q is not a named type", definition.ID, definition.Object.Receiver))
				continue
			}
			underlying := types.Unalias(receiverObject.Type()).Underlying()
			interfaceType, ok := underlying.(*types.Interface)
			if !ok {
				problems = append(problems, fmt.Sprintf("boundary %q: receiver %q is not an interface", definition.ID, definition.Object.Receiver))
				continue
			}
			boundary.interfaceType = interfaceType.Complete()
		}
		resolved = append(resolved, boundary)
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("effect discovery could not resolve boundaries:\n- %s", strings.Join(compactStrings(problems), "\n- "))
	}
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].definition.ID < resolved[j].definition.ID
	})
	return resolved, nil
}

func resolveFunctionObject(pkg *types.Package, reference ObjectRef) (*types.Func, error) {
	object, err := resolveObject(pkg, reference)
	if err != nil {
		return nil, err
	}
	function, ok := object.(*types.Func)
	if !ok {
		return nil, fmt.Errorf("object %s is not a function or method", reference.key())
	}
	return function, nil
}

func resolveObject(pkg *types.Package, reference ObjectRef) (types.Object, error) {
	if reference.Receiver == "" {
		object := pkg.Scope().Lookup(reference.Name)
		if object == nil {
			return nil, fmt.Errorf("object %s does not exist", reference.key())
		}
		return object, nil
	}

	receiverObject, ok := pkg.Scope().Lookup(reference.Receiver).(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("receiver %q is not a named type", reference.Receiver)
	}
	if receiverObject.IsAlias() {
		declaring := receiverTypeName(types.Unalias(receiverObject.Type()))
		return nil, fmt.Errorf("receiver %q is an alias; exact boundaries must name declaring receiver %q", reference.Receiver, declaring)
	}
	receiverType := types.Unalias(receiverObject.Type())
	object, index, _ := types.LookupFieldOrMethod(receiverType, true, pkg, reference.Name)
	if object == nil {
		object, index, _ = types.LookupFieldOrMethod(types.NewPointer(receiverType), true, pkg, reference.Name)
	}
	if object == nil {
		return nil, fmt.Errorf("object %s is not an unambiguous field or method", reference.key())
	}
	if len(index) != 1 {
		return nil, fmt.Errorf("object %s is promoted; exact boundaries must name its declaring receiver", reference.key())
	}
	if function, ok := object.(*types.Func); ok {
		signature, _ := function.Type().(*types.Signature)
		if signature != nil && signature.Recv() != nil && receiverTypeName(signature.Recv().Type()) != reference.Receiver {
			return nil, fmt.Errorf("object %s is declared by receiver %q", reference.key(), receiverTypeName(signature.Recv().Type()))
		}
	}
	return object, nil
}

func validateChannelBoundaryObject(boundary *resolvedBoundary) error {
	if !boundary.definition.Input.zero() && !boundary.definition.Output.zero() {
		return fmt.Errorf("object %s cannot name both channel input and output slots", boundary.definition.Object.key())
	}
	if boundary.definition.Input.zero() && boundary.definition.Output.zero() {
		if _, ok := types.Unalias(boundary.object.Type()).Underlying().(*types.Chan); !ok {
			return fmt.Errorf("object %s does not have channel type", boundary.definition.Object.key())
		}
		boundary.channel = boundary.object.Type()
		return nil
	}
	if boundary.function == nil {
		return fmt.Errorf("object %s names a result but is not a function", boundary.definition.Object.key())
	}
	signature, ok := boundary.function.Type().(*types.Signature)
	if !ok {
		return fmt.Errorf("object %s has no function signature", boundary.definition.Object.key())
	}
	if !boundary.definition.Input.zero() {
		index := boundary.definition.Input.Index - 1
		if boundary.definition.Input.Kind != SlotParameter || index < 0 || index >= signature.Params().Len() {
			return fmt.Errorf("object %s has no parameter slot %d", boundary.definition.Object.key(), boundary.definition.Input.Index)
		}
		if _, ok := types.Unalias(signature.Params().At(index).Type()).Underlying().(*types.Chan); !ok {
			return fmt.Errorf("object %s parameter %d does not have channel type", boundary.definition.Object.key(), boundary.definition.Input.Index)
		}
		boundary.channel = signature.Params().At(index).Type()
		return nil
	}
	index := boundary.definition.Output.Index - 1
	if boundary.definition.Output.Kind != SlotResult || index < 0 || index >= signature.Results().Len() {
		return fmt.Errorf("object %s has no result slot %d", boundary.definition.Object.key(), boundary.definition.Output.Index)
	}
	if _, ok := types.Unalias(signature.Results().At(index).Type()).Underlying().(*types.Chan); !ok {
		return fmt.Errorf("object %s result %d does not have channel type", boundary.definition.Object.key(), boundary.definition.Output.Index)
	}
	boundary.channel = signature.Results().At(index).Type()
	return nil
}

func operationForCall(call ssa.CallInstruction) (OperationKind, bool) {
	switch call.(type) {
	case *ssa.Call:
		return OperationCall, true
	case *ssa.Go:
		return OperationGo, true
	case *ssa.Defer:
		return OperationDefer, true
	default:
		return "", false
	}
}

func (analysis *loadedAnalysis) observeCallInstruction(function *ssa.Function, call ssa.CallInstruction, boundaries []resolvedBoundary) (*observedCall, []string) {
	operation, ok := operationForCall(call)
	if !ok || !call.Pos().IsValid() {
		// Synthetic import-initializer calls have no authored source
		// location. Effectful global initializers retain a real position.
		return nil, nil
	}
	ref, err := analysis.functionRef(function, call.Pos())
	if err != nil {
		return nil, []string{err.Error()}
	}
	if builtin, ok := call.Common().Value.(*ssa.Builtin); ok && builtin.Name() == "close" && len(call.Common().Args) == 1 {
		provenance := analysis.traceChannel(call.Common().Args[0], boundaries, make(map[ssa.Value]bool))
		matches := provenance.sortedMatches()
		if len(matches) != 0 {
			return nil, []string{fmt.Sprintf("%s: unsupported close operation on inventoried channel boundary %s", ref.key(), strings.Join(matches, ", "))}
		}
		if hasCompatibleChannelBoundary(call.Common().Args[0].Type(), boundaries) && (provenance.openWorld || provenance.unsafe) {
			return nil, []string{fmt.Sprintf("%s: unsupported close operation has unresolved or unsafe channel provenance", ref.key())}
		}
	}
	callees := resolvedCallees(analysis.callGraph, function, call)
	var problems []string
	if reflection := reflectionTarget(callees); reflection != "" {
		problems = append(problems, fmt.Sprintf("%s: reflective execution through %s cannot be inventoried statically", ref.key(), reflection))
	}
	problems = append(problems, analysis.unanalyzedBoundaryArgumentProblems(ref, call, callees, boundaries)...)

	var matches []string
	for _, boundary := range boundaries {
		if callMatchesBoundary(call, callees, boundary, analysis.callGraph, analysis.effectiveReceiverTypes(call)) {
			matches = append(matches, boundary.definition.ID)
		}
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		problems = append(problems, fmt.Sprintf("%s: call matches multiple boundaries: %s", ref.key(), strings.Join(matches, ", ")))
		return nil, problems
	}
	if unresolvedEffectCall(call, callees, boundaries) {
		problems = append(problems, fmt.Sprintf("%s: unresolved effect-compatible dynamic call", ref.key()))
	}
	if len(matches) == 0 {
		return nil, problems
	}
	if analysis.initReachable[function] {
		problems = append(problems, fmt.Sprintf("%s: effectful package initialization route has no injective FunctionRef", ref.key()))
		return nil, problems
	}
	return &observedCall{
		boundaryID: matches[0],
		function:   ref,
		operation:  operation,
		position:   call.Pos(),
	}, problems
}

func (analysis *loadedAnalysis) functionRef(function *ssa.Function, fallback token.Pos) (FunctionRef, error) {
	if origin := function.Origin(); origin != nil {
		function = origin
	}
	var reversedPath []int
	ancestor := function
	for ancestor.Parent() != nil {
		parent := ancestor.Parent()
		index := -1
		for candidateIndex, candidate := range parent.AnonFuncs {
			if candidate == ancestor {
				index = candidateIndex
				break
			}
		}
		if index < 0 {
			return FunctionRef{}, fmt.Errorf("effect discovery: anonymous function %s is absent from its parent", ancestor)
		}
		reversedPath = append(reversedPath, index+1)
		ancestor = parent
	}
	closurePath := make([]int, len(reversedPath))
	for index := range reversedPath {
		closurePath[len(reversedPath)-1-index] = reversedPath[index]
	}

	var object ObjectRef
	if functionObject, ok := ancestor.Object().(*types.Func); ok {
		object = objectRefForFunction(functionObject)
	} else if ancestor.Name() == "init" && ancestor.Package() != nil && ancestor.Package().Pkg != nil {
		object = ObjectRef{Package: ancestor.Package().Pkg.Path(), Name: "init"}
	} else {
		return FunctionRef{}, fmt.Errorf("effect discovery: authored function %s has no stable object", ancestor)
	}

	position := ancestor.Pos()
	if !position.IsValid() {
		position = fallback
	}
	physical := analysis.program.Fset.PositionFor(position, false)
	if physical.Filename == "" {
		return FunctionRef{}, fmt.Errorf("effect discovery: function %s has no physical source file", object.key())
	}
	relative, err := filepath.Rel(analysis.config.RepoRoot, physical.Filename)
	if err != nil {
		return FunctionRef{}, fmt.Errorf("effect discovery: locating function %s: %w", object.key(), err)
	}
	relative = filepath.Clean(relative)
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return FunctionRef{}, fmt.Errorf("effect discovery: function %s escapes repository root through %q", object.key(), physical.Filename)
	}
	return FunctionRef{Object: object, File: filepath.ToSlash(relative), ClosurePath: closurePath}, nil
}

func objectRefForFunction(function *types.Func) ObjectRef {
	reference := ObjectRef{Name: function.Name()}
	if function.Pkg() != nil {
		reference.Package = function.Pkg().Path()
	}
	if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
		reference.Receiver = receiverTypeName(signature.Recv().Type())
	}
	return reference
}

func receiverTypeName(receiver types.Type) string {
	receiver = types.Unalias(receiver)
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = types.Unalias(pointer.Elem())
	}
	if named, ok := receiver.(*types.Named); ok && named.Obj() != nil {
		return named.Obj().Name()
	}
	return ""
}

func resolvedCallees(graph *callgraph.Graph, function *ssa.Function, call ssa.CallInstruction) []*ssa.Function {
	seen := make(map[*ssa.Function]bool)
	var callees []*ssa.Function
	add := func(candidate *ssa.Function) {
		if candidate == nil || seen[candidate] {
			return
		}
		seen[candidate] = true
		callees = append(callees, candidate)
	}
	add(call.Common().StaticCallee())
	if node := graph.Nodes[function]; node != nil {
		for _, edge := range node.Out {
			if edge.Site == call && edge.Callee != nil {
				add(edge.Callee.Func)
			}
		}
	}
	sort.Slice(callees, func(i, j int) bool {
		return functionSortKey(callees[i]) < functionSortKey(callees[j])
	})
	return callees
}

func functionSortKey(function *ssa.Function) string {
	if function == nil {
		return ""
	}
	if object, ok := function.Object().(*types.Func); ok {
		return objectRefForFunction(object).key() + "|" + function.String()
	}
	return function.String()
}

func collectSelectionReceivers(roots []*packages.Package) map[token.Pos]types.Type {
	receivers := make(map[token.Pos]types.Type)
	for _, pkg := range roots {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				switch expression := node.(type) {
				case *ast.SelectorExpr:
					if selection := pkg.TypesInfo.Selections[expression]; selection != nil {
						receivers[expression.Sel.Pos()] = selection.Recv()
					}
				case *ast.CallExpr:
					if selector := calledSelector(expression.Fun); selector != nil {
						if selection := pkg.TypesInfo.Selections[selector]; selection != nil {
							receivers[expression.Lparen] = selection.Recv()
						}
					}
				}
				return true
			})
		}
	}
	return receivers
}

func calledSelector(expression ast.Expr) *ast.SelectorExpr {
	for {
		switch current := expression.(type) {
		case *ast.ParenExpr:
			expression = current.X
		case *ast.IndexExpr:
			expression = current.X
		case *ast.IndexListExpr:
			expression = current.X
		default:
			selector, _ := expression.(*ast.SelectorExpr)
			return selector
		}
	}
}

func (analysis *loadedAnalysis) effectiveReceiverTypes(call ssa.CallInstruction) []types.Type {
	seenTypes := make(map[types.Type]bool)
	var result []types.Type
	add := func(candidate types.Type) {
		if candidate == nil || seenTypes[candidate] {
			return
		}
		seenTypes[candidate] = true
		result = append(result, candidate)
	}
	add(analysis.receivers[call.Pos()])
	if call.Common().IsInvoke() {
		add(call.Common().Value.Type())
	}
	collectReceiverTypesFromValue(call.Common().Value, analysis.receivers, add, make(map[ssa.Value]bool))
	if static := call.Common().StaticCallee(); static != nil && static.Signature.Recv() != nil && len(call.Common().Args) != 0 {
		add(call.Common().Args[0].Type())
	}
	return result
}

func collectReceiverTypesFromValue(value ssa.Value, indexed map[token.Pos]types.Type, add func(types.Type), visiting map[ssa.Value]bool) {
	if value == nil || visiting[value] {
		return
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.MakeClosure:
		add(indexed[value.Pos()])
		for _, binding := range value.Bindings {
			add(binding.Type())
		}
	case *ssa.UnOp:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.ChangeType:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.ChangeInterface:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.Convert:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.MakeInterface:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.TypeAssert:
		collectReceiverTypesFromValue(value.X, indexed, add, visiting)
	case *ssa.Extract:
		collectReceiverTypesFromValue(value.Tuple, indexed, add, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			collectReceiverTypesFromValue(edge, indexed, add, visiting)
		}
	case *ssa.Alloc:
		if references := value.Referrers(); references != nil {
			for _, instruction := range *references {
				if store, ok := instruction.(*ssa.Store); ok && store.Addr == value {
					collectReceiverTypesFromValue(store.Val, indexed, add, visiting)
				}
			}
		}
	}
}

func callMatchesBoundary(call ssa.CallInstruction, callees []*ssa.Function, boundary resolvedBoundary, graph *callgraph.Graph, effectiveReceivers []types.Type) bool {
	if boundary.definition.Match == ObjectMatchChannel {
		return false
	}
	if method := call.Common().Method; method != nil && functionMatchesBoundary(method, boundary) {
		return true
	}
	for _, callee := range callees {
		if calleeMatchesBoundary(callee, boundary, graph, effectiveReceivers, make(map[*ssa.Function]bool)) {
			return true
		}
	}
	return false
}

func calleeMatchesBoundary(function *ssa.Function, boundary resolvedBoundary, graph *callgraph.Graph, effectiveReceivers []types.Type, visiting map[*ssa.Function]bool) bool {
	if function == nil || visiting[function] {
		return false
	}
	visiting[function] = true
	defer delete(visiting, function)
	if ssaFunctionMatchesBoundary(function, boundary, effectiveReceivers) {
		return true
	}
	if !dispatchOnlySynthetic(function.Synthetic) {
		return false
	}
	node := graph.Nodes[function]
	if node == nil {
		return false
	}
	for _, edge := range node.Out {
		if edge.Callee != nil && calleeMatchesBoundary(edge.Callee.Func, boundary, graph, effectiveReceivers, visiting) {
			return true
		}
	}
	return false
}

func dispatchOnlySynthetic(description string) bool {
	return strings.HasPrefix(description, "wrapper for ") ||
		strings.HasPrefix(description, "thunk for ") ||
		strings.HasPrefix(description, "bound method wrapper for ")
}

func ssaFunctionMatchesBoundary(function *ssa.Function, boundary resolvedBoundary, effectiveReceivers []types.Type) bool {
	if function == nil || boundary.function == nil {
		return false
	}
	object, ok := function.Object().(*types.Func)
	if !ok {
		return false
	}
	if object.Origin() == boundary.function.Origin() {
		return true
	}
	if boundary.definition.Match != ObjectMatchInterfaceImplementors || boundary.interfaceType == nil {
		return false
	}
	if object.Name() != boundary.function.Name() || !types.Identical(function.Signature, boundary.function.Type()) {
		return false
	}
	for _, receiver := range effectiveReceivers {
		if receiverImplements(receiver, boundary.interfaceType) {
			return true
		}
	}
	if function.Signature.Recv() != nil && receiverImplements(function.Signature.Recv().Type(), boundary.interfaceType) {
		return true
	}
	for _, freeVariable := range function.FreeVars {
		if receiverImplements(freeVariable.Type(), boundary.interfaceType) {
			return true
		}
	}
	return false
}

func receiverImplements(receiver types.Type, interfaceType *types.Interface) bool {
	receiver = types.Unalias(receiver)
	if types.Implements(receiver, interfaceType) {
		return true
	}
	if _, pointer := receiver.(*types.Pointer); !pointer {
		return types.Implements(types.NewPointer(receiver), interfaceType)
	}
	return false
}

func functionMatchesBoundary(function *types.Func, boundary resolvedBoundary) bool {
	if boundary.function == nil {
		return false
	}
	if function.Origin() == boundary.function.Origin() {
		return true
	}
	if boundary.definition.Match != ObjectMatchInterfaceImplementors || boundary.interfaceType == nil {
		return false
	}
	if function.Name() != boundary.function.Name() {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	if !ok || signature.Recv() == nil || !types.Identical(signature, boundary.function.Type()) {
		return false
	}
	return receiverImplements(signature.Recv().Type(), boundary.interfaceType)
}

func unresolvedEffectCall(call ssa.CallInstruction, callees []*ssa.Function, boundaries []resolvedBoundary) bool {
	common := call.Common()
	if _, builtin := common.Value.(*ssa.Builtin); builtin {
		return false
	}
	if common.Method != nil {
		for _, boundary := range boundaries {
			if functionMatchesBoundary(common.Method, boundary) {
				return false
			}
		}
	}
	static := common.StaticCallee()
	for _, boundary := range boundaries {
		if !callMayReachBoundary(common, boundary) {
			continue
		}
		if boundClosureOpenForBoundary(common.Value, boundary, make(map[ssa.Value]bool)) {
			return true
		}
		if static != nil {
			continue
		}
		if len(callees) == 0 || hasOpenWorldFunctionSource(common.Value, make(map[ssa.Value]bool)) {
			return true
		}
	}
	return false
}

func callMayReachBoundary(common *ssa.CallCommon, boundary resolvedBoundary) bool {
	if boundary.function == nil {
		return false
	}
	if common.Method != nil {
		return common.Method.Name() == boundary.function.Name() && types.Identical(common.Method.Type(), boundary.function.Type())
	}
	return callableSignatureCompatible(common.Signature(), boundary)
}

func callableSignatureCompatible(candidate *types.Signature, boundary resolvedBoundary) bool {
	if candidate == nil || boundary.function == nil {
		return false
	}
	boundarySignature, ok := boundary.function.Type().(*types.Signature)
	if !ok {
		return false
	}
	if types.Identical(candidate, boundarySignature) {
		return true
	}
	if boundarySignature.Recv() == nil || candidate.Recv() != nil || candidate.Params().Len() != boundarySignature.Params().Len()+1 {
		return false
	}
	if candidate.Variadic() != boundarySignature.Variadic() || !tuplesIdentical(candidate.Results(), boundarySignature.Results(), 0) || !tuplesIdentical(candidate.Params(), boundarySignature.Params(), 1) {
		return false
	}
	receiver := candidate.Params().At(0).Type()
	return receiverMaySelectBoundaryMethod(receiver, boundary, boundarySignature)
}

func receiverMaySelectBoundaryMethod(receiver types.Type, boundary resolvedBoundary, boundarySignature *types.Signature) bool {
	if boundary.definition.Match == ObjectMatchInterfaceImplementors && boundary.interfaceType != nil {
		if receiverImplements(receiver, boundary.interfaceType) {
			return true
		}
		selection := types.NewMethodSet(receiver).Lookup(boundary.function.Pkg(), boundary.function.Name())
		if selection == nil {
			return false
		}
		method, _ := selection.Obj().(*types.Func)
		return method != nil && types.Identical(method.Type(), boundary.function.Type())
	}
	boundaryReceiver := types.Unalias(boundarySignature.Recv().Type())
	receiver = types.Unalias(receiver)
	if types.Identical(receiver, boundaryReceiver) || types.AssignableTo(receiver, boundaryReceiver) || types.ConvertibleTo(receiver, boundaryReceiver) {
		return true
	}
	if selection := types.NewMethodSet(receiver).Lookup(boundary.function.Pkg(), boundary.function.Name()); selection != nil {
		if method, ok := selection.Obj().(*types.Func); ok && method.Origin() == boundary.function.Origin() {
			return true
		}
	}
	if pointer, ok := receiver.(*types.Pointer); ok {
		return types.Identical(types.Unalias(pointer.Elem()), boundaryReceiver)
	}
	return false
}

func tuplesIdentical(candidate, boundary *types.Tuple, candidateOffset int) bool {
	candidateLength := 0
	if candidate != nil {
		candidateLength = candidate.Len()
	}
	boundaryLength := 0
	if boundary != nil {
		boundaryLength = boundary.Len()
	}
	if candidateLength != boundaryLength+candidateOffset {
		return false
	}
	for index := 0; index < boundaryLength; index++ {
		if !types.Identical(candidate.At(index+candidateOffset).Type(), boundary.At(index).Type()) {
			return false
		}
	}
	return true
}

func boundClosureOpenForBoundary(value ssa.Value, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.FreeVar:
		return boundClosureFreeVarOpenForBoundary(value, boundary, visiting)
	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)
		if function == nil {
			return false
		}
		object, _ := function.Object().(*types.Func)
		if object == nil || !strings.HasPrefix(function.Synthetic, "bound method wrapper for ") || object.Name() != boundary.function.Name() || !types.Identical(object.Type(), boundary.function.Type()) {
			return false
		}
		if functionMatchesBoundary(object, boundary) {
			return false
		}
		signature, _ := object.Type().(*types.Signature)
		if signature == nil || signature.Recv() == nil {
			return false
		}
		if _, isInterface := types.Unalias(signature.Recv().Type()).Underlying().(*types.Interface); !isInterface {
			return false
		}
		for _, binding := range value.Bindings {
			if hasOpenWorldDispatchReceiver(binding, make(map[ssa.Value]bool)) {
				return true
			}
		}
	case *ssa.UnOp:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.MakeInterface:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.ChangeInterface:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.TypeAssert:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.Extract:
		return boundClosureOpenForBoundary(value.Tuple, boundary, visiting)
	case *ssa.FieldAddr:
		return boundClosureFieldOpenForBoundary(value.X, value.Field, boundary, visiting)
	case *ssa.Field:
		return boundClosureFieldOpenForBoundary(value.X, value.Field, boundary, visiting)
	case *ssa.ChangeType:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.Convert:
		return boundClosureOpenForBoundary(value.X, boundary, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if boundClosureOpenForBoundary(edge, boundary, visiting) {
				return true
			}
		}
	case *ssa.Alloc:
		if references := value.Referrers(); references != nil {
			for _, instruction := range *references {
				if store, ok := instruction.(*ssa.Store); ok && store.Addr == value && boundClosureOpenForBoundary(store.Val, boundary, visiting) {
					return true
				}
			}
		}
	}
	return false
}

func boundClosureFreeVarOpenForBoundary(freeVariable *ssa.FreeVar, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil {
		return true
	}
	index := -1
	for candidateIndex, candidate := range owner.FreeVars {
		if candidate == freeVariable {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return true
	}
	found := false
	for _, block := range owner.Parent().Blocks {
		for _, instruction := range block.Instrs {
			closure, ok := instruction.(*ssa.MakeClosure)
			if !ok || closure.Fn != owner || index >= len(closure.Bindings) {
				continue
			}
			found = true
			if boundClosureOpenForBoundary(closure.Bindings[index], boundary, visiting) {
				return true
			}
		}
	}
	return !found
}

func boundClosureFieldOpenForBoundary(base ssa.Value, field int, boundary resolvedBoundary, visiting map[ssa.Value]bool) bool {
	allocation, local := base.(*ssa.Alloc)
	if !local {
		if load, ok := base.(*ssa.UnOp); ok {
			allocation, local = load.X.(*ssa.Alloc)
		}
	}
	if !local {
		return true
	}
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
							if fieldInstruction.Addr == instruction && boundClosureOpenForBoundary(fieldInstruction.Val, boundary, visiting) {
								return true
							}
						case *ssa.UnOp, *ssa.DebugRef:
						default:
							return true
						}
					}
				}
			case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure:
			case *ssa.Store:
				return true
			default:
				return true
			}
		}
	}
	return false
}

func hasOpenWorldDispatchReceiver(value ssa.Value, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.Parameter:
		_, isInterface := types.Unalias(value.Type()).Underlying().(*types.Interface)
		return isInterface
	case *ssa.FreeVar, *ssa.Global:
		return true
	case *ssa.MakeInterface:
		// MakeInterface fixes the dynamic concrete type at this source site.
		return false
	case *ssa.ChangeInterface:
		return hasOpenWorldDispatchReceiver(value.X, visiting)
	case *ssa.TypeAssert:
		return hasOpenWorldDispatchReceiver(value.X, visiting)
	case *ssa.UnOp:
		return hasOpenWorldDispatchReceiver(value.X, visiting)
	case *ssa.Field, *ssa.FieldAddr, *ssa.Lookup, *ssa.Index:
		return true
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if hasOpenWorldDispatchReceiver(edge, visiting) {
				return true
			}
		}
	default:
		_, isInterface := types.Unalias(value.Type()).Underlying().(*types.Interface)
		return isInterface
	}
	return false
}

func hasOpenWorldFunctionSource(value ssa.Value, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)

	switch value := value.(type) {
	case *ssa.Parameter, *ssa.Global:
		return true
	case *ssa.FreeVar:
		return openWorldFunctionFreeVar(value, visiting)
	case *ssa.Function, *ssa.MakeClosure, *ssa.Builtin, *ssa.Const:
		return false
	case *ssa.UnOp:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.Field:
		return openWorldFunctionField(value.X, value.Field, visiting)
	case *ssa.FieldAddr:
		return openWorldFunctionField(value.X, value.Field, visiting)
	case *ssa.Index, *ssa.IndexAddr, *ssa.Lookup:
		// Container element provenance is not closed merely because the
		// container allocation is local; stores and map updates may select an
		// inventoried callable that this tracer cannot project injectively.
		return true
	case *ssa.Slice:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.ChangeType:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.ChangeInterface:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.Convert:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.MakeInterface:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.TypeAssert:
		return hasOpenWorldFunctionSource(value.X, visiting)
	case *ssa.Extract:
		if _, isFunction := types.Unalias(value.Type()).Underlying().(*types.Signature); isFunction {
			// Multi-result callable factories require return-sensitive tracing.
			// Until such a proof exists, their extracted callable is open-world.
			return true
		}
		return hasOpenWorldFunctionSource(value.Tuple, visiting)
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if hasOpenWorldFunctionSource(edge, visiting) {
				return true
			}
		}
		return false
	case *ssa.Alloc:
		return openWorldFunctionAllocation(value, visiting)
	default:
		// Unknown higher-order SSA values are not a closed-world proof.
		_, isFunction := types.Unalias(value.Type()).Underlying().(*types.Signature)
		return isFunction
	}
}

func openWorldFunctionFreeVar(freeVariable *ssa.FreeVar, visiting map[ssa.Value]bool) bool {
	owner := freeVariable.Parent()
	if owner == nil || owner.Parent() == nil {
		return true
	}
	index := -1
	for candidateIndex, candidate := range owner.FreeVars {
		if candidate == freeVariable {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return true
	}
	found := false
	for _, block := range owner.Parent().Blocks {
		for _, instruction := range block.Instrs {
			closure, ok := instruction.(*ssa.MakeClosure)
			if !ok || closure.Fn != owner || index >= len(closure.Bindings) {
				continue
			}
			found = true
			if hasOpenWorldFunctionSource(closure.Bindings[index], visiting) {
				return true
			}
		}
	}
	return !found
}

func openWorldFunctionField(base ssa.Value, field int, visiting map[ssa.Value]bool) bool {
	allocation, local := base.(*ssa.Alloc)
	if !local {
		if load, ok := base.(*ssa.UnOp); ok {
			allocation, local = load.X.(*ssa.Alloc)
		}
	}
	if !local {
		return true
	}
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
							if fieldInstruction.Addr == instruction && hasOpenWorldFunctionSource(fieldInstruction.Val, visiting) {
								return true
							}
						case *ssa.UnOp, *ssa.DebugRef:
						default:
							return true
						}
					}
				}
			case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure:
			case *ssa.Store:
				return true
			default:
				return true
			}
		}
	}
	return false
}

func openWorldFunctionAllocation(allocation *ssa.Alloc, visiting map[ssa.Value]bool) bool {
	if references := allocation.Referrers(); references != nil {
		for _, instruction := range *references {
			switch instruction := instruction.(type) {
			case *ssa.Store:
				if instruction.Addr == allocation && hasOpenWorldFunctionSource(instruction.Val, visiting) {
					return true
				}
			case *ssa.UnOp, *ssa.DebugRef, *ssa.MakeClosure, *ssa.FieldAddr:
			default:
				return true
			}
		}
	}
	return false
}

func reflectionTarget(callees []*ssa.Function) string {
	for _, callee := range callees {
		object, ok := callee.Object().(*types.Func)
		if !ok || object.Pkg() == nil || object.Pkg().Path() != "reflect" {
			continue
		}
		reference := objectRefForFunction(object)
		if reference.Receiver == "Value" && oneOf(reference.Name,
			"Call", "CallSlice", "Send", "TrySend", "Recv", "TryRecv", "Close",
		) {
			return reference.key()
		}
		if reference.Receiver == "" && reference.Name == "Select" {
			return reference.key()
		}
	}
	return ""
}

func numberObservedCalls(calls []observedCall, profile BuildProfileID) []ObservedSite {
	sort.Slice(calls, func(i, j int) bool {
		left := calls[i].boundaryID + "|" + calls[i].function.key() + "|" + string(calls[i].operation)
		right := calls[j].boundaryID + "|" + calls[j].function.key() + "|" + string(calls[j].operation)
		if left != right {
			return left < right
		}
		return calls[i].position < calls[j].position
	})
	ordinals := make(map[string]int)
	result := make([]ObservedSite, 0, len(calls))
	for _, call := range calls {
		group := call.boundaryID + "|" + call.function.key() + "|" + string(call.operation)
		ordinals[group]++
		result = append(result, ObservedSite{
			BoundaryID: call.boundaryID,
			Matcher: OperationSite{
				Operation: call.operation,
				Enclosing: call.function,
				Ordinal:   ordinals[group],
			},
			Profile: profile,
		})
	}
	return result
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
