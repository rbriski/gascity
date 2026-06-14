package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// TestResolveRealBdPathHonorsGCBDReal proves the shim resolves the real bd
// binary from GC_BD_REAL by absolute path and does NOT depend on a PATH lookup
// of "bd" — the recursion trap that would resolve back to the shim once it is
// installed as `bd` first on an agent's PATH (graph-store-rollout-plan.md §C2).
func TestResolveRealBdPathHonorsGCBDReal(t *testing.T) {
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	// Empty PATH: any accidental exec.LookPath("bd") would fail, so a successful
	// resolve proves GC_BD_REAL is honored without consulting PATH.
	t.Setenv("PATH", "")

	got, err := resolveRealBdPath()
	if err != nil {
		t.Fatalf("resolveRealBdPath: %v", err)
	}
	if got != realBd {
		t.Fatalf("resolveRealBdPath = %q, want %q", got, realBd)
	}
}

// TestResolveRealBdPathRejectsRelativeGCBDReal guards the install-time contract:
// GC_BD_REAL must be an absolute path (a relative one would re-introduce PATH
// ambiguity and the recursion risk).
func TestResolveRealBdPathRejectsRelativeGCBDReal(t *testing.T) {
	t.Setenv("GC_BD_REAL", filepath.Join("relative", "bd"))
	if _, err := resolveRealBdPath(); err == nil {
		t.Fatal("expected an error for a relative GC_BD_REAL, got nil")
	}
}

// TestExecRealBdUsesGCBDRealAndPropagatesExit proves the passthrough path execs
// the GC_BD_REAL binary (no LookPath), streams its stdout, and propagates its
// exit code unchanged — the bd exit-code contract the shim must preserve.
func TestExecRealBdUsesGCBDRealAndPropagatesExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake bd is POSIX-only")
	}
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\necho \"args:$*\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("PATH", "") // prove no dependence on a PATH-resolved bd

	var out, errb bytes.Buffer
	code := execRealBd([]string{"version"}, dir, nil, nil, &out, &errb)
	if code != 7 {
		t.Fatalf("execRealBd exit = %d, want 7 (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "args:version") {
		t.Fatalf("execRealBd stdout = %q, want it to contain %q", out.String(), "args:version")
	}
}

// TestClassifyBdShimVerb pins the three-way disposition policy: routed verbs
// always route; provably-graph-free verbs always passthrough; graph-touching
// unrouted verbs passthrough in the identity phase (byte-identical, safe) but
// are refused in the split phase rather than silently bypassing the graph store.
func TestClassifyBdShimVerb(t *testing.T) {
	cases := []struct {
		verb  string
		args  []string
		split bool
		want  bdShimDisposition
	}{
		{"close", []string{"x"}, false, bdRoute},
		{"close", []string{"x"}, true, bdRoute},
		{"show", []string{"x", "--json"}, true, bdRoute},
		{"version", nil, false, bdPassthrough},
		{"version", nil, true, bdPassthrough},
		{"mol", []string{"current", "m"}, false, bdPassthrough}, // identity phase: one backend, byte-identical
		{"mol", []string{"current", "m"}, true, bdRefuse},       // split phase: would silently miss graph beads
		{"gate", []string{"check"}, true, bdRefuse},
		{"query", []string{"ephemeral=true"}, true, bdRefuse},
		// ready: the simple assigned form routes (graph-aware), but predicate
		// flags the Router cannot yet replicate (pool-demand; C3/ga-2gap48.11)
		// passthrough to the work-only bd — byte-identical in the identity phase.
		{"ready", []string{"--assignee=w", "--json", "--limit", "1"}, true, bdRoute},
		{"ready", []string{"--metadata-field", "gc.routed_to=x", "--unassigned", "--json"}, true, bdPassthrough},
		{"ready", []string{"--exclude-type=epic", "--json"}, false, bdPassthrough},
		// update: the cleanly-mappable flag set routes (the canonical graph-worker
		// close), but flags with no UpdateOpts mapping (--notes/--claim/...)
		// passthrough — byte-identical in the identity phase.
		{"update", []string{"x", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, true, bdRoute},
		{"update", []string{"x", "--notes", "done", "--status=closed"}, true, bdPassthrough},
		{"update", []string{"x", "--claim"}, true, bdPassthrough},
		{"reopen", []string{"x"}, true, bdRoute},
		{"delete", []string{"x", "--force"}, true, bdRoute},
	}
	for _, tc := range cases {
		if got := classifyBdShimVerb(tc.verb, tc.args, tc.split); got != tc.want {
			t.Errorf("classifyBdShimVerb(%q, %v, split=%v) = %v, want %v", tc.verb, tc.args, tc.split, got, tc.want)
		}
	}
}

// TestDispatchBdShimCloseRoutesGraphBeadToSQLite proves a routed `bd close`
// lands in the owning backend: a graph bead closes in the embedded SQLite store
// and a work bead closes in the work backend — routed by id through the Router,
// exactly as a worker's `bd close` must behave under graph_store=sqlite.
func TestDispatchBdShimCloseRoutesGraphBeadToSQLite(t *testing.T) {
	// Offset the work MemStore so it occupies a distinct id namespace from the
	// SQLite graph store (both otherwise mint gc-N — see ga-y5pwx3).
	work := beads.NewMemStoreFrom(1000, nil, nil)
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	r := coordrouter.New(work)
	r.Register(coordclass.ClassGraph, graph)

	gb, err := r.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	var out, errb bytes.Buffer
	if code := dispatchBdShimVerb(r, "close", []string{gb.ID}, nil, &out, &errb); code != 0 {
		t.Fatalf("close exit = %d, stderr=%q", code, errb.String())
	}
	stored, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get graph bead from SQLite: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("graph bead status = %q, want closed", stored.Status)
	}

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if code := dispatchBdShimVerb(r, "close", []string{wb.ID}, nil, &out, &errb); code != 0 {
		t.Fatalf("close work exit = %d, stderr=%q", code, errb.String())
	}
	wstored, err := work.Get(wb.ID)
	if err != nil {
		t.Fatalf("re-get work bead: %v", err)
	}
	if wstored.Status != "closed" {
		t.Fatalf("work bead status = %q, want closed", wstored.Status)
	}
}

