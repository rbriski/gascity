package beads

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/settlementfold"
)

// provFactWant is the expected (seq, engine, type, outcome) of one provenance
// fact, shared by the gate test and the assertFacts helper.
type provFactWant struct {
	seq     uint64
	engine  string
	typ     string
	outcome string
}

// openProvenanceJournal opens a fresh journal-backed store for provenance tests
// and registers the lumen terminal vocabulary the settlement/fence vocab
// registration does not cover (NewJournalStore registers settlement.* + the
// control-epoch fence; a lumen root's run stream carries the lumen terminal
// events these tests fold alongside the settlement stream).
func openProvenanceJournal(t *testing.T) (*JournalStore, *graphstore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "prov-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	store := NewJournalStore(gs)
	gs.RegisterEventType("lumen", "lumen.outcome.settled")
	gs.RegisterEventType("lumen", "lumen.run.closed")
	return store, gs
}

// appendOne appends a single event with the given engine/type/payload to streamID
// at its current head, advancing the dense seq.
func appendOne(t *testing.T, gs *graphstore.Store, streamID, engine, typ, idem string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	head, err := gs.Head(context.Background(), streamID)
	if err != nil {
		t.Fatalf("head %s: %v", streamID, err)
	}
	if _, err := gs.Append(context.Background(), streamID, engine, head, 0,
		[]graphstore.JournalEvent{{Type: typ, IdemToken: idem, Payload: raw}}); err != nil {
		t.Fatalf("append (%s,%s): %v", engine, typ, err)
	}
}

func tierACounts(t *testing.T, gs *graphstore.Store) (nodes, edges, frontier int) {
	t.Helper()
	q := func(table string) int {
		var n int
		if err := gs.ReadDB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return q("nodes"), q("edges"), q("frontier")
}

// TestEngineForContract pins the pure formula_contract→engine mapping.
func TestEngineForContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		contract string
		want     string
	}{
		{"graph.v2", SettlementEngineV2},
		{"  graph.v2  ", SettlementEngineV2}, // trimmed
		{"", SettlementEngineV1},
		{"molecule.v1", SettlementEngineV1},
		{"anything.else", SettlementEngineV1},
	}
	for _, c := range cases {
		if got := EngineForContract(c.contract); got != c.want {
			t.Fatalf("EngineForContract(%q) = %q, want %q", c.contract, got, c.want)
		}
	}
}

