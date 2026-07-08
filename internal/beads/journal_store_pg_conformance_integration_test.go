//go:build integration

package beads_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// This file is P6.3's real-Postgres validation of the beads JournalStore façade:
// the beads.Store surface run through the storage-conformance suite against a
// graphstore.OpenPostgres-backed store (bare and CachingStore-wrapped), plus
// SQLite-vs-Postgres parity for the hot façade paths (ApplyGraphPlan + the Ready
// frontier), the settlement/provenance fold, and the Delete CASCADE + fold-owned
// tripwire leg the conformance suite does not execute. See
// TestJournalStorePGConformance for exactly which methods the enrolled suites
// drive on Postgres versus which are covered by composition. It shares the P6.2
// env-DSN-gated, per-test-schema isolation pattern (GRAPHSTORE_PG_DSN primary,
// GC_GRAPH_TEST_PG_DSN alias; a private CREATE SCHEMA per store, dropped in
// cleanup) so it is parallel-safe against a shared dev Postgres and skips cleanly
// when no DSN is configured. It lives in package beads_test (external) because the
// conformance suite imports beadstest, which imports beads — an internal beads test
// file enrolling it would form an import cycle.

// pgConfTestDSN returns the Postgres DSN for the façade conformance/parity arm, or
// skips cleanly when none is configured — the same gate the graphstore and P6.2
// façade Postgres arms use.
func pgConfTestDSN(t *testing.T) string {
	t.Helper()
	for _, env := range []string{"GRAPHSTORE_PG_DSN", "GC_GRAPH_TEST_PG_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(env)); dsn != "" {
			return dsn
		}
	}
	t.Skip("GRAPHSTORE_PG_DSN not set; skipping Postgres façade conformance tests")
	return ""
}

// pgConfRandSchema returns a fresh, identifier-safe schema name for per-store
// isolation.
func pgConfRandSchema(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "gs_conf_" + hex.EncodeToString(b[:])
}

// pgConfWithSearchPathDSN pins every connection opened from the returned DSN to
// schema by adding the search_path startup parameter. lib/pq forwards any
// non-driver connection-string key as a startup GUC, so graphstore.OpenPostgres
// lands its tables and operates entirely inside the private schema. Handles both
// the postgres:// URL and keyword DSN forms lib/pq accepts.
func pgConfWithSearchPathDSN(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " search_path=" + schema
}

// newPGConformanceFactory returns a beads.Store factory that opens a JournalStore
// over a fresh private Postgres schema for every call — the Postgres analog of the
// SQLite conformance factory's fresh-temp-DB-per-store.
//
// It is deliberately connection-frugal: it closes the previous store and drops its
// schema at the START of each call, so at most one conformance store (plus the
// bootstrap connection) is open at any moment. The conformance suite calls
// newStore() exactly once per subtest and never reuses a prior subtest's store, so
// closing the previous store on the next call is safe — and it keeps the whole run
// within a handful of Postgres connections, so it works against a shared dev server
// with the default max_connections rather than needing ~50 simultaneous stores.
func newPGConformanceFactory(t *testing.T) func() beads.Store {
	t.Helper()
	dsn := pgConfTestDSN(t)
	ctx := context.Background()

	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}

	var (
		mu         sync.Mutex
		prevStore  *graphstore.Store
		prevSchema string
	)
	closePrev := func() {
		if prevStore != nil {
			_ = prevStore.Close()
			prevStore = nil
		}
		if prevSchema != "" {
			if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+prevSchema+" CASCADE"); err != nil {
				t.Logf("cleanup drop schema %s: %v", prevSchema, err)
			}
			prevSchema = ""
		}
	}
	t.Cleanup(func() {
		closePrev()
		_ = boot.Close()
	})

	return func() beads.Store {
		mu.Lock()
		defer mu.Unlock()
		closePrev()

		schema := pgConfRandSchema(t)
		if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		gs, err := graphstore.OpenPostgres(ctx, pgConfWithSearchPathDSN(dsn, schema), graphstore.Options{CityID: "conformance-city"})
		if err != nil {
			_, _ = boot.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			t.Fatalf("OpenPostgres on %s: %v", schema, err)
		}
		prevStore, prevSchema = gs, schema
		return beads.NewJournalStore(gs)
	}
}

