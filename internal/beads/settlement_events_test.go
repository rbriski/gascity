package beads

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

func openSettlementJournal(t *testing.T) (*JournalStore, *graphstore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "settle-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return NewJournalStore(gs), gs
}

// TestSettlementVocabRegisteredAtOpen proves NewJournalStore registers the coarse
// settlement vocabulary so an emit is accepted (I-5), and that an unregistered
// (engine, type) — e.g. an attempt settle under v1, which is not part of the v1
// closed vocabulary — is still rejected.
func TestSettlementVocabRegisteredAtOpen(t *testing.T) {
	t.Parallel()
	store, _ := openSettlementJournal(t)
	ctx := context.Background()

	accepted := []struct {
		engine string
		typ    string
	}{
		{SettlementEngineV2, SettlementRootType},
		{SettlementEngineV2, SettlementAttemptType},
		{SettlementEngineV2, SettlementWorkflowFinalizedType},
		{SettlementEngineV1, SettlementRootType},
	}
	for _, tc := range accepted {
		ev, err := SettlementEvent(tc.typ, SettlementPayload{Root: "r1", Bead: "b1", Outcome: "pass"})
		if err != nil {
			t.Fatalf("SettlementEvent(%s): %v", tc.typ, err)
		}
		streamID := SettlementStreamID("r1-" + tc.engine + "-" + tc.typ)
		if _, err := store.AppendEvent(ctx, streamID, tc.engine, 0, 0, []graphstore.JournalEvent{ev}); err != nil {
			t.Fatalf("append (%s,%s): %v", tc.engine, tc.typ, err)
		}
	}

	// v1 attempt is NOT registered (blueprint: v1 emits only root settlements) —
	// the closed vocabulary must reject it.
	ev, err := SettlementEvent(SettlementAttemptType, SettlementPayload{Root: "r2", Bead: "b2", Outcome: "fail", Attempt: 1})
	if err != nil {
		t.Fatalf("SettlementEvent: %v", err)
	}
	if _, err := store.AppendEvent(ctx, SettlementStreamID("r2"), SettlementEngineV1, 0, 0, []graphstore.JournalEvent{ev}); !errors.Is(err, graphstore.ErrUnknownEventType) {
		t.Fatalf("append v1 attempt: err = %v, want ErrUnknownEventType", err)
	}
}

// TestSettlementPayloadDeterministic proves the payload marshals to byte-identical
// bytes across calls (clock-free, map-free), which is what makes an idempotent
// redo dedupe under R-IDEM rather than tripping ErrIdemTokenReuse.
func TestSettlementPayloadDeterministic(t *testing.T) {
	t.Parallel()
	p := SettlementPayload{Root: "root-9", Bead: "logical-3", Kind: "retry", Outcome: "pass", Attempt: 2, StoreRef: "rig:alpha"}
	a, err := SettlementEvent(SettlementAttemptType, p)
	if err != nil {
		t.Fatalf("SettlementEvent a: %v", err)
	}
	b, err := SettlementEvent(SettlementAttemptType, p)
	if err != nil {
		t.Fatalf("SettlementEvent b: %v", err)
	}
	if string(a.Payload) != string(b.Payload) {
		t.Fatalf("payload not deterministic:\n a=%s\n b=%s", a.Payload, b.Payload)
	}
	if canon.Hash(a.Payload) != canon.Hash(b.Payload) {
		t.Fatalf("payload hash not deterministic")
	}
	if a.IRContractVersion != "" {
		t.Fatalf("IRContractVersion = %q, want empty (coarse v1/v2 events carry no IR contract)", a.IRContractVersion)
	}
}

func TestSettlementIdemTokenScheme(t *testing.T) {
	t.Parallel()
	if got, want := SettlementIdemToken(SettlementRootType, "root-1", "pass", 0), "settlement.root/root-1/pass"; got != want {
		t.Fatalf("root token = %q, want %q", got, want)
	}
	// A different outcome mints a different token (a genuine re-settlement is a
	// second provenance fact, not a dedupe).
	if SettlementIdemToken(SettlementRootType, "root-1", "pass", 0) == SettlementIdemToken(SettlementRootType, "root-1", "fail", 0) {
		t.Fatalf("distinct outcomes must mint distinct tokens")
	}
	// Attempt tokens carry the attempt suffix so attempt N and N+1 are distinct.
	if got, want := SettlementIdemToken(SettlementAttemptType, "log-1", "fail", 2), "settlement.attempt/log-1/fail/2"; got != want {
		t.Fatalf("attempt token = %q, want %q", got, want)
	}
	if SettlementIdemToken(SettlementAttemptType, "log-1", "fail", 1) == SettlementIdemToken(SettlementAttemptType, "log-1", "fail", 2) {
		t.Fatalf("distinct attempts must mint distinct tokens")
	}
}

func TestSettlementStreamIDPerRoot(t *testing.T) {
	t.Parallel()
	if got, want := SettlementStreamID("gcg-abc"), "settlement/gcg-abc"; got != want {
		t.Fatalf("stream id = %q, want %q", got, want)
	}
	if SettlementStreamID("a") == SettlementStreamID("b") {
		t.Fatalf("distinct roots must map to distinct streams")
	}
}
