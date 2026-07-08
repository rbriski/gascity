package dispatch

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// orphanedControlFixture builds an open control bead whose workflow root is named
// in metadata but does NOT exist in the store, so closeOrphanedControl fires. The
// contract governs the engine the coarse settlement is tagged with.
func orphanedControlFixture(t *testing.T, store beads.Store, kind, contract string) (missingRoot string, control beads.Bead) {
	t.Helper()
	missingRoot = "gcg-ghost-root"
	meta := map[string]string{
		beadmeta.KindMetadataKey:         kind,
		beadmeta.RootBeadIDMetadataKey:   missingRoot,
		beadmeta.RootStoreRefMetadataKey: "city:test",
	}
	if contract != "" {
		meta[beadmeta.FormulaContractMetadataKey] = contract
	}
	control = mustCreate(t, store, beads.Bead{Title: "orphaned control", Metadata: meta})
	return missingRoot, control
}

// TestOrphanedControlEmitsV1RootSettlement proves the P5.4 v1 failure-terminal
// anchor: an orphaned control with no graph.v2 contract emits a coarse
// settlement.root under engine v1, keyed on the (missing) root, with the control
// bead as the settled bead and outcome fail.
func TestOrphanedControlEmitsV1RootSettlement(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	missingRoot, control := orphanedControlFixture(t, store, beadmeta.KindFanout, "")
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, control, ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(orphaned): %v", err)
	}
	if !result.Processed || result.Action != "orphaned-workflow" {
		t.Fatalf("result = %+v, want processed orphaned-workflow", result)
	}
	if len(spy.root) != 1 || spy.total() != 1 {
		t.Fatalf("emits = %d (root=%d), want exactly one root settlement", spy.total(), len(spy.root))
	}
	got := spy.root[0]
	if got.Root != missingRoot || got.Bead != control.ID || got.Outcome != beadmeta.OutcomeFail {
		t.Fatalf("root settlement = %+v, want {root=%s bead=%s outcome=fail}", got, missingRoot, control.ID)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.engines) != 1 || spy.engines[0] != beads.SettlementEngineV1 {
		t.Fatalf("engines = %v, want [v1] (no graph.v2 contract)", spy.engines)
	}
}

// TestOrphanedControlEmitsV2RootSettlement proves the engine is DATA-derived: the
// same orphaned close emits under v2 when the control carries the graph.v2
// contract.
func TestOrphanedControlEmitsV2RootSettlement(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	_, control := orphanedControlFixture(t, store, beadmeta.KindRetry, beadmeta.FormulaContractGraphV2)
	spy := &spySettlementEmitter{}

	if _, err := ProcessControl(store, control, ProcessOptions{Settlements: spy}); err != nil {
		t.Fatalf("ProcessControl(orphaned v2): %v", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.engines) != 1 || spy.engines[0] != beads.SettlementEngineV2 {
		t.Fatalf("engines = %v, want [v2] (graph.v2 contract)", spy.engines)
	}
}

// TestOrphanedControlNilEmitterByteIdentity proves the anchor is strictly
// after-the-fact: with a nil emitter the control result and the closed control
// bead's terminal state are identical to the spy run.
func TestOrphanedControlNilEmitterByteIdentity(t *testing.T) {
	t.Parallel()
	spyStore := beads.NewMemStore()
	_, spyControl := orphanedControlFixture(t, spyStore, beadmeta.KindFanout, "")
	spyResult, err := ProcessControl(spyStore, spyControl, ProcessOptions{Settlements: &spySettlementEmitter{}})
	if err != nil {
		t.Fatalf("ProcessControl(spy): %v", err)
	}

	nilStore := beads.NewMemStore()
	_, nilControl := orphanedControlFixture(t, nilStore, beadmeta.KindFanout, "")
	nilResult, err := ProcessControl(nilStore, nilControl, ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil): %v", err)
	}

	if nilResult != spyResult {
		t.Fatalf("result differs: nil=%+v spy=%+v", nilResult, spyResult)
	}
	nilAfter := mustGet(t, nilStore, nilControl.ID)
	spyAfter := mustGet(t, spyStore, spyControl.ID)
	if nilAfter.Status != spyAfter.Status ||
		nilAfter.Metadata[beadmeta.OutcomeMetadataKey] != spyAfter.Metadata[beadmeta.OutcomeMetadataKey] {
		t.Fatalf("closed control differs: nil=%s/%s spy=%s/%s",
			nilAfter.Status, nilAfter.Metadata[beadmeta.OutcomeMetadataKey],
			spyAfter.Status, spyAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestOrphanedControlEmitFailureNeverAltersBead proves an emit failure is
// swallowed: the orphaned control still closes fail and ProcessControl returns
// its normal result.
func TestOrphanedControlEmitFailureNeverAltersBead(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	_, control := orphanedControlFixture(t, store, beadmeta.KindFanout, "")

	result, err := ProcessControl(store, control, ProcessOptions{
		Settlements: failingSettlementEmitter{err: errors.New("journal boom")},
	})
	if err != nil {
		t.Fatalf("ProcessControl with failing emitter returned err = %v, want nil", err)
	}
	if !result.Processed || result.Action != "orphaned-workflow" {
		t.Fatalf("result = %+v, want processed orphaned-workflow", result)
	}
	after := mustGet(t, store, control.ID)
	if after.Status != "closed" || after.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("control = %s/%s, want closed/fail (bead unaffected by emit failure)",
			after.Status, after.Metadata[beadmeta.OutcomeMetadataKey])
	}
}
