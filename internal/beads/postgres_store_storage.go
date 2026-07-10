package beads

import "fmt"

// CreateWithStorage creates a bead in an explicitly selected physical storage
// tier, satisfying [StorageCreateStore] — the Postgres peer of
// [SQLiteStore.CreateWithStorage]. It maps the StorageClass onto the bead's tier
// fields (which insertBeadTx persists as the `tier` column / JSON) and delegates to
// Create. StorageDefault leaves the bead's tier fields untouched.
//
// createWithStoragePolicy already falls back to Create(applyBeadStorage(...)) when a
// store lacks this capability, so the gap was non-fatal — but implementing it keeps
// the Postgres backend at strict parity with SQLite so the policy middleware takes
// the same code path on both, rather than diverging onto the fallback.
func (s *PostgresStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	switch storage {
	case StorageEphemeral:
		b.Ephemeral, b.NoHistory = true, false
	case StorageNoHistory:
		b.Ephemeral, b.NoHistory = false, true
	case StorageHistory:
		b.Ephemeral, b.NoHistory = false, false
	case StorageDefault:
		// leave the bead's existing tier fields as-is
	default:
		return Bead{}, fmt.Errorf("postgres create-with-storage: unknown storage class %q", storage)
	}
	return s.Create(b)
}

var _ StorageCreateStore = (*PostgresStore)(nil)
