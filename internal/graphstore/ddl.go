package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// schemaV1 is the frozen journal-core DDL (01-architecture §2.1/§2.3), copied
// verbatim: the append-only journal with its two indexes and two triggers, the
// retention_gate that structurally gates the sole deleter, the writer_lease, the
// graph_meta identity/migration table, and the snapshots table. Tier-A
// projection tables (nodes/edges/frontier/...) are intentionally absent — they
// belong to the projection slice.
const schemaV1 = `
CREATE TABLE journal (
  stream_id           TEXT    NOT NULL,
  seq                 INTEGER NOT NULL,
  substream           TEXT    NOT NULL DEFAULT '',
  engine              TEXT    NOT NULL CHECK (engine IN ('lumen','v2','v1')),
  type                TEXT    NOT NULL,
  ir_contract_version TEXT    NOT NULL,
  idem_token          TEXT,
  payload             BLOB    NOT NULL,
  payload_hash        BLOB    NOT NULL CHECK (length(payload_hash) = 32),
  chain_hash          BLOB    NOT NULL CHECK (length(chain_hash) = 32),
  lease_epoch         INTEGER NOT NULL,
  appended_at         TEXT    NOT NULL,
  PRIMARY KEY (stream_id, seq)
);

CREATE UNIQUE INDEX journal_idem
  ON journal (stream_id, idem_token) WHERE idem_token IS NOT NULL;

CREATE INDEX journal_substream
  ON journal (stream_id, substream, seq) WHERE substream <> '';

CREATE TRIGGER journal_no_update BEFORE UPDATE ON journal
BEGIN SELECT RAISE(ABORT, 'journal is append-only'); END;

CREATE TABLE retention_gate (
  stream_id TEXT PRIMARY KEY,
  max_seq   INTEGER NOT NULL
);

CREATE TRIGGER journal_no_delete BEFORE DELETE ON journal
WHEN NOT EXISTS (SELECT 1 FROM retention_gate g
                 WHERE g.stream_id = OLD.stream_id AND OLD.seq <= g.max_seq)
BEGIN SELECT RAISE(ABORT, 'journal is append-only (retention gate closed)'); END;

CREATE TABLE snapshots (
  stream_id               TEXT    NOT NULL,
  covered_seq             INTEGER NOT NULL,
  engine                  TEXT    NOT NULL,
  reducer_version         INTEGER NOT NULL,
  snapshot_format_version INTEGER NOT NULL,
  state_hash              BLOB    NOT NULL CHECK (length(state_hash) = 32),
  state                   BLOB    NOT NULL,
  cut_chain_hash          BLOB CHECK (cut_chain_hash IS NULL
                                      OR length(cut_chain_hash) = 32),
  created_at              TEXT    NOT NULL,
  PRIMARY KEY (stream_id, covered_seq)
);

CREATE TABLE writer_lease (
  stream_id  TEXT    PRIMARY KEY,
  holder     TEXT    NOT NULL,
  epoch      INTEGER NOT NULL,
  expires_at TEXT    NOT NULL
);

CREATE TABLE graph_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// schemaV2 adds the Tier-A projection tables (01-architecture §2.2), copied
// verbatim: nodes (+ its four indexes), node_labels, node_metadata, edges (+
// edges_reverse), the frontier (WITHOUT ROWID + its covering index),
// defer_wakeups, and channel_cursors. Tier A is write-closed and rebuildable
// (I-13/I-14): the fold applier in projection.go is the ONLY writer of
// fold_owned=1 rows. The write-closure is enforced structurally here — the same
// migration that creates nodes ships the tripwire triggers (DET-T-18), so the
// table is never live without them, mirroring the journal's append-only triggers
// (SEC-3). tier_a_write_gate is the projection's analog of retention_gate: the
// fold applier opens it inside its own transaction, writes, and closes it, so any
// non-fold writer of a fold_owned=1 row hits a closed gate and a loud ABORT.
//
// Scope of this slice's write-closure (honest narrowing): the trigger guard
// covers `nodes` (fold_owned=1 rows) ONLY. It blocks INSERT/UPDATE/DELETE of a
// fold-owned node while the gate is closed, INCLUDING the escalation path where
// a non-fold writer inserts fold_owned=0 and then flips it to 1 (the UPDATE
// guard fires on NEW.fold_owned=1 as well as OLD.fold_owned=1). The child and
// sibling tables — edges, frontier, node_labels, node_metadata, defer_wakeups,
// channel_cursors — are NOT yet trigger-guarded; a rogue writer can still mutate
// them directly. Table-wide write-closure (a gate guard on every Tier-A table)
// is a P2/P3 follow-up, not part of this slice.
const schemaV2 = `
CREATE TABLE nodes (
  id           TEXT PRIMARY KEY,
  title        TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL DEFAULT 'open',
  bead_type    TEXT    NOT NULL DEFAULT 'task',
  priority     INTEGER,
  description  TEXT    NOT NULL DEFAULT '',
  assignee     TEXT    NOT NULL DEFAULT '',
  from_actor   TEXT    NOT NULL DEFAULT '',
  parent_id    TEXT    NOT NULL DEFAULT '',
  ref          TEXT    NOT NULL DEFAULT '',
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL DEFAULT '',
  defer_until  TEXT,
  storage_tier TEXT    NOT NULL DEFAULT 'history'
               CHECK (storage_tier IN ('history','no_history','ephemeral')),
  is_blocked   INTEGER NOT NULL DEFAULT 0,
  fold_owned   INTEGER NOT NULL DEFAULT 0,
  stream_id    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX nodes_status   ON nodes (status, bead_type);
CREATE INDEX nodes_parent   ON nodes (parent_id)        WHERE parent_id <> '';
CREATE INDEX nodes_assignee ON nodes (assignee, status) WHERE assignee <> '';
CREATE INDEX nodes_stream   ON nodes (stream_id)        WHERE stream_id <> '';

CREATE TABLE node_labels (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  label   TEXT NOT NULL,
  PRIMARY KEY (node_id, label)
);
CREATE INDEX node_labels_by_label ON node_labels (label);

CREATE TABLE node_metadata (
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  value   TEXT NOT NULL,
  PRIMARY KEY (node_id, key)
);
CREATE INDEX node_metadata_kv ON node_metadata (key, value);

CREATE TABLE edges (
  from_id  TEXT NOT NULL,
  to_id    TEXT NOT NULL,
  dep_type TEXT NOT NULL DEFAULT 'blocks',
  metadata TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (from_id, to_id, dep_type),
  FOREIGN KEY (from_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX edges_reverse ON edges (to_id);

CREATE TABLE frontier (
  node_id        TEXT PRIMARY KEY,
  root_id        TEXT NOT NULL,
  route          TEXT NOT NULL DEFAULT '',
  ready_priority INTEGER NOT NULL DEFAULT 2,
  created_at     TEXT NOT NULL,
  id             TEXT NOT NULL,
  defer_until    TEXT
) WITHOUT ROWID;
CREATE INDEX frontier_route_order
  ON frontier (route, ready_priority, created_at, id);

CREATE TABLE defer_wakeups (
  node_id TEXT PRIMARY KEY,
  wake_at TEXT NOT NULL
);
CREATE INDEX defer_wakeups_by_time ON defer_wakeups (wake_at, node_id);

CREATE TABLE channel_cursors (
  stream_id    TEXT NOT NULL,
  substream    TEXT NOT NULL,
  reader_key   TEXT NOT NULL,
  position     INTEGER NOT NULL,
  planted_seq  INTEGER NOT NULL,
  advanced_seq INTEGER NOT NULL,
  PRIMARY KEY (stream_id, substream, reader_key)
);

CREATE TABLE tier_a_write_gate (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 0),
  open      INTEGER NOT NULL DEFAULT 0
);
INSERT INTO tier_a_write_gate(singleton, open) VALUES (0, 0);

CREATE TRIGGER nodes_fold_owned_no_insert BEFORE INSERT ON nodes
WHEN NEW.fold_owned = 1
 AND NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'nodes: fold-owned row is write-closed (I-14)'); END;

CREATE TRIGGER nodes_fold_owned_no_update BEFORE UPDATE ON nodes
WHEN (OLD.fold_owned = 1 OR NEW.fold_owned = 1)
 AND NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'nodes: fold-owned row is write-closed (I-14)'); END;

