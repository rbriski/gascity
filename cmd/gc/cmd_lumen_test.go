package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// lumenWriteIRFile marshals a decoded do-only IR to hello.lumen.json in cityPath
// and returns its path.
func lumenWriteIRFile(t *testing.T, cityPath string) string {
	t.Helper()
	raw, err := json.Marshal(tbHookDoc(t))
	if err != nil {
		t.Fatalf("marshal IR: %v", err)
	}
	path := filepath.Join(cityPath, "hello.lumen.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write IR file: %v", err)
	}
	return path
}

// stubPokeLumenRuns replaces the poke seam with a recorder for the duration of a
// test, returning a pointer to the invocation count.
func stubPokeLumenRuns(t *testing.T) *int {
	t.Helper()
	var calls int
	orig := pokeLumenRuns
	pokeLumenRuns = func(string) error { calls++; return nil }
	t.Cleanup(func() { pokeLumenRuns = orig })
	return &calls
}

// TestLumenEnqueueWritesCASThenJournal (T-C1) proves lumenEnqueue writes the IR
// blob (content-addressed) and the input blob (under runs/<stream>/), seeds
// run.started (head==1), invokes the poke seam, and reuses the IR blob on a
// re-enqueue of the same formula.
func TestLumenEnqueueWritesCASThenJournal(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := lumenWriteIRFile(t, cityPath)
	pokes := stubPokeLumenRuns(t)

	var stderr bytes.Buffer
	res, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath:    irPath,
		Route:     tbHookRoute,
		InputJSON: `{"topic":"gears"}`,
	}, &stderr)
	if err != nil {
		t.Fatalf("lumenEnqueue: %v; stderr=%s", err, stderr.String())
	}
	if res.StreamID == "" || res.IRHash == "" {
		t.Fatalf("result = %+v, want non-empty StreamID and IRHash", res)
	}

	// CAS blobs present (both content-addressed by the hashes run.started pins).
	if _, err := os.Stat(lumenIRBlobPath(cityPath, res.IRHash)); err != nil {
		t.Fatalf("IR blob missing: %v", err)
	}
	if _, err := os.Stat(lumenInputBlobPath(cityPath, engine.InputHash(map[string]any{"topic": "gears"}))); err != nil {
		t.Fatalf("input blob missing: %v", err)
	}

	// Head == 1 (run.started only).
	gs := tbHookOpenStore(t, cityPath)
	head, err := gs.Head(ctx, res.StreamID)
	_ = gs.Close()
	if err != nil || head != 1 {
		t.Fatalf("head = %d, err %v; want 1", head, err)
	}
	if *pokes != 1 {
		t.Fatalf("poke seam invoked %d times, want 1", *pokes)
	}

	// Re-enqueue the SAME formula: the content-addressed IR blob is reused (one file
	// in ir/), and a distinct nonce stream opens.
	res2, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{IRPath: irPath, Route: tbHookRoute}, &stderr)
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if res2.IRHash != res.IRHash {
		t.Fatalf("re-enqueue IRHash %q != %q (same formula must hash identically)", res2.IRHash, res.IRHash)
	}
	if res2.StreamID == res.StreamID {
		t.Fatalf("re-enqueue reused the stream id %q (no nonce)", res2.StreamID)
	}
	entries, err := os.ReadDir(filepath.Join(cityPath, ".gc", "graph", "ir"))
	if err != nil {
		t.Fatalf("read ir dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ir/ has %d blobs, want 1 (content-addressed reuse)", len(entries))
	}
}

// TestLumenEnqueueWritesBothBlobsBeforeRunStarted is the F1 regression pin: BOTH
// content-addressed blobs (IR and input) must be durable at the instant run.started
// is appended (the instant the run becomes discoverable by ListOpenRuns). A run
// discoverable before its pinned input blob exists is a permanent wedge — every
// patrol reloads it, Advance's rebuild hits ErrInputHashMismatch (not retryable),
// and the run can never seal, re-logging forever with no way to cancel it. The seam
// captures blob presence at the exact moment the run.started append fires.
func TestLumenEnqueueWritesBothBlobsBeforeRunStarted(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := lumenWriteIRFile(t, cityPath)
	stubPokeLumenRuns(t)

	var irDurable, inputDurable bool
	orig := lumenEngineEnqueueRun
	lumenEngineEnqueueRun = func(ctx context.Context, gs *graphstore.Store, doc *ir.IR, in map[string]any, formulaRef, defaultRoute string) (string, error) {
		// At the moment run.started is appended, both blobs MUST already be on disk.
		_, irErr := os.Stat(lumenIRBlobPath(cityPath, engine.IRHash(doc)))
		_, inErr := os.Stat(lumenInputBlobPath(cityPath, engine.InputHash(in)))
		irDurable = irErr == nil
		inputDurable = inErr == nil
		return orig(ctx, gs, doc, in, formulaRef, defaultRoute)
	}
	defer func() { lumenEngineEnqueueRun = orig }()

	var stderr bytes.Buffer
	if _, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath:    irPath,
		Route:     tbHookRoute,
		InputJSON: `{"topic":"gears"}`,
	}, &stderr); err != nil {
		t.Fatalf("lumenEnqueue: %v; stderr=%s", err, stderr.String())
	}
	if !irDurable {
		t.Fatal("IR blob was not durable when run.started was appended")
	}
	if !inputDurable {
		t.Fatal("input blob was not durable when run.started was appended (F1: a pinned-input run becomes discoverable before its input blob → permanent wedge)")
	}
}