// TestJournalStorePGConformance runs the shared beads.Store conformance suite
// against a JournalStore backed by a REAL Postgres graphstore — the P6.3 headline
// gate. Every RunStoreTests / RunMetadataTests / RunConditionalMetadataTests
// subtest is a cross-backend parity contract that passes byte-identically on
// Postgres, through the same `?`-placeholder SQL the SQLite arm uses (rewritten to
// `$N` by the pgqmark shim).
//
// Precisely, the enrolled suites drive these façade methods on Postgres:
// Get/List/ListOpen/Ready/Children/ListByLabel/Create/Update/Close/SetMetadata/
// SetMetadataIf/Tx. The sibling tests in this file add ApplyGraphPlan + the Ready
// frontier (TestJournalStorePGApplyGraphPlanReadyParity), the settlement/provenance
// fold read path (TestJournalStorePGProvenanceFoldParity), and Delete's schema
// machinery (TestJournalStorePGDeleteCascadeParity).
//
// The enrolled suites do NOT call Reopen, CloseAll, Delete, DepAdd, DepRemove,
// DepList, SetMetadataBatch, Count, Ping, ListByAssignee, or ListByMetadata — those
// live in RunDepTests and the sequential/creation-order suites, which are not
// enrolled here. Most are compositions of SQL the enrolled suites already exercise:
// Reopen/CloseAll reuse the Close/Update UPDATE path; ListByAssignee/ListByMetadata
// reuse the hydrateWhere SELECT; DepAdd/DepRemove reuse the edge insert/delete that
// ApplyGraphPlan also drives; SetMetadataBatch reuses the SetMetadata UPSERT;
// DepList/Count/Ping are plain SELECTs. The one real gap is Delete, whose ON DELETE
// CASCADE fan-out (node_labels/node_metadata/edges child rows) and BEFORE-DELETE
// fold-owned tripwire are schema machinery no enrolled subtest executes on
// Postgres; TestJournalStorePGDeleteCascadeParity below closes that leg directly.
//
// RunConditionalMetadataTestsConcurrent is used (not the bare variant) because the
// journal store serializes SetMetadataIf cleanly in-process via its single write
// connection — the same reason the SQLite bare conformance uses it. See
// TestJournalStoreConformance for why RunDepTests is not enrolled.
func TestJournalStorePGConformance(t *testing.T) {
	factory := newPGConformanceFactory(t)
	beadstest.RunStoreTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
	beadstest.RunConditionalMetadataTestsConcurrent(t, factory)
}

// TestJournalStorePGConformanceCachingWrapped runs the same conformance suite
// against a Postgres-backed JournalStore wrapped in a CachingStore, proving the
// cache layer preserves the Postgres backend's parity behavior exactly as it does
// the SQLite backend's.
func TestJournalStorePGConformanceCachingWrapped(t *testing.T) {
	backing := newPGConformanceFactory(t)
	factory := func() beads.Store {
		return beads.NewCachingStoreForTest(backing(), nil)
	}
	beadstest.RunStoreTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
	beadstest.RunConditionalMetadataTests(t, factory)
}

// --- cross-arm parity: ApplyGraphPlan + Ready --------------------------------

