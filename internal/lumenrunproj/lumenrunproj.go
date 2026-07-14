package lumenrunproj

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/runproj"
)

// runDetailSnapshotVersion is the snapshot version stamped on a projected Lumen
// run detail. It mirrors the dashboard BFF's own constant; the value is opaque
// to the frontend (a display label).
const runDetailSnapshotVersion = 1

// Projector folds a city's Lumen runs into the dashboard run-view DTOs. It
// satisfies the dashboard BFF's optional LumenRunProjector seam (nil in upstream
// builds). It holds one read-only-used *graphstore.Store per city (opened once,
// reused) and reads the graph journal only through the read pool — it never
// writes.
type Projector struct {
	mu     sync.Mutex
	stores map[string]*graphstore.Store // keyed by city root path
	// sealed memoizes the journal fold of CLOSED runs (a sealed stream is
	// immutable, so its RunView never changes) keyed by stream id, so the hot
	// dashboard poll re-folds only the still-open runs.
	sealed sync.Map // streamID -> engine.RunView
}

// New returns an empty Projector. Stores are opened lazily on first use per city.
func New() *Projector {
	return &Projector{stores: map[string]*graphstore.Store{}}
}

// foldRunView folds a run's journal stream into a RunView, serving a sealed run
// from the immutable memo and folding an open run fresh (caching it once it
// seals).
func (p *Projector) foldRunView(ctx context.Context, store *graphstore.Store, streamID string) (engine.RunView, error) {
	if v, ok := p.sealed.Load(streamID); ok {
		return v.(engine.RunView), nil
	}
	view, err := engine.FoldRunView(ctx, store, streamID)
	if err != nil {
		return engine.RunView{}, err
	}
	if view.Closed {
		p.sealed.Store(streamID, view)
	}
	return view, nil
}

// Close closes every opened graph store. It is safe to call once on plane
// shutdown.
func (p *Projector) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	for root, s := range p.stores {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(p.stores, root)
	}
	return errors.Join(errs...)
}

// storeFor returns the cached graph store for a city root, opening it once. It
// returns (nil, nil) when the city has no graph journal on disk (a city that has
// run no Lumen runs) — the caller treats that as "no Lumen lanes", not an error.
func (p *Projector) storeFor(ctx context.Context, cityRoot string) (*graphstore.Store, error) {
	path := filepath.Join(graphScopeRoot(cityRoot), "journal.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.stores[cityRoot]; ok {
		return s, nil
	}
	s, err := graphstore.Open(ctx, path, graphstore.Options{})
	if err != nil {
		return nil, err
	}
	p.stores[cityRoot] = s
	return s, nil
}

// graphScopeRoot mirrors cmd/gc's graphScopeRoot (unimportable, package main):
// the graph journal lives under <cityRoot>/.gc/graph.
func graphScopeRoot(cityRoot string) string {
	return filepath.Join(cityRoot, ".gc", "graph")
}

// SummaryLanes projects every open + recent-closed Lumen run in the city into a
// run lane, joining live per-step status/session from foldedBeads (the tailer's
// warm do beads). It returns an empty slice (nil error) when the city has no
// Lumen runs.
func (p *Projector) SummaryLanes(ctx context.Context, cityName, cityRoot string, foldedBeads []beads.Bead) ([]runproj.RunLane, error) {
	store, err := p.storeFor(ctx, cityRoot)
	if err != nil || store == nil {
		return nil, err
	}
	streamIDs, err := runStreamIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	if len(streamIDs) == 0 {
		return nil, nil
	}
	byRun := indexDoBeadsByRun(foldedBeads)

	var synth []beads.Bead
	for _, streamID := range streamIDs {
		view, err := p.foldRunView(ctx, store, streamID)
		if err != nil {
			// A root row without a foldable run stream is a torn/partial run;
			// skip it rather than failing the whole summary.
			continue
		}
		synth = append(synth, syntheticBeads(view, byRun[streamID], cityName)...)
	}
	if len(synth) == 0 {
		return nil, nil
	}

	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(synth))
	lanes := make([]runproj.RunLane, 0, len(summary.Lanes)+len(summary.HistoricalLanes)+len(summary.BlockedLanes))
	lanes = append(lanes, summary.Lanes...)
	lanes = append(lanes, summary.HistoricalLanes...)
	lanes = append(lanes, summary.BlockedLanes...)
	return lanes, nil
}

// Detail projects a single Lumen run's detail graph. The bool reports whether
// runID resolves to a Lumen run: false (nil error) means "not a Lumen run" and
// the caller keeps its existing 404 — never a fabricated detail.
func (p *Projector) Detail(ctx context.Context, cityName, cityRoot, runID string, foldedBeads []beads.Bead) (runproj.FormulaRunDetail, bool, error) {
	store, err := p.storeFor(ctx, cityRoot)
	if err != nil || store == nil {
		return runproj.FormulaRunDetail{}, false, err
	}
	view, err := p.foldRunView(ctx, store, runID)
	if err != nil {
		// No foldable run stream for this id → not a Lumen run.
		return runproj.FormulaRunDetail{}, false, nil
	}
	byRun := indexDoBeadsByRun(foldedBeads)
	synth := syntheticBeads(view, byRun[runID], cityName)

	detail, _, _, _, _, _, err := runproj.BuildRunDetailForRun(
		synth, runID, runDetailSnapshotVersion, 0, nil, nil, runproj.FormulaDetailUpstreamError)
	if err != nil {
		return runproj.FormulaRunDetail{}, true, err
	}
	return detail, true, nil
}

// indexDoBeadsByRun groups the folded do beads by their run stream id
// (gc.lumen_run), then by activation (gc.lumen_activation), so syntheticBeads can
// join each activation to its live work bead. When a node has multiple attempts,
// the last-seen bead for an activation wins (activations are attempt-unique, so
// this only dedupes exact re-emits).
func indexDoBeadsByRun(foldedBeads []beads.Bead) map[string]map[string]beads.Bead {
	out := map[string]map[string]beads.Bead{}
	for _, b := range foldedBeads {
		run := b.Metadata[beadmeta.LumenRunMetadataKey]
		if run == "" {
			continue
		}
		act := b.Metadata[beadmeta.LumenActivationMetadataKey]
		if act == "" {
			continue
		}
		byAct := out[run]
		if byAct == nil {
			byAct = map[string]beads.Bead{}
			out[run] = byAct
		}
		byAct[act] = b
	}
	return out
}
