package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
	"github.com/spf13/cobra"
)

// lumenEnqueueRequest is the input to the plain-Go enqueue entry: a compiled IR
// file, the default pool route for its do nodes (a pool TEMPLATE name — it must
// match gc.routed_to route matching for both claim and demand), and an optional
// JSON-object input.
type lumenEnqueueRequest struct {
	IRPath    string
	Route     string
	InputJSON string
}

// lumenEnqueueResult reports the opened run's stream id and the content hash of
// its IR (the CAS blob key).
type lumenEnqueueResult struct {
	StreamID string
	IRHash   string
}

// pokeLumenRuns wakes a running controller's Lumen-runs loop via the dedicated
// "lumen-runs" socket verb — the sub-patrol fast path. It is best-effort and a
// package var so tests can observe invocation without a live controller: a missed
// poke (controller down, socket race, an older binary that does not know the verb)
// costs at most one patrol interval, never correctness.
var pokeLumenRuns = func(cityPath string) error {
	_, err := sendControllerCommand(cityPath, "lumen-runs")
	return err
}

// lumenEngineEnqueueRun is engine.EnqueueRun (the run.started append) behind a
// package var so a test can assert that BOTH content-addressed blobs are already
// durable at the moment the run becomes discoverable. Production always runs the
// real engine entry.
var lumenEngineEnqueueRun = engine.EnqueueRun

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

	streamID, err := lumenEngineEnqueueRun(ctx, gs, doc, input, req.IRPath, req.Route)
	if err != nil {
		return lumenEnqueueResult{}, fmt.Errorf("enqueuing run: %w", err)
	}
	if err := pokeLumenRuns(cityPath); err != nil {
		// Best-effort: the patrol backstop drives the run within one interval.
		fmt.Fprintf(stderr, "gc lumen sling: controller poke failed (%v); the run advances on the next patrol tick\n", err) //nolint:errcheck // best-effort stderr
	}
	return lumenEnqueueResult{StreamID: streamID, IRHash: irHash}, nil
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
	return cmd
}

func newLumenSlingCmd(stdout, stderr io.Writer) *cobra.Command {
	var inputJSON string
	c := &cobra.Command{
		Use:   "sling <route> <formula.lumen.json>",
		Short: "Enqueue a Lumen run: the controller loop drives it, materializing do work as pool-claimable beads",
		Long: "Enqueue a compiled Lumen formula as a run on the city's graph journal. " +
			"The <route> is the default pool template for the formula's do nodes. The " +
			"controller loop discovers the run, materializes each ready do as a claimable " +
			"Tier-B work bead, and drives the DAG to run.closed as workers settle it.\n\n" +
			"The compiled IR and input are copied into the content-addressed run dir " +
			"(.gc/graph/ir, .gc/graph/runs) so the run survives a controller restart; " +
			"deleting a blob afterward is a loud per-tick refusal until it is re-placed " +
			"(any byte-identical copy works — the IR blob is content-addressed).",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc lumen sling: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			res, err := lumenEnqueue(context.Background(), cityPath, lumenEnqueueRequest{
				Route:     args[0],
				IRPath:    args[1],
				InputJSON: inputJSON,
			}, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc lumen sling: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			fmt.Fprintf(stdout, "enqueued lumen run %s (ir %s)\n", res.StreamID, res.IRHash) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	c.Flags().StringVar(&inputJSON, "input", "", "run input as a JSON object")
	return c
}