// openSQLiteJournalArm opens a fresh SQLite-backed JournalStore arm.
func openSQLiteJournalArm(t *testing.T, cityID string) (*beads.JournalStore, *graphstore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: cityID})
	if err != nil {
		t.Fatalf("open sqlite graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return beads.NewJournalStore(gs), gs
}

// openPGJournalArm opens a fresh Postgres-backed JournalStore arm over a private
// schema (dropped in cleanup), skipping cleanly when no DSN is configured.
func openPGJournalArm(t *testing.T, cityID string) (*beads.JournalStore, *graphstore.Store) {
	t.Helper()
	dsn := pgConfTestDSN(t)
	ctx := context.Background()
	schema := pgConfRandSchema(t)

	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	gs, err := graphstore.OpenPostgres(ctx, pgConfWithSearchPathDSN(dsn, schema), graphstore.Options{CityID: cityID})
	if err != nil {
		_, _ = boot.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = boot.Close()
		t.Fatalf("OpenPostgres on %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_ = gs.Close()
		if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
		_ = boot.Close()
	})
	return beads.NewJournalStore(gs), gs
}

// parityGraphApplyPlan is a fixed graph exercising the façade's ApplyGraphPlan write
// surface non-vacuously: nodes with distinct priorities, labels, and metadata; a
// blocking edge; and an independent ready frontier. mintID is deterministic
// (gcg-j1..gcg-jN in plan order), so both arms assign identical ids and the
// projection is directly comparable. Ready order is (priority asc, created_at, id);
// all nodes in one plan share created_at, so it reduces to (priority, id).
func parityGraphApplyPlan() *beads.GraphApplyPlan {
	pri := func(p int) *int { return &p }
	return &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			// a=gcg-j1 pri5, b=gcg-j2 pri1, c=gcg-j3 pri3, d=gcg-j4 pri1, e=gcg-j5 blocked.
			{Key: "a", Title: "alpha", Priority: pri(5), Labels: []string{"y", "x"}, Metadata: map[string]string{"route": "alpha", "k": "v"}},
			{Key: "b", Title: "bravo", Priority: pri(1)},
			{Key: "c", Title: "charlie", Priority: pri(3), Type: "bug"},
			{Key: "d", Title: "delta", Priority: pri(1)},
			{Key: "e", Title: "echo", Priority: pri(0), Description: "blocked on a"},
		},
		Edges: []beads.GraphApplyEdge{
			{FromKey: "e", ToKey: "a", Type: "blocks"},
		},
	}
}

// TestJournalStorePGApplyGraphPlanReadyParity proves the hot dispatcher-facing
// façade paths — ApplyGraphPlan materialization and the Ready frontier — produce a
// byte-identical projection and an identical Ready ordering on a Postgres-backed
// JournalStore and a SQLite-backed one seeded with the SAME plan. It is the P6.3
// façade-written-rows analog of the engine's TestPGSQLiteChainByteParity: the
// projection tables the façade writes directly (nodes/node_labels/node_metadata/
// edges, plus the empty frontier the façade maintains for its own rows) match
// across arms, and the live-computed Ready frontier (candidates selected in SQL,
// then ordered in Go by sortBeadsReadyOrder on `(priority, created_at, id)`)
// returns the same ordered ids — the dependency-blocking semantics (e blocked by
// open a) included.
func TestJournalStorePGApplyGraphPlanReadyParity(t *testing.T) {
	ctx := context.Background()
	const city = "graphapply-parity"

	sqliteStore, sqliteGS := openSQLiteJournalArm(t, city)
	pgStore, pgGS := openPGJournalArm(t, city)

	plan := parityGraphApplyPlan()
	sres, err := sqliteStore.ApplyGraphPlan(ctx, plan)
	if err != nil {
		t.Fatalf("sqlite ApplyGraphPlan: %v", err)
	}
	pres, err := pgStore.ApplyGraphPlan(ctx, plan)
	if err != nil {
		t.Fatalf("pg ApplyGraphPlan: %v", err)
	}

	// Deterministic minting: both arms assign identical symbolic-key → id maps.
	if !reflect.DeepEqual(sres.IDs, pres.IDs) {
		t.Fatalf("ApplyGraphPlan id maps differ across arms:\n sqlite=%v\n pg=%v", sres.IDs, pres.IDs)
	}

	// Byte-identical projection (nodes/labels/metadata/edges/frontier). Every
	// deterministic column is in the dump — including the node defer_until/is_blocked
	// and the frontier ready_priority/defer_until — so a defer- or block-bearing
	// fixture cannot silently escape the parity net. Only the wall-clock timestamp
	// columns are excluded (nodes' created_at/updated_at and the frontier's
	// created_at), the sole storage-legitimate cross-arm difference since both arms
	// stamp their own now().
	if sdump, pdump := dumpFacadeTierA(t, sqliteGS), dumpFacadeTierA(t, pgGS); sdump != pdump {
		t.Fatalf("façade projection differs across arms:\n--- sqlite ---\n%s\n--- postgres ---\n%s", sdump, pdump)
	}

	// Identical Ready frontier: e (gcg-j5) is blocked by open a; the rest sort by
	// (priority, id) → b(1,j2), d(1,j4), c(3,j3), a(5,j1).
	sReady := readyIDs(t, sqliteStore)
	pReady := readyIDs(t, pgStore)
	want := []string{"gcg-j2", "gcg-j4", "gcg-j3", "gcg-j1"}
	if !reflect.DeepEqual(sReady, want) {
		t.Fatalf("sqlite Ready order = %v, want %v", sReady, want)
	}
	if !reflect.DeepEqual(sReady, pReady) {
		t.Fatalf("Ready order differs across arms:\n sqlite=%v\n pg=%v", sReady, pReady)
	}

	// The blocking edge is honored on both: closing a unblocks e (gcg-j5) on each
	// arm, and the new Ready frontier still matches across arms.
	for _, arm := range []struct {
		name  string
		store *beads.JournalStore
	}{{"sqlite", sqliteStore}, {"postgres", pgStore}} {
		if err := arm.store.Close(pres.IDs["a"]); err != nil {
			t.Fatalf("%s close a: %v", arm.name, err)
		}
	}
	sReady2 := readyIDs(t, sqliteStore)
	pReady2 := readyIDs(t, pgStore)
	want2 := []string{"gcg-j5", "gcg-j2", "gcg-j4", "gcg-j3"} // e(0), b(1), d(1), c(3); a now closed.
	if !reflect.DeepEqual(sReady2, want2) {
		t.Fatalf("sqlite Ready after closing a = %v, want %v", sReady2, want2)
	}
	if !reflect.DeepEqual(sReady2, pReady2) {
		t.Fatalf("Ready-after-unblock differs across arms:\n sqlite=%v\n pg=%v", sReady2, pReady2)
	}
}

