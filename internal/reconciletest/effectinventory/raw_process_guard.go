package effectinventory

import (
	"fmt"
	"go/constant"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

// rawProcessOperation identifies one destructive standard-library process
// primitive by go/types object identity. These values are diagnostics and
// evidence keys; they are not reconciler boundary definitions.
type rawProcessOperation string

const (
	rawProcessSyscallKill rawProcessOperation = "raw syscall.Kill"
	rawProcessSignal      rawProcessOperation = "raw os.Process.Signal"
)

type rawProcessVehicle struct {
	ID   string
	Root ObjectRef
}

type rawProcessEvidence struct {
	Operation  rawProcessOperation
	Matcher    OperationSite
	VehicleIDs []string
	Profiles   []BuildProfileID
}

type rawProcessTarget struct {
	operation rawProcessOperation
	boundary  resolvedBoundary
	hasSignal bool
}

type rawProcessObservation struct {
	site     ObservedSite
	function *ssa.Function
}

// validateCanonicalRawProcessGuard is the single integration seam for the
// canonical production analysis. Boundary resolution and future canonical
// discovery can call it after loading one profile without duplicating a
// package load or coupling raw-process policy to the registry compiler.
func validateCanonicalRawProcessGuard(analysis *loadedAnalysis) error {
	return validateRawProcessGuard(analysis, canonicalRawProcessVehicles(), canonicalRawProcessEvidence())
}

func canonicalRawProcessVehicles() []rawProcessVehicle {
	return []rawProcessVehicle{
		{ID: "pidutil.signal-process", Root: ObjectRef{Package: pidutilPackage, Name: "SignalProcess"}},
		{ID: "processgroup.signal-group", Root: ObjectRef{Package: processgroupPackage, Name: "SignalGroup"}},
		{ID: "processgroup.signal-command", Root: ObjectRef{Package: processgroupPackage, Name: "SignalCommand"}},
		{ID: "processgroup.terminate", Root: ObjectRef{Package: processgroupPackage, Name: "Terminate"}},
		{ID: "runtime.signal-process-group", Root: ObjectRef{Package: runtimePackage, Name: "SignalProcessGroup"}},
		{ID: "runtime.proctable.kill-by-pid", Root: ObjectRef{Package: proctablePackage, Name: "KillByPID"}},
	}
}

func canonicalRawProcessEvidence() []rawProcessEvidence {
	unixProfiles := []BuildProfileID{
		BuildDarwinDefault,
		BuildDarwinNative,
		BuildLinuxDefault,
		BuildLinuxNative,
	}
	allProfiles := append(append([]BuildProfileID(nil), unixProfiles...), BuildWindowsCompile)
	evidence := func(operation rawProcessOperation, packagePath, receiver, function, file string, ordinal int, vehicleID string, profiles []BuildProfileID) rawProcessEvidence {
		return rawProcessEvidence{
			Operation: operation,
			Matcher: OperationSite{
				Operation: OperationCall,
				Enclosing: FunctionRef{
					Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: function},
					File:   file,
				},
				Ordinal: ordinal,
			},
			VehicleIDs: []string{vehicleID},
			Profiles:   append([]BuildProfileID(nil), profiles...),
		}
	}
	return []rawProcessEvidence{
		evidence(rawProcessSignal, pidutilPackage, "", "SignalProcess", "internal/pidutil/pidutil.go", 1, "pidutil.signal-process", allProfiles),
		evidence(rawProcessSyscallKill, processgroupPackage, "", "SignalGroup", "internal/processgroup/processgroup_unix.go", 1, "processgroup.signal-group", unixProfiles),
		evidence(rawProcessSyscallKill, processgroupPackage, "", "SignalCommand", "internal/processgroup/processgroup_unix.go", 1, "processgroup.signal-command", unixProfiles),
		evidence(rawProcessSignal, processgroupPackage, "", "SignalCommand", "internal/processgroup/processgroup_unix.go", 1, "processgroup.signal-command", unixProfiles),
		evidence(rawProcessSyscallKill, processgroupPackage, "Options", "kill", "internal/processgroup/processgroup_unix.go", 1, "processgroup.terminate", unixProfiles),
		evidence(rawProcessSyscallKill, processgroupPackage, "Options", "kill", "internal/processgroup/processgroup_unix.go", 2, "processgroup.terminate", unixProfiles),

		evidence(rawProcessSyscallKill, runtimePackage, "", "signalProcessGroup", "internal/runtime/process_control_notwindows.go", 1, "runtime.signal-process-group", unixProfiles),
		evidence(rawProcessSignal, runtimePackage, "", "signalProcessGroup", "internal/runtime/process_control_notwindows.go", 1, "runtime.signal-process-group", unixProfiles),
		evidence(rawProcessSyscallKill, proctablePackage, "", "signalPIDWith", "internal/runtime/proctable/kill_unix.go", 1, "runtime.proctable.kill-by-pid", unixProfiles),
		evidence(rawProcessSyscallKill, proctablePackage, "", "signalPIDWith", "internal/runtime/proctable/kill_unix.go", 2, "runtime.proctable.kill-by-pid", unixProfiles),

		evidence(rawProcessSignal, processgroupPackage, "", "SignalCommand", "internal/processgroup/processgroup_windows.go", 1, "processgroup.signal-command", []BuildProfileID{BuildWindowsCompile}),
		evidence(rawProcessSignal, runtimePackage, "", "signalProcessGroup", "internal/runtime/process_control_windows.go", 1, "runtime.signal-process-group", []BuildProfileID{BuildWindowsCompile}),
	}
}

