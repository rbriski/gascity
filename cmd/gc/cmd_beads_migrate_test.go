package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestRunBeadsMigrateSQLite_RoutesByClassAndReports(t *testing.T) {
	src := beads.NewMemStore()
	for _, b := range []beads.Bead{
		{ID: "m1", Type: "message", Title: "a"},
		{ID: "m2", Type: "message", Title: "b"},
		{ID: "s1", Type: "session", Title: "sess"},
		{ID: "t1", Type: "task", Title: "work"},
	} {
		if _, err := src.Create(b); err != nil {
			t.Fatalf("seed %s: %v", b.ID, err)
		}
	}

	dests := map[string]beads.Store{}
	deps := migrateDeps{
		openSource: func() (beads.Store, func(), error) { return src, func() {}, nil },
		openDest: func(class string) (beads.Store, func(), error) {
			d := beads.NewMemStore()
			dests[class] = d
			return d, func() {}, nil
		},
	}

	var out, errb bytes.Buffer
	if code := runBeadsMigrateSQLite([]string{"messaging"}, deps, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}

	// Verify ROUTING (the command's job): only the two message beads landed in the
	// messaging store. ID-preserving fidelity is proven separately against SQLite
	// in internal/storemigrate (MemStore here does not preserve pinned IDs), so
	// assert by listing rather than by pinned ID.
	migrated, err := dests["messaging"].List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messaging dest: %v", err)
	}
	if len(migrated) != 2 {
		t.Fatalf("messaging dest has %d beads, want 2 (only the message beads)", len(migrated))
	}
	for _, b := range migrated {
		if b.Type != "message" {
			t.Fatalf("non-message bead routed into the messaging store: %+v", b)
		}
	}
	if !strings.Contains(out.String(), "messaging: scanned=2 migrated=2 skipped=0") {
		t.Fatalf("unexpected report: %q", out.String())
	}
}

// TestRunBeadsMigrateSQLite_AllClassesEndToEnd proves the dolt->sqlite migration
// path works for EVERY relocatable class, against real SQLite destination stores:
// each class's bead lands in its own store, ID-preserved, with no cross-class
// leakage and the work bead migrated nowhere; a second run is idempotent. This is
// the end-to-end answer to "we have a way to migrate everything to sqlite".
func TestRunBeadsMigrateSQLite_AllClassesEndToEnd(t *testing.T) {
	// Bead shapes mirror coordclass.Classify (internal/coordclass/classify.go).
	src := beads.NewMemStore()
	seeds := []struct {
		class string
		bead  beads.Bead
	}{
		{config.BeadClassMessaging, beads.Bead{Type: "message", Title: "mail"}},
		{config.BeadClassSessions, beads.Bead{Type: "session", Title: "sess", Labels: []string{"gc:session"}}},
		{config.BeadClassOrders, beads.Bead{Type: "task", Title: "order", Labels: []string{"order-tracking"}}},
		{config.BeadClassNudges, beads.Bead{Type: "chore", Title: "nudge", Labels: []string{"gc:nudge"}}},
		{config.BeadClassGraph, beads.Bead{Type: "molecule", Title: "wisp", Metadata: map[string]string{"gc.kind": "workflow"}}},
	}
	wantID := map[string]string{}
	for _, s := range seeds {
		created, err := src.Create(s.bead)
		if err != nil {
			t.Fatalf("seed %s: %v", s.class, err)
		}
		wantID[s.class] = created.ID
	}
	workBead, err := src.Create(beads.Bead{Type: "task", Title: "real work"})
	if err != nil {
		t.Fatalf("seed work: %v", err)
	}

	cityPath := t.TempDir()
	opened := map[string]beads.Store{}
	deps := migrateDeps{
		openSource: func() (beads.Store, func(), error) { return src, func() {}, nil },
		openDest: func(class string) (beads.Store, func(), error) {
			if d, ok := opened[class]; ok {
				return d, func() {}, nil // reuse across runs (idempotency)
			}
			// Real SQLite dest so ID-preservation is proven on the actual backend.
			d, err := beads.OpenSQLiteStore(
				classSQLiteDir(cityPath, class),
				beads.WithSQLiteStoreIDPrefix(classSQLitePrefix[class]),
				beads.WithSQLiteStoreRetention(0, 0),
			)
			if err != nil {
				return nil, nil, err
			}
			opened[class] = d
			return d, func() {}, nil // keep open for assertions
		},
	}
	t.Cleanup(func() {
		for _, d := range opened {
			if c, ok := d.(interface{ CloseStore() error }); ok {
				_ = c.CloseStore() //nolint:errcheck // best-effort
			}
		}
	})

	classes := []string{
		config.BeadClassMessaging, config.BeadClassSessions, config.BeadClassOrders,
		config.BeadClassNudges, config.BeadClassGraph,
	}
	var out, errb bytes.Buffer
	if code := runBeadsMigrateSQLite(classes, deps, &out, &errb); code != 0 {
		t.Fatalf("migrate code=%d stderr=%s", code, errb.String())
	}

	for _, class := range classes {
		store := opened[class]
		list, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("%s: list dest: %v", class, err)
		}
		if len(list) != 1 {
			t.Fatalf("%s store has %d beads, want exactly 1", class, len(list))
		}
		if list[0].ID != wantID[class] {
			t.Fatalf("%s store bead id = %q, want %q (ID not preserved)", class, list[0].ID, wantID[class])
		}
		if _, err := store.Get(workBead.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("%s store unexpectedly holds the work bead %q (err=%v)", class, workBead.ID, err)
		}
		for other, id := range wantID {
			if other == class {
				continue
			}
			if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("%s store unexpectedly holds the %s bead %q", class, other, id)
			}
		}
		if !strings.Contains(out.String(), class+": scanned=1 migrated=1 skipped=0") {
			t.Fatalf("first-run report for %s missing/wrong: %q", class, out.String())
		}
	}

	// Idempotent re-run: everything already present, nothing re-copied.
	var out2, errb2 bytes.Buffer
	if code := runBeadsMigrateSQLite(classes, deps, &out2, &errb2); code != 0 {
		t.Fatalf("re-run code=%d stderr=%s", code, errb2.String())
	}
	for _, class := range classes {
		if !strings.Contains(out2.String(), class+": scanned=1 migrated=0 skipped=1") {
			t.Fatalf("re-run not idempotent for %s: %q", class, out2.String())
		}
	}
}

