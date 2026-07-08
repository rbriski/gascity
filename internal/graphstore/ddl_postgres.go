package graphstore

// This file is the PostgreSQL port of the frozen SQLite schema in ddl.go. It is
// a faithful, rung-for-rung translation: pgSchemaV1..V4 apply at the SAME
// schema_version numbers as schemaV1..V4, so a SQLite store and a Postgres store
// at version N have equivalent schemas. Every table, CHECK, UNIQUE, index, and
// tripwire trigger in the SQLite DDL has an equivalent here.
//
// Translation rules (blueprint §2.3, §0.1):
//
//   - INTEGER → BIGINT for seq/epoch/position-like columns; INTEGER kept for the
//     small flag/version columns (is_blocked, fold_owned, open, singleton,
//     priority, ready_priority, reducer_version, snapshot_format_version) so the
//     Go bind/scan code is unchanged (lib/pq decodes both to int64).
//   - BLOB → BYTEA; length(x)=32 CHECK → octet_length(x)=32.
//   - Every TEXT column gets COLLATE "C". SQLite compares TEXT with BINARY
//     (memcmp) everywhere by default; COLLATE "C" reproduces that byte ordering
//     and equality, so ORDER BY / index / WHERE parity holds even on a database
//     whose default collation is a locale (e.g. en_US.utf8). This is the
//     collation-trap fix (§0.1 #18).
//   - WITHOUT ROWID dropped (a SQLite storage optimization with no semantics).
//   - TEXT timestamps kept as TEXT (RFC3339Nano strings compared in Go); NOT
//     migrated to timestamptz — byte-parity of projections depends on the
//     strings (§0.1 #17).
//   - Append-only / write-closure triggers: SQLite's `CREATE TRIGGER … WHEN
//     <cond> BEGIN SELECT RAISE(ABORT,'msg'); END` becomes a plpgsql function
//     that RAISE EXCEPTIONs plus a `CREATE TRIGGER … BEFORE <op> … FOR EACH ROW
//     EXECUTE FUNCTION`. A PG trigger WHEN clause may reference only NEW/OLD, so
//     every `NOT EXISTS (SELECT … FROM <gate-table>)` gate moves INTO the
//     function body. The gate TABLES port verbatim; MVCC makes the tripwire at
//     least as strict (an uncommitted open=1 in another txn is invisible).
//   - INSERT … ON CONFLICT(…) DO NOTHING / DO UPDATE SET … = excluded.… is
//     identical syntax in PG (SQLite copied it), so those statements are
//     dialect-shared and not restated here.
//
// The `?` placeholders that the shared migrate/seed helpers use are rewritten to
// `$N` by the pgqmark shim; this DDL itself has no placeholders.

