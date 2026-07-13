package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// lumenSeedV1Run enqueues a v1 (Driver="self") run — CAS blobs first, then run.started
// stamped Driver="self" — and returns its stream id, as `gc lumen sling --v1` does.
func lumenSeedV1Run(t *testing.T, cityPath string, doc *ir.IR, input map[string]any, route string) string {
	t.Helper()
	if err := writeLumenIRBlob(cityPath, engine.IRHash(doc), doc); err != nil {
		t.Fatalf("write IR blob: %v", err)
	}
	if err := writeLumenInputBlob(cityPath, engine.InputHash(input), input); err != nil {
		t.Fatalf("write input blob: %v", err)
	}
	gs := tbHookOpenStore(t, cityPath)
	streamID, err := engine.EnqueueRunWithDriver(context.Background(), gs, doc, input, "test/formula@v1", route, "self")
	if err != nil {
		_ = gs.Close()
		t.Fatalf("enqueue v1: %v", err)
	}
	_ = gs.Close()
	return streamID
}

// TestLumenRunsTickSkipsV1Run is the controller-skip pin (mutation (i)): with a POOL run
// and a v1 (Driver="self") run both open, one controller tick advances ONLY the pool run
// and leaves the v1 run entirely untouched (its journal stays at run.started). Removing
// the Driver=="self" skip in advanceLumenRun makes the pool loop advance the v1 run too,
// reddening this pin.
func TestLumenRunsTickSkipsV1Run(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)

	poolStream := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	v1Stream := lumenSeedV1Run(t, cityPath, tbHookDoc(t), nil, "")

	var advanced []string
	orig := lumenAdvance
	lumenAdvance = func(c context.Context, store *graphstore.Store, doc *ir.IR, sid string, input map[string]any, opts engine.Options) (engine.AdvanceResult, error) {
		advanced = append(advanced, sid)
		return orig(c, store, doc, sid, input, opts)
	}
	defer func() { lumenAdvance = orig }()

	cr.lumenRunsTick(ctx)

	// The pool run was advanced; the v1 run was NOT.
	if !slices.Contains(advanced, poolStream) {
		t.Fatalf("pool run %q was not advanced (advanced: %v)", poolStream, advanced)
	}
	if slices.Contains(advanced, v1Stream) {
		t.Fatalf("v1 run %q was advanced by the controller — it must be skipped (Driver=self)", v1Stream)
	}

	// The v1 journal is untouched: still exactly run.started (the controller drove nothing).
	types := lumenStreamEventTypes(t, cityPath, v1Stream)
	if len(types) != 1 || types[0] != engine.EventRunStarted {
		t.Fatalf("v1 journal = %v, want [run.started] (controller must not drive a v1 run)", types)
	}
}

