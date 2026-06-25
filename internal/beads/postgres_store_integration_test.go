package beads_test

import (
	"errors"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestPostgresStoreFullSurface exercises the Store methods the shared classed-store
// conformance suite does not cover (deps, metadata, CloseAll, the label/metadata
// list helpers, Ready exclusion, Delete, Tx) against a real Postgres. SKIPPED
// unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE database (it truncates).
func TestPostgresStoreFullSurface(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the full-surface test")
	}
	s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreIDPrefix("gcp"))
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	truncatePostgresBeadTables(t, dsn)
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
