package beads_test

import (
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func openPostgresSchema(t *testing.T, dsn, schema, prefix string) beads.Store {
	t.Helper()
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	truncatePostgresSchema(t, dsn, schema)
	s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema), beads.WithPostgresStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("OpenPostgresStore(%q): %v", schema, err)
	}
	t.Cleanup(func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore() //nolint:errcheck
		}
	})
	return s
}

// TestPostgresStoreSchemaIsolation proves the per-class isolation model: two
// stores in different schemas of the same database do not see each other's beads
// (the property that would silently break if classes shared one public.beads).
func TestPostgresStoreSchemaIsolation(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN")
	}
	a := openPostgresSchema(t, dsn, "iso_a", "gca")
	b := openPostgresSchema(t, dsn, "iso_b", "gcb")
	ba, err := a.Create(beads.Bead{Title: "in-a", Type: "task"})
	if err != nil {
		t.Fatalf("create in iso_a: %v", err)
	}
	bb, err := b.Create(beads.Bead{Title: "in-b", Type: "task"})
	if err != nil {
		t.Fatalf("create in iso_b: %v", err)
	}
	la, err := a.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(la) != 1 || la[0].ID != ba.ID {
		t.Fatalf("iso_a List = %+v (err=%v), want only its own bead %q", la, err, ba.ID)
	}
	if _, err := a.Get(bb.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("iso_a Get(%q) = %v, want ErrNotFound — schemas must not leak", bb.ID, err)
	}
	lb, err := b.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(lb) != 1 || lb[0].ID != bb.ID {
		t.Fatalf("iso_b List = %+v (err=%v), want only its own bead %q", lb, err, bb.ID)
	}
}

// TestPostgresStoreOpenRequiresProvisionedSchema proves Open does NOT run DDL: an
// unprovisioned schema fails fast with a clear "not provisioned" error rather than
// silently creating tables on the hot path.
func TestPostgresStoreOpenRequiresProvisionedSchema(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS unprovisioned_xyz CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	_, err = beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema("unprovisioned_xyz"))
	if err == nil {
		t.Fatal("OpenPostgresStore on an unprovisioned schema should error")
	}
	if !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("error = %v, want a 'not provisioned' message", err)
	}
}