// pgSchemaV1 is the PostgreSQL port of schemaV1: the append-only journal with
// its two partial indexes and its two append-only triggers, the retention_gate,
// the writer_lease, graph_meta, and the snapshots table.
const pgSchemaV1 = `
CREATE TABLE journal (
  stream_id           TEXT    COLLATE "C" NOT NULL,
  seq                 BIGINT  NOT NULL,
  substream           TEXT    COLLATE "C" NOT NULL DEFAULT '',
  engine              TEXT    COLLATE "C" NOT NULL CHECK (engine IN ('lumen','v2','v1')),
  type                TEXT    COLLATE "C" NOT NULL,
  ir_contract_version TEXT    COLLATE "C" NOT NULL,
  idem_token          TEXT    COLLATE "C",
  payload             BYTEA   NOT NULL,
  payload_hash        BYTEA   NOT NULL CHECK (octet_length(payload_hash) = 32),
  chain_hash          BYTEA   NOT NULL CHECK (octet_length(chain_hash) = 32),
  lease_epoch         BIGINT  NOT NULL,
  appended_at         TEXT    COLLATE "C" NOT NULL,
  PRIMARY KEY (stream_id, seq)
);

CREATE UNIQUE INDEX journal_idem
  ON journal (stream_id, idem_token) WHERE idem_token IS NOT NULL;

CREATE INDEX journal_substream
  ON journal (stream_id, substream, seq) WHERE substream <> '';

CREATE FUNCTION journal_no_update_fn() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'journal is append-only';
END;
$fn$;
CREATE TRIGGER journal_no_update BEFORE UPDATE ON journal
FOR EACH ROW EXECUTE FUNCTION journal_no_update_fn();

CREATE TABLE retention_gate (
  stream_id TEXT   COLLATE "C" PRIMARY KEY,
  max_seq   BIGINT NOT NULL
);

CREATE FUNCTION journal_no_delete_fn() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM retention_gate g
                 WHERE g.stream_id = OLD.stream_id AND OLD.seq <= g.max_seq) THEN
    RAISE EXCEPTION 'journal is append-only (retention gate closed)';
  END IF;
  RETURN OLD;
END;
$fn$;
CREATE TRIGGER journal_no_delete BEFORE DELETE ON journal
FOR EACH ROW EXECUTE FUNCTION journal_no_delete_fn();

CREATE TABLE snapshots (
  stream_id               TEXT    COLLATE "C" NOT NULL,
  covered_seq             BIGINT  NOT NULL,
  engine                  TEXT    COLLATE "C" NOT NULL,
  reducer_version         INTEGER NOT NULL,
  snapshot_format_version INTEGER NOT NULL,
  state_hash              BYTEA   NOT NULL CHECK (octet_length(state_hash) = 32),
  state                   BYTEA   NOT NULL,
  cut_chain_hash          BYTEA   CHECK (cut_chain_hash IS NULL
                                         OR octet_length(cut_chain_hash) = 32),
  created_at              TEXT    COLLATE "C" NOT NULL,
  PRIMARY KEY (stream_id, covered_seq)
);

CREATE TABLE writer_lease (
  stream_id  TEXT   COLLATE "C" PRIMARY KEY,
  holder     TEXT   COLLATE "C" NOT NULL,
  epoch      BIGINT NOT NULL,
  expires_at TEXT   COLLATE "C" NOT NULL
);

CREATE TABLE graph_meta (
  key   TEXT COLLATE "C" PRIMARY KEY,
  value TEXT COLLATE "C" NOT NULL
);
`

