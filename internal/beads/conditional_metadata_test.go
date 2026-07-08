package beads_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

const casKey = "gc.control_epoch"

// TestConditionalMetadataReachableBareAndCachingWrapped is the wrapper-lockstep
// test: it proves the SetMetadataIf CAS is reachable via ConditionalMetadataStoreFor
// on bare stores AND on CachingStore-wrapped ones, and that a swap round-trips
// through the wrapper (the cache forwards, it does not mask or silently drop).
func TestConditionalMetadataReachableBareAndCachingWrapped(t *testing.T) {
	ctx := context.Background()

	newMem := func() beads.Store { return beads.NewMemStore() }
	newCachedMem := func() beads.Store { return beads.NewCachingStoreForTest(beads.NewMemStore(), nil) }

	for _, tc := range []struct {
		name     string
		newStore func() beads.Store
	}{
		{"bareMem", newMem},
		{"cachingWrappedMem", newCachedMem},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := tc.newStore()
			cas, ok := beads.ConditionalMetadataStoreFor(s)
			if !ok {
				t.Fatalf("ConditionalMetadataStoreFor(%s) = false, want reachable", tc.name)
			}
			b, err := s.Create(beads.Bead{Title: "wrapped cas", Metadata: map[string]string{casKey: "1"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			swapped, err := cas.SetMetadataIf(ctx, b.ID, casKey, "1", "2")
			if err != nil {
				t.Fatalf("SetMetadataIf: %v", err)
			}
			if !swapped {
				t.Fatal("swapped = false, want true through the wrapper")
			}
			got, err := s.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata[casKey] != "2" {
				t.Fatalf("value = %q, want 2 through the wrapper", got.Metadata[casKey])
			}
		})
	}
}

// TestConditionalMetadataCacheCoherentAfterSwap pins that a compare-and-set
// through the CachingStore leaves no stale cached row: a cache-only read after a
// swap sees the new value, not the value the cache held before the CAS.
func TestConditionalMetadataCacheCoherentAfterSwap(t *testing.T) {
	ctx := context.Background()
	cache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)

	// Create through the cache so it holds a primed row with the stale value.
	b, err := cache.Create(beads.Bead{Title: "coherence", Metadata: map[string]string{casKey: "1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cachedBefore, err := beads.HandlesFor(cache).Cached.Get(b.ID)
	if err != nil {
		t.Fatalf("cached Get (pre): %v", err)
	}
	if cachedBefore.Metadata[casKey] != "1" {
		t.Fatalf("precondition: cached value = %q, want 1", cachedBefore.Metadata[casKey])
	}

	cas, _ := beads.ConditionalMetadataStoreFor(cache)
	swapped, err := cas.SetMetadataIf(ctx, b.ID, casKey, "1", "2")
	if err != nil || !swapped {
		t.Fatalf("SetMetadataIf = (%v, %v), want (true, nil)", swapped, err)
	}

	// The cache-only read must now see the swapped value, not the stale 1.
	cachedAfter, err := beads.HandlesFor(cache).Cached.Get(b.ID)
	if err != nil {
		t.Fatalf("cached Get (post): %v", err)
	}
	if cachedAfter.Metadata[casKey] != "2" {
		t.Fatalf("cached value after CAS = %q, want 2 (no stale cache)", cachedAfter.Metadata[casKey])
	}
}

// noCASStore is a Store that does not implement ConditionalMetadataStore. It
// embeds the Store interface, so only Store's method set is promoted (SetMetadataIf
// is not), which makes it the honest "capability absent" fixture.
type noCASStore struct{ beads.Store }

// TestConditionalMetadataProbeAbsentOnUnsupportedStore proves the probe returns
// an honest (nil, false) when neither the store nor its backing supports the CAS,
// and that a CachingStore over such a backing reports the loud unsupported error
// rather than silently dropping the write.
func TestConditionalMetadataProbeAbsentOnUnsupportedStore(t *testing.T) {
	ctx := context.Background()
	bare := noCASStore{beads.NewMemStore()}
	if got, ok := beads.ConditionalMetadataStoreFor(bare); ok || got != nil {
		t.Fatalf("ConditionalMetadataStoreFor(noCASStore) = (%v, %v), want (nil, false)", got, ok)
	}

	// A CachingStore always advertises the method, but a call must fail loudly
	// when its backing cannot service the CAS — never a silent no-op.
	cache := beads.NewCachingStoreForTest(bare, nil)
	cas, ok := beads.ConditionalMetadataStoreFor(cache)
	if !ok {
		t.Fatal("ConditionalMetadataStoreFor(cachingWrappedNoCAS) = false, want the caching store to advertise the method")
	}
	if _, err := cas.SetMetadataIf(ctx, "any-id", casKey, "1", "2"); !errors.Is(err, beads.ErrConditionalMetadataUnsupported) {
		t.Fatalf("SetMetadataIf err = %v, want ErrConditionalMetadataUnsupported", err)
	}
}