// TestV1RunCompletesWithoutController is the SDK-self-sufficiency pin: a v1 run seals with
// NO controller advancing it and NO [[agent]] role — the agent-driven stepper (engine.Step
// / engine.Settle) alone drives it to run.closed. This is the same drive an orphan-release
// + re-claim performs, so a dead agent's run finishes when a fresh session re-steps it.
func TestV1RunCompletesWithoutController(t *testing.T) {
	ctx := context.Background()
	_, cityPath, _ := lumenTestRuntime(t)
	doc := tbHookDoc(t)
	v1Stream := lumenSeedV1Run(t, cityPath, doc, nil, "")

	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()

	// Turn 1: step offers the single do; the agent performs it and settles it.
	step, err := engine.Step(ctx, gs, doc, v1Stream, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if step.Done || step.NodeID != "hello" {
		t.Fatalf("step = %+v, want the ready do hello", step)
	}
	final, err := engine.Settle(ctx, gs, doc, v1Stream, nil, step.NodeID, "pass", "hi", engine.Options{})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !final.Done || final.Outcome != engine.OutcomePass {
		t.Fatalf("settle = %+v, want Done pass (run sealed by the agent alone)", final)
	}
	if err := gs.Verify(ctx, v1Stream); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// runLumenStepperJSON drives a `gc lumen {step,settle} … --json` invocation through the
// FULL gc front-door (run → the `--json` contract gate → the command), asserts a clean exit,
// and returns the single JSON result line. This is the CLI surface the in-process
// engine.Step/Settle pins never exercise: the front-door intercepts a bare `--json` BEFORE
// cobra and refuses any command with no registered result schema, so a stepper verb missing
// schemas/lumen/{step,settle}/result.schema.json fails json_unsupported here (exit 1) no
// matter how correct engine.Step is.
func runLumenStepperJSON(t *testing.T, cityPath string, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--city", cityPath}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(%v) = %d (want 0); stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
	}
	return bytes.TrimSpace(stdout.Bytes())
}

// TestLumenStepperJSONThroughFrontDoor is the CLI-path pin closing the exact gap the
// in-process engine.Step/Settle unit tests leave open: it drives the v1 stepper's two verbs
// THROUGH the gc `--json` front-door and validates each result against its registered schema.
// Before schemas/lumen/{step,settle}/result.schema.json existed, the front-door rejected
// `gc lumen step … --json` with json_unsupported (exit 1); the driver's
// `step="$(gc lumen step … --json)"` then died under `set -euo pipefail`, and the run stalled
// at run.started with no node.activated — invisible to the dolt e2e because the failure landed
// in the driver session's log, not the test output. Deleting either schema file (or dropping
// the SchemaVersion/OK/Command discriminators) reddens this pin at the CLI layer, no dolt city
// required.
func TestLumenStepperJSONThroughFrontDoor(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityPath := tbHookGraphCity(t)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"lumen-stepper\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	// A two-do chain (A → B "use {{A}}") so the CLI path also proves value plumbing: B's
	// rendered prompt must interpolate A's settle --output.
	doc := tbHookChainDoc(t)
	stream := lumenSeedV1Run(t, cityPath, doc, nil, "")

	// Turn 1: `gc lumen step --json` offers do A. The result must clear the front-door
	// (exit 0), validate against schemas/lumen/step/result.schema.json, and carry ok:true.
	stepOut := runLumenStepperJSON(t, cityPath, "lumen", "step", "--run", stream, "--json")
	validateJSONAgainstResultSchema(t, []string{"lumen", "step"}, stepOut)
	assertTopLevelOKTrue(t, stepOut)
	var step lumenStepResultJSON
	if err := json.Unmarshal(stepOut, &step); err != nil {
		t.Fatalf("decode step result: %v\n%s", err, stepOut)
	}
	if step.Command != "lumen step" {
		t.Fatalf("step result command = %q, want %q", step.Command, "lumen step")
	}
	if step.Done || step.Node != "A" {
		t.Fatalf("step = %+v, want the ready do A (done=false)", step)
	}

	// Turn 2: `gc lumen settle --json` records A pass with output "hi" and fuses the next
	// step — do B, whose prompt must interpolate "hi" (value plumbs over the CLI settle path).
	settleA := runLumenStepperJSON(t, cityPath, "lumen", "settle", "--run", stream, "--node", "A", "--outcome", "pass", "--output", "hi", "--json")
	validateJSONAgainstResultSchema(t, []string{"lumen", "settle"}, settleA)
	assertTopLevelOKTrue(t, settleA)
	var stepB lumenStepResultJSON
	if err := json.Unmarshal(settleA, &stepB); err != nil {
		t.Fatalf("decode settle A result: %v\n%s", err, settleA)
	}
	if stepB.Command != "lumen settle" {
		t.Fatalf("settle result command = %q, want %q", stepB.Command, "lumen settle")
	}
	if stepB.Done || stepB.Node != "B" {
		t.Fatalf("settle A = %+v, want the next ready do B", stepB)
	}
	if !strings.Contains(stepB.Prompt, "hi") {
		t.Fatalf("do B prompt = %q, want it to interpolate A's output %q", stepB.Prompt, "hi")
	}

	// Turn 3: settle B pass — the run seals. The done result validates and reports pass.
	settleB := runLumenStepperJSON(t, cityPath, "lumen", "settle", "--run", stream, "--node", "B", "--outcome", "pass", "--output", "done", "--json")
	validateJSONAgainstResultSchema(t, []string{"lumen", "settle"}, settleB)
	assertTopLevelOKTrue(t, settleB)
	var final lumenStepResultJSON
	if err := json.Unmarshal(settleB, &final); err != nil {
		t.Fatalf("decode settle B result: %v\n%s", err, settleB)
	}
	if !final.Done || final.Outcome != engine.OutcomePass {
		t.Fatalf("settle B = %+v, want Done pass (run sealed by the agent alone)", final)
	}
}

// TestV1SlingMintsDriverBead proves `gc lumen sling --v1` mints ONE ordinary fold_owned=0
// work bead carrying the generic driver-loop prompt: task-typed, routed, stamped with the
// run stream id (gc.lumen_run + gc.root_bead_id) and NO gc.continuation_group.
func TestV1SlingMintsDriverBead(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := lumenWriteIRFile(t, cityPath)

	// Inject an in-memory work store for the mint and stub the reconcile poke.
	mem := beads.NewMemStore()
	origOpen := openCityWorkStoreForMint
	openCityWorkStoreForMint = func(string) (beads.Store, error) { return mem, nil }
	defer func() { openCityWorkStoreForMint = origOpen }()
	origPoke := pokeReconcile
	pokeReconcile = func(string) error { return nil }
	defer func() { pokeReconcile = origPoke }()

	var stderr bytes.Buffer
	res, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath: irPath,
		Route:  tbHookRoute,
		Driver: "self",
	}, &stderr)
	if err != nil {
		t.Fatalf("lumenEnqueue v1: %v (stderr: %s)", err, stderr.String())
	}
	if res.DriverBeadID == "" {
		t.Fatalf("v1 sling returned no driver bead id")
	}

	rows, err := mem.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list work store: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("work beads = %d, want exactly 1 (one v1 driver bead)", len(rows))
	}
	b := rows[0]
	if b.Type != "task" {
		t.Errorf("bead type = %q, want task", b.Type)
	}
	if b.Metadata[beadmeta.LumenRunMetadataKey] != res.StreamID {
		t.Errorf("gc.lumen_run = %q, want %q", b.Metadata[beadmeta.LumenRunMetadataKey], res.StreamID)
	}
	if b.Metadata[beadmeta.RootBeadIDMetadataKey] != res.StreamID {
		t.Errorf("gc.root_bead_id = %q, want %q", b.Metadata[beadmeta.RootBeadIDMetadataKey], res.StreamID)
	}
	if b.Metadata[beadmeta.RoutedToMetadataKey] != tbHookRoute {
		t.Errorf("gc.routed_to = %q, want %q", b.Metadata[beadmeta.RoutedToMetadataKey], tbHookRoute)
	}
	if _, ok := b.Metadata[beadmeta.ContinuationGroupMetadataKey]; ok {
		t.Errorf("v1 driver bead carries gc.continuation_group — it must not (one bead, nothing to vacuum)")
	}
	if b.Description == "" || !strings.Contains(b.Description, res.StreamID) {
		t.Errorf("driver bead description must embed RUN=%s; got %q", res.StreamID, b.Description)
	}
}