// newShimRouter builds a Router{work: MemStore(offset), graph: SQLite} for the
// read dispatch tests, mirroring the production wiring under graph_store=sqlite.
func newShimRouter(t *testing.T) *coordrouter.Router {
	t.Helper()
	work := beads.NewMemStoreFrom(1000, nil, nil)
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })
	r := coordrouter.New(work)
	r.Register(coordclass.ClassGraph, graph)
	return r
}

// TestDispatchBdShimShowFederatesGraphBead proves `bd show <id>` returns a graph
// bead resident in SQLite (federated by the Router), shaped as a one-element bd
// JSON array a `bd show ... --json | jq '.[0]'` consumer can read.
func TestDispatchBdShimShowFederatesGraphBead(t *testing.T) {
	r := newShimRouter(t)
	gb, err := r.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	var out, errb bytes.Buffer
	if code := dispatchBdShimVerb(r, "show", []string{gb.ID, "--json"}, nil, &out, &errb); code != 0 {
		t.Fatalf("show exit = %d, stderr=%q", code, errb.String())
	}
	parsed, err := decodeHookClaimBeads(out.String())
	if err != nil {
		t.Fatalf("a bd-show consumer could not parse output %q: %v", out.String(), err)
	}
	if len(parsed) != 1 || parsed[0].ID != gb.ID {
		t.Fatalf("show output = %+v, want one bead %s", parsed, gb.ID)
	}
}

// TestDispatchBdShimShowMissingEmitsEmptyArray proves `bd show <unknown>` emits
// `[]` and exits 0, matching raw bd (whose empty array a consumer reads as "no
// such bead"), rather than erroring.
func TestDispatchBdShimShowMissingEmitsEmptyArray(t *testing.T) {
	r := newShimRouter(t)
	var out, errb bytes.Buffer
	if code := dispatchBdShimVerb(r, "show", []string{"nope-404", "--json"}, nil, &out, &errb); code != 0 {
		t.Fatalf("show missing exit = %d, stderr=%q", code, errb.String())
	}
	parsed, err := decodeHookClaimBeads(out.String())
	if err != nil {
		t.Fatalf("empty show output %q not parseable: %v", out.String(), err)
	}
	if len(parsed) != 0 {
		t.Fatalf("show missing parsed to %d beads, want 0", len(parsed))
	}
}