CREATE TRIGGER nodes_fold_owned_no_delete BEFORE DELETE ON nodes
WHEN OLD.fold_owned = 1
 AND NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'nodes: fold-owned row is write-closed (I-14)'); END;
`

// schemaV3 adds the explicit residence record and completes the Tier-A
// write-closure (P3.2 — 12-p3-migration-blueprint §2, §3.2; 09a §A-2).
//
// graph_residence is the tri-state residence primitive keyed by root id. A root
// is legacy-resident by DEFAULT — the absence of a row (∅). A row records one of
// two non-default states: `migrating(fence_epoch=N)` while a strand migration is
// copying the root's subgraph into the journal leg (reads still route legacy, the
// half-copied journal rows stay hidden, conflicting controller writes are
// blocked), and `journal` once the post-re-verify CAS flip has made the journal
// copy authoritative. The record is the durable checkpoint of the migration
// state machine: a crash at any step leaves a row whose state tells the next run
// whether to resume (journal) or revert (migrating). CAS transitions serialize on
// the store's single write connection.
//
// The frontier triggers finish the P1.2 write-closure (deferred there): the
// `frontier` table is a PURE fold projection — only the fold applier writes it,
// always with tier_a_write_gate open — so every INSERT/UPDATE/DELETE is gated on
// that same gate. A rogue writer (or a stray façade write) that does not open the
// gate hits a loud ABORT, mirroring the `nodes` fold-owned guard. Unlike `nodes`
// there is no fold_owned column to narrow on: every frontier row is fold-owned,
// so the guard is unconditional on ownership and conditional only on the gate.
const schemaV3 = `
CREATE TABLE graph_residence (
  root_id     TEXT    PRIMARY KEY,
  state       TEXT    NOT NULL CHECK (state IN ('migrating','journal')),
  fence_epoch INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX graph_residence_state ON graph_residence (state);

CREATE TRIGGER frontier_write_closed_no_insert BEFORE INSERT ON frontier
WHEN NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'frontier is write-closed (I-14)'); END;

