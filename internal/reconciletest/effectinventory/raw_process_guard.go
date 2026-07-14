package effectinventory

import (
	"fmt"
	"go/constant"
	"sort"
	"strings"

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
	Operation rawProcessOperation
	Matcher   OperationSite
	VehicleID string
	Profiles  []BuildProfileID
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
			VehicleID: vehicleID,
			Profiles:  append([]BuildProfileID(nil), profiles...),
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
				callees := resolvedCallees(analysis.callGraph, function, call)
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
		if _, exists := vehicleRoots[item.VehicleID]; !exists {
			problems = append(problems, fmt.Sprintf("raw process evidence %s names unknown typed vehicle %q", item.Matcher.key(), item.VehicleID))
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
				problems = append(problems, fmt.Sprintf("duplicate raw process evidence for %s (%q and %q)", key, previous.VehicleID, item.VehicleID))
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
		root := vehicleRoots[item.VehicleID]
		if root == nil {
			continue
		}
		if !rawProcessFunctionReachable(analysis, root, observation.function) {
			problems = append(problems, fmt.Sprintf("raw process evidence %s at %s is not reachable from typed vehicle %q (%s)", operation, observation.site.Matcher.key(), item.VehicleID, itemRootKey(vehicles, item.VehicleID)))
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

func itemRootKey(vehicles []rawProcessVehicle, id string) string {
	for _, vehicle := range vehicles {
		if vehicle.ID == id {
			return vehicle.Root.key()
		}
	}
	return "<unknown>"
}