func discoverRawProcessEffects(analysis *loadedAnalysis) ([]ObservedSite, error) {
	observations, err := scanRawProcessEffects(analysis)
	if err != nil {
		return nil, err
	}
	sites := make([]ObservedSite, len(observations))
	for index, observation := range observations {
		sites[index] = observation.site
	}
	return sites, nil
}

func scanRawProcessEffects(analysis *loadedAnalysis) ([]rawProcessObservation, error) {
	targets, err := resolveRawProcessTargets(analysis)
	if err != nil {
		return nil, err
	}

	type unnumbered struct {
		operation rawProcessOperation
		function  *ssa.Function
		ref       FunctionRef
		call      ssa.CallInstruction
	}
	var found []unnumbered
	var problems []string
	for function := range analysis.sourceFuncs {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok || !call.Pos().IsValid() {
					continue
				}
				_, ok = operationForCall(call)
				if !ok {
					continue
				}
				callees := analysis.closedWorldCallees(function, call)
				matches := rawProcessMatches(call, callees, targets, analysis)
				if len(matches) == 0 {
					continue
				}
				ref, refErr := analysis.functionRef(function, call.Pos())
				if refErr != nil {
					problems = append(problems, refErr.Error())
					continue
				}
				if len(matches) != 1 {
					labels := make([]string, len(matches))
					for index, match := range matches {
						labels[index] = string(match.operation)
					}
					problems = append(problems, fmt.Sprintf("%s: raw process call matches multiple typed primitives: %s", ref.key(), strings.Join(labels, ", ")))
					continue
				}
				match := matches[0]
				if match.hasSignal && rawProcessSignalIsDefinitelyZero(call) {
					continue
				}
				found = append(found, unnumbered{
					operation: match.operation,
					function:  function,
					ref:       ref,
					call:      call,
				})
			}
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("raw process discovery failed for profile %q:\n- %s", analysis.profile.ID, strings.Join(compactStrings(problems), "\n- "))
	}

	sort.Slice(found, func(i, j int) bool {
		left := string(found[i].operation) + "|" + found[i].ref.key() + "|" + stringOperation(found[i].call)
		right := string(found[j].operation) + "|" + found[j].ref.key() + "|" + stringOperation(found[j].call)
		if left != right {
			return left < right
		}
		return found[i].call.Pos() < found[j].call.Pos()
	})
	ordinals := make(map[string]int)
	observations := make([]rawProcessObservation, 0, len(found))
	for _, item := range found {
		operation, _ := operationForCall(item.call)
		group := string(item.operation) + "|" + item.ref.key() + "|" + string(operation)
		ordinals[group]++
		observations = append(observations, rawProcessObservation{
			site: ObservedSite{
				BoundaryID: string(item.operation),
				Matcher: OperationSite{
					Operation: operation,
					Enclosing: item.ref,
					Ordinal:   ordinals[group],
				},
				Profile: analysis.profile.ID,
			},
			function: item.function,
		})
	}
	return observations, nil
}