// TestV1SlingStampsDriverSelf is the fast unit pin (P3-B) for the run.started Driver stamp:
// `gc lumen sling --v1` must seed run.started with Driver="self" so the controller's
// lumenRunsTick skips the run. A mutant hardcoding Driver="" in the enqueue call (while
// still minting the bead) would leave the run pool-driven and wedged on ErrNoPoolRoute; the
// manifest read reds it here without needing the integration dolt e2e.
func TestV1SlingStampsDriverSelf(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := lumenWriteIRFile(t, cityPath)

	origOpen := openCityWorkStoreForMint
	openCityWorkStoreForMint = func(string) (beads.Store, error) { return beads.NewMemStore(), nil }
	defer func() { openCityWorkStoreForMint = origOpen }()
	origPoke := pokeReconcile
	pokeReconcile = func(string) error { return nil }
	defer func() { pokeReconcile = origPoke }()

	var stderr bytes.Buffer
	res, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath: irPath,
		Route:  tbHookRoute,
		Driver: "self",
	}, &stderr)
	if err != nil {
		t.Fatalf("lumenEnqueue v1: %v (stderr: %s)", err, stderr.String())
	}

	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	m, err := engine.ReadRunManifest(ctx, gs, res.StreamID)
	if err != nil {
		t.Fatalf("ReadRunManifest: %v", err)
	}
	if m.Driver != "self" {
		t.Fatalf("run.started Driver = %q, want %q (the v1 sling must stamp it so the controller skips the run)", m.Driver, "self")
	}
}
