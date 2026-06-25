package beads

import (
	"errors"
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

func TestPostgresStoreScaffoldStubsReturnSentinel(t *testing.T) {
	s := &PostgresStore{prefix: "gcn"}
	if got := s.IDPrefix(); got != "gcn" {
		t.Errorf("IDPrefix() = %q, want gcn", got)
	}
	// The CRUD/query/dep methods are scaffolded; until ported each returns the
	// sentinel (and never touches the nil db here).
	checks := []struct {
		name string
		err  error
	}{
		{"Create", func() error { _, e := s.Create(Bead{}); return e }()},
		{"Update", s.Update("x", UpdateOpts{})},
		{"Close", s.Close("x")},
		{"Reopen", s.Reopen("x")},
		{"Delete", s.Delete("x")},
		{"SetMetadata", s.SetMetadata("x", "k", "v")},
		{"SetMetadataBatch", s.SetMetadataBatch("x", map[string]string{"k": "v"})},
		{"DepAdd", s.DepAdd("a", "b", "blocks")},
		{"DepRemove", s.DepRemove("a", "b")},
		{"List", func() error { _, e := s.List(ListQuery{AllowScan: true}); return e }()},
		{"Ready", func() error { _, e := s.Ready(); return e }()},
		{"Tx", s.Tx("msg", func(Tx) error { return nil })},
	}
	for _, c := range checks {
		if !errors.Is(c.err, errPostgresNotImplemented) {
			t.Errorf("%s err = %v, want errPostgresNotImplemented", c.name, c.err)
		}
	}
}

func TestOpenPostgresStore_RejectsEmptyDSN(t *testing.T) {
	if _, err := OpenPostgresStore("   "); err == nil {
		t.Fatal("OpenPostgresStore with a blank dsn should error")
	}
}