// readyIDs returns the ids Ready reports, in Ready order.
func readyIDs(t *testing.T, s *beads.JournalStore) []string {
	t.Helper()
	beadsList, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	ids := make([]string, len(beadsList))
	for i, b := range beadsList {
		ids[i] = b.ID
	}
	return ids
}

// dumpFacadeTierA renders the façade-owned projection tables in a deterministic,
// column-labeled form so two arms can be compared byte-for-byte. It reads through
// the graphstore's pooled read handle (never the single write connection). Every
// deterministic column is dumped; only the wall-clock timestamp columns are
// excluded — nodes' created_at/updated_at and the frontier's created_at — since
// each arm stamps its own now(). All other columns, including nodes'
// defer_until/is_blocked and the frontier's ready_priority/defer_until, are in the
// dump so a defer- or block-bearing fixture cannot silently escape the parity net.
func dumpFacadeTierA(t *testing.T, gs *graphstore.Store) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder

	// nodes: deterministic columns only (created_at/updated_at excluded), by id.
	rows, err := gs.ReadDB().QueryContext(ctx, `
		SELECT id, title, status, bead_type, COALESCE(priority, -1), description, assignee,
		       from_actor, parent_id, ref, storage_tier, fold_owned, stream_id,
		       COALESCE(defer_until, ''), is_blocked
		  FROM nodes ORDER BY id`)
	if err != nil {
		t.Fatalf("dump nodes: %v", err)
	}
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, title, status, bt, desc, assignee, from, parent, ref, tier, stream, deferUntil string
				priority, foldOwned, isBlocked                                                     int
			)
			if err := rows.Scan(&id, &title, &status, &bt, &priority, &desc, &assignee, &from, &parent, &ref, &tier, &foldOwned, &stream, &deferUntil, &isBlocked); err != nil {
				t.Fatalf("scan node: %v", err)
			}
			fmt.Fprintf(&b, "node id=%q title=%q status=%q type=%q pri=%d desc=%q assignee=%q from=%q parent=%q ref=%q tier=%q fold=%d stream=%q defer=%q blocked=%d\n",
				id, title, status, bt, priority, desc, assignee, from, parent, ref, tier, foldOwned, stream, deferUntil, isBlocked)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("nodes rows: %v", err)
		}
	}()

	dumpKV := func(label, query string, cols int) {
		r, err := gs.ReadDB().QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("dump %s: %v", label, err)
		}
		defer func() { _ = r.Close() }()
		for r.Next() {
			vals := make([]sql.NullString, cols)
			ptrs := make([]any, cols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s: %v", label, err)
			}
			fmt.Fprintf(&b, "%s", label)
			for _, v := range vals {
				fmt.Fprintf(&b, " %q", v.String)
			}
			b.WriteByte('\n')
		}
		if err := r.Err(); err != nil {
			t.Fatalf("%s rows: %v", label, err)
		}
	}
	dumpKV("label", `SELECT node_id, label FROM node_labels ORDER BY node_id, label`, 2)
	dumpKV("meta", `SELECT node_id, key, value FROM node_metadata ORDER BY node_id, key`, 3)
	dumpKV("edge", `SELECT from_id, to_id, dep_type, metadata FROM edges ORDER BY from_id, to_id, dep_type`, 4)
	dumpKV("frontier", `SELECT node_id, root_id, route, ready_priority, COALESCE(defer_until, ''), id FROM frontier ORDER BY node_id`, 6)
	return b.String()
}