func stringOperation(call ssa.CallInstruction) string {
	operation, _ := operationForCall(call)
	return string(operation)
}

func resolveRawProcessTargets(analysis *loadedAnalysis) ([]rawProcessTarget, error) {
	definitions := []struct {
		operation rawProcessOperation
		object    ObjectRef
		hasSignal bool
		optional  bool
	}{
		{operation: rawProcessSyscallKill, object: ObjectRef{Package: "syscall", Name: "Kill"}, hasSignal: true, optional: true},
		{operation: rawProcessSignal, object: ObjectRef{Package: "os", Receiver: "Process", Name: "Signal"}, hasSignal: true},
	}

	targets := make([]rawProcessTarget, 0, len(definitions))
	var problems []string
	for _, definition := range definitions {
		pkg := analysis.packages[definition.object.Package]
		if pkg == nil || pkg.Types == nil {
			problems = append(problems, fmt.Sprintf("raw process target package %q was not loaded", definition.object.Package))
			continue
		}
		function, resolveErr := resolveFunctionObject(pkg.Types, definition.object)
		if resolveErr != nil {
			if definition.optional && strings.Contains(resolveErr.Error(), "does not exist") {
				continue
			}
			problems = append(problems, fmt.Sprintf("raw process target %s: %v", definition.object.key(), resolveErr))
			continue
		}
		targets = append(targets, rawProcessTarget{
			operation: definition.operation,
			boundary: resolvedBoundary{
				definition: BoundaryDefinition{Object: definition.object, Match: ObjectMatchExact},
				object:     function,
				function:   function,
			},
			hasSignal: definition.hasSignal,
		})
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("resolving raw process primitives:\n- %s", strings.Join(compactStrings(problems), "\n- "))
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].operation < targets[j].operation })
	return targets, nil
}

func rawProcessMatches(call ssa.CallInstruction, callees []*ssa.Function, targets []rawProcessTarget, analysis *loadedAnalysis) []rawProcessTarget {
	var matches []rawProcessTarget
	for _, target := range targets {
		if callMatchesBoundary(call, callees, target.boundary, analysis.callGraph, analysis.effectiveReceiverTypes(call)) {
			matches = append(matches, target)
			continue
		}
		// syscall.Kill is routinely injected behind a typed test seam. An
		// unresolved callable with its exact named signature is potentially the
		// raw syscall and therefore remains destructive unless signal 0 is
		// statically proven. Other methods have signatures too broad to infer
		// identity without a concrete callee.
		if target.operation == rawProcessSyscallKill &&
			callMayReachBoundary(call.Common(), target.boundary) &&
			call.Common().StaticCallee() == nil &&
			hasOpenWorldFunctionSource(call.Common().Value, make(map[ssa.Value]bool)) {
			matches = append(matches, target)
		}
	}
	return matches
}

func rawProcessSignalIsDefinitelyZero(call ssa.CallInstruction) bool {
	args := call.Common().Args
	if len(args) == 0 {
		return false
	}
	return definitelyZeroInteger(args[len(args)-1], make(map[ssa.Value]bool))
}

