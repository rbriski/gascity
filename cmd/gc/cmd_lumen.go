package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
	"github.com/spf13/cobra"
)

// lumenEnqueueRequest is the input to the plain-Go enqueue entry: a compiled IR
// file, the default pool route for its do nodes (a pool TEMPLATE name — it must
// match gc.routed_to route matching for both claim and demand), an optional
// JSON-object input, and zero or more convoy bindings that seed input fields from a
// pre-existing convoy's live membership at enqueue.
type lumenEnqueueRequest struct {
	IRPath       string
	Route        string
	InputJSON    string
	InputConvoys []inputConvoyBinding
	// Driver is the run's dispatch discriminator: "" (default) enqueues a pool run the
	// controller loop drives; "self" enqueues a v1 agent-driven run (the controller
	// SKIPS it) and mints ONE ordinary work bead carrying the generic driver-loop prompt,
	// which a single session claims and drives to completion via `gc lumen step/settle`.
	Driver string
}

// lumenEnqueueResult reports the opened run's stream id and the content hash of
// its IR (the CAS blob key). DriverBeadID is set only for a v1 (--v1) run: the id of
// the ordinary work bead the driver-loop agent claims and self-drives.
type lumenEnqueueResult struct {
	StreamID     string
	IRHash       string
	DriverBeadID string
}

// pokeLumenRuns wakes a running controller's Lumen-runs loop via the dedicated
// "lumen-runs" socket verb — the sub-patrol fast path. It is best-effort and a
// package var so tests can observe invocation without a live controller: a missed
// poke (controller down, socket race, an older binary that does not know the verb)
// costs at most one patrol interval, never correctness.
var pokeLumenRuns = func(cityPath string) error {
	const pokeTimeout = 500 * time.Millisecond
	_, err := sendControllerCommandWithTimeouts(cityPath, "lumen-runs", pokeTimeout, pokeTimeout, pokeTimeout)
	return err
}

// lumenEngineEnqueueRun is engine.EnqueueRunWithDriver (the run.started append, with the
// v1 Driver discriminator) behind a package var so a test can assert that BOTH
// content-addressed blobs are already durable at the moment the run becomes discoverable.
// Production always runs the real engine entry. The pool path passes driver "".
var lumenEngineEnqueueRun = engine.EnqueueRunWithDriver