// TestDispatchBdShimReadyFederatesAssigned proves the routed simple `bd ready
// --assignee` federates the assignee's ready work across the work backend and
// the SQLite graph store — so a worker's assigned-ready probe sees graph steps.
func TestDispatchBdShimReadyFederatesAssigned(t *testing.T) {
	r := newShimRouter(t)
	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}, Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	var out, errb bytes.Buffer
	if code := dispatchBdShimVerb(r, "ready", []string{"--assignee=worker-1", "--json"}, nil, &out, &errb); code != 0 {
		t.Fatalf("ready exit = %d, stderr=%q", code, errb.String())
	}
	parsed, err := decodeHookClaimBeads(out.String())
	if err != nil {
		t.Fatalf("ready output %q not parseable: %v", out.String(), err)
	}
	ids := make(map[string]bool, len(parsed))
	for _, b := range parsed {
		ids[b.ID] = true
	}
	if !ids[wb.ID] || !ids[gb.ID] {
		t.Fatalf("ready output ids = %v, want both %s (work) and %s (graph)", ids, wb.ID, gb.ID)
	}
}

// TestDispatchBdShimUpdateRoutesToOwningBackend proves the canonical graph-worker
// close — `bd update <id> --set-metadata gc.outcome=pass --status closed` —
// routes by id to the owning backend: a graph bead's state change lands in
// SQLite, a work bead's in the work backend.
func TestDispatchBdShimUpdateRoutesToOwningBackend(t *testing.T) {
	r := newShimRouter(t)
	gb, err := r.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	var out, errb bytes.Buffer
	args := []string{gb.ID, "--set-metadata", "gc.outcome=pass", "--status", "closed"}
	if code := dispatchBdShimVerb(r, "update", args, nil, &out, &errb); code != 0 {
		t.Fatalf("update exit = %d, stderr=%q", code, errb.String())
	}
	got, err := r.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get graph bead: %v", err)
	}
	if got.Status != "closed" || got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("graph bead = status %q outcome %q, want closed/pass", got.Status, got.Metadata["gc.outcome"])
	}

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if code := dispatchBdShimVerb(r, "update", []string{wb.ID, "--status=in_progress"}, nil, &out, &errb); code != 0 {
		t.Fatalf("update work exit = %d, stderr=%q", code, errb.String())
	}
	wgot, err := r.Get(wb.ID)
	if err != nil {
		t.Fatalf("re-get work bead: %v", err)
	}
	if wgot.Status != "in_progress" {
		t.Fatalf("work bead status = %q, want in_progress", wgot.Status)
	}
}

// TestDispatchBdShimReopenAndDelete proves `bd reopen` and `bd delete` route by
// id to the owning backend (the embedded SQLite graph store here).
func TestDispatchBdShimReopenAndDelete(t *testing.T) {
	r := newShimRouter(t)
	gb, err := r.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	var out, errb bytes.Buffer
	if code := dispatchBdShimVerb(r, "close", []string{gb.ID}, nil, &out, &errb); code != 0 {
		t.Fatalf("close exit = %d, stderr=%q", code, errb.String())
	}
	if code := dispatchBdShimVerb(r, "reopen", []string{gb.ID}, nil, &out, &errb); code != 0 {
		t.Fatalf("reopen exit = %d, stderr=%q", code, errb.String())
	}
	got, err := r.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get after reopen: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("reopen left status = %q (still closed)", got.Status)
	}
	if code := dispatchBdShimVerb(r, "delete", []string{gb.ID, "--force"}, nil, &out, &errb); code != 0 {
		t.Fatalf("delete exit = %d, stderr=%q", code, errb.String())
	}
	if _, err := r.Get(gb.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("after delete, Get err = %v, want ErrNotFound", err)
	}
}

// TestIsBdShimInvocation pins which argv[0] basenames trigger shim mode: only an
// executable named exactly `bd` (the PATH install symlinks bd -> gc). gc invoked
// normally, or a differently-named helper, must not enter the shim.
func TestIsBdShimInvocation(t *testing.T) {
	cases := map[string]bool{
		"bd":                true,
		"/usr/local/bin/bd": true,
		"./bd":              true,
		"gc":                false,
		"/x/gc":             false,
		"bd-real":           false,
		"/x/bd-shim":        false,
	}
	for arg0, want := range cases {
		if got := isBdShimInvocation(arg0); got != want {
			t.Errorf("isBdShimInvocation(%q) = %v, want %v", arg0, got, want)
		}
	}
}

