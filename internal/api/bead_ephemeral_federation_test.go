package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// scriptStore is a beads.Store stub whose List returns a fixed bead set after an
// optional gate. The embedded Store satisfies the rest of the interface; the
// ephemeral federation only calls List.
type scriptStore struct {
	beads.Store
	out  []beads.Bead
	gate func() error
}

func (s *scriptStore) List(beads.ListQuery) ([]beads.Bead, error) {
	if s.gate != nil {
		if err := s.gate(); err != nil {
			return nil, err
		}
	}
	return s.out, nil
}

// TestBeadEphemeralFederatesRigStoresConcurrently proves the ephemeral handler
// queries the backing stores in parallel, not serially. A dolt-backed rig store
// can take seconds; serial federation made the worker claim path
// (gc hook --claim hits /beads/ephemeral) scale with rig count and time out.
//
// Every rig store shares a barrier: each List blocks until ALL rig stores have
// entered List. Serial federation would leave the first store blocked until the
// barrier's timeout, returning an error that this test rejects; concurrent
// federation lets every store reach the barrier together and return.
func TestBeadEphemeralFederatesRigStoresConcurrently(t *testing.T) {
	const nRigs = 4
	var arrived int32
	allArrived := make(chan struct{})
	gate := func() error {
		if atomic.AddInt32(&arrived, 1) == nRigs {
			close(allArrived)
		}
		select {
		case <-allArrived:
			return nil
		case <-time.After(3 * time.Second):
			return fmt.Errorf("rig List did not run concurrently: federation barrier timed out")
		}
	}

	rigStores := map[string]beads.Store{}
	for i := 0; i < nRigs; i++ {
		name := fmt.Sprintf("rig%02d", i)
		rigStores[name] = &scriptStore{
			Store: beads.NewMemStore(),
			out:   []beads.Bead{{ID: name + "-wisp"}},
			gate:  gate,
		}
	}

	state := newFakeState(t)
	state.cityBeadStore = &scriptStore{Store: beads.NewMemStore(), out: []beads.Bead{{ID: "city-wisp"}}}
	state.stores = rigStores
	s := New(state)

	out, err := s.humaHandleBeadEphemeral(context.Background(), &BeadEphemeralInput{})
	if err != nil {
		t.Fatalf("humaHandleBeadEphemeral: %v (serial federation trips the concurrency barrier)", err)
	}
	got := map[string]bool{}
	for _, b := range out.Body.Items {
		got[b.ID] = true
	}
	if !got["city-wisp"] {
		t.Errorf("city wisp missing from federation (items=%d)", len(out.Body.Items))
	}
	for name := range rigStores {
		if !got[name+"-wisp"] {
			t.Errorf("rig wisp %s-wisp missing from federation", name)
		}
	}
}

// TestBeadEphemeralFederationDedupesAndOrdersCityFirst preserves the federation
// contract that the concurrency rewrite must not regress: the city store is
// federated first, duplicate IDs across stores are returned once, and a
// per-store failure is surfaced as a partial result rather than dropped.
func TestBeadEphemeralFederationDedupesAndOrdersCityFirst(t *testing.T) {
	state := newFakeState(t)
	// City and rig "alpha" both report a shared ID; the city copy wins (federated
	// first) and the rig copy is deduped. Rig "bravo" fails outright.
	state.cityBeadStore = &scriptStore{Store: beads.NewMemStore(), out: []beads.Bead{{ID: "shared"}, {ID: "city-only"}}}
	state.stores = map[string]beads.Store{
		"alpha": &scriptStore{Store: beads.NewMemStore(), out: []beads.Bead{{ID: "shared"}, {ID: "alpha-only"}}},
		"bravo": &scriptStore{Store: beads.NewMemStore(), gate: func() error { return fmt.Errorf("bravo down") }},
	}
	s := New(state)

	out, err := s.humaHandleBeadEphemeral(context.Background(), &BeadEphemeralInput{})
	if err != nil {
		t.Fatalf("humaHandleBeadEphemeral: %v", err)
	}
	if len(out.Body.Items) == 0 || out.Body.Items[0].ID != "shared" {
		t.Fatalf("city store not federated first: items=%v", beadIDs(out.Body.Items))
	}
	count := 0
	for _, b := range out.Body.Items {
		if b.ID == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared ID returned %d times, want 1 (dedup regressed)", count)
	}
	if !out.Body.Partial || len(out.Body.PartialErrors) == 0 {
		t.Errorf("rig failure not surfaced as partial: partial=%v errs=%v", out.Body.Partial, out.Body.PartialErrors)
	}
}

func beadIDs(bs []beads.Bead) []string {
	ids := make([]string, len(bs))
	for i, b := range bs {
		ids[i] = b.ID
	}
	return ids
}