func definitelyZeroInteger(value ssa.Value, visiting map[ssa.Value]bool) bool {
	if value == nil || visiting[value] {
		return false
	}
	visiting[value] = true
	defer delete(visiting, value)
	switch value := value.(type) {
	case *ssa.Const:
		return value.Value != nil && constant.Sign(value.Value) == 0
	case *ssa.MakeInterface:
		return definitelyZeroInteger(value.X, visiting)
	case *ssa.ChangeInterface:
		return definitelyZeroInteger(value.X, visiting)
	case *ssa.ChangeType:
		return definitelyZeroInteger(value.X, visiting)
	case *ssa.Convert:
		return definitelyZeroInteger(value.X, visiting)
	case *ssa.TypeAssert:
		return definitelyZeroInteger(value.X, visiting)
	case *ssa.Extract:
		return definitelyZeroInteger(value.Tuple, visiting)
	case *ssa.Phi:
		if len(value.Edges) == 0 {
			return false
		}
		for _, edge := range value.Edges {
			if !definitelyZeroInteger(edge, visiting) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validateRawProcessGuard(analysis *loadedAnalysis, vehicles []rawProcessVehicle, evidence []rawProcessEvidence) error {
	observed, err := scanRawProcessEffects(analysis)
	if err != nil {
		return err
	}

	vehicleRoots := make(map[string]*ssa.Function, len(vehicles))
	var problems []string
	for _, vehicle := range vehicles {
		if strings.TrimSpace(vehicle.ID) == "" {
			problems = append(problems, "raw process vehicle has an empty id")
			continue
		}
		if _, exists := vehicleRoots[vehicle.ID]; exists {
			problems = append(problems, fmt.Sprintf("raw process vehicle %q is defined more than once", vehicle.ID))
			continue
		}
		pkg := analysis.packages[vehicle.Root.Package]
		if pkg == nil || pkg.Types == nil {
			problems = append(problems, fmt.Sprintf("raw process vehicle %q package %q was not loaded", vehicle.ID, vehicle.Root.Package))
			continue
		}
		rootObject, rootErr := resolveFunctionObject(pkg.Types, vehicle.Root)
		if rootErr != nil {
			problems = append(problems, fmt.Sprintf("raw process vehicle %q: %v", vehicle.ID, rootErr))
			continue
		}
		root := analysis.program.FuncValue(rootObject)
		if root == nil || !analysis.sourceFuncs[root] {
			problems = append(problems, fmt.Sprintf("raw process vehicle %q root %s is not authored production source", vehicle.ID, vehicle.Root.key()))
			continue
		}
		vehicleRoots[vehicle.ID] = root
	}

	evidenceBySite := make(map[string]rawProcessEvidence)
	for _, item := range evidence {
		if !knownRawProcessOperation(item.Operation) {
			problems = append(problems, fmt.Sprintf("raw process evidence names unknown operation %q", item.Operation))
		}
		if len(item.VehicleIDs) == 0 {
			problems = append(problems, fmt.Sprintf("raw process evidence %s names no typed vehicles", item.Matcher.key()))
		}
		seenVehicles := make(map[string]bool)
		for _, vehicleID := range item.VehicleIDs {
			if seenVehicles[vehicleID] {
				problems = append(problems, fmt.Sprintf("raw process evidence %s repeats typed vehicle %q", item.Matcher.key(), vehicleID))
				continue
			}
			seenVehicles[vehicleID] = true
			if _, exists := vehicleRoots[vehicleID]; !exists {
				problems = append(problems, fmt.Sprintf("raw process evidence %s names unknown typed vehicle %q", item.Matcher.key(), vehicleID))
			}
		}
		seenProfiles := make(map[BuildProfileID]bool)
		if len(item.Profiles) == 0 {
			problems = append(problems, fmt.Sprintf("raw process evidence %s has no build profiles", item.Matcher.key()))
		}
		for _, profile := range item.Profiles {
			if _, ok := canonicalAnalysisProfile(profile); !ok {
				problems = append(problems, fmt.Sprintf("raw process evidence %s names non-canonical profile %q", item.Matcher.key(), profile))
			}
			if seenProfiles[profile] {
				problems = append(problems, fmt.Sprintf("raw process evidence %s repeats profile %q", item.Matcher.key(), profile))
				continue
			}
			seenProfiles[profile] = true
			if profile != analysis.profile.ID {
				continue
			}
			key := rawProcessEvidenceKey(item.Operation, item.Matcher)
			if previous, exists := evidenceBySite[key]; exists {
				problems = append(problems, fmt.Sprintf("duplicate raw process evidence for %s (%q and %q)", key, previous.VehicleIDs, item.VehicleIDs))
				continue
			}
			evidenceBySite[key] = item
		}
	}

	observedKeys := make(map[string]bool, len(observed))
	for _, observation := range observed {
		operation := rawProcessOperation(observation.site.BoundaryID)
		key := rawProcessEvidenceKey(operation, observation.site.Matcher)
		observedKeys[key] = true
		item, exists := evidenceBySite[key]
		if !exists {
			problems = append(problems, fmt.Sprintf("%s at %s has no evidence for profile %q", operation, observation.site.Matcher.key(), analysis.profile.ID))
			continue
		}
		roots := make(map[*ssa.Function]string, len(item.VehicleIDs))
		for _, vehicleID := range item.VehicleIDs {
			root := vehicleRoots[vehicleID]
			if root == nil {
				continue
			}
			roots[root] = vehicleID
			if !rawProcessFunctionReachable(analysis, root, observation.function) {
				problems = append(problems, fmt.Sprintf("raw process evidence %s at %s is not reachable from typed vehicle %q (%s)", operation, observation.site.Matcher.key(), vehicleID, itemRootKey(vehicles, vehicleID)))
			}
		}
		if len(roots) != 0 {
			problems = append(problems, rawProcessCallerContainmentProblems(analysis, observation, roots)...)
		}
	}
	for key := range evidenceBySite {
		if !observedKeys[key] {
			problems = append(problems, fmt.Sprintf("stale raw process evidence %s was not observed in profile %q", key, analysis.profile.ID))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return fmt.Errorf("raw process guard failed for profile %q:\n- %s", analysis.profile.ID, strings.Join(compactStrings(problems), "\n- "))
	}
	return nil
}

func knownRawProcessOperation(operation rawProcessOperation) bool {
	switch operation {
	case rawProcessSyscallKill, rawProcessSignal:
		return true
	default:
		return false
	}
}

func rawProcessEvidenceKey(operation rawProcessOperation, matcher OperationSite) string {
	return string(operation) + "|" + matcher.key()
}

func rawProcessFunctionReachable(analysis *loadedAnalysis, root, target *ssa.Function) bool {
	seen := make(map[*ssa.Function]bool)
	var visit func(*ssa.Function) bool
	visit = func(function *ssa.Function) bool {
		if function == nil || seen[function] {
			return false
		}
		seen[function] = true
		if function == target || function.Origin() == target || target.Origin() == function {
			return true
		}
		node := analysis.callGraph.Nodes[function]
		if node == nil {
			return false
		}
		for _, edge := range node.Out {
			if edge.Callee != nil && visit(edge.Callee.Func) {
				return true
			}
		}
		return false
	}
	return visit(root)
}

// rawProcessCallerContainmentProblems proves that every current production
// caller path to one raw leaf crosses one of its explicitly named vehicles.
// Vehicle roots are barriers: callers may reach the vehicle however they
// choose, but no caller may enter the raw implementation below that boundary.
func rawProcessCallerContainmentProblems(analysis *loadedAnalysis, observation rawProcessObservation, roots map[*ssa.Function]string) []string {
	if _, ok := rawProcessRootID(observation.function, roots); ok {
		return nil
	}

	nodes := map[*ssa.Function]bool{observation.function: true}
	edges := make(map[*ssa.Function]map[*ssa.Function]bool)
	rootIncoming := make(map[*ssa.Function]map[string]bool)
	externalIncoming := make(map[*ssa.Function]map[string]bool)
	queue := []*ssa.Function{observation.function}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		node := analysis.callGraph.Nodes[current]
		if node == nil {
			continue
		}
		incoming := append([]*callgraph.Edge(nil), node.In...)
		sort.Slice(incoming, func(i, j int) bool {
			return rawProcessCallerEdgeKey(incoming[i]) < rawProcessCallerEdgeKey(incoming[j])
		})
		for _, edge := range incoming {
			var caller *ssa.Function
			if edge != nil && edge.Caller != nil {
				caller = edge.Caller.Func
			}
			sources, external := rawProcessExpandCaller(analysis, caller, make(map[*ssa.Function]bool))
			for externalCaller := range external {
				if externalIncoming[current] == nil {
					externalIncoming[current] = make(map[string]bool)
				}
				externalIncoming[current][externalCaller] = true
			}
			for source := range sources {
				if vehicleID, ok := rawProcessRootID(source, roots); ok {
					if rootIncoming[current] == nil {
						rootIncoming[current] = make(map[string]bool)
					}
					rootIncoming[current][vehicleID] = true
					continue
				}
				if edges[source] == nil {
					edges[source] = make(map[*ssa.Function]bool)
				}
				edges[source][current] = true
				if !nodes[source] {
					nodes[source] = true
					queue = append(queue, source)
				}
			}
		}
	}

	components := rawProcessCallerComponents(nodes, edges)
	componentOf := make(map[*ssa.Function]int, len(nodes))
	for componentIndex, component := range components {
		for _, function := range component {
			componentOf[function] = componentIndex
		}
	}
	incomingComponents := make([]map[int]bool, len(components))
	componentRoots := make([]map[string]bool, len(components))
	componentExternal := make([]map[string]bool, len(components))
	for caller, callees := range edges {
		callerComponent := componentOf[caller]
		for callee := range callees {
			calleeComponent := componentOf[callee]
			if callerComponent == calleeComponent {
				continue
			}
			if incomingComponents[calleeComponent] == nil {
				incomingComponents[calleeComponent] = make(map[int]bool)
			}
			incomingComponents[calleeComponent][callerComponent] = true
		}
	}
	for function, vehicleIDs := range rootIncoming {
		component := componentOf[function]
		if componentRoots[component] == nil {
			componentRoots[component] = make(map[string]bool)
		}
		for vehicleID := range vehicleIDs {
			componentRoots[component][vehicleID] = true
		}
	}
	for function, callers := range externalIncoming {
		component := componentOf[function]
		if componentExternal[component] == nil {
			componentExternal[component] = make(map[string]bool)
		}
		for caller := range callers {
			componentExternal[component][caller] = true
		}
	}

	vehicleIDs := sortedRawProcessStringsFromRoots(roots)
	var problems []string
	for componentIndex, component := range components {
		if len(componentExternal[componentIndex]) != 0 {
			problems = append(problems, fmt.Sprintf(
				"%s at %s caller path bypasses typed vehicles %q through non-production caller %q",
				observation.site.BoundaryID,
				observation.site.Matcher.key(),
				vehicleIDs,
				sortedStringSet(componentExternal[componentIndex]),
			))
		}
		if len(incomingComponents[componentIndex]) != 0 || len(componentRoots[componentIndex]) != 0 {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s at %s caller path bypasses typed vehicles %q through %s",
			observation.site.BoundaryID,
			observation.site.Matcher.key(),
			vehicleIDs,
			functionSortKey(component[0]),
		))
	}
	sort.Strings(problems)
	return compactStrings(problems)
}

func rawProcessRootID(function *ssa.Function, roots map[*ssa.Function]string) (string, bool) {
	if function == nil {
		return "", false
	}
	if id, ok := roots[function]; ok {
		return id, true
	}
	if origin := function.Origin(); origin != nil {
		id, ok := roots[origin]
		return id, ok
	}
	return "", false
}

func rawProcessExpandCaller(analysis *loadedAnalysis, function *ssa.Function, visiting map[*ssa.Function]bool) (map[*ssa.Function]bool, map[string]bool) {
	sources := make(map[*ssa.Function]bool)
	external := make(map[string]bool)
	if function == nil {
		external["<nil>"] = true
		return sources, external
	}
	if visiting[function] {
		return sources, external
	}
	visiting[function] = true
	defer delete(visiting, function)
	if analysis.sourceFuncs[function] {
		sources[function] = true
		return sources, external
	}
	if origin := function.Origin(); origin != nil && analysis.sourceFuncs[origin] {
		sources[function] = true
		return sources, external
	}
	if !dispatchOnlySynthetic(function.Synthetic) {
		external[functionSortKey(function)] = true
		return sources, external
	}
	node := analysis.callGraph.Nodes[function]
	if node == nil || len(node.In) == 0 {
		// CHA creates unused adaptation wrappers. With no caller they are not a
		// production path and must not become synthetic bypass entries.
		return sources, external
	}
	incoming := append([]*callgraph.Edge(nil), node.In...)
	sort.Slice(incoming, func(i, j int) bool {
		return rawProcessCallerEdgeKey(incoming[i]) < rawProcessCallerEdgeKey(incoming[j])
	})
	for _, edge := range incoming {
		var caller *ssa.Function
		if edge != nil && edge.Caller != nil {
			caller = edge.Caller.Func
		}
		nestedSources, nestedExternal := rawProcessExpandCaller(analysis, caller, visiting)
		for source := range nestedSources {
			sources[source] = true
		}
		for externalCaller := range nestedExternal {
			external[externalCaller] = true
		}
	}
	return sources, external
}

func rawProcessCallerEdgeKey(edge *callgraph.Edge) string {
	if edge == nil || edge.Caller == nil {
		return ""
	}
	return functionSortKey(edge.Caller.Func)
}

func rawProcessCallerComponents(nodes map[*ssa.Function]bool, edges map[*ssa.Function]map[*ssa.Function]bool) [][]*ssa.Function {
	ordered := sortedRawProcessFunctions(nodes)
	visited := make(map[*ssa.Function]bool, len(nodes))
	var finish []*ssa.Function
	var visitForward func(*ssa.Function)
	visitForward = func(function *ssa.Function) {
		if visited[function] {
			return
		}
		visited[function] = true
		for _, callee := range sortedRawProcessFunctions(edges[function]) {
			visitForward(callee)
		}
		finish = append(finish, function)
	}
	for _, function := range ordered {
		visitForward(function)
	}

	reverse := make(map[*ssa.Function]map[*ssa.Function]bool)
	for caller, callees := range edges {
		for callee := range callees {
			if reverse[callee] == nil {
				reverse[callee] = make(map[*ssa.Function]bool)
			}
			reverse[callee][caller] = true
		}
	}
	visited = make(map[*ssa.Function]bool, len(nodes))
	var components [][]*ssa.Function
	var collectReverse func(*ssa.Function, *[]*ssa.Function)
	collectReverse = func(function *ssa.Function, component *[]*ssa.Function) {
		if visited[function] {
			return
		}
		visited[function] = true
		*component = append(*component, function)
		for _, caller := range sortedRawProcessFunctions(reverse[function]) {
			collectReverse(caller, component)
		}
	}
	for index := len(finish) - 1; index >= 0; index-- {
		if visited[finish[index]] {
			continue
		}
		var component []*ssa.Function
		collectReverse(finish[index], &component)
		sort.Slice(component, func(i, j int) bool {
			return functionSortKey(component[i]) < functionSortKey(component[j])
		})
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		return functionSortKey(components[i][0]) < functionSortKey(components[j][0])
	})
	return components
}

func sortedRawProcessFunctions(set map[*ssa.Function]bool) []*ssa.Function {
	result := make([]*ssa.Function, 0, len(set))
	for function := range set {
		result = append(result, function)
	}
	sort.Slice(result, func(i, j int) bool {
		return functionSortKey(result[i]) < functionSortKey(result[j])
	})
	return result
}

func sortedRawProcessStringsFromRoots(roots map[*ssa.Function]string) []string {
	set := make(map[string]bool, len(roots))
	for _, id := range roots {
		set[id] = true
	}
	return sortedStringSet(set)
}

func sortedStringSet(set map[string]bool) []string {
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func itemRootKey(vehicles []rawProcessVehicle, id string) string {
	for _, vehicle := range vehicles {
		if vehicle.ID == id {
			return vehicle.Root.key()
		}
	}
	return "<unknown>"
}