// TestThreeEngineProvenanceTimeline is the P5 EXIT gate. It seeds the REAL
// two-stream production topology of a root: the lumen run stream <root> (lumen
// fine-grained terminal facts) and the settlement stream settlement/<root> (v2
// and v1 coarse settlements plus the v2 control-epoch fence). ProvenanceTimeline
// reads it back as TWO per-stream groups, each seq-ordered and correctly
// engine-tagged; all three engines are represented across the groups; the fold is
// total over the interleaved settlement stream; the merge is deterministic; and it
// produces ZERO Tier-A deltas. This proves unified provenance across lumen/v2/v1 —
// as the two streams the store actually produces, NOT a fabricated single-stream
// global sequence — and ends P5.
func TestThreeEngineProvenanceTimeline(t *testing.T) {
	t.Parallel()
	store, gs := openProvenanceJournal(t)
	root := "gcg-root"
	settlementStream := SettlementStreamID(root)

	nodes0, edges0, frontier0 := tierACounts(t, gs)

	// The lumen run stream <root>: the lumen engine's fine-grained terminal facts,
	// dense seq 1..2 in ITS OWN seq space.
	appendOne(t, gs, root, "lumen", "lumen.outcome.settled", "l1",
		map[string]any{"activation": "impl:0", "outcome": "pass"})
	appendOne(t, gs, root, "lumen", "lumen.run.closed", "l2",
		map[string]any{"outcome": "pass"})

	// The settlement stream settlement/<root>: v2 and v1 coarse facts (a root
	// genuinely settles via both a v2 finalize and a v1 autoclose) plus the v2
	// control-epoch fence, interleaved on ONE stream with its OWN dense seq 1..4.
	appendOne(t, gs, settlementStream, SettlementEngineV2, SettlementAttemptType, "a1",
		SettlementPayload{Root: root, Bead: "gcg-log", Outcome: "fail", Attempt: 2})
	appendOne(t, gs, settlementStream, SettlementEngineV2, "control.epoch.fenced", "f1",
		map[string]any{"bead": "gcg-ctl"})
	appendOne(t, gs, settlementStream, SettlementEngineV1, SettlementRootType, "r1",
		SettlementPayload{Root: root, Bead: root, Outcome: "fail"})
	appendOne(t, gs, settlementStream, SettlementEngineV2, SettlementWorkflowFinalizedType, "w1",
		SettlementPayload{Root: root, Bead: "gcg-fin", Outcome: "fail"})

	streams, err := ProvenanceTimeline(context.Background(), store, root)
	if err != nil {
		t.Fatalf("ProvenanceTimeline: %v", err)
	}

	// Two per-stream groups in deterministic order: settlement stream first, then
	// the lumen run stream. No fabricated global sequence across them.
	if len(streams) != 2 {
		t.Fatalf("timeline has %d stream groups, want 2: %+v", len(streams), streams)
	}
	if streams[0].StreamID != settlementStream {
		t.Fatalf("group 0 stream = %q, want %q", streams[0].StreamID, settlementStream)
	}
	if streams[1].StreamID != root {
		t.Fatalf("group 1 stream = %q, want the lumen run stream %q", streams[1].StreamID, root)
	}

	// Settlement group: v2/v2-fence/v1/v2, seq-ordered 1..4 in its own space.
	assertFacts(t, "settlement", streams[0].Facts, []provFactWant{
		{1, SettlementEngineV2, SettlementAttemptType, "fail"},
		{2, SettlementEngineV2, "control.epoch.fenced", ""},
		{3, SettlementEngineV1, SettlementRootType, "fail"},
		{4, SettlementEngineV2, SettlementWorkflowFinalizedType, "fail"},
	})
	// Lumen group: seq-ordered 1..2 in its own space (independent of the above).
	assertFacts(t, "lumen", streams[1].Facts, []provFactWant{
		{1, "lumen", "lumen.outcome.settled", "pass"},
		{2, "lumen", "lumen.run.closed", "pass"},
	})

	// Coarse fact detail survives: the attempt number and the settled bead id.
	if streams[0].Facts[0].Attempt != 2 || streams[0].Facts[0].Bead != "gcg-log" {
		t.Fatalf("attempt fact = %+v, want bead=gcg-log attempt=2", streams[0].Facts[0])
	}

	// All three engines are represented across the two groups.
	engines := map[string]bool{}
	for _, s := range streams {
		for _, f := range s.Facts {
			engines[f.Engine] = true
		}
	}
	for _, want := range []string{"lumen", SettlementEngineV2, SettlementEngineV1} {
		if !engines[want] {
			t.Fatalf("engine %q missing from timeline; engines seen = %v", want, engines)
		}
	}

	// Deterministic merge: a re-read yields the identical grouping (same stream
	// order, same per-stream facts).
	streams2, err := ProvenanceTimeline(context.Background(), store, root)
	if err != nil {
		t.Fatalf("ProvenanceTimeline (re-read): %v", err)
	}
	if !reflect.DeepEqual(streams, streams2) {
		t.Fatalf("re-read differs — merge is not deterministic:\n first=%+v\n second=%+v", streams, streams2)
	}

	// ZERO Tier-A: nothing folded these events into nodes/edges/frontier.
	nodes1, edges1, frontier1 := tierACounts(t, gs)
	if nodes1 != nodes0 || edges1 != edges0 || frontier1 != frontier0 {
		t.Fatalf("Tier-A changed: nodes %d->%d edges %d->%d frontier %d->%d (settlement fold must be provenance-only)",
			nodes0, nodes1, edges0, edges1, frontier0, frontier1)
	}

	// Both hash chains stay intact.
	if err := gs.Verify(context.Background(), settlementStream); err != nil {
		t.Fatalf("Verify(settlement stream): %v (mixed-engine chain must stay intact)", err)
	}
	if err := gs.Verify(context.Background(), root); err != nil {
		t.Fatalf("Verify(lumen run stream): %v", err)
	}
}

