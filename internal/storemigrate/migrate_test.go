package storemigrate_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storemigrate"
)

func newSQLiteDst(t *testing.T, prefix string) beads.Store {
	t.Helper()
	dst, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	if closer, ok := dst.(interface{ CloseStore() error }); ok {
		t.Cleanup(func() { _ = closer.CloseStore() })
	}
	return dst
}

func seedSource(t *testing.T) (src beads.Store, msgIDs []string) {
	t.Helper()
	src = beads.NewMemStore()
	// Two message beads (the relocated class) with full fields + metadata.
	for i, body := range []string{"hello", "world"} {
		b, err := src.Create(beads.Bead{
			ID:        "msg-" + string(rune('a'+i)),
			Title:     "subject " + body,
			Type:      "message",
			Assignee:  "worker",
			From:      "human",
			Labels:    []string{"thread:t" + body, "read"},
			Metadata:  map[string]string{"mail.read": "true", "mail.from_display": "human", "unknown_key": body},
			CreatedAt: time.Unix(int64(1000+i), 0).UTC(),
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("seed message: %v", err)
		}
		msgIDs = append(msgIDs, b.ID)
	}
	// Non-message beads that MUST NOT be migrated.
	if _, err := src.Create(beads.Bead{ID: "sess-1", Title: "session", Type: "session"}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := src.Create(beads.Bead{ID: "task-1", Title: "real work", Type: "task"}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return src, msgIDs
}

func TestMigrate_CopiesSelectedBeadsIDPreservingAndFullFidelity(t *testing.T) {
	src, msgIDs := seedSource(t)
	dst := newSQLiteDst(t, "gcm")

	rep, err := storemigrate.Migrate(src, dst, storemigrate.TypeSelector("message"))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if rep.Migrated != 2 || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want Migrated=2 Skipped=0", rep)
	}

	// Every message bead is now in dst with the SAME id and identical fields.
	for _, id := range msgIDs {
		orig, err := src.Get(id)
		if err != nil {
			t.Fatalf("src.Get(%s): %v", id, err)
		}
		got, err := dst.Get(id)
		if err != nil {
			t.Fatalf("dst.Get(%s) after migrate: %v (id not preserved or not migrated)", id, err)
		}
		if got.ID != orig.ID || got.Title != orig.Title || got.Type != orig.Type ||
			got.Status != orig.Status || got.Assignee != orig.Assignee || got.From != orig.From ||
			!got.CreatedAt.Equal(orig.CreatedAt) || got.Ephemeral != orig.Ephemeral {
			t.Fatalf("field mismatch for %s:\n src %+v\n dst %+v", id, orig, got)
		}
		for k, v := range orig.Metadata {
			if got.Metadata[k] != v {
				t.Fatalf("metadata %q not preserved for %s: got %q want %q", k, id, got.Metadata[k], v)
			}
		}
	}

	// Non-message beads were NOT migrated.
	for _, id := range []string{"sess-1", "task-1"} {
		if _, err := dst.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("dst.Get(%s) = %v, want ErrNotFound (non-message bead must not migrate)", id, err)
		}
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	src, _ := seedSource(t)
	dst := newSQLiteDst(t, "gcm")

	if _, err := storemigrate.Migrate(src, dst, storemigrate.TypeSelector("message")); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	rep, err := storemigrate.Migrate(src, dst, storemigrate.TypeSelector("message"))
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if rep.Migrated != 0 || rep.Skipped != 2 {
		t.Fatalf("re-run report = %+v, want Migrated=0 Skipped=2 (idempotent)", rep)
	}
}

func TestMigrate_ClassSelectorUsesClassifier(t *testing.T) {
	src, _ := seedSource(t)
	dst := newSQLiteDst(t, "gcm")
	// The ClassMessaging selector picks the same message beads via coordclass.Classify.
	rep, err := storemigrate.Migrate(src, dst, storemigrate.ClassSelector("messaging"))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if rep.Migrated != 2 {
		t.Fatalf("class selector migrated %d, want 2", rep.Migrated)
	}
}
