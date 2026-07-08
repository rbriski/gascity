//go:build integration

package graphstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/graphstore/fold/foldtest"
	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// pgSchema creates a fresh private schema on the configured Postgres and returns
// the DSN plus the schema name, dropping the schema in cleanup. It is the base for
// opening TWO independent stores on the SAME schema — the only way to model two
// processes (two write pools) racing one stream, since a single store's write pool
// is capped at one connection and would serialize the goroutines on the pool
// instead of on the Postgres advisory lock under test. It skips cleanly when no
// DSN is configured (via pgTestDSN).
func pgSchema(t *testing.T) (dsn, schema string) {
	t.Helper()
	dsn = pgTestDSN(t)
	schema = randSchema(t)
	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := boot.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
		_ = boot.Close()
	})
	return dsn, schema
}

// openPGOnSchema opens a store whose connections are pinned to schema. extra
// appends per-connection setup after the search_path pin (e.g. a lowered
// lock_timeout for the contention test). Each call is an independent write pool —
// a distinct Postgres backend — so two stores on one schema contend only through
// the server-side advisory lock.
func openPGOnSchema(t *testing.T, dsn, schema string, opts Options, extra ...string) *Store {
	t.Helper()
	setup := append([]string{"SET search_path TO " + schema}, extra...)
	st, err := openPostgres(context.Background(), dsn, opts, setup)
	if err != nil {
		t.Fatalf("openPostgres on %s: %v", schema, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPGConcurrentSameHeadAppendCAS is the P6.2 headline gate on real Postgres: two
// independent write pools race to append at the SAME expectedVersion on the same
// stream. The per-stream advisory lock (taken as the first statement of each
// Append txn) forces the loser to block until the winner commits, then read the
// winner's committed head under READ COMMITTED and fail the expectedVersion CAS
// LOUDLY — never a silent lost update (the S0.4 killer the substrate exists to
// kill). Exactly one wins, the head advances by exactly one, the survivor's
// payload is the stored one, and the chain verifies. Runs under -race over many
// independent streams so a rare interleaving-dependent silent loss surfaces.
func TestPGConcurrentSameHeadAppendCAS(t *testing.T) {
	dsn, schema := pgSchema(t)
	ctx := context.Background()

	stA := openPGOnSchema(t, dsn, schema, Options{CityID: "race-city"})
	stB := openPGOnSchema(t, dsn, schema, Options{CityID: "race-city"})
	for _, s := range []*Store{stA, stB} {
		s.RegisterEventType(testEngine, testType)
	}

	// A high stream count so the durable same-head CAS is a real stress: each stream
	// is an independent two-writer race, so more streams means more chances for a rare
	// interleaving-dependent silent loss to surface under -race.
	const streams = 200
	for i := 0; i < streams; i++ {
		stream := fmt.Sprintf("gcj-pg-race-%d", i)
		payloads := [2][]byte{
			canonPayload(t, fmt.Sprintf(`{"who":"A","i":%d}`, i)),
			canonPayload(t, fmt.Sprintf(`{"who":"B","i":%d}`, i)),
		}
		stores := [2]*Store{stA, stB}

		type outcome struct {
			payload []byte
			res     AppendResult
			err     error
		}
		var results [2]outcome
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				res, err := stores[g].Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
					Type: testType, Payload: payloads[g],
				}})
				results[g] = outcome{payload: payloads[g], res: res, err: err}
			}(g)
		}
		close(start)
		wg.Wait()

		var winners, losers int
		var winnerPayload []byte
		for _, o := range results {
			switch {
			case o.err == nil:
				winners++
				winnerPayload = o.payload
				if o.res.FirstSeq != 1 {
					t.Fatalf("stream %s: winner FirstSeq = %d, want 1", stream, o.res.FirstSeq)
				}
			case errors.Is(o.err, ErrWrongExpectedVersion):
				losers++
			default:
				t.Fatalf("stream %s: unexpected error (want nil or ErrWrongExpectedVersion): %v", stream, o.err)
			}
		}
		if winners != 1 || losers != 1 {
			t.Fatalf("stream %s: winners=%d losers=%d, want exactly 1 each (silent loss or double-commit)", stream, winners, losers)
		}

		head, err := stA.Head(ctx, stream)
		if err != nil {
			t.Fatalf("stream %s: head: %v", stream, err)
		}
		if head != 1 {
			t.Fatalf("stream %s: head = %d, want exactly 1 after the race", stream, head)
		}
		events, err := stA.ReadStream(ctx, stream, 1, 0)
		if err != nil {
			t.Fatalf("stream %s: read: %v", stream, err)
		}
		if len(events) != 1 {
			t.Fatalf("stream %s: stored %d events, want 1", stream, len(events))
		}
		if string(events[0].Payload) != string(winnerPayload) {
			t.Fatalf("stream %s: stored payload %q is not the winner's %q", stream, events[0].Payload, winnerPayload)
		}
		if err := stA.Verify(ctx, stream); err != nil {
			t.Fatalf("stream %s: verify after race: %v", stream, err)
		}
	}
}