// --- cross-arm parity: settlement / provenance fold --------------------------

// seedProvenanceStreams appends the REAL two-stream production topology of a root
// (blueprint P5.4 / TestThreeEngineProvenanceTimeline) onto gs: the lumen run
// stream <root> and the interleaved settlement stream settlement/<root>. It uses
// the exact same deterministic script for every arm so the folds are comparable.
func seedProvenanceStreams(t *testing.T, gs *graphstore.Store, root string) {
	t.Helper()
	gs.RegisterEventType("lumen", "lumen.outcome.settled")
	gs.RegisterEventType("lumen", "lumen.run.closed")
	settlement := beads.SettlementStreamID(root)

	appendOne := func(streamID, engine, typ, idem string, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		head, err := gs.Head(context.Background(), streamID)
		if err != nil {
			t.Fatalf("head %s: %v", streamID, err)
		}
		if _, err := gs.Append(context.Background(), streamID, engine, head, 0,
			[]graphstore.JournalEvent{{Type: typ, IdemToken: idem, Payload: raw}}); err != nil {
			t.Fatalf("append (%s,%s): %v", engine, typ, err)
		}
	}

	// Lumen run stream <root>: fine-grained terminal facts, dense seq 1..2.
	appendOne(root, "lumen", "lumen.outcome.settled", "l1", map[string]any{"activation": "impl:0", "outcome": "pass"})
	appendOne(root, "lumen", "lumen.run.closed", "l2", map[string]any{"outcome": "pass"})

	// Settlement stream settlement/<root>: interleaved v2/v1 coarse facts + the v2
	// control-epoch fence, dense seq 1..4.
	appendOne(settlement, beads.SettlementEngineV2, beads.SettlementAttemptType, "a1",
		beads.SettlementPayload{Root: root, Bead: "gcg-log", Outcome: "fail", Attempt: 2})
	appendOne(settlement, beads.SettlementEngineV2, "control.epoch.fenced", "f1", map[string]any{"bead": "gcg-ctl"})
	appendOne(settlement, beads.SettlementEngineV1, beads.SettlementRootType, "r1",
		beads.SettlementPayload{Root: root, Bead: root, Outcome: "fail"})
	appendOne(settlement, beads.SettlementEngineV2, beads.SettlementWorkflowFinalizedType, "w1",
		beads.SettlementPayload{Root: root, Bead: "gcg-fin", Outcome: "fail"})
}