// lumenEnqueue is the plain-Go enqueue entry — the frm-shared seam a peer
// worktree's `gc run` lumen-arm and, later, orders call directly. It:
//
//  1. hard-fails on a city without graph scope OR an opted-but-unopenable journal
//     (loud, no legacy fallback);
//  2. decodes + contract-pins the IR and parses the input;
//  3. writes BOTH CAS blobs — the IR (by ir_hash) and the input (by input_hash) —
//     BEFORE the journal append, so a crash never leaves a discoverable run whose
//     formula or pinned input cannot be loaded (a permanent wedge / phantom run);
//  4. opens the write store (backend-dispatched, sqlite/postgres) and appends
//     run.started via engine.EnqueueRun (which pins those same hashes);
//  5. best-effort pokes the controller's Lumen-runs loop.
func lumenEnqueue(ctx context.Context, cityPath string, req lumenEnqueueRequest, stderr io.Writer) (lumenEnqueueResult, error) {
	if !cityHasGraphScope(cityPath) {
		return lumenEnqueueResult{}, fmt.Errorf("city %q has no graph journal scope (.gc/graph); enable it before enqueuing a lumen run", cityPath)
	}
	// Validate the backend selector up front so a malformed/unsupported marker
	// hard-fails BEFORE any blob is written (no orphan blob, no legacy fallback).
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("resolving graph journal backend: %w", err)
	}

	raw, err := os.ReadFile(req.IRPath)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("reading IR %q: %w", req.IRPath, err)
	}
	doc, err := ir.Decode(raw)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("decoding IR %q: %w", req.IRPath, err)
	}
	input, err := parseLumenInput(req.InputJSON)
	if err != nil {
		return lumenEnqueueResult{}, err
	}
	// Resolve any --input-convoy bindings into seeded input fields BEFORE the blobs
	// are written (the durable-before-seed order is preserved: the seed feeds the
	// input hash the blob is keyed by). A resolution failure — an unresolvable convoy
	// or an inter-member ordering edge — returns here, so no blob and no run.started
	// are written: a broken convoy never becomes a discoverable (or silently-empty)
	// run.
	input, err = seedInputConvoys(cityPath, input, req.InputConvoys, stderr)
	if err != nil {
		return lumenEnqueueResult{}, err
	}

	// Blobs FIRST, run.started SECOND — both content-addressed by the hashes
	// run.started pins. A crash (or a blob-write failure) between the appends and the
	// run.started append leaves at most an orphan blob, never a discoverable run whose
	// IR/input cannot be loaded (which would re-log ErrInputHashMismatch every patrol
	// forever with no way to seal it).
	irHash := engine.IRHash(doc)
	inputHash := engine.InputHash(input)
	if err := writeLumenIRBlob(cityPath, irHash, doc); err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("writing IR blob: %w", err)
	}
	if err := writeLumenInputBlob(cityPath, inputHash, input); err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("writing input blob: %w", err)
	}

	gs, err := backend.openGraphStore(ctx, cityPath)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("opening graph store: %w", err)
	}
	defer func() { _ = gs.Close() }()

	streamID, err := lumenEngineEnqueueRun(ctx, gs, doc, input, req.IRPath, req.Route, req.Driver)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("enqueuing run: %w", err)
	}

	if req.Driver == lumenDriverSelf {
		// v1 agent-driven run: the controller SKIPS it (Driver=="self"), so instead of
		// poking the pool loop we mint ONE ordinary fold_owned=0 work bead carrying the
		// generic driver-loop prompt. A single session claims it through the normal pool
		// path and self-drives the run to completion via `gc lumen step`/`settle`. No
		// gc.continuation_group — one bead, nothing to vacuum.
		//
		// SCOPE (risk-7 per-formula driver choice, follow-up): v1 drives a HOST-LESS-LINEAR
		// formula — a sequence of top-level `do` steps (fanned scatter/gather members drive
		// too, but SERIALIZE). A `do` nested in a retry/guard/for-each/dispatch/timeout/
		// cleanup/recover BODY, or a run sub-formula, is NOT stepper-drivable: the stepper
		// threads no host, so it fails LOUD at the first `gc lumen step` with
		// ErrUnsupportedNode (combine-`do` is already refused at enqueue by the host-less
		// lowering flags). A genuinely branching/parallel formula belongs on the v2 pool
		// driver; the sling does not auto-route by shape yet. EnqueueRunWithDriver's
		// buildUnits(doc, true, false) pre-validation catches un-lowerable IR here; the
		// stepper-drivability check is the deferred follow-up.
		beadID, err := mintLumenV1DriverBead(cityPath, streamID, req.Route, doc.Name)
		if err != nil {
			return lumenEnqueueResult{}, fmt.Errorf("minting v1 driver bead: %w", err)
		}
		// Best-effort: wake the reconcile/demand loop so a session is spawned to claim the
		// born-claimable driver bead promptly; the patrol backstop covers a missed poke.
		if err := pokeReconcile(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc lumen sling --v1: controller poke failed (%v); the run advances on the next patrol tick\n", err) //nolint:errcheck // best-effort stderr
		}
		return lumenEnqueueResult{StreamID: streamID, IRHash: irHash, DriverBeadID: beadID}, nil
	}

	if err := pokeLumenRuns(cityPath); err != nil {
		// Best-effort: the patrol backstop drives the run within one interval.
		fmt.Fprintf(stderr, "gc lumen sling: controller poke failed (%v); the run advances on the next patrol tick\n", err) //nolint:errcheck // best-effort stderr
	}
	return lumenEnqueueResult{StreamID: streamID, IRHash: irHash}, nil
}

// lumenDriverSelf is the run.started Driver value for a v1 agent-driven run — the
// discriminator the controller's lumenRunsTick reads to SKIP the run (the agent owns it).
const lumenDriverSelf = "self"

// pokeReconcile wakes a running controller's reconcile/demand loop so a freshly-minted
// born-claimable bead is picked up promptly. Best-effort (a package var so tests can
// observe it without a live controller): a missed poke costs at most one patrol interval.
var pokeReconcile = func(cityPath string) error {
	_, err := sendControllerCommand(cityPath, "poke")
	return err
}

// openCityWorkStoreForMint opens the city work store the v1 driver bead is minted into,
// behind a package var so a test can inject an in-memory store. Production opens the real
// city store.
var openCityWorkStoreForMint = openCityStoreAt

