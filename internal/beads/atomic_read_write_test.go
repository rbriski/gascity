package beads

import "testing"

func TestAtomicReadWriteForReportsUnsupportedStores(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		store Store
	}{
		{name: "mem", store: NewMemStore()},
		{name: "file", store: &FileStore{}},
		{name: "bd", store: NewBdStore(t.TempDir(), nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if capability, ok := AtomicReadWriteFor(tt.store); ok || capability != nil {
				t.Fatalf("AtomicReadWriteFor(%T) = (%T, %v), want typed absence", tt.store, capability, ok)
			}
		})
	}
}

func TestAtomicReadWriteForNilReportsAbsence(t *testing.T) {
	t.Parallel()

	if capability, ok := AtomicReadWriteFor(nil); ok || capability != nil {
		t.Fatalf("AtomicReadWriteFor(nil) = (%T, %v), want typed absence", capability, ok)
	}
}
