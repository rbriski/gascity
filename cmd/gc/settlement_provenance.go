package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// emitCmdRootSettlement records a coarse settlement.root provenance fact for a
// root or control bead closed by a cmd-side v1/v2 terminal closer — molecule
// autoclose (molecule_autoclose.go), the wisp-autoclose on-close closer
// (wisp_autoclose.go), the wisp-GC abandoned-root sweep (wisp_gc.go), and the
// control-dispatch quarantine (cmd_convoy_dispatch.go). It is the cmd-layer peer
// of the dispatcher's emit helpers (internal/dispatch/settlement.go): PROVENANCE
// ONLY and strictly AFTER the projection-of-record close.
//
// Best-effort, loud-but-non-fatal (the wisp-flood + don't-swallow lessons): a nil
// emitter (a non-opted city, or a nil journal) is inert and byte-identical to
// pre-P5; an emit error is surfaced (slog warn) and swallowed — the close already
// committed and its gc.outcome column write is untouched. The engine tag is the
// pure gc.formula_contract mapping (beads.EngineForContract): a graph.v2 root
// emits v2, a v1 molecule/wisp emits v1 — no judgment, no role names.
//
// Coarse means coarse: callers invoke this exactly once per GENUINE root/control
// settlement (one abandoned root actually closed, one molecule autoclosed, one
// control quarantined), never per GC sweep tick and never per wisp reaped/purged
// (a wisp reaped from an already-closed root is not a settlement).
//
// LOW-2 alignment: a ROOT SELF-settlement (the settled bead IS the root) mints
// the byte-identical payload the dispatcher's v2 root emit does — Kind omitted —
// so a root closed by BOTH a late v2 finalize (dispatch.emitRootSettled) and this
// reactive close dedupes to ONE identical fact under the shared
// settlement.root/<root>/<outcome> idem token, instead of the second being
// dropped as a divergent idem-token reuse. Kind is meaningful provenance only
// when a DISTINCT control bead settles a (missing) root — the orphaned/quarantine
// closers, where Bead != Root and no self-settlement can collide with it.
func emitCmdRootSettlement(emitter dispatch.SettlementEmitter, rootID string, settled beads.Bead, outcome string) {
	if emitter == nil || rootID == "" {
		return
	}
	engine := beads.EngineForContract(settled.Metadata[beadmeta.FormulaContractMetadataKey])
	s := dispatch.Settlement{
		Root:    rootID,
		Bead:    settled.ID,
		Outcome: outcome,
	}
	if settled.ID != rootID {
		s.Kind = settled.Metadata[beadmeta.KindMetadataKey]
	}
	if err := emitter.EmitRootSettled(context.Background(), engine, s); err != nil {
		slog.Warn("v1 settlement provenance emit failed (dropped; close committed, projection of record unaffected)",
			"root", rootID, "bead", settled.ID, "engine", engine, "err", err)
	}
}

// lazySettlementEmitter defers construction of the real (journal-opening) emitter
// until the first Emit* call, so an on-close autoclose that closes NOTHING never
// opens the city's graph journal. In the bd on_close hook subprocess the
// cachedCityGraphJournal memo does not amortize across closes (a fresh process per
// hook), so an eager open on every bead close would pay a loadCityConfig + SQLite
// WAL open for nothing on the common no-close path. The build func is invoked at
// most once; a nil result (a non-opted city) stays inert.
type lazySettlementEmitter struct {
	build func() dispatch.SettlementEmitter
	once  sync.Once
	inner dispatch.SettlementEmitter
}

// newLazyCityGraphSettlementEmitter returns a settlement emitter that opens the
// city's shared graph journal lazily — only if and when a close actually emits.
// It is the CLI/hook-path constructor (doMoleculeAutoclose, doWispAutoclose);
// the long-lived controller path holds an already-open journal handle and wires
// dispatch.NewJournalSettlementEmitter directly.
func newLazyCityGraphSettlementEmitter(cityPath string) dispatch.SettlementEmitter {
	return &lazySettlementEmitter{build: func() dispatch.SettlementEmitter {
		return dispatch.NewJournalSettlementEmitter(cachedCityGraphJournal(cityPath))
	}}
}

func (l *lazySettlementEmitter) resolve() dispatch.SettlementEmitter {
	l.once.Do(func() { l.inner = l.build() })
	return l.inner
}

func (l *lazySettlementEmitter) EmitRootSettled(ctx context.Context, engine string, s dispatch.Settlement) error {
	inner := l.resolve()
	if inner == nil {
		return nil
	}
	return inner.EmitRootSettled(ctx, engine, s)
}

func (l *lazySettlementEmitter) EmitAttemptSettled(ctx context.Context, engine string, s dispatch.Settlement) error {
	inner := l.resolve()
	if inner == nil {
		return nil
	}
	return inner.EmitAttemptSettled(ctx, engine, s)
}

func (l *lazySettlementEmitter) EmitWorkflowFinalized(ctx context.Context, engine string, s dispatch.Settlement) error {
	inner := l.resolve()
	if inner == nil {
		return nil
	}
	return inner.EmitWorkflowFinalized(ctx, engine, s)
}