// pgSchemaV2 is the PostgreSQL port of schemaV2: the Tier-A projection tables
// (nodes + four partial indexes, node_labels, node_metadata, edges +
// edges_reverse, frontier + covering index, defer_wakeups, channel_cursors), the
// tier_a_write_gate singleton, and the three nodes fold-owned write-closure
// tripwires. WITHOUT ROWID is dropped from frontier (PG heap+PK). The three
// tripwires share one guard function dispatching on TG_OP; each op has its own
// trigger, so all three fire exactly as the SQLite guards do.
const pgSchemaV2 = `
CREATE TABLE nodes (
  id           TEXT    COLLATE "C" PRIMARY KEY,
  title        TEXT    COLLATE "C" NOT NULL DEFAULT '',
  status       TEXT    COLLATE "C" NOT NULL DEFAULT 'open',
  bead_type    TEXT    COLLATE "C" NOT NULL DEFAULT 'task',
  priority     INTEGER,
  description  TEXT    COLLATE "C" NOT NULL DEFAULT '',
  assignee     TEXT    COLLATE "C" NOT NULL DEFAULT '',
  from_actor   TEXT    COLLATE "C" NOT NULL DEFAULT '',
  parent_id    TEXT    COLLATE "C" NOT NULL DEFAULT '',
  ref          TEXT    COLLATE "C" NOT NULL DEFAULT '',
  created_at   TEXT    COLLATE "C" NOT NULL,
  updated_at   TEXT    COLLATE "C" NOT NULL DEFAULT '',
  defer_until  TEXT    COLLATE "C",
  storage_tier TEXT    COLLATE "C" NOT NULL DEFAULT 'history'
               CHECK (storage_tier IN ('history','no_history','ephemeral')),
  is_blocked   INTEGER NOT NULL DEFAULT 0,
  fold_owned   INTEGER NOT NULL DEFAULT 0,
  stream_id    TEXT    COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX nodes_status   ON nodes (status, bead_type);
CREATE INDEX nodes_parent   ON nodes (parent_id)        WHERE parent_id <> '';
CREATE INDEX nodes_assignee ON nodes (assignee, status) WHERE assignee <> '';
CREATE INDEX nodes_stream   ON nodes (stream_id)        WHERE stream_id <> '';

CREATE TABLE node_labels (
  node_id TEXT COLLATE "C" NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  label   TEXT COLLATE "C" NOT NULL,
  PRIMARY KEY (node_id, label)
);
CREATE INDEX node_labels_by_label ON node_labels (label);

CREATE TABLE node_metadata (
  node_id TEXT COLLATE "C" NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  key     TEXT COLLATE "C" NOT NULL,
  value   TEXT COLLATE "C" NOT NULL,
  PRIMARY KEY (node_id, key)
);
CREATE INDEX node_metadata_kv ON node_metadata (key, value);

CREATE TABLE edges (
  from_id  TEXT COLLATE "C" NOT NULL,
  to_id    TEXT COLLATE "C" NOT NULL,
  dep_type TEXT COLLATE "C" NOT NULL DEFAULT 'blocks',
  metadata TEXT COLLATE "C" NOT NULL DEFAULT '',
  PRIMARY KEY (from_id, to_id, dep_type),
  FOREIGN KEY (from_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX edges_reverse ON edges (to_id);

CREATE TABLE frontier (
  node_id        TEXT    COLLATE "C" PRIMARY KEY,
  root_id        TEXT    COLLATE "C" NOT NULL,
  route          TEXT    COLLATE "C" NOT NULL DEFAULT '',
  ready_priority INTEGER NOT NULL DEFAULT 2,
  created_at     TEXT    COLLATE "C" NOT NULL,
  id             TEXT    COLLATE "C" NOT NULL,
  defer_until    TEXT    COLLATE "C"
);
CREATE INDEX frontier_route_order
  ON frontier (route, ready_priority, created_at, id);

CREATE TABLE defer_wakeups (
  node_id TEXT COLLATE "C" PRIMARY KEY,
  wake_at TEXT COLLATE "C" NOT NULL
);
CREATE INDEX defer_wakeups_by_time ON defer_wakeups (wake_at, node_id);

CREATE TABLE channel_cursors (
  stream_id    TEXT   COLLATE "C" NOT NULL,
  substream    TEXT   COLLATE "C" NOT NULL,
  reader_key   TEXT   COLLATE "C" NOT NULL,
  position     BIGINT NOT NULL,
  planted_seq  BIGINT NOT NULL,
  advanced_seq BIGINT NOT NULL,
  PRIMARY KEY (stream_id, substream, reader_key)
);

CREATE TABLE tier_a_write_gate (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 0),
  open      INTEGER NOT NULL DEFAULT 0
);
INSERT INTO tier_a_write_gate(singleton, open) VALUES (0, 0);

CREATE FUNCTION nodes_fold_owned_guard_fn() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.fold_owned = 1
       AND NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1) THEN
      RAISE EXCEPTION 'nodes: fold-owned row is write-closed (I-14)';
    END IF;
    RETURN OLD;
  END IF;
  IF ((TG_OP = 'INSERT' AND NEW.fold_owned = 1)
      OR (TG_OP = 'UPDATE' AND (OLD.fold_owned = 1 OR NEW.fold_owned = 1)))
     AND NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1) THEN
    RAISE EXCEPTION 'nodes: fold-owned row is write-closed (I-14)';
  END IF;
  RETURN NEW;
END;
$fn$;
CREATE TRIGGER nodes_fold_owned_no_insert BEFORE INSERT ON nodes
FOR EACH ROW EXECUTE FUNCTION nodes_fold_owned_guard_fn();
CREATE TRIGGER nodes_fold_owned_no_update BEFORE UPDATE ON nodes
FOR EACH ROW EXECUTE FUNCTION nodes_fold_owned_guard_fn();
CREATE TRIGGER nodes_fold_owned_no_delete BEFORE DELETE ON nodes
FOR EACH ROW EXECUTE FUNCTION nodes_fold_owned_guard_fn();
`

