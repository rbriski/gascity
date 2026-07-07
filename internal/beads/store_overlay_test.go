package beads

import "testing"

// overlayStore is a minimal StoreOverlay: it overlays exactly the store held in
// leg. It embeds a MemStore only to satisfy the Store method set.
type overlayStore struct {
	*MemStore
	leg Store
}

func (o overlayStore) OverlaysStore(other Store) bool {
	return other != nil && o.leg == other
}

func TestStoreOverlapsTrueWhenOverlayComposesBase(t *testing.T) {
	base := NewMemStore()
	overlay := overlayStore{MemStore: NewMemStore(), leg: base}

	if !StoreOverlaps(overlay, base) {
		t.Fatal("StoreOverlaps(overlay, base) = false, want true")
	}
}

func TestStoreOverlapsFalseForDisjointOrPlainStore(t *testing.T) {
	base := NewMemStore()
	other := NewMemStore()
	overlay := overlayStore{MemStore: NewMemStore(), leg: other}

	// Overlay composes `other`, not `base`.
	if StoreOverlaps(overlay, base) {
		t.Fatal("StoreOverlaps(overlay, base) = true, want false for a store it does not overlay")
	}
	// A plain store that does not implement StoreOverlay never overlaps.
	if StoreOverlaps(NewMemStore(), base) {
		t.Fatal("StoreOverlaps(plainStore, base) = true, want false")
	}
}

func TestStoreOverlapsFalseOnNil(t *testing.T) {
	base := NewMemStore()
	overlay := overlayStore{MemStore: NewMemStore(), leg: base}
	if StoreOverlaps(nil, base) {
		t.Fatal("StoreOverlaps(nil, base) = true, want false")
	}
	if StoreOverlaps(overlay, nil) {
		t.Fatal("StoreOverlaps(overlay, nil) = true, want false")
	}
}