func TestResolveMigrateClasses_RejectsUnknownClass(t *testing.T) {
	var errb bytes.Buffer
	if _, err := resolveMigrateClasses(t.TempDir(), []string{"bogus"}, &errb); err == nil {
		t.Fatal("expected error for unknown class name")
	}
	if !strings.Contains(errb.String(), "unknown class") {
		t.Fatalf("stderr should explain the unknown class: %q", errb.String())
	}
}

func TestResolveMigrateClasses_ExplicitArgsPassThrough(t *testing.T) {
	var errb bytes.Buffer
	got, err := resolveMigrateClasses(t.TempDir(), []string{"messaging", "orders"}, &errb)
	if err != nil {
		t.Fatalf("unexpected error: %v (%s)", err, errb.String())
	}
	if strings.Join(got, ",") != "messaging,orders" {
		t.Fatalf("classes = %v, want [messaging orders]", got)
	}
}

// TestClassSQLitePrefixesDisjoint guards cross-store routing: each class's id
// namespace (prefix+"-") must not be a string-prefix of another class's or of the
// work prefixes, so a relocated-class bead id can never resolve into the wrong
// store / pollute a work cache (ownsBeadID fan-out safety).
func TestClassSQLitePrefixesDisjoint(t *testing.T) {
	var classPrefixes []string
	seen := map[string]bool{}
	for class, p := range classSQLitePrefix {
		if p == "" {
			t.Fatalf("class %q has an empty SQLite prefix", class)
		}
		if seen[p] {
			t.Fatalf("duplicate class SQLite prefix %q", p)
		}
		seen[p] = true
		classPrefixes = append(classPrefixes, p)
	}
	// Common work prefixes the relocated classes must stay disjoint from.
	all := append(append([]string{}, classPrefixes...), "gc", "ga")
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			an, bn := a+"-", b+"-"
			if strings.HasPrefix(bn, an) {
				t.Fatalf("id namespace %q is a prefix of %q — cross-store routing would be ambiguous", an, bn)
			}
		}
	}
}

// TestClassSQLitePrefixRegistryParity pins cmd/gc's class prefix map to the
// internal/config registry and to graphStoreIDPrefix, so the consolidation never
// drifts (the graph store opener at api_state.go still uses graphStoreIDPrefix).
func TestClassSQLitePrefixRegistryParity(t *testing.T) {
	if got := classSQLitePrefix[config.BeadClassGraph]; got != graphStoreIDPrefix {
		t.Fatalf("classSQLitePrefix[graph] = %q, want graphStoreIDPrefix %q", got, graphStoreIDPrefix)
	}
	want := map[string]string{
		config.BeadClassMessaging: "gcm",
		config.BeadClassSessions:  "gcs",
		config.BeadClassOrders:    "gco",
		config.BeadClassNudges:    "gcn",
		config.BeadClassGraph:     "gcg",
	}
	for class, wantPrefix := range want {
		if got := classSQLitePrefix[class]; got != wantPrefix {
			t.Errorf("classSQLitePrefix[%q] = %q, want %q", class, got, wantPrefix)
		}
	}
	if len(classSQLitePrefix) != len(want) {
		t.Errorf("classSQLitePrefix has %d entries, want %d", len(classSQLitePrefix), len(want))
	}
}
