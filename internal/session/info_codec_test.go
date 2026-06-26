package session

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/gastownhall/gascity/internal/beads"
)

// canonicalSessionBead exercises every persisted field infoFromPersisted reads,
// with a pinned id so backends that preserve pinned ids (SQLite, Postgres)
// round-trip it identically.
func canonicalSessionBead() beads.Bead {
	return beads.Bead{
		ID:        "gcs-1",
		Title:     "worker session",
		Status:    "open",
		Type:      "session",
		CreatedAt: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
		Labels:    []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":               "test-city/worker.main",
			"template":                   "worker",
			"state":                      string(StateActive),
			"alias":                      "w1",
			"agent_name":                 "worker",
			"provider":                   "test-agent",
			"transport":                  "tmux",
			"command":                    "claude",
			"work_dir":                   "/tmp/myrig",
			"session_key":                "abc123",
			"resume_flag":                "--resume",
			"resume_style":               "flag",
			"resume_command":             "claude --resume",
			MetadataLastNudgeDeliveredAt: "2026-06-26T11:00:00Z",
		},
	}
}

// TestInfoFromPersisted_ProjectionInvariantAcrossBackends is the zero-behavior-change
// proof for the sessions relocation: an identical session bead written to SQLite and
// Postgres (both preserve the pinned id + the EAV metadata) yields a byte-equal
// infoFromPersisted. The pure codec must not vary by storage backend, so migrating
// sessions to a relocated backend cannot distort the session projection. The
// Postgres leg is DSN-gated (GC_TEST_POSTGRES_DSN, a disposable database).
func TestInfoFromPersisted_ProjectionInvariantAcrossBackends(t *testing.T) {
	seed := canonicalSessionBead()
	project := func(store beads.Store) Info {
		created, err := store.Create(seed)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return infoFromPersisted(got)
	}

	sqStore, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = sqStore.(interface{ CloseStore() error }).CloseStore() })
	sqliteInfo := project(sqStore)

	// SQLite must round-trip the pinned id and the full persisted field set.
	if sqliteInfo.ID != "gcs-1" || sqliteInfo.SessionName != "test-city/worker.main" {
		t.Fatalf("SQLite distorted the projection: %+v", sqliteInfo)
	}
	if sqliteInfo.LastNudgeDeliveredAt.IsZero() || sqliteInfo.ResumeCommand != "claude --resume" {
		t.Fatalf("SQLite dropped a persisted field: %+v", sqliteInfo)
	}

	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres for the cross-backend leg")
	}
	const schema = "gcs_projection"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`TRUNCATE %[1]s.beads, %[1]s.labels, %[1]s.metadata, %[1]s.deps, %[1]s.kv CASCADE`, schema)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER SEQUENCE %s.bead_seq RESTART WITH 1`, schema)); err != nil {
		t.Fatalf("reset seq: %v", err)
	}
	_ = db.Close() //nolint:errcheck

	pgStore, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = pgStore.(interface{ CloseStore() error }).CloseStore() })
	pgInfo := project(pgStore)

	if !reflect.DeepEqual(sqliteInfo, pgInfo) {
		t.Fatalf("infoFromPersisted differs across backends:\n sqlite=%+v\n     pg=%+v", sqliteInfo, pgInfo)
	}
}