// TestJournalStorePGProvenanceFoldParity proves the settlement/provenance fold
// (settlementfold + beads.ProvenanceTimeline) folds a Postgres-backed settlement
// stream byte-identically to the SQLite arm. The fold is pure over ReadStream, so
// parity rests on ReadStream returning the SAME ordered events on both engines; the
// per-stream seq order (ORDER BY seq, an INTEGER-safe sort) and the mixed-engine
// fold must reproduce identical facts. It also asserts the fold is deterministic on
// the PG arm (a re-read reproduces it exactly — the DROP+refold-style idempotence
// the pure fold guarantees) and that both hash chains stay intact on Postgres.
func TestJournalStorePGProvenanceFoldParity(t *testing.T) {
	ctx := context.Background()
	const root = "gcg-root"

	sqliteStore, sqliteGS := openSQLiteJournalArm(t, "prov-parity")
	pgStore, pgGS := openPGJournalArm(t, "prov-parity")

	seedProvenanceStreams(t, sqliteGS, root)
	seedProvenanceStreams(t, pgGS, root)

	sqliteStreams, err := beads.ProvenanceTimeline(ctx, sqliteStore, root)
	if err != nil {
		t.Fatalf("sqlite ProvenanceTimeline: %v", err)
	}
	pgStreams, err := beads.ProvenanceTimeline(ctx, pgStore, root)
	if err != nil {
		t.Fatalf("pg ProvenanceTimeline: %v", err)
	}

	// Byte-for-byte fact parity across engines (per-stream groups, seq order,
	// engine tags, decoded fields — everything the timeline commits to).
	if !reflect.DeepEqual(sqliteStreams, pgStreams) {
		t.Fatalf("provenance timeline differs across arms:\n--- sqlite ---\n%+v\n--- postgres ---\n%+v", sqliteStreams, pgStreams)
	}
	// Sanity: the fold is non-vacuous (settlement group of 4 + lumen group of 2).
	if len(pgStreams) != 2 || len(pgStreams[0].Facts) != 4 || len(pgStreams[1].Facts) != 2 {
		t.Fatalf("pg provenance shape = %d groups (%v), want 2 groups of 4 and 2", len(pgStreams), pgStreams)
	}

	// The pure fold is deterministic on Postgres: a re-read reproduces it exactly.
	pgStreams2, err := beads.ProvenanceTimeline(ctx, pgStore, root)
	if err != nil {
		t.Fatalf("pg ProvenanceTimeline (re-read): %v", err)
	}
	if !reflect.DeepEqual(pgStreams, pgStreams2) {
		t.Fatalf("pg provenance re-read differs (fold not deterministic):\n first=%+v\n second=%+v", pgStreams, pgStreams2)
	}

	// Both mixed-engine hash chains stay intact on Postgres.
	if err := pgGS.Verify(ctx, beads.SettlementStreamID(root)); err != nil {
		t.Fatalf("pg Verify(settlement stream): %v", err)
	}
	if err := pgGS.Verify(ctx, root); err != nil {
		t.Fatalf("pg Verify(lumen run stream): %v", err)
	}
}

// --- cross-arm parity: Delete CASCADE + fold-owned tripwire ------------------