// TestPostgresAdvisoryLockContentionErrBusy proves a blocked writer that cannot
// take the per-stream advisory lock within lock_timeout surfaces the retryable
// ErrBusy — the busy_timeout isomorphism. One store holds the stream's advisory
// lock inside an open transaction; a second store (opened with a short
// lock_timeout) attempts an Append on the same stream, blocks on lockStream, and
// after the timeout gets SQLSTATE 55P03 mapped to ErrBusy.
func TestPostgresAdvisoryLockContentionErrBusy(t *testing.T) {
	dsn, schema := pgSchema(t)
	ctx := context.Background()
	const stream = "gcj-pg-busy"

	holder := openPGOnSchema(t, dsn, schema, Options{CityID: "busy-city"})
	// The blocked writer needs a short lock_timeout so the test does not wait the
	// full 5s default; the later SET wins over the default set by openPostgres.
	blocked := openPGOnSchema(t, dsn, schema, Options{CityID: "busy-city"}, "SET lock_timeout = 300")
	blocked.RegisterEventType(testEngine, testType)

	// Hold the stream's advisory lock in an open transaction on the holder store,
	// using the exact key lockStream derives so the blocked Append collides.
	tx, err := holder.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, stream); err != nil {
		t.Fatalf("holder take advisory lock: %v", err)
	}

	// The blocked Append must fail with ErrBusy (lock_timeout), not hang and not a
	// raw untyped error.
	_, err = blocked.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"blocked":true}`),
	}})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("blocked Append under a held advisory lock = %v, want ErrBusy (lock_timeout → 55P03)", err)
	}

	// Releasing the holder lets a retry succeed — the contention was transient.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("holder rollback: %v", err)
	}
	if _, err := blocked.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"blocked":true}`),
	}}); err != nil {
		t.Fatalf("Append after the lock released = %v, want success", err)
	}
}

