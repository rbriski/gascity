package beads_test

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	bbolt "go.etcd.io/bbolt"
)

func TestBboltStoreConformance(t *testing.T) {
	factory := func() beads.Store {
		t.Helper()
		store := openTestBboltStore(t)
		return store
	}

	beadstest.RunStoreTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
}

func TestOpenBboltStoreCreatesExpectedBuckets(t *testing.T) {
	path := testBboltPath(t)
	store, err := beads.OpenBboltStore(path, beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenBboltStore: %v", err)
	}
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("open readonly bbolt: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("readonly close: %v", err)
		}
	})

	want := [][]byte{[]byte("records"), []byte("wisps"), []byte("deps"), []byte("metadata")}
	if err := db.View(func(tx *bbolt.Tx) error {
		for _, name := range want {
			if tx.Bucket(name) == nil {
				t.Fatalf("bucket %q missing", string(name))
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("view buckets: %v", err)
	}
}

func TestBboltStorePersistsRecordsWispsDepsMetadataAndSequence(t *testing.T) {
	path := testBboltPath(t)
	store, err := beads.OpenBboltStore(path, beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenBboltStore: %v", err)
	}

	main, err := store.Create(beads.Bead{Title: "main", Labels: []string{"scope:main"}})
	if err != nil {
		t.Fatalf("Create main: %v", err)
	}
	wisp, err := store.Create(beads.Bead{Title: "wisp", Type: "message", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}
	if err := store.SetMetadataBatch(wisp.ID, map[string]string{"kind": "handoff", "phase": "ready"}); err != nil {
		t.Fatalf("SetMetadataBatch wisp: %v", err)
	}
	if err := store.DepAdd(main.ID, wisp.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown first store: %v", err)
	}

	recovered, err := beads.OpenBboltStore(path, beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("reopen OpenBboltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Shutdown(); err != nil {
			t.Errorf("Shutdown recovered: %v", err)
		}
	})

	gotMain, err := recovered.Get(main.ID)
	if err != nil {
		t.Fatalf("Get main after reopen: %v", err)
	}
	if gotMain.Title != "main" {
		t.Fatalf("main title after reopen = %q, want main", gotMain.Title)
	}
	wisps, err := recovered.ListByMetadata(map[string]string{"kind": "handoff", "phase": "ready"}, 1, beads.WithEphemeral)
	if err != nil {
		t.Fatalf("ListByMetadata wisp after reopen: %v", err)
	}
	if len(wisps) != 1 || wisps[0].ID != wisp.ID || !wisps[0].Ephemeral {
		t.Fatalf("recovered wisps = %+v, want ephemeral %s", wisps, wisp.ID)
	}
	deps, err := recovered.DepList(main.ID, "down")
	if err != nil {
		t.Fatalf("DepList after reopen: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != wisp.ID || deps[0].Type != "tracks" {
		t.Fatalf("deps after reopen = %+v, want tracks dep on %s", deps, wisp.ID)
	}
	next, err := recovered.Create(beads.Bead{Title: "next"})
	if err != nil {
		t.Fatalf("Create after reopen: %v", err)
	}
	if next.ID != "gc-3" {
		t.Fatalf("next ID after reopen = %q, want gc-3", next.ID)
	}
}

func TestBboltStorePersistsParentChildAndBlockingDependencyForSamePair(t *testing.T) {
	path := testBboltPath(t)
	store, err := beads.OpenBboltStore(path, beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenBboltStore: %v", err)
	}
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := store.DepAdd(child.ID, parent.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := store.DepAdd(child.ID, parent.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown first store: %v", err)
	}

	recovered, err := beads.OpenBboltStore(path, beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("reopen OpenBboltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Shutdown(); err != nil {
			t.Errorf("Shutdown recovered: %v", err)
		}
	})
	deps, err := recovered.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("DepList child: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("deps after reopen = %+v, want blocks and parent-child", deps)
	}

	ready, err := recovered.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == child.ID {
			t.Fatalf("child is ready while same-pair blocks dep exists; ready=%+v deps=%+v", ready, deps)
		}
	}
}

func openTestBboltStore(t *testing.T) *beads.BboltStore {
	t.Helper()
	store, err := beads.OpenBboltStore(testBboltPath(t), beads.WithBboltStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenBboltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return store
}

func testBboltPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".gc", "state", "bbolt", "beads.bolt")
}