CREATE TRIGGER frontier_write_closed_no_update BEFORE UPDATE ON frontier
WHEN NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'frontier is write-closed (I-14)'); END;

CREATE TRIGGER frontier_write_closed_no_delete BEFORE DELETE ON frontier
WHEN NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1)
BEGIN SELECT RAISE(ABORT, 'frontier is write-closed (I-14)'); END;
`

// migrations is the forward-only schema ladder. Index i applies to move the
// database from schema_version i to i+1. Never edit an existing entry once it
// has shipped; append a new one.
var migrations = []string{
	schemaV1,
	schemaV2,
	schemaV3,
}

// schemaVersionLatest is the target schema_version after all migrations apply.
var schemaVersionLatest = len(migrations)

// migrate brings db up to schemaVersionLatest, applying each pending migration
// in its own transaction and recording the new schema_version in graph_meta.
func migrate(ctx context.Context, db *sql.DB) error {
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if current > schemaVersionLatest {
		return fmt.Errorf("graphstore: database schema_version %d is newer than this binary supports (%d)", current, schemaVersionLatest)
	}
	for v := current; v < schemaVersionLatest; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			return fmt.Errorf("graphstore: applying migration to schema_version %d: %w", v+1, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, index int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.ExecContext(ctx, migrations[index]); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO graph_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(index+1),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// currentSchemaVersion reads the recorded schema_version, treating an absent
// graph_meta table or key as version 0.
func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='graph_meta'`,
	).Scan(&present); err != nil {
		return 0, fmt.Errorf("graphstore: probing graph_meta: %w", err)
	}
	if present == 0 {
		return 0, nil
	}
	var value string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM graph_meta WHERE key='schema_version'`,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("graphstore: reading schema_version: %w", err)
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("graphstore: malformed schema_version %q: %w", value, err)
	}
	return n, nil
}