// mintLumenV1DriverBead creates the single ordinary fold_owned=0 work bead that carries a
// v1 run: type "task", routed to the run's pool template, stamped with the run stream id
// (gc.lumen_run + gc.root_bead_id for correlation) and NO gc.continuation_group (one bead,
// nothing to vacuum), with the generic driver-loop prompt as its description. It is
// claimed and driven by the native pool path — ZERO role names.
func mintLumenV1DriverBead(cityPath, streamID, route, formulaName string) (string, error) {
	store, err := openCityWorkStoreForMint(cityPath)
	if err != nil {
		return "", fmt.Errorf("opening city work store: %w", err)
	}
	created, err := store.Create(beads.Bead{
		Type:        "task",
		Title:       "lumen v1 run: " + formulaName,
		Description: lumenV1DriverPrompt(streamID),
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:   route,
			beadmeta.LumenRunMetadataKey:   streamID,
			beadmeta.RootBeadIDMetadataKey: streamID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating v1 driver bead: %w", err)
	}
	return created.ID, nil
}

// lumenV1DriverPrompt is the generic, role-free driver-loop instruction the v1 run-bead
// carries: run `gc lumen step`, perform the printed work in THIS session, run
// `gc lumen settle`, repeat until "done", then close the bead with the aggregated outcome.
// The run stream id is embedded as RUN=<id> so a scripted worker can parse it off the
// claim JSON description exactly like the EMIT=<token> convention.
func lumenV1DriverPrompt(streamID string) string {
	return fmt.Sprintf(`You hold a Lumen v1 run. RUN=%s

Drive it turn by turn in THIS session — do not wait for anyone:

  1. Run: gc lumen step --run %s --json
     It prints {"done":true,"outcome":"<o>"} when the run is complete, or
     {"done":false,"node":"<id>","prompt":"<work>"} for the next step.
  2. If done, close THIS bead with your aggregated outcome:
       gc bd update <this-bead-id> --set-metadata gc.outcome=<pass|fail> --status closed
     then stop.
  3. Otherwise, perform the printed prompt's work in THIS session, then run:
       gc lumen settle --run %s --node <id> --outcome <pass|fail|degraded> --output "<result>" --json
     It prints the next step (same shape). Repeat from step 1.
`, streamID, streamID, streamID)
}

// parseLumenInput parses the --input JSON object (empty ⇒ nil, an unpinned run).
func parseLumenInput(inputJSON string) (map[string]any, error) {
	if strings.TrimSpace(inputJSON) == "" {
		return nil, nil
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return nil, fmt.Errorf("parsing --input as a JSON object: %w", err)
	}
	return input, nil
}

// newLumenCmd registers the hidden `gc lumen` command tree. `gc lumen sling` is a
// thin cobra wrapper over lumenEnqueue — deliberately NOT `gc sling` (that is the
// v2 molecule path). Hidden until the L3 e2e demonstrates it.
func newLumenCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "lumen",
		Short:  "Lumen graph-run operations (enqueue a run for the controller loop to drive)",
		Hidden: true,
	}
	cmd.AddCommand(newLumenSlingCmd(stdout, stderr))
	cmd.AddCommand(newLumenStepCmd(stdout, stderr))
	cmd.AddCommand(newLumenSettleCmd(stdout, stderr))
	return cmd
}

func newLumenSlingCmd(stdout, stderr io.Writer) *cobra.Command {
	var inputJSON string
	var inputConvoySpecs []string
	var v1 bool
	c := &cobra.Command{
		Use:   "sling <route> <formula.lumen.json>",
		Short: "Enqueue a Lumen run: the controller loop drives it, dispatching do work as ordinary pool beads",
		Long: "Enqueue a compiled Lumen formula as a run on the city's graph journal. " +
			"The <route> is the default pool template for the formula's do nodes. The " +
			"controller loop discovers the run, dispatches each ready do as an ordinary " +
			"work bead in the city work store (claimed and closed through the normal pool " +
			"path), and drives the DAG to run.closed as those beads settle.\n\n" +
			"The compiled IR and input are copied into the content-addressed run dir " +
			"(.gc/graph/ir, .gc/graph/runs) so the run survives a controller restart; " +
			"deleting a blob afterward is a loud per-tick refusal until it is re-placed " +
			"(any byte-identical copy works — the IR blob is content-addressed).\n\n" +
			"Use --input-convoy <field>=<convoyID> to seed a run input field from a " +
			"pre-existing convoy's live membership: the convoy is resolved to a " +
			"canonically-sorted member-id array at enqueue and the formula fans one " +
			"sub-graph per id via `for-each over: input.<field>`. The membership is " +
			"frozen into the run's input at enqueue (the fold never re-reads the convoy).",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc lumen sling: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			bindings := make([]inputConvoyBinding, 0, len(inputConvoySpecs))
			for _, spec := range inputConvoySpecs {
				b, err := parseInputConvoyFlag(spec)
				if err != nil {
					fmt.Fprintf(stderr, "gc lumen sling: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				bindings = append(bindings, b)
			}
			driver := ""
			if v1 {
				driver = lumenDriverSelf
			}
			res, err := lumenEnqueue(context.Background(), cityPath, lumenEnqueueRequest{
				Route:        args[0],
				IRPath:       args[1],
				InputJSON:    inputJSON,
				InputConvoys: bindings,
				Driver:       driver,
			}, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc lumen sling: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if res.DriverBeadID != "" {
				fmt.Fprintf(stdout, "enqueued lumen v1 run %s (ir %s, driver bead %s)\n", res.StreamID, res.IRHash, res.DriverBeadID) //nolint:errcheck // best-effort stdout
				return nil
			}
			fmt.Fprintf(stdout, "enqueued lumen run %s (ir %s)\n", res.StreamID, res.IRHash) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	c.Flags().StringVar(&inputJSON, "input", "", "run input as a JSON object")
	c.Flags().StringArrayVar(&inputConvoySpecs, "input-convoy", nil, "seed a run input field from a pre-existing convoy's members: <field>=<convoyID> (repeatable)")
	c.Flags().BoolVar(&v1, "v1", false, "enqueue a v1 agent-driven run: mint one work bead the agent self-drives via `gc lumen step`/`settle` (the controller does not drive it). For host-less-linear formulas (a sequence of top-level do steps); a do nested in a loop/guard/for-each/dispatch/run body is not stepper-drivable and fails at drive time — route those to the pool driver")
	return c
}
