package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// frontDoorStoreFreeFiles are the cmd/gc source files whose every function was
// converted to take a dependency-injected typed front door
// (*session.Store / *orders.Store / *nudgequeue.Store) in place of a raw
// bead store. They must never regress to holding a raw store: with no
// beads.Store in scope, a raw bead op on a non-work object (a session
// state-heal, a circuit-breaker metadata write, …) is *untypeable* rather than
// merely absent — the compile-time half of the object-model front-door boundary
// (engdocs/plans/infra-store-decouple/OBJECT-MODEL-FRONT-DOOR-DESIGN.md).
//
// Only files that are ENTIRELY store-free belong here. Mixed/root files
// (session_reconciler.go, cmd_nudge.go, order_dispatch.go, …) legitimately keep
// a raw store for their work/by-id/federation/graph residual and construct the
// front door inline from it — that is the front door being used, not a leak —
// so they are intentionally not listed. Add a file here once all of its
// functions take the injected front door.
var frontDoorStoreFreeFiles = []string{
	"session_circuit_breaker.go",
	"soft_reload.go",
}

// frontDoorForbiddenInStoreFreeFiles are the raw-store parameter types and the
// inline front-door constructors that must not reappear in a store-free file. A
// store-free file receives its front door already constructed at a composition
// root and threaded in.
var frontDoorForbiddenInStoreFreeFiles = []string{
	"beads.Store",
	"beads.SessionStore",
	"beads.OrdersStore",
	"beads.NudgesStore",
	"sessionFrontDoor(",
	"orders.NewStore(",
	"nudgeFrontDoor(",
	"workAssignment{",
}

// TestFrontDoorStoreFreeFilesStayStoreFree pins the front-door dependency-injection
// boundary: the fully-converted files must never reintroduce a raw store —
// neither as a parameter type nor by constructing a front door inline. Mirrors
// TestGCNonTestFilesStayOnWorkerBoundary.
func TestFrontDoorStoreFreeFilesStayStoreFree(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range frontDoorStoreFreeFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range frontDoorForbiddenInStoreFreeFiles {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw-store/front-door-construction pattern %q — this file is dependency-injection store-free; receive the typed front door (*session.Store / *orders.Store / *nudgequeue.Store) as a parameter instead of holding a raw store", name, needle)
			}
		}
	}
}

// snapshotInfoOnlyFiles are the cmd/gc source files whose every session-bead
// snapshot read was converted to the typed session.Info front door
// (snapshot.OpenInfos() / FindInfoBy*) by the P4 non-work field-door cleanup
// (engdocs/plans/infra-store-decouple/NONWORK-BEAD-FIELDDOOR-PLAN.md). They must
// never regress to the raw-bead accessors: a raw session bead escaping the
// snapshot is exactly the leak this migration closes — the field would then be
// read straight off bead metadata instead of through the one codec edge.
//
// Add a file here once it calls NONE of the raw snapshot accessors below — i.e.
// every session bead it consumes from the snapshot arrives as a session.Info.
// Files still mid-conversion (build_desired_state.go, city_runtime.go,
// session_reconciler.go, the pool-demand cascade, …) are intentionally absent.
var snapshotInfoOnlyFiles = []string{
	"template_resolve.go",
	"session_name_lookup.go",
	"cmd_citystatus.go",
	"city_status_snapshot.go",
	"session_reconciler_trace_cycle.go",
	"providers.go",
	"nudge_dispatcher.go",
	"named_sessions.go",
	"soft_reload.go",
}

// forbiddenRawSnapshotAccessors are the *sessionBeadSnapshot methods that return
// a raw beads.Bead (or []beads.Bead). The typed mirrors OpenInfos()/FindInfoByID/
// FindInfoByTemplate/FindInfoByNamedIdentity do not contain these substrings, so
// a converted file matching one of these has reintroduced a raw session-bead read.
var forbiddenRawSnapshotAccessors = []string{
	".Open()",
	".FindByID(",
	".FindSessionBeadByTemplate(",
	".FindSessionBeadByNamedIdentity(",
}

// TestSnapshotInfoOnlyFilesStayOnInfoAccessors pins the read half of the
// non-work field-door boundary: the converted snapshot consumers must keep
// reading session beads through session.Info (OpenInfos/FindInfo*), never the
// raw-bead accessors. Mirrors TestFrontDoorStoreFreeFilesStayStoreFree for the
// read surface.
func TestSnapshotInfoOnlyFilesStayOnInfoAccessors(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range snapshotInfoOnlyFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbiddenRawSnapshotAccessors {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw snapshot accessor %q — this file was converted to the session.Info front door; read session beads via snapshot.OpenInfos()/FindInfoByID/FindInfoByTemplate/FindInfoByNamedIdentity instead of the raw-bead accessor", name, needle)
			}
		}
	}
}