// TestPostgresStorePinnedIDBumpsSequence proves a caller-pinned id lifts the native
// sequence past its suffix, so a later auto-id (nextval) never re-mints a pinned id
// — the property a dolt->pg migration (which pins ids) relies on.
func TestPostgresStorePinnedIDBumpsSequence(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN")
	}
	s := openPostgresSchema(t, dsn, "seqbump", "gcx")
	if _, err := s.Create(beads.Bead{ID: "gcx-100", Title: "pinned"}); err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	auto, err := s.Create(beads.Bead{Title: "auto"})
	if err != nil {
		t.Fatalf("create auto: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(auto.ID, "gcx-"))
	if err != nil || n <= 100 {
		t.Fatalf("auto id = %q (suffix %d); want a suffix > 100 (sequence not bumped past the pinned id)", auto.ID, n)
	}
	// And a duplicate pinned id is rejected.
	if _, err := s.Create(beads.Bead{ID: "gcx-100", Title: "dup"}); err == nil {
		t.Fatal("creating a duplicate pinned id should error")
	}
}

// TestPostgresStoreFullSurface exercises the Store methods the shared classed-store
// conformance suite does not cover (deps, metadata, CloseAll, the label/metadata
// list helpers, Ready exclusion, Delete, Tx) against a real Postgres. SKIPPED
// unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE database (it truncates).
func TestPostgresStoreFullSurface(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the full-surface test")
	}
	const schema = "gcp_fullsurface"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	truncatePostgresSchema(t, dsn, schema)
	s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema), beads.WithPostgresStoreIDPrefix("gcp"))
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore() //nolint:errcheck
		}
	})

	// Create with labels + metadata, then read them back.
	a, err := s.Create(beads.Bead{
		Title:    "alpha",
		Type:     "task",
		Labels:   []string{"team:core", "kind:x"},
		Metadata: map[string]string{"k1": "v1"},
	})
	if err != nil {
		t.Fatalf("Create alpha: %v", err)
	}
	if a.ID == "" || a.ID[:4] != "gcp-" {
		t.Fatalf("minted id %q does not use the configured prefix gcp", a.ID)
	}
	b, err := s.Create(beads.Bead{Title: "beta", Type: "task", ParentID: a.ID, Labels: []string{"team:core"}})
	if err != nil {
		t.Fatalf("Create beta: %v", err)
	}

	// SetMetadata round-trip.
	if err := s.SetMetadata(a.ID, "k2", "v2"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["k1"] != "v1" || got.Metadata["k2"] != "v2" {
		t.Fatalf("metadata = %v, want k1=v1,k2=v2", got.Metadata)
	}

	// Label + metadata + child listers.
	if out, err := s.ListByLabel("team:core", 0); err != nil || len(out) != 2 {
		t.Fatalf("ListByLabel(team:core) = %d beads, err=%v, want 2", len(out), err)
	}
	if out, err := s.ListByMetadata(map[string]string{"k2": "v2"}, 0); err != nil || len(out) != 1 || out[0].ID != a.ID {
		t.Fatalf("ListByMetadata(k2=v2) = %+v, err=%v, want [alpha]", out, err)
	}
	if out, err := s.Children(a.ID); err != nil || len(out) != 1 || out[0].ID != b.ID {
		t.Fatalf("Children(alpha) = %+v, err=%v, want [beta]", out, err)
	}

	// Dependencies.
	if err := s.DepAdd(b.ID, a.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	deps, err := s.DepList(b.ID, "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != a.ID {
		t.Fatalf("DepList(beta,down) = %+v, err=%v, want [->alpha]", deps, err)
	}
	if err := s.DepRemove(b.ID, a.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	if deps, _ := s.DepList(b.ID, "down"); len(deps) != 0 {
		t.Fatalf("DepList after remove = %+v, want empty", deps)
	}

	// Ready: a message bead is excluded (infra type); the open task beta is ready.
	if _, err := s.Create(beads.Bead{Title: "mail", Type: "message"}); err != nil {
		t.Fatalf("Create message: %v", err)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var sawMessage, sawBeta bool
	for _, r := range ready {
		if r.Type == "message" {
			sawMessage = true
		}
		if r.ID == b.ID {
			sawBeta = true
		}
	}
	if sawMessage {
		t.Fatal("Ready returned a type=message bead (infra types must be excluded)")
	}
	if !sawBeta {
		t.Fatal("Ready did not include the open task beta")
	}

	// CloseAll + Tx round-trip.
	n, err := s.CloseAll([]string{a.ID, b.ID}, map[string]string{"close_reason": "done by full-surface test"})
	if err != nil || n != 2 {
		t.Fatalf("CloseAll = %d, err=%v, want 2", n, err)
	}
	if cl, _ := s.Get(a.ID); cl.Status != "closed" {
		t.Fatalf("alpha status = %q, want closed", cl.Status)
	}
	if err := s.Tx("reopen-alpha", func(tx beads.Tx) error {
		return tx.Update(a.ID, beads.UpdateOpts{Status: ptrToStr("open")})
	}); err != nil {
		t.Fatalf("Tx reopen: %v", err)
	}
	if op, _ := s.Get(a.ID); op.Status != "open" {
		t.Fatalf("alpha status after Tx reopen = %q, want open", op.Status)
	}

	// Delete removes the bead; Get then reports not-found.
	if err := s.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(b.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	// Duplicate-id create is rejected.
	if _, err := s.Create(beads.Bead{ID: a.ID, Title: "dup"}); err == nil {
		t.Fatal("Create with a duplicate pinned id should error")
	}
}

func ptrToStr(s string) *string { return &s }