// pgSchemaV3 is the PostgreSQL port of schemaV3: graph_residence (tri-state
// residence keyed by root id) with its state index, and the three frontier
// write-closure tripwires (unconditional on ownership — every frontier row is
// fold-owned — gated only on tier_a_write_gate). One shared guard function backs
// the three per-op triggers.
const pgSchemaV3 = `
CREATE TABLE graph_residence (
  root_id     TEXT   COLLATE "C" PRIMARY KEY,
  state       TEXT   COLLATE "C" NOT NULL CHECK (state IN ('migrating','journal')),
  fence_epoch BIGINT NOT NULL DEFAULT 0,
  updated_at  TEXT   COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX graph_residence_state ON graph_residence (state);

CREATE FUNCTION frontier_write_closed_guard_fn() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM tier_a_write_gate WHERE singleton = 0 AND open = 1) THEN
    RAISE EXCEPTION 'frontier is write-closed (I-14)';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$fn$;
CREATE TRIGGER frontier_write_closed_no_insert BEFORE INSERT ON frontier
FOR EACH ROW EXECUTE FUNCTION frontier_write_closed_guard_fn();
CREATE TRIGGER frontier_write_closed_no_update BEFORE UPDATE ON frontier
FOR EACH ROW EXECUTE FUNCTION frontier_write_closed_guard_fn();
CREATE TRIGGER frontier_write_closed_no_delete BEFORE DELETE ON frontier
FOR EACH ROW EXECUTE FUNCTION frontier_write_closed_guard_fn();
`

// pgSchemaV4 is the PostgreSQL port of schemaV4: the snapshot_write_gate
// singleton (the analog of tier_a_write_gate) and the three snapshots
// write-closure tripwires. One shared guard function backs the three per-op
// triggers.
const pgSchemaV4 = `
CREATE TABLE snapshot_write_gate (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 0),
  open      INTEGER NOT NULL DEFAULT 0
);
INSERT INTO snapshot_write_gate(singleton, open) VALUES (0, 0);

CREATE FUNCTION snapshots_write_closed_guard_fn() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM snapshot_write_gate WHERE singleton = 0 AND open = 1) THEN
    RAISE EXCEPTION 'snapshots is write-closed (R-SNAP-WRITE)';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$fn$;
CREATE TRIGGER snapshots_write_closed_no_insert BEFORE INSERT ON snapshots
FOR EACH ROW EXECUTE FUNCTION snapshots_write_closed_guard_fn();
CREATE TRIGGER snapshots_write_closed_no_update BEFORE UPDATE ON snapshots
FOR EACH ROW EXECUTE FUNCTION snapshots_write_closed_guard_fn();
CREATE TRIGGER snapshots_write_closed_no_delete BEFORE DELETE ON snapshots
FOR EACH ROW EXECUTE FUNCTION snapshots_write_closed_guard_fn();
`

// pgMigrations is the PostgreSQL forward-only ladder, one rung per shared schema
// version, matching the length and version numbering of the SQLite migrations
// slice. Never edit a shipped entry; append.
var pgMigrations = []string{
	pgSchemaV1,
	pgSchemaV2,
	pgSchemaV3,
	pgSchemaV4,
}
