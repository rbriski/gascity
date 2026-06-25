package beads

import (
	"testing"
)

func TestPostgresStoreOptions(t *testing.T) {
	var o postgresStoreOptions
	WithPostgresStoreIDPrefix("  GCN ")(&o)
	if o.prefix != "gcn" {
		t.Errorf("prefix = %q, want %q (trimmed + normalized)", o.prefix, "gcn")
	}
	WithPostgresStoreIDPrefix("   ")(&o) // blank must not clobber a set prefix
	if o.prefix != "gcn" {
		t.Errorf("blank prefix clobbered the value: %q", o.prefix)
	}

	called := false
	WithPostgresStoreRecorder(func(RowChange) { called = true })(&o)
	if o.emit == nil {
		t.Fatal("recorder option did not set emit")
	}
	o.emit(RowChange{})
	if !called {
		t.Error("emit did not invoke the registered recorder")
	}
}

func TestPostgresStoreIDPrefixAndCloseAreSafeWithoutDB(t *testing.T) {
	s := &PostgresStore{prefix: "gcn"}
	if got := s.IDPrefix(); got != "gcn" {
		t.Errorf("IDPrefix() = %q, want gcn", got)
	}
	if err := s.CloseStore(); err != nil {
		t.Errorf("CloseStore() on a db-less store = %v, want nil", err)
	}
}

func TestOpenPostgresStore_RejectsEmptyDSN(t *testing.T) {
	if _, err := OpenPostgresStore("   "); err == nil {
		t.Fatal("OpenPostgresStore with a blank dsn should error")
	}
}