// TestPostgresLockThenReadObservesCommittedHead pins the READ COMMITTED
// requirement (blueprint §3.2): after taking the per-stream advisory lock as the
// first statement of a transaction, the head read observes a head that COMMITTED
// while this transaction was blocked on that lock. This is the exact interleaving
// Append relies on — a competing writer commits seq 1 while we wait for the lock,
// and our post-lock head read must then compute the CAS against head 1, not a stale
// 0.
//
// The construction is discriminating: the committing writer commits AFTER the
// reader has already begun its transaction and BLOCKED on the advisory lock, so the
// reader's snapshot is established before that commit. Under READ COMMITTED the
// reader's post-lock head read takes a fresh snapshot and sees head 1; under
// REPEATABLE READ/SERIALIZABLE the snapshot would be frozen at the (blocked) lock
// statement — before the commit — and the read would return a stale head 0.
// requireReadCommitted refuses to open a stricter server; this proves the guarantee
// holds for the isolation the store actually runs at.
func TestPostgresLockThenReadObservesCommittedHead(t *testing.T) {
	dsn, schema := pgSchema(t)
	ctx := context.Background()
	const stream = "gcj-pg-rc"

	holder := openPGOnSchema(t, dsn, schema, Options{CityID: "rc-city"})
	reader := openPGOnSchema(t, dsn, schema, Options{CityID: "rc-city"})

	// The holder opens a txn, takes the stream's advisory lock, and INSERTs seq 1
	// WITHOUT committing yet (INSERT is allowed; only UPDATE/DELETE are trigger-gated).
	htx, err := holder.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer func() { _ = htx.Rollback() }()
	if err := holder.dialect.lockStream(ctx, htx, stream); err != nil {
		t.Fatalf("holder lockStream: %v", err)
	}
	if _, err := htx.ExecContext(ctx,
		`INSERT INTO journal(stream_id, seq, engine, type, ir_contract_version,
		    payload, payload_hash, chain_hash, lease_epoch, appended_at)
		 VALUES (?, 1, 'lumen', 't', '', ?, ?, ?, 0, '2026-01-01T00:00:00Z')`,
		stream, []byte{0x01}, b32(0xAA), b32(0xBB),
	); err != nil {
		t.Fatalf("holder insert seq 1 (uncommitted): %v", err)
	}

	// The reader opens its txn and takes the SAME advisory lock as its first
	// statement — it BLOCKS behind the holder, establishing its snapshot now, before
	// the holder commits.
	type result struct {
		head uint64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		rtx, err := reader.writeDB.BeginTx(ctx, nil)
		if err != nil {
			done <- result{err: fmt.Errorf("reader begin: %w", err)}
			return
		}
		defer func() { _ = rtx.Rollback() }()
		if err := reader.dialect.lockStream(ctx, rtx, stream); err != nil {
			done <- result{err: fmt.Errorf("reader lockStream: %w", err)}
			return
		}
		head, _, err := headAndChain(ctx, rtx, stream)
		done <- result{head: head, err: err}
	}()

	// Only commit the holder once the reader is actually blocked on the advisory lock
	// — otherwise the reader could read after the commit and the test would not
	// distinguish a fresh post-lock read from a stale snapshot.
	waitForBlockedAdvisoryLock(t, holder)
	if err := htx.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("reader: %v", res.err)
	}
	if res.head != 1 {
		t.Fatalf("post-lock head = %d, want 1 — the reader's post-lock read did not observe the head committed while it was blocked (stale snapshot; not READ COMMITTED)", res.head)
	}
}

// waitForBlockedAdvisoryLock polls until at least one advisory lock is waiting
// (granted=false) on the cluster, i.e. the reader goroutine has reached lockStream
// and blocked behind the holder. The graphstore Postgres tests run sequentially, so
// on a dedicated test database the only advisory-lock waiter is the one under test.
// It reads through probe's pooled read handle so it never contends the single write
// connection. It fails the test if no waiter appears before the deadline.
func waitForBlockedAdvisoryLock(t *testing.T, probe *Store) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := probe.ReadDB().QueryRowContext(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
			t.Fatalf("probe pg_locks: %v", err)
		}
		if n >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the reader to block on the advisory lock")
}

