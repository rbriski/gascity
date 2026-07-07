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

// migrations is the forward-only schema ladder. Index i applies to move the
// database from schema_version i to i+1. Never edit an existing entry once it
// has shipped; append a new one.
var migrations = []string{
	schemaV1,
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
