package effectinventory

import (
	"context"
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestTraceChannelExpandsRepeatedCallContextOnce(t *testing.T) {
	const packageName = "channelprov/memo"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	analysis, err := loadAnalysis(context.Background(), config, fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	boundary := BoundaryDefinition{
		ID:     "channelprov.memo.approved",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Approved"},
		Match:  ObjectMatchChannel,
	}
	boundaries, err := resolveBoundaries(analysis.packages, []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("resolveBoundaries() error: %v", err)
	}

	argument := escapedCallArgument(t, analysis, "Escape", "AcceptStringChannel")
	tracer := newChannelTracer(analysis, boundaries, nil)
	provenance := tracer.trace(argument, nil)
	if got := provenance.sortedMatches(); len(got) != 1 || got[0] != boundary.ID {
		t.Fatalf("trace() matches = %v, want [%s]", got, boundary.ID)
	}
	const maxExpandedStates = 160
	if tracer.stats.ExpandedStates > maxExpandedStates {
		t.Fatalf("trace() expanded %d states, want <= %d for the linear SSA graph", tracer.stats.ExpandedStates, maxExpandedStates)
	}
}

func escapedCallArgument(t *testing.T, analysis *loadedAnalysis, owner, calleeName string) ssa.Value {
	t.Helper()
	for function := range analysis.sourceFuncs {
		if function.Name() != owner {
			continue
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(*ssa.Call)
				if !ok || call.Call.StaticCallee() == nil || call.Call.StaticCallee().Name() != calleeName || len(call.Call.Args) != 1 {
					continue
				}
				return call.Call.Args[0]
			}
		}
	}
	t.Fatalf("%s call to %s not found", owner, calleeName)
	return nil
}
