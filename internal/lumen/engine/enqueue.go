package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// EnqueueRun opens a fresh run on its own nonce stream and seeds run.started so a
// controller loop can discover it (ListOpenRuns), reload its inputs by the stamped
// provenance (ReadRunManifest), and drive it (Advance's rebuild path). It is the
// exported entry the in-city enqueue (gc lumen sling) and, later, orders build on;
// the loop never seeds a run itself.
//
// It derives a nonce stream id from the formula name (streamIDForRun with a
// per-run nonce, so repeated enqueues of one formula never contend on a single
// stream and the id never contains the ':' activation-key delimiter Advance
// refuses), acquires the writer lease so run.started carries a LIVE fencing epoch
// (never the permanently-fenced 0), and appends run.started under the SAME idem
// token Advance's fresh-seed path uses (streamID+":run:started"). It stamps the IR
// hash, the input hash (empty input → unpinned), the formula ref, and the default
// pool route. On return Head==1, so every later Advance takes the rebuild path and
// the fresh-seed path — which stamps neither formula_ref nor default_route — is
// unreachable for an enqueued run.
//
// The caller is responsible for making the IR and input durable (the CAS blobs
// keyed by the returned/stamped hashes) BEFORE relying on the run being drivable:
// the journal pins the hashes but Advance takes the doc and input as arguments.
func EnqueueRun(ctx context.Context, store *graphstore.Store, doc *ir.IR, input map[string]any, formulaRef, defaultRoute string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("lumen: enqueue: nil store")
	}
	if doc == nil {
		return "", fmt.Errorf("lumen: enqueue: nil IR document")
	}
	// Pre-validate lowering BEFORE any side effect (REDESIGN §6, the L6 enqueue-wedge
	// HIGH). EnqueueRun otherwise seeds run.started without ever lowering the IR;
	// buildUnits runs first inside the controller's Advance, so an un-lowerable IR
	// (unsupported node kind, malformed loop body, dangling after, a loop nested under
	// a scatter) would error there and the run root would stay open forever, re-logged
	// every tick — a silently wedged, unsealable run. The flags mirror EXACTLY what the
	// controller loop drives with (Advance computes buildUnits(nodes, PoolRouter!=nil,
	// Host!=nil); the loop passes Options{PoolRouter:…} with nil Host ⇒ (true, false)),
	// so an IR that lowers here is one Advance can drive. Failing here fails the enqueue
	// LOUD at the CLI with no discoverable run.
	if _, err := buildUnits(doc, true, false); err != nil {
		return "", fmt.Errorf("lumen: enqueue: IR does not lower: %w", err)
	}
	// ⚑B2 (ga-ospbql): refuse a required-unbound input LOUDLY beside the lowering
	// gate, before any stream exists — no discoverable run is minted (an
	// invalid_input settle would journal-litter a dead run the controller loop
	// re-logs forever). Enqueue threads input ONLY to the raw inputHash stamp below
	// — no seeding here, ever (⚑B1: the controller's Advance seeds at drive time).
	if _, err := resolveDeclaredInput(doc.Input.Fields, input); err != nil {
		return "", fmt.Errorf("lumen: enqueue: %w", err)
	}

	streamID := streamIDForRun(doc.Name, true)
	if strings.ContainsRune(streamID, ':') {
		// Defensive: streamIDForRun never emits ':' (hash + hex nonce), but the run
		// root id must not carry the activation-key delimiter (Advance refuses it).
		return "", fmt.Errorf("lumen: enqueue: derived stream id %q must not contain ':'", streamID)
	}
	RegisterVocabulary(store)

	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return "", fmt.Errorf("lumen: enqueue: acquire writer lease %q: %w", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return "", fmt.Errorf("lumen: enqueue: reading stream head %q: %w", streamID, err)
	}
	if head != 0 {
		// A nonce collision (astronomically unlikely) or a re-enqueue of the same
		// stream: refuse loudly rather than fold a second run.started onto a live run.
		return "", fmt.Errorf("lumen: enqueue: stream %q already exists (head %d) — refusing to re-seed", streamID, head)
	}

	reducer := lumenReducer{}
	d := &driver{
		ctx:      ctx,
		store:    store,
		streamID: streamID,
		irVer:    doc.Contract.Version,
		epoch:    lease.Epoch,
		reducer:  reducer,
		state:    reducer.Zero(streamID),
	}
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := d.append(EventRunStarted, streamID+":run:started", runStartedPayload{
		RootID:       streamID,
		Name:         doc.Name,
		IRHash:       irHash(doc),
		InputHash:    inputHash(input),
		FormulaRef:   formulaRef,
		DefaultRoute: defaultRoute,
		CreatedAt:    createdAt,
	}); err != nil {
		return "", fmt.Errorf("lumen: enqueue: seed run.started %q: %w", streamID, err)
	}
	return streamID, nil
}

// IRHash returns the content hash EnqueueRun stamps on run.started for doc — the
// key a composition-root CAS dir names the IR blob by. Exposing it lets the
// enqueue write the blob under the SAME hash the run pins (so a later Advance's
// rebuild guard resolves the blob it stored) without re-implementing the hash in
// cmd/gc.
func IRHash(doc *ir.IR) string { return irHash(doc) }

// InputHash returns the content hash EnqueueRun stamps on run.started for the run
// input — the key a composition-root CAS dir names the input blob by. Exposing it
// lets the enqueue write the input blob content-addressed (under the SAME hash the
// run pins) BEFORE run.started is appended, and lets the controller loop reload the
// input by the manifest's InputHash across a restart. An empty (unpinned) input
// hashes to "" — no blob, and Resume imposes no input constraint.
func InputHash(input map[string]any) string { return inputHash(input) }

// RunManifest is the provenance a run.started pins: the controller loop loads the
// IR/input blobs by these hashes and routes the run's do work by DefaultRoute.
type RunManifest struct {
	Name         string
	IRHash       string
	InputHash    string
	FormulaRef   string
	DefaultRoute string
	CreatedAt    string
}

// ReadRunManifest decodes the run.started at seq 1 of streamID into a RunManifest.
// It is how the controller loop recovers a run's inputs and route across a restart:
// the journal is the single source of truth, and run.started is always the first
// event (EnqueueRun seeds a fresh stream; Advance seeds one too when it drives a
// run whose stream is still fresh).
func ReadRunManifest(ctx context.Context, store *graphstore.Store, streamID string) (RunManifest, error) {
	if store == nil {
		return RunManifest{}, fmt.Errorf("lumen: run manifest: nil store")
	}
	if streamID == "" {
		return RunManifest{}, fmt.Errorf("lumen: run manifest: empty stream id")
	}
	events, err := store.ReadStream(ctx, streamID, 1, 1)
	if err != nil {
		return RunManifest{}, fmt.Errorf("lumen: run manifest %q: read stream: %w", streamID, err)
	}
	if len(events) == 0 {
		return RunManifest{}, fmt.Errorf("lumen: run manifest %q: stream is empty (no run.started)", streamID)
	}
	if events[0].Type != EventRunStarted {
		return RunManifest{}, fmt.Errorf("lumen: run manifest %q: seq 1 is %q, not %s", streamID, events[0].Type, EventRunStarted)
	}
	var p runStartedPayload
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		return RunManifest{}, fmt.Errorf("lumen: run manifest %q: decode run.started: %w", streamID, err)
	}
	return RunManifest{
		Name:         p.Name,
		IRHash:       p.IRHash,
		InputHash:    p.InputHash,
		FormulaRef:   p.FormulaRef,
		DefaultRoute: p.DefaultRoute,
		CreatedAt:    p.CreatedAt,
	}, nil
}