// metadataInfoOnlyFiles are the files whose session beads are read AND written
// exclusively through the typed session.Info projection (infoByID /
// InfoFromPersistedBead) / session.CircuitState — never by cracking bead
// metadata off a raw bead. This is the SHAPE half of the object-model front-door
// boundary: the reconciler decision-path files completed by the lockstep drop
// (engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md)
// plus the session-class periphery files converted by the periphery closure
// (engdocs/plans/infra-store-decouple/SESSION-PERIPHERY-CLOSURE-PLAN.md). Once a
// file routes every session field through the typed projection, a reappearing
// `.Metadata[...]` bead crack is a regression to raw-bead reads.
//
// SHAPE-SEALED IS NOT RELOCATION-SAFE. Membership here means field reads go
// through the Info codec (backend-shape-invariant); it does NOT mean the bead
// LOAD is routed through the session-class store (sessionsBeadStore() /
// resolveSessionStore). That access half is the separate frontDoorStoreFreeFiles
// boundary; a [beads.classes.sessions] relocation captures a file only once BOTH
// halves close. Several files here (e.g. cmd_prime.go, session_template_start.go)
// still load their session bead from a raw store and are shape-sealed only.
//
// Only files that crack NO bead metadata inline (session OR work) belong here —
// each listed file currently contains zero `.Metadata[` of any receiver spelling,
// so the guard forbids the whole family (session.Metadata[, target.session.Metadata[,
// b.Metadata[, bead.Metadata[) with no false positive.
//
// session_reconciler.go and session_reconcile.go are intentionally ABSENT and
// CANNOT be added with a file-level substring guard: they retain a bounded,
// DOCUMENTED raw-by-design census — the raw classifier helpers that take a
// `session beads.Bead` parameter (the oracle-verified siblings of the typed Info
// classifiers, kept for TestSessionClassifierInfoEquivalence and boundary
// projections) plus the start-execution / cross-tick emit-once coupled survivor
// mirrors (S1-S5 in the lockstep-drop census) — which a substring needle cannot
// distinguish from a new decision-path leak. Their protection is the in-code
// census comments plus the LOCKSTEP-DROP census. session_sleep.go /
// session_wake.go / session_lifecycle_parallel.go / session_bead_snapshot.go are
// likewise raw-by-design (sleep-policy helpers, start execution, the bead
// constructor) and stay off this list.
var metadataInfoOnlyFiles = []string{
	"compute_awake_bridge.go",
	"session_progress.go",
	"session_circuit_breaker.go",
	"city_status_snapshot.go",
	"session_template_start.go",
	"adoption_barrier.go",
	"cmd_prime.go",
	"cmd_skill.go",
	"session_resolve.go",
	"cmd_session_logs.go",
	"mcp_integration.go",
	"session_index.go",
	"cmd_session_wake.go",
	"soft_reload.go",
}

// forbiddenRawBeadMetadata is the raw bead-metadata crack this guard forbids.
// The needle `.Metadata[` matches every receiver spelling (session.Metadata[,
// target.session.Metadata[, b.Metadata[, bead.Metadata[, item.bead.Metadata[).
// The listed files are Info/CircuitState-only decision helpers that read no bead
// metadata at all, so the broad needle is exact for them and catches the dominant
// `b.Metadata[` / `bead.Metadata[` leak spelling that a `session.`-anchored needle
// would miss.
var forbiddenRawBeadMetadata = []string{
	".Metadata[",
}

// TestMetadataInfoOnlyFilesStayOnInfoSnapshot pins the write+read half of the
// reconciler front-door boundary: the fully-converted decision-path files must
// keep routing every session field through the typed Info/CircuitState
// projection, never cracking bead metadata off a raw bead. Mirrors
// TestSnapshotInfoOnlyFilesStayOnInfoAccessors for the metadata surface.
func TestMetadataInfoOnlyFilesStayOnInfoSnapshot(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range metadataInfoOnlyFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbiddenRawBeadMetadata {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw bead-metadata crack %q — this file was converted to the typed session.Info / CircuitState projection; read and write session fields through the typed accessor (info.<Field> / infoByID / ApplyPatch / CircuitState) instead of cracking the raw bead", name, needle)
			}
		}
	}
}
