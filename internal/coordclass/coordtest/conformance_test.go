package coordtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// fakeGraph is a minimal beads.GraphApplyStore that resolves every node key,
// used to exercise RunGraphStoreTests non-vacuously.
type fakeGraph struct{}

func (fakeGraph) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	ids := make(map[string]string, len(plan.Nodes))
	for i, n := range plan.Nodes {
		ids[n.Key] = fmt.Sprintf("g-%d", i)
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

// TestClassedSuiteRunsForEveryClass proves the classed-store suite is not vacuous:
// run with Skip:false against a MemStore, every subtest executes and passes for
// every class.
func TestClassedSuiteRunsForEveryClass(t *testing.T) {
	for _, class := range coordclass.Classes() {
		class := class
		t.Run(class.String(), func(t *testing.T) {
			RunClassedStoreTestsWithOptions(t, class, func() beads.Store { return beads.NewMemStore() }, Options{Skip: false})
		})
	}
}

// TestClassedSuiteSkipsByDefault proves the P0 default skips the suite BEFORE any
// subtest runs: the factory is never invoked.
func TestClassedSuiteSkipsByDefault(t *testing.T) {
	called := 0
	factory := func() beads.Store { called++; return beads.NewMemStore() }
	t.Run("default", func(t *testing.T) {
		RunClassedStoreTests(t, coordclass.ClassMessaging, factory)
	})
	if called != 0 {
		t.Fatalf("default suite invoked the factory %d time(s); expected skip before any subtest", called)
	}
}

// TestGraphSuiteRunsNonVacuously proves RunGraphStoreTests executes and passes
// when run with Skip:false against a working GraphStore.
func TestGraphSuiteRunsNonVacuously(t *testing.T) {
	RunGraphStoreTestsWithOptions(t, func() beads.GraphApplyStore { return fakeGraph{} }, Options{Skip: false})
}

// TestGraphSuiteSkipsByDefault proves the P0 default skips before invoking the
// factory.
func TestGraphSuiteSkipsByDefault(t *testing.T) {
	called := 0
	//nolint:unparam // skip-path probe: the default suite must skip before ever calling this factory, so its return is intentionally constant.
	factory := func() beads.GraphApplyStore { called++; return fakeGraph{} }
	t.Run("default", func(t *testing.T) {
		RunGraphStoreTests(t, factory)
	})
	if called != 0 {
		t.Fatalf("default graph suite invoked the factory %d time(s); expected skip before any subtest", called)
	}
}