// TestJournalStorePGDeleteCascadeParity closes the one façade write-path leg the
// enrolled conformance suites never execute on Postgres: JournalStore.Delete's
// schema machinery. Delete leans on two things the enrolled subtests do not drive —
// the FK ON DELETE CASCADE fan-out (node_labels/node_metadata/edges child rows drop
// with the node) and the BEFORE-DELETE fold-owned tripwire refusing a fold_owned=1
// row (surfaced by the façade guard as ErrFoldOwnedWriteClosed). Neither is a
// composition of an already-exercised statement, so both are proven directly here,
// cross-arm: the SQLite and Postgres arms run the SAME deterministic script and must
// agree byte-for-byte on the surviving child-row counts and on the refusal.
func TestJournalStorePGDeleteCascadeParity(t *testing.T) {
	ctx := context.Background()
	const city = "delete-cascade-parity"

	sqliteStore, sqliteGS := openSQLiteJournalArm(t, city)
	pgStore, pgGS := openPGJournalArm(t, city)

	// Identical script on both arms → deterministic mint, so the victim/dep ids and
	// every child row match across arms. The victim carries two labels, two metadata
	// rows, and one outbound blocking edge — one child in each cascade table.
	seed := func(t *testing.T, s *beads.JournalStore) string {
		t.Helper()
		victim, err := s.Create(beads.Bead{
			Title:    "victim",
			Labels:   []string{"x", "y"},
			Metadata: map[string]string{"k": "val", "route": "alpha"},
		})
		if err != nil {
			t.Fatalf("create victim: %v", err)
		}
		dep, err := s.Create(beads.Bead{Title: "dep"})
		if err != nil {
			t.Fatalf("create dep: %v", err)
		}
		// Outbound edge from victim (from_id=victim → cascades on the victim's delete).
		if err := s.DepAdd(victim.ID, dep.ID, "blocks"); err != nil {
			t.Fatalf("dep add: %v", err)
		}
		return victim.ID
	}

	sVictim := seed(t, sqliteStore)
	pVictim := seed(t, pgStore)
	if sVictim != pVictim {
		t.Fatalf("deterministic mint diverged: sqlite victim=%q pg victim=%q", sVictim, pVictim)
	}

	// childCounts returns [nodes, node_labels, node_metadata, edges-from] for id.
	// The `?` placeholder is native on SQLite and rewritten to `$1` by the pgqmark
	// shim on the Postgres read handle.
	childCounts := func(t *testing.T, gs *graphstore.Store, id string) [4]int {
		t.Helper()
		one := func(query string) int {
			var n int
			if err := gs.ReadDB().QueryRowContext(ctx, query, id).Scan(&n); err != nil {
				t.Fatalf("count %q: %v", query, err)
			}
			return n
		}
		return [4]int{
			one(`SELECT COUNT(*) FROM nodes WHERE id = ?`),
			one(`SELECT COUNT(*) FROM node_labels WHERE node_id = ?`),
			one(`SELECT COUNT(*) FROM node_metadata WHERE node_id = ?`),
			one(`SELECT COUNT(*) FROM edges WHERE from_id = ?`),
		}
	}

	// Pre-delete: the victim has real child rows on both arms, byte-identical.
	sBefore, pBefore := childCounts(t, sqliteGS, sVictim), childCounts(t, pgGS, pVictim)
	wantBefore := [4]int{1, 2, 2, 1} // node, 2 labels, 2 metadata, 1 outbound edge.
	if sBefore != wantBefore {
		t.Fatalf("sqlite pre-delete counts = %v, want %v", sBefore, wantBefore)
	}
	if sBefore != pBefore {
		t.Fatalf("pre-delete child counts differ across arms: sqlite=%v pg=%v", sBefore, pBefore)
	}

	// Delete on both arms; the FK ON DELETE CASCADE must drop every child row.
	if err := sqliteStore.Delete(sVictim); err != nil {
		t.Fatalf("sqlite delete: %v", err)
	}
	if err := pgStore.Delete(pVictim); err != nil {
		t.Fatalf("pg delete: %v", err)
	}
	sAfter, pAfter := childCounts(t, sqliteGS, sVictim), childCounts(t, pgGS, pVictim)
	wantAfter := [4]int{0, 0, 0, 0}
	if sAfter != wantAfter {
		t.Fatalf("sqlite post-delete counts = %v, want all-zero %v", sAfter, wantAfter)
	}
	if sAfter != pAfter {
		t.Fatalf("post-delete child counts differ across arms: sqlite=%v pg=%v", sAfter, pAfter)
	}

	// The victim is gone through the façade on both arms.
	for _, arm := range []struct {
		name  string
		store *beads.JournalStore
	}{{"sqlite", sqliteStore}, {"postgres", pgStore}} {
		if _, err := arm.store.Get(sVictim); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("%s: victim still present after delete: %v", arm.name, err)
		}
	}

	// The BEFORE-DELETE fold-owned tripwire refuses a fold_owned=1 row identically on
	// both arms: Delete returns ErrFoldOwnedWriteClosed, loudly, on Postgres just as
	// on SQLite. A fold row is inserted directly (the façade never writes one) with
	// the write gate briefly opened, mimicking the fold applier.
	for _, arm := range []struct {
		name  string
		store *beads.JournalStore
		gs    *graphstore.Store
	}{{"sqlite", sqliteStore, sqliteGS}, {"postgres", pgStore, pgGS}} {
		insertFoldOwnedNode(t, arm.gs, "gcg-fold-del", city)
		if err := arm.store.Delete("gcg-fold-del"); !errors.Is(err, beads.ErrFoldOwnedWriteClosed) {
			t.Fatalf("%s: Delete(fold-owned) = %v, want ErrFoldOwnedWriteClosed", arm.name, err)
		}
	}
}

