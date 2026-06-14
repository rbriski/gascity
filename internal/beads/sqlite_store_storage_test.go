package beads

import "testing"

func TestSQLiteCreateWithStorageMapsTier(t *testing.T) {
	s := openTestSQLiteStore(t)
	cases := []struct {
		storage   StorageClass
		ephemeral bool
		noHistory bool
	}{
		{StorageEphemeral, true, false},
		{StorageNoHistory, false, true},
		{StorageHistory, false, false},
	}
	for _, tc := range cases {
		got, err := s.CreateWithStorage(Bead{Title: "x", Type: "task"}, tc.storage)
		if err != nil {
			t.Fatalf("CreateWithStorage(%q): %v", tc.storage, err)
		}
		if got.Ephemeral != tc.ephemeral || got.NoHistory != tc.noHistory {
			t.Fatalf("%q -> ephemeral=%v no_history=%v, want %v/%v", tc.storage, got.Ephemeral, got.NoHistory, tc.ephemeral, tc.noHistory)
		}
		re, err := s.Get(got.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if re.Ephemeral != tc.ephemeral || re.NoHistory != tc.noHistory {
			t.Fatalf("persisted %q -> ephemeral=%v no_history=%v, want %v/%v", tc.storage, re.Ephemeral, re.NoHistory, tc.ephemeral, tc.noHistory)
		}
	}
}