// TestEnsureRealBdResolvablePrependsGCBDRealDir proves the recursion guard for
// the in-process work BdStore (which execs a bare `bd`, including when the Router
// probes backends by id): the directory of GC_BD_REAL is prepended to PATH so
// `bd` resolves to the real binary, not this shim — and idempotently.
func TestEnsureRealBdResolvablePrependsGCBDRealDir(t *testing.T) {
	dir := t.TempDir()
	realBd := filepath.Join(dir, "bd-real")
	if err := os.WriteFile(realBd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("PATH", "/usr/bin")

	ensureRealBdResolvable()
	want := dir + string(os.PathListSeparator) + "/usr/bin"
	if got := os.Getenv("PATH"); got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	// Idempotent: a second call must not double-prepend.
	ensureRealBdResolvable()
	if got := os.Getenv("PATH"); got != want {
		t.Fatalf("PATH after second call = %q, want %q (no double prepend)", got, want)
	}
}

// TestDispatchBdShimArgv0NonBdIsNotHandled proves gc invoked under its own name
// is left to the normal cobra root (the shim does not hijack it).
func TestDispatchBdShimArgv0NonBdIsNotHandled(t *testing.T) {
	if code, handled := dispatchBdShimArgv0("/x/gc", []string{"version"}, nil, nil, nil); handled {
		t.Fatalf("dispatchBdShimArgv0 handled a non-bd argv0 (code=%d), want not-handled", code)
	}
}

// TestDispatchBdShimArgv0RefusesWithoutGCBDReal proves the misinstall guard: a
// gc symlinked as bd but with no GC_BD_REAL refuses loudly instead of recursing
// (a bare bd lookup would resolve back to the shim).
func TestDispatchBdShimArgv0RefusesWithoutGCBDReal(t *testing.T) {
	t.Setenv("GC_BD_REAL", "")
	var stderr bytes.Buffer
	code, handled := dispatchBdShimArgv0("/agent/bin/bd", []string{"ready"}, nil, nil, &stderr)
	if !handled {
		t.Fatal("dispatchBdShimArgv0 did not handle a bd argv0")
	}
	if code == 0 {
		t.Fatal("expected non-zero exit when GC_BD_REAL is unset, got 0")
	}
	if !strings.Contains(stderr.String(), "GC_BD_REAL") {
		t.Fatalf("stderr = %q, want it to mention GC_BD_REAL", stderr.String())
	}
}

// TestRunBdShimPassthroughProjectsScopeEnv proves runBdShim routes a passthrough
// verb to the real bd — resolved via GC_BD_REAL (no PATH lookup) — in the
// resolved rig scope, with the projected bd env (GC_STORE_* set, BD_EXPORT_AUTO
// suppressed), the same scope/env contract `gc bd` enforces.
func TestRunBdShimPassthroughProjectsScopeEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake bd is POSIX-only")
	}
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	t.Cleanup(func() { cityFlag = origCityFlag; rigFlag = origRigFlag })
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	writeReachableManagedDoltState(t, cityDir) // so bdCommandEnv can project the rig's dolt target
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "repo"
path = "repo"
prefix = "repo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	capture := filepath.Join(t.TempDir(), "shim-bd-env.txt")
	realBd := filepath.Join(t.TempDir(), "bd-real")
	if err := os.WriteFile(realBd, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
  printf 'GC_BEADS_PREFIX=%s\n' "${GC_BEADS_PREFIX:-}"
  printf 'BD_EXPORT_AUTO=%s\n' "${BD_EXPORT_AUTO:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BD_REAL", realBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if code := runBdShim([]string{"--rig", "repo", "version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("runBdShim passthrough = %d, want 0; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture: %v (stderr=%q)", err, stderr.String())
	}
	got := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	if !samePath(got["pwd"], rigDir) {
		t.Fatalf("real bd pwd = %q, want %q", got["pwd"], rigDir)
	}
	if got["args"] != "version" {
		t.Fatalf("real bd args = %q, want %q", got["args"], "version")
	}
	if !samePath(got["GC_STORE_ROOT"], rigDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], rigDir)
	}
	if got["GC_STORE_SCOPE"] != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "rig")
	}
	if got["GC_BEADS_PREFIX"] != "repo" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want %q", got["GC_BEADS_PREFIX"], "repo")
	}
	if got["BD_EXPORT_AUTO"] != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want %q (export suppression)", got["BD_EXPORT_AUTO"], "false")
	}
}
