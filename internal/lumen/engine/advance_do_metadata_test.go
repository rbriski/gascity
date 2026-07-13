package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// metaDoDoc is a single pool-mode do carrying one static routing key
// (gc.continuation_group=main) — the ITEM B marquee shape.
func metaDoDoc() (doc, streamID string) {
	return blockDoc("meta", doNodeWithMetadata("hello", "Say hello.", nil,
		`{"gc.continuation_group":"main"}`)), "gcg-run-adv-do-meta"
}

// activatedMetadata returns the metadata map carried by an activation's
// node.activated payload (the pool-dispatch observability parity field).
func activatedMetadata(t *testing.T, events []graphstore.StoredEvent, activation string) map[string]string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string            `json:"activation"`
			Metadata   map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		if p.Activation == activation {
			return p.Metadata
		}
	}
	t.Fatalf("no node.activated for activation %q", activation)
	return nil
}

// TestAdvanceDispatchCarriesStaticMetadata proves the passthrough seam end to end at
// the driver: a pool-do carrying static metadata dispatches with WorkDispatch.Metadata
// populated AND stamps the same map on its node.activated payload — and the TWO SOURCES
// AGREE (design open-risk #5), because both derive from the one static IR unit in the
// same pass.
func TestAdvanceDispatchCarriesStaticMetadata(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := metaDoDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	res, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !res.Parked {
		t.Fatalf("advance = %+v err %v, want Parked", res, err)
	}

	want := map[string]string{"gc.continuation_group": "main"}

	// Source 1: the WorkDispatch handed to the create seam.
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork called %d times, want 1", fake.dispatchCount())
	}
	dispatched := fake.dispatches[0].Metadata
	if !reflect.DeepEqual(dispatched, want) {
		t.Fatalf("WorkDispatch.Metadata = %v, want %v", dispatched, want)
	}

	// Source 2: the pool node.activated payload (journal observability parity).
	journaled := activatedMetadata(t, streamStored(t, store, streamID), "hello:0")
	if !reflect.DeepEqual(journaled, want) {
		t.Fatalf("node.activated metadata = %v, want %v", journaled, want)
	}

	// The two sources AGREE (the single-decode-point invariant).
	if !reflect.DeepEqual(dispatched, journaled) {
		t.Fatalf("two metadata sources diverged: dispatch %v != payload %v", dispatched, journaled)
	}
}

// TestAdvanceMetadataRebuildByteIdentity pins that a metadata-bearing pool-do stream
// folds byte-identically live-vs-rebuild (DET-T-17): the reducer is transparent to the
// node.activated metadata field, so the incremental Tier-A projection equals a
// DROP+refold from genesis. If applyNodeActivated ever folded metadata, the two would
// still match — that mutation is caught by the StateHash pin in the internal package;
// this proves the PROJECTION stays clean.
func TestAdvanceMetadataRebuildByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := metaDoDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	// Drive to seal so the stream carries node.activated + owned.admitted + settle.
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settle("wb-1", engine.OutcomePass, "hi")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v err %v, want Sealed", r2, err)
	}

	// The node.activated genuinely carried metadata (non-vacuous).
	if got := activatedMetadata(t, streamStored(t, store, streamID), "hello:0")["gc.continuation_group"]; got != "main" {
		t.Fatalf("node.activated metadata gc.continuation_group = %q, want main (comparison would be vacuous)", got)
	}

	live := dumpTierA(t, store, streamID)
	if err := store.RebuildTierA(ctx, engine.Reducer(), streamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := dumpTierA(t, store, streamID)
	if live != rebuilt {
		t.Fatalf("metadata-bearing stream: live projection != rebuild:\n--- live ---\n%s\n--- rebuild ---\n%s", live, rebuilt)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestAdvanceMetadataDeterministicAcrossCrashReadopt is the load-bearing determinism
// claim: static metadata is byte-identical across genesis and a crashAfterDispatch
// re-adopt. A crash after the bead is created but before the dispatch fact commits
// leaves BeadID unrecorded; the re-Advance re-derives metadata from u.leaf (rebuilt from
// the IR) and re-dispatches with the IDENTICAL map — so the re-dispatch metadata equals
// the pre-crash bead metadata, exactly one bead minted.
func TestAdvanceMetadataDeterministicAcrossCrashReadopt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := metaDoDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == "hello:0" {
			return fmt.Errorf("injected crash after dispatch")
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1", fake.dispatchCount())
	}

	// Re-Advance: re-adopt the findable bead and re-dispatch (byte-identical metadata).
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("re-advance: %v", err)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork total = %d, want 2 (re-looked-up)", fake.dispatchCount())
	}
	preCrash := fake.dispatches[0].Metadata
	reAdopt := fake.dispatches[1].Metadata
	if !reflect.DeepEqual(preCrash, reAdopt) {
		t.Fatalf("re-adopt metadata %v != pre-crash metadata %v (static metadata must be byte-identical across resume)", reAdopt, preCrash)
	}
	if reAdopt["gc.continuation_group"] != "main" {
		t.Fatalf("re-adopt metadata = %v, want gc.continuation_group=main", reAdopt)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 (idempotent re-adopt)", minted)
	}
}