// TestLumenEnqueueHardFailsOutsideGraphScope (T-C2) proves enqueue hard-fails
// loudly with nothing written when the city has no graph scope, and when the scope
// is present but the journal backend is unopenable (a malformed marker) — no legacy
// fallback.
func TestLumenEnqueueHardFailsOutsideGraphScope(t *testing.T) {
	ctx := context.Background()

	// (a) No graph scope.
	noScope := t.TempDir()
	irPath := lumenWriteIRFile(t, noScope)
	var stderr bytes.Buffer
	if _, err := lumenEnqueue(ctx, noScope, lumenEnqueueRequest{IRPath: irPath, Route: "workers"}, &stderr); err == nil {
		t.Fatal("lumenEnqueue accepted a city with no graph scope; want a loud error")
	}
	if _, err := os.Stat(filepath.Join(noScope, ".gc", "graph", "ir")); !os.IsNotExist(err) {
		t.Fatalf("no-scope enqueue wrote a blob dir (want nothing written): stat err=%v", err)
	}

	// (b) Opted-but-unopenable: a malformed backend marker.
	badCity := t.TempDir()
	graphBeads := filepath.Join(badCity, ".gc", "graph", ".beads")
	if err := os.MkdirAll(graphBeads, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphBeads, "config.yaml"), []byte("backend: bogus\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	badIR := lumenWriteIRFile(t, badCity)
	if _, err := lumenEnqueue(ctx, badCity, lumenEnqueueRequest{IRPath: badIR, Route: "workers"}, &stderr); err == nil {
		t.Fatal("lumenEnqueue accepted an unopenable journal; want a loud error (no legacy fallback)")
	}
	if _, err := os.Stat(filepath.Join(badCity, ".gc", "graph", "ir")); !os.IsNotExist(err) {
		t.Fatalf("unopenable-journal enqueue wrote a blob dir (want nothing written): stat err=%v", err)
	}
}

// TestLumenSlingCobraSurface (T-C3) drives the cobra wrapper end-to-end: a valid
// sling enqueues a run; a bad IR surfaces the ir.Decode error verbatim and exits
// non-zero.
func TestLumenSlingCobraSurface(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	irPath := lumenWriteIRFile(t, cityPath)
	stubPokeLumenRuns(t)
	t.Chdir(cityPath)

	// Valid sling.
	var stdout, stderr bytes.Buffer
	cmd := newLumenCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"sling", tbHookRoute, irPath, "--input", `{"topic":"x"}`})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc lumen sling: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gcg-run-") {
		t.Fatalf("stdout did not report the enqueued run: %s", stdout.String())
	}

	// Bad IR: ir.Decode error, non-zero exit.
	badPath := filepath.Join(cityPath, "bad.lumen.json")
	if err := os.WriteFile(badPath, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("write bad IR: %v", err)
	}
	var stdout2, stderr2 bytes.Buffer
	cmd2 := newLumenCmd(&stdout2, &stderr2)
	cmd2.SetArgs([]string{"sling", tbHookRoute, badPath})
	cmd2.SetOut(&stdout2)
	cmd2.SetErr(&stderr2)
	if err := cmd2.Execute(); err == nil {
		t.Fatal("gc lumen sling accepted a malformed IR; want a non-zero exit")
	}
	if !strings.Contains(stderr2.String(), badPath) {
		t.Fatalf("stderr did not name the bad IR path %q: %s", badPath, stderr2.String())
	}
}

// TestLumenEnqueueEngineOnlyStamps confirms the engine-only default-route-empty
// path enqueues cleanly (no pool route required at enqueue; only do execution
// needs a route).
func TestLumenEnqueueEngineOnlyStamps(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	stubPokeLumenRuns(t)
	// An engine-only exec IR file.
	raw, err := json.Marshal(lumenExecDoc(t, "econly"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	irPath := filepath.Join(cityPath, "exec.lumen.json")
	if err := os.WriteFile(irPath, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stderr bytes.Buffer
	res, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{IRPath: irPath}, &stderr)
	if err != nil {
		t.Fatalf("lumenEnqueue engine-only: %v", err)
	}
	m, err := engine.ReadRunManifest(ctx, tbHookOpenStore(t, cityPath), res.StreamID)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.DefaultRoute != "" {
		t.Fatalf("default_route = %q, want empty (engine-only)", m.DefaultRoute)
	}
	if m.FormulaRef != irPath {
		t.Fatalf("formula_ref = %q, want the IR path %q", m.FormulaRef, irPath)
	}
}
