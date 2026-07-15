//go:build gascity_native_beads

package beads

import "testing"

func TestAtomicReadWriteForDoltliteReportsAbsence(t *testing.T) {
	t.Parallel()

	store := &DoltliteReadStore{BdStore: NewBdStore(t.TempDir(), nil)}
	if capability, ok := AtomicReadWriteFor(store); ok || capability != nil {
		t.Fatalf("AtomicReadWriteFor(DoltliteReadStore) = (%T, %v), want typed absence", capability, ok)
	}
}