// assertFacts checks a per-stream fact slice against the wanted seq/engine/type/
// outcome tuples, requiring the stream's own seq to be dense and ordered.
func assertFacts(t *testing.T, label string, facts []settlementfold.SettlementFact, wants []provFactWant) {
	t.Helper()
	if len(facts) != len(wants) {
		t.Fatalf("%s group has %d facts, want %d: %+v", label, len(facts), len(wants), facts)
	}
	for i, w := range wants {
		f := facts[i]
		if f.Seq != w.seq {
			t.Fatalf("%s fact %d seq = %d, want %d (per-stream seq must be dense/ordered)", label, i, f.Seq, w.seq)
		}
		if f.Engine != w.engine {
			t.Fatalf("%s fact %d engine = %q, want %q", label, i, f.Engine, w.engine)
		}
		if f.Type != w.typ {
			t.Fatalf("%s fact %d type = %q, want %q", label, i, f.Type, w.typ)
		}
		if f.Outcome != w.outcome {
			t.Fatalf("%s fact %d outcome = %q, want %q", label, i, f.Outcome, w.outcome)
		}
	}
}

// TestProvenanceTimelineLumenRunStream proves the second read arm in isolation: a
// lumen root's terminal facts, which live on its own run stream (rootID == stream
// id, not a settlement/ stream), fold into a single lumen group.
func TestProvenanceTimelineLumenRunStream(t *testing.T) {
	t.Parallel()
	store, gs := openProvenanceJournal(t)
	root := "lumen-run-1"

	// A lumen run writes to a stream keyed by the run/root id itself.
	appendOne(t, gs, root, "lumen", "lumen.outcome.settled", "lo1",
		map[string]any{"activation": "step:0", "outcome": "pass"})
	appendOne(t, gs, root, "lumen", "lumen.run.closed", "lc1",
		map[string]any{"outcome": "pass"})

	streams, err := ProvenanceTimeline(context.Background(), store, root)
	if err != nil {
		t.Fatalf("ProvenanceTimeline: %v", err)
	}
	if len(streams) != 1 || streams[0].StreamID != root {
		t.Fatalf("streams = %+v, want a single group for the lumen run stream %q", streams, root)
	}
	facts := streams[0].Facts
	if len(facts) != 2 {
		t.Fatalf("lumen group has %d facts, want 2: %+v", len(facts), facts)
	}
	for _, f := range facts {
		if f.Engine != "lumen" {
			t.Fatalf("fact engine = %q, want lumen", f.Engine)
		}
	}
	if facts[1].Type != "lumen.run.closed" || facts[1].Root != root {
		t.Fatalf("run.closed fact = %+v, want type=lumen.run.closed root=%s", facts[1], root)
	}
}

// TestProvenanceTimelineEmptyRoot proves an unknown root folds to an empty
// timeline (no error) — both stream reads are absent, so no group is returned.
func TestProvenanceTimelineEmptyRoot(t *testing.T) {
	t.Parallel()
	store, _ := openProvenanceJournal(t)
	streams, err := ProvenanceTimeline(context.Background(), store, "gcg-nonexistent")
	if err != nil {
		t.Fatalf("ProvenanceTimeline on empty root: %v", err)
	}
	if len(streams) != 0 {
		t.Fatalf("empty root timeline has %d stream groups, want 0", len(streams))
	}
}

// TestProvenanceTimelineNonJournalStore proves a store without the journal read
// capability is a loud error, not a silent empty timeline (provenance only exists
// on the shared journal).
func TestProvenanceTimelineNonJournalStore(t *testing.T) {
	t.Parallel()
	if _, err := ProvenanceTimeline(context.Background(), NewMemStore(), "gcg-root"); err == nil {
		t.Fatal("ProvenanceTimeline on a non-journal store must error")
	}
}