// TestPGConcurrentRebuildTierAVsAppend proves the subtle cross-process TOCTOU
// close (blueprint §3.3). A concurrent Append (a separate write pool) commits in
// the window between RebuildTierA's from-genesis read and its write transaction.
// The rebuild's in-txn head recheck — sound because the advisory lock now holds
// the same serialization SQLite's BEGIN IMMEDIATE gives — catches the drift and
// aborts with ErrRebuildRaced rather than durably committing a SILENTLY STALE
// projection. A clean retry then rebuilds against the new head.
func TestPGConcurrentRebuildTierAVsAppend(t *testing.T) {
	dsn, schema := pgSchema(t)
	ctx := context.Background()
	r := foldtest.EchoReducer{}
	const stream = "gcj-pg-rebuild-race"

	rebuilder := openPGOnSchema(t, dsn, schema, Options{CityID: "rebuild-city"})
	appender := openPGOnSchema(t, dsn, schema, Options{CityID: "rebuild-city"})
	for _, s := range []*Store{rebuilder, appender} {
		s.RegisterEventType(foldtest.Engine, foldtest.EventNode)
	}

	for i := 0; i < 2; i++ {
		if _, err := rebuilder.Append(ctx, stream, foldtest.Engine, uint64(i), 0, []JournalEvent{{
			Type: foldtest.EventNode, Payload: canonPayload(t, fmt.Sprintf(`{"id":"n%d"}`, i)),
		}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// The racing append lands from a DIFFERENT write pool after RebuildTierA has
	// read+folded seq 1..2 but before its write txn/advisory lock — the exact
	// cross-process window the lock+recheck closes.
	var raced bool
	rebuilder.rebuildAfterRead = func() {
		if raced {
			return
		}
		raced = true
		if _, err := appender.Append(ctx, stream, foldtest.Engine, 2, 0, []JournalEvent{{
			Type: foldtest.EventNode, Payload: canonPayload(t, `{"id":"n2"}`),
		}}); err != nil {
			t.Errorf("racing append: %v", err)
		}
	}

	err := rebuilder.RebuildTierA(ctx, r, stream)
	if !errors.Is(err, ErrRebuildRaced) {
		t.Fatalf("rebuild racing a concurrent append = %v, want ErrRebuildRaced (a stale projection would have committed silently)", err)
	}

	// The aborted rebuild left no partial projection; a clean retry against the new
	// head (n0, n1, n2) succeeds.
	rebuilder.rebuildAfterRead = nil
	if err := rebuilder.RebuildTierA(ctx, r, stream); err != nil {
		t.Fatalf("clean retry after race: %v", err)
	}
	var nodes int
	if err := rebuilder.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE stream_id = ?`, stream).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 3 {
		t.Fatalf("nodes after clean rebuild = %d, want 3", nodes)
	}
}

// TestPGUpsertNodeGuardRefusesFacadeAdoption pins the MEDIUM-3 loud-on-Postgres I-14
// guard at the SQL level that upsertNode relies on: the fold applier's ON
// CONFLICT(id) DO UPDATE is guarded `WHERE nodes.fold_owned = 1`, so a conflict
// against a façade-owned (fold_owned=0) row — the cross-process TOCTOU that
// upsertNode's pre-read cannot catch on Postgres — updates ZERO rows and leaves the
// façade row uncorrupted. upsertNode promotes that 0-row outcome to a loud
// ErrProjectionIDCollision rather than silently adopting (write-closing) the beads
// façade's own bead.
func TestPGUpsertNodeGuardRefusesFacadeAdoption(t *testing.T) {
	dsn, schema := pgSchema(t)
	ctx := context.Background()
	st := openPGOnSchema(t, dsn, schema, Options{CityID: "guard-city"})

	// Seed a façade-owned row (fold_owned=0); the narrow tripwire allows this.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO nodes(id, created_at, fold_owned, stream_id) VALUES('gcg-x', '2026-01-01', 0, 'gcg-root')`,
	); err != nil {
		t.Fatalf("seed façade row: %v", err)
	}

	// Run the fold applier's guarded upsert (the load-bearing WHERE guard from
	// upsertNode) with the Tier-A gate open, exactly as ApplyDelta does: the proposed
	// INSERT carries fold_owned=1 (allowed while the gate is open), the conflict
	// routes to DO UPDATE ... WHERE nodes.fold_owned = 1, and that predicate excludes
	// the fold_owned=0 façade row.
	setGate(t, st, "tier_a_write_gate", 1)
	res, err := st.DB().ExecContext(ctx,
		`INSERT INTO nodes(id, created_at, fold_owned, stream_id) VALUES('gcg-x', '2026-02-02', 1, 'gcg-root')
		 ON CONFLICT(id) DO UPDATE SET created_at = excluded.created_at, fold_owned = 1, stream_id = excluded.stream_id
		 WHERE nodes.fold_owned = 1`,
	)
	if err != nil {
		t.Fatalf("guarded upsert: %v", err)
	}
	setGate(t, st, "tier_a_write_gate", 0)

	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 0 {
		t.Fatalf("guarded upsert affected %d rows against a fold_owned=0 row, want 0 (the guard must refuse adoption; upsertNode would report ErrProjectionIDCollision)", n)
	}

	// The façade row is intact: still fold_owned=0 with its original created_at.
	var fo int
	var created string
	if err := st.ReadDB().QueryRowContext(ctx,
		`SELECT fold_owned, created_at FROM nodes WHERE id = 'gcg-x'`).Scan(&fo, &created); err != nil {
		t.Fatalf("re-read façade row: %v", err)
	}
	if fo != 0 || created != "2026-01-01" {
		t.Fatalf("façade row corrupted: fold_owned=%d created_at=%q, want 0 and '2026-01-01'", fo, created)
	}
}

// TestPGSQLiteChainByteParity runs one deterministic journal+projection script
// against a SQLite store and a Postgres store opened with the SAME city id, and
// asserts byte-for-byte cross-arm parity of everything the chain and the
// projection commit to:
//
//   - identical journal rows (seq, substream, engine, type, ir_contract_version,
//     idem_token, payload, payload_hash, and the CHAIN_HASH) — everything a
//     verifier walks, excluding only the wall-clock appended_at;
//   - identical Head and a green Verify on both arms;
//   - a byte-identical Tier-A projection (all seven tables) on both arms; and
//   - DROP+refold (RebuildTierA) reproducing that same projection on each arm.
//
// The same `?`-placeholder SQL, the I/O-free canon/fold packages, and the shared
// chain-hash preimage are why the bytes match; the only storage-legitimate
// difference (appended_at) is excluded.
func TestPGSQLiteChainByteParity(t *testing.T) {
	ctx := context.Background()
	const stream = "gcj-parity"
	r := foldtest.EchoReducer{}

	sqliteStore := newTestStore(t) // registers testEngine types, unused here
	dsn, schema := pgSchema(t)
	pgStore := openPGOnSchema(t, dsn, schema, Options{CityID: "city-under-test"})

	// Both arms MUST share the city id, since it seeds the genesis chain hash.
	if sqliteStore.CityID() != pgStore.CityID() {
		t.Fatalf("arm city ids differ: sqlite=%q pg=%q — chains cannot be byte-equal", sqliteStore.CityID(), pgStore.CityID())
	}

	driveEchoParityScript(t, sqliteStore, stream, r)
	driveEchoParityScript(t, pgStore, stream, r)

	// Journal row parity (chain hashes byte-equal across arms).
	if sj, pj := dumpJournalRows(t, sqliteStore, stream), dumpJournalRows(t, pgStore, stream); sj != pj {
		t.Fatalf("journal rows differ across arms:\n--- sqlite ---\n%s\n--- postgres ---\n%s", sj, pj)
	}

	// Head + Verify parity.
	sh, err := sqliteStore.Head(ctx, stream)
	if err != nil {
		t.Fatalf("sqlite head: %v", err)
	}
	ph, err := pgStore.Head(ctx, stream)
	if err != nil {
		t.Fatalf("pg head: %v", err)
	}
	if sh != ph {
		t.Fatalf("head differs across arms: sqlite=%d pg=%d", sh, ph)
	}
	if err := sqliteStore.Verify(ctx, stream); err != nil {
		t.Fatalf("sqlite verify: %v", err)
	}
	if err := pgStore.Verify(ctx, stream); err != nil {
		t.Fatalf("pg verify: %v", err)
	}

	// Live projection byte parity.
	sliveSQLite := dumpTierA(t, sqliteStore, stream)
	slivePG := dumpTierA(t, pgStore, stream)
	if sliveSQLite != slivePG {
		t.Fatalf("live projection differs across arms:\n--- sqlite ---\n%s\n--- postgres ---\n%s", sliveSQLite, slivePG)
	}

	// DROP+refold reproduces the same projection on each arm (byte-identical to the
	// live one, and to the other arm).
	if err := sqliteStore.RebuildTierA(ctx, r, stream); err != nil {
		t.Fatalf("sqlite rebuild: %v", err)
	}
	if err := pgStore.RebuildTierA(ctx, r, stream); err != nil {
		t.Fatalf("pg rebuild: %v", err)
	}
	srebuild := dumpTierA(t, sqliteStore, stream)
	prebuild := dumpTierA(t, pgStore, stream)
	if srebuild != sliveSQLite {
		t.Fatalf("sqlite rebuild is not byte-identical to its live projection:\n--- live ---\n%s\n--- rebuild ---\n%s", sliveSQLite, srebuild)
	}
	if srebuild != prebuild {
		t.Fatalf("rebuilt projection differs across arms:\n--- sqlite ---\n%s\n--- postgres ---\n%s", srebuild, prebuild)
	}
}

// driveEchoParityScript appends a fixed multi-event journal to stream and builds
// its Tier-A projection live (one delta per serve cycle, each in its own txn),
// exactly like the DET-T-17 path. The script exercises nodes (+ labels/metadata),
// edges (blocking), a cursor (+ defer wakeup), a substream (folded into the chain
// but ignored by the reducer), and an idem token — so the cross-arm dumps compare
// every parity-bearing column non-vacuously.
func driveEchoParityScript(t *testing.T, s *Store, stream string, r foldtest.EchoReducer) {
	t.Helper()
	ctx := context.Background()
	s.RegisterEventType(foldtest.Engine, foldtest.EventNode)
	s.RegisterEventType(foldtest.Engine, foldtest.EventEdge)
	s.RegisterEventType(foldtest.Engine, foldtest.EventCursor)

	events := []JournalEvent{
		{Type: foldtest.EventNode, Substream: "chan-a", IdemToken: "tok-n1", Payload: canonPayload(t, `{"id":"n1","title":"one"}`)},
		{Type: foldtest.EventNode, Payload: canonPayload(t, `{"id":"n2","title":"two"}`)},
		{Type: foldtest.EventEdge, Payload: canonPayload(t, `{"from":"n1","to":"n2"}`)},
		{Type: foldtest.EventNode, Payload: canonPayload(t, `{"id":"n3","title":"three"}`)},
		{Type: foldtest.EventEdge, Payload: canonPayload(t, `{"from":"n2","to":"n3"}`)},
		{Type: foldtest.EventCursor, Payload: canonPayload(t, `{"reader":"r1","node":"n1","position":2,"wake_at":"2020-02-02T00:00:00Z"}`)},
	}

	state := r.Zero(stream)
	for i, e := range events {
		if _, err := s.Append(ctx, stream, foldtest.Engine, uint64(i), 0, []JournalEvent{e}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		next, delta, err := r.Apply(state, fold.Event{
			StreamID: stream, Seq: uint64(i + 1), Engine: foldtest.Engine,
			Substream: e.Substream, Type: e.Type, IdemToken: e.IdemToken, Payload: e.Payload,
		})
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		state = next

		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if err := s.ApplyDelta(ctx, tx, delta); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply delta %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
}

// dumpJournalRows renders every committed journal row for streamID in a
// deterministic, column-labeled form (blob columns hex-encoded) so two arms'
// journals — including the chain_hash — can be compared byte-for-byte. The
// wall-clock appended_at is deliberately excluded: it is the only
// storage-legitimate cross-arm difference and is not in the chain preimage.
func dumpJournalRows(t *testing.T, s *Store, streamID string) string {
	t.Helper()
	rows, err := s.ReadDB().QueryContext(context.Background(),
		`SELECT seq, substream, engine, type, ir_contract_version, idem_token,
		        payload, payload_hash, chain_hash, lease_epoch
		   FROM journal WHERE stream_id = ? ORDER BY seq`, streamID)
	if err != nil {
		t.Fatalf("dump journal rows: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var (
			seq, leaseEpoch          uint64
			substream, engine, typ   string
			irVersion                string
			token                    sql.NullString
			payload, payHash, chainH []byte
		)
		if err := rows.Scan(&seq, &substream, &engine, &typ, &irVersion, &token,
			&payload, &payHash, &chainH, &leaseEpoch); err != nil {
			t.Fatalf("dump journal scan: %v", err)
		}
		tok := "NULL"
		if token.Valid {
			tok = token.String
		}
		fmt.Fprintf(&b, "seq=%d substream=%q engine=%q type=%q irv=%q idem=%s payload=%s payload_hash=%s chain_hash=%s lease_epoch=%d\n",
			seq, substream, engine, typ, irVersion, tok,
			hex.EncodeToString(payload), hex.EncodeToString(payHash), hex.EncodeToString(chainH), leaseEpoch)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump journal rows: %v", err)
	}
	return b.String()
}
