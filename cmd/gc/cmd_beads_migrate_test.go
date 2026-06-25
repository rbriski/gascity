package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
