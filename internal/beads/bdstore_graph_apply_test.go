package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreSupportsEphemeralGraphApply(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), nil)
	if !store.SupportsEphemeralGraphApply() {
		t.Fatal("SupportsEphemeralGraphApply() = false, want true")
	}
}
