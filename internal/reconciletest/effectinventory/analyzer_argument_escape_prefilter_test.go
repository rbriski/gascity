package effectinventory

import (
	"context"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestDiscoverProfileTracesCompatibleEscapedChannelArguments(t *testing.T) {
	const packageName = "callescape/channelprefilter"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:     "channelprefilter.approved",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Approved"},
		Match:  ObjectMatchChannel,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want escaped-channel diagnostics")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, owner := range []string{"EscapeChannelResult", "EscapeInjectedChannel", "EscapeUnsafeChannel"} {
		diagnostic := diagnosticLine(err.Error(), owner)
		if !strings.Contains(diagnostic, boundary.ID) {
			t.Errorf("discoverProfile() diagnostic for %s = %q, want boundary %q", owner, diagnostic, boundary.ID)
		}
	}
}

func TestCollectEscapedArgumentBoundariesSkipsInapplicableChannelTrace(t *testing.T) {
	argument := fixtureEscapedCallResult(t)
	tests := []struct {
		name       string
		boundaries []resolvedBoundary
	}{
		{
			name: "no channel definitions",
			boundaries: []resolvedBoundary{{definition: BoundaryDefinition{
				ID:    "channelprefilter.store-only",
				Match: ObjectMatchExact,
			}}},
		},
		{
			name: "incompatible channel definition",
			boundaries: []resolvedBoundary{{
				definition: BoundaryDefinition{ID: "channelprefilter.channel", Match: ObjectMatchChannel},
				channel:    types.NewChan(types.SendRecv, types.Typ[types.String]),
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A channel trace of the call-valued argument requires a call graph.
			// Leaving it absent makes an accidental trace fail deterministically.
			analysis := &loadedAnalysis{}
			matches := make(map[string]bool)
			analysis.collectEscapedArgumentBoundaries(argument, tt.boundaries, matches, make(map[ssa.Value]bool))
			if len(matches) != 0 {
				t.Fatalf("collectEscapedArgumentBoundaries() matches = %v, want none", matches)
			}
		})
	}
}

func fixtureEscapedCallResult(t *testing.T) ssa.Value {
	t.Helper()
	const packageName = "callescape/channelprefilter"
	analysis, err := loadAnalysis(context.Background(), fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	}), fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	for function := range analysis.sourceFuncs {
		if function.Name() != "EscapeResult" {
			continue
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(*ssa.Call)
				if !ok || call.Call.StaticCallee() == nil || call.Call.StaticCallee().Name() != "AcceptInt" || len(call.Call.Args) != 1 {
					continue
				}
				if _, ok := call.Call.Args[0].(*ssa.Call); !ok {
					t.Fatalf("AcceptInt argument = %T, want call-valued fixture", call.Call.Args[0])
				}
				return call.Call.Args[0]
			}
		}
	}
	t.Fatal("EscapeResult AcceptInt argument not found")
	return nil
}