// insertFoldOwnedNode writes a fold_owned=1 Tier-A row by briefly opening the
// tier_a_write_gate — the façade never writes fold-owned rows — so a test can prove
// the write-closure guards refuse to mutate it. It runs unchanged on SQLite and on
// the pgqmark-wrapped Postgres write handle (both accept the `?` placeholder).
func insertFoldOwnedNode(t *testing.T, gs *graphstore.Store, id, root string) {
	t.Helper()
	ctx := context.Background()
	db := gs.DB()
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO nodes (id, title, status, created_at, fold_owned, stream_id)
		VALUES (?, 'fold owned', 'open', ?, 1, ?)`,
		id, time.Now().UTC().Format(time.RFC3339Nano), root,
	); err != nil {
		t.Fatalf("insert fold-owned node: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}
}

// --- cross-arm smoke: routed control-frontier read ---------------------------

// TestJournalStorePGControlFrontierRoutedSmoke exercises the routed control-frontier
// read path on real Postgres. ControlFrontier's routed tier runs
// frontierRoutedTier's metadata-EXISTS SELECT and frontierProjectionTier's
// `... ORDER BY ... LIMIT ?` walk — both through the pgqmark shim, and neither
// reached by the enrolled conformance suites. It is a cross-arm parity smoke: the
// same routed/unrouted fixture on a SQLite and a Postgres arm must return the same
// ready routed id.
//
// P6.4: this covers only Arm A (fold_owned=0 routed nodes). The Arm-B fold-owned
// frontier projection — frontierProjectionTier returning ids that hydrateIDsOrdered
// resolves through the nodes IN(...) read — stays unexercised on Postgres, because
// the façade writes no frontier rows and seeding one needs a direct gate-opened
// insert. Exercise that hydration leg before a Postgres city dispatches fold-routed
// work.
func TestJournalStorePGControlFrontierRoutedSmoke(t *testing.T) {
	ctx := context.Background()
	const (
		city  = "routed-frontier-smoke"
		route = "alpha"
		key   = "gc.routed_to"
	)

	// One routed bead (matched on key=route) and one unrouted bead per arm.
	seed := func(t *testing.T, s *beads.JournalStore) string {
		t.Helper()
		routed, err := s.Create(beads.Bead{Title: "routed"})
		if err != nil {
			t.Fatalf("create routed: %v", err)
		}
		if err := s.SetMetadata(routed.ID, key, route); err != nil {
			t.Fatalf("route metadata: %v", err)
		}
		if _, err := s.Create(beads.Bead{Title: "unrouted"}); err != nil {
			t.Fatalf("create unrouted: %v", err)
		}
		return routed.ID
	}

	sqliteStore, _ := openSQLiteJournalArm(t, city)
	pgStore, _ := openPGJournalArm(t, city)
	sRouted := seed(t, sqliteStore)
	pRouted := seed(t, pgStore)

	// LimitPerTier is set so the frontier-projection tier runs its `LIMIT ?` SQL
	// (empty here, but the statement executes on Postgres).
	params := beads.ControlFrontierParams{
		Routes:            []string{route},
		RouteMetadataKeys: []string{key},
		LimitPerTier:      5,
	}
	sGot, err := sqliteStore.ControlFrontier(ctx, params)
	if err != nil {
		t.Fatalf("sqlite ControlFrontier: %v", err)
	}
	pGot, err := pgStore.ControlFrontier(ctx, params)
	if err != nil {
		t.Fatalf("pg ControlFrontier: %v", err)
	}

	sIDs, pIDs := controlFrontierIDs(sGot), controlFrontierIDs(pGot)
	if want := []string{sRouted}; !reflect.DeepEqual(sIDs, want) {
		t.Fatalf("sqlite routed frontier = %v, want %v", sIDs, want)
	}
	if sRouted != pRouted {
		t.Fatalf("deterministic mint diverged: sqlite=%q pg=%q", sRouted, pRouted)
	}
	if !reflect.DeepEqual(sIDs, pIDs) {
		t.Fatalf("routed frontier differs across arms: sqlite=%v pg=%v", sIDs, pIDs)
	}
}

// controlFrontierIDs projects a frontier result to its ids in return order.
func controlFrontierIDs(bs []beads.Bead) []string {
	ids := make([]string, len(bs))
	for i, b := range bs {
		ids[i] = b.ID
	}
	return ids
}
