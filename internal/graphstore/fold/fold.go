// Package fold defines the per-engine reducer contract and the pure fold driver
// that turns a journal event stream into Tier-A projection deltas
// (01-architecture §4, 02-determinism §2). It is deliberately I/O-free: it
// imports no store, clock, filesystem, network, or database package, so the
// DET-T-11 / R-PURE purity tripwire holds structurally. That is why this package
// does NOT import internal/graphstore (which pulls database/sql and the SQLite
// driver): the journal reader maps graphstore.StoredEvent onto the minimal,
// I/O-free Event view below — applying upcasters on the way — before the fold
// ever sees a row.
//
// This package contains ONLY the contract and the engine-agnostic driver. The
// concrete lumen/v2/v1 reducers are separate slices; a "generic" fold is
// forbidden (02-determinism §2.1).
package fold

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrReducerVersionSkew is returned when a resume crosses a reducer-version
	// boundary this binary cannot certify (R-VERSION-GATE). This slice has no
	// cross-version golden gate, so ANY skew between a snapshot's stamped
	// reducer_version and the running reducer is uncertifiable and fails loudly
	// here — never a silent best-effort fold. It is the fold-layer peer of
	// graphstore.ErrReducerVersionSkew (that one guards the store boundary; this
	// package cannot import graphstore without breaking purity).
	ErrReducerVersionSkew = errors.New("fold: reducer version skew")

	// ErrEngineMismatch is returned when a snapshot's engine tag does not match
	// the reducer asked to resume it — a mis-routed resume, never control flow.
	ErrEngineMismatch = errors.New("fold: snapshot engine mismatch")

	// ErrNonContiguousTail is returned when the tail handed to Fold is not the
	// dense continuation the snapshot (or genesis) expects. seq is a dense total
	// order (I-1/I-2); a gap or overlap means the caller mis-sliced the stream,
	// which would silently corrupt the fold — so it fails loudly.
	ErrNonContiguousTail = errors.New("fold: non-contiguous event tail")

	// ErrForeignEvent is returned when an event in the tail does not belong to the
	// fold's stream (the snapshot's StreamID, or tail[0]'s when resuming from
	// genesis) or carries an engine tag that differs from the reducer's. seq
	// contiguity alone cannot catch a cross-stream or cross-engine splice — two
	// streams share the same dense seq space — so a mis-sliced or mis-routed tail
	// would fold foreign events into this stream's projection. It is the
	// cross-stream/cross-engine peer of ErrNonContiguousTail.
	ErrForeignEvent = errors.New("fold: foreign event in tail")
)

// Event is the minimal, I/O-free view of a committed journal row that a reducer
// consumes. It mirrors the semantically-relevant fields of
// graphstore.StoredEvent; the hash and lease-epoch columns are excluded because
// no pure fold may depend on them. Payload holds R-CANON bytes verbatim.
type Event struct {
	StreamID          string
	Seq               uint64
	Engine            string
	Substream         string
	Type              string
	IRContractVersion string
	IdemToken         string
	Payload           []byte
}

// State is engine-specific fold state. It must serialize deterministically under
// R-CANON so that StateHash gates a snapshot commit (R-SNAP-WRITE) and two folds
// over the same events produce byte-identical snapshots (I-11).
type State interface {
	// StateHash is the SHA-256 over the R-CANON serialization of the state.
	StateHash() [32]byte
	// MarshalSnapshot returns the R-CANON serialization of the state, whose
	// layout is pinned by the reducer's snapshot_format_version.
	MarshalSnapshot() ([]byte, error)
}

// NodeRow is a Tier-A `nodes` upsert (plus its label/metadata child rows). The
// applier stamps fold_owned=1; the reducer never sets ownership.
type NodeRow struct {
	ID          string
	Title       string
	Status      string
	BeadType    string
	Priority    *int // nil = unset (Bead.Priority round-trip)
	Description string
	Assignee    string
	FromActor   string
	ParentID    string
	Ref         string
	CreatedAt   string // RFC3339Nano; sourced from payload, never a live clock
	UpdatedAt   string
	DeferUntil  *string // nil or RFC3339Nano
	StorageTier string  // '', 'history', 'no_history', or 'ephemeral'
	IsBlocked   bool
	StreamID    string
	Labels      []string          // replaces the node's node_labels set
	Metadata    map[string]string // replaces the node's node_metadata set
}

// EdgeRow is a Tier-A `edges` upsert.
type EdgeRow struct {
	FromID   string
	ToID     string
	DepType  string
	Metadata string
}

// FrontierRow is a Tier-A `frontier` upsert.
type FrontierRow struct {
	NodeID        string
	RootID        string
	Route         string
	ReadyPriority int
	CreatedAt     string
	ID            string
	DeferUntil    *string
}

// CursorRow is a Tier-A `channel_cursors` upsert (the projection OF cursor
// plant/advance journal events, D-2).
type CursorRow struct {
	StreamID    string
	Substream   string
	ReaderKey   string
	Position    int64
	PlantedSeq  int64
	AdvancedSeq int64
}

// WakeupRow is a Tier-A `defer_wakeups` upsert.
type WakeupRow struct {
	NodeID string
	WakeAt string
}

// Delta is the Tier-A projection delta a single transition produces. The applier
// commits it INSIDE the append transaction (I-13). Tier-B is absent by design:
// work-surface intents are effects the writer materializes via ApplyGraphPlan, so
// a rebuild replays Deltas but never re-mints beads (I-15).
//
// Scoping obligation: the rows a reducer emits must be scoped to the owning
// stream so a per-stream rebuild can reclaim exactly its own rows. Concretely,
// NodeRow.StreamID and FrontierRow.RootID must be the event's stream, and
// CursorRow.StreamID must be the cursor's stream. RebuildTierA's dropStreamTierA
// relies on this: it clears a stream's frontier rows by root_id, its cursors by
// stream_id, and its nodes/labels/metadata/wakeups by node membership in the
// stream. A reducer that emits cross-stream RootID/StreamID values would strand
// rows a rebuild cannot drop, breaking DROP+refold byte-identity (DET-T-17).
type Delta struct {
	NodeUpserts    []NodeRow
	EdgeUpserts    []EdgeRow
	FrontierInsert []FrontierRow
	FrontierDelete []string // node_ids
	CursorUpserts  []CursorRow
	WakeupUpserts  []WakeupRow
	WakeupDeletes  []string // node_ids
}

// SnapshotProjector is an OPTIONAL capability of a fold.State: it renders the
// FULL Tier-A projection of the state as a single Delta. RebuildTierA uses it to
// reconstruct the covered-prefix rows of a retention-truncated stream — whose
// journal prefix is gone but whose cumulative projection is captured by the
// snapshot state — before applying the folded surviving tail on top. A State that
// does not implement it can only be rebuilt from a genesis fold (an untruncated
// journal); a truncated stream whose reducer state is not projectable cannot be
// rebuilt.
type SnapshotProjector interface {
	// ProjectDelta renders the state as one full Tier-A delta scoped to streamID
	// (NodeRow.StreamID / FrontierRow.RootID), so a per-stream rebuild reclaims
	// exactly its own rows (the Delta scoping obligation above).
	ProjectDelta(streamID string) Delta
}

// Snapshot is a persisted fold-state anchor covering seq <= CoveredSeq
// (01-architecture §2.3). Resume folds only the tail after CoveredSeq (R-RESUME).
type Snapshot struct {
	StreamID              string
	CoveredSeq            uint64
	Engine                string
	ReducerVersion        int
	SnapshotFormatVersion int
	StateHash             [32]byte
	State                 []byte
}

// Reducer is a pure, total per-engine transition function (R-PURE, R-TOTAL). It
// performs no I/O, reads no clock, consults no ledger/bus/config/env, and walks
// maps only in canonical key order. Structurally impossible sequences return a
// typed error (journal corruption), never ordinary control flow.
type Reducer interface {
	Engine() string      // "lumen" | "v2" | "v1"
	ReducerVersion() int // monotonic; bumped on ANY semantic change to fold or upcasters
	Zero(streamID string) State
	UnmarshalSnapshot(formatVersion int, b []byte) (State, error)
	Apply(s State, e Event) (State, Delta, error)
}

// Fold is THE fold: the pure, engine-agnostic driver. It resumes from snap (or
// genesis when snap is nil), applies every registered upcaster to each event
// before the reducer sees it (§3.3), and returns the final state plus one Delta
// per event in seq order.
//
// Resume law (R-RESUME): for every split point k,
//
//	Fold(r, snapshotAt(k), tail[k+1..H]) ≡ Fold(r, nil, events[1..H])
//
// in both final StateHash and concatenated Deltas (DET-T-20).
//
// Version gate (R-VERSION-GATE): a snapshot whose reducer_version differs from
// r.ReducerVersion() cannot be certified in this slice and yields
// ErrReducerVersionSkew — never a silent best-effort fold.
func Fold(r Reducer, snap *Snapshot, tail []Event) (State, []Delta, error) {
	var (
		state    State
		expected uint64
		// stream is the fold's anchor stream: the snapshot's when resuming, else
		// tail[0]'s at genesis. Every tail event must belong to it (ErrForeignEvent).
		stream string
	)
	if snap == nil {
		streamID := ""
		if len(tail) > 0 {
			streamID = tail[0].StreamID
		}
		state = r.Zero(streamID)
		expected = 1
		stream = streamID
	} else {
		if snap.ReducerVersion != r.ReducerVersion() {
			return nil, nil, fmt.Errorf(
				"fold: snapshot reducer_version %d != reducer %d (engine %q): %w",
				snap.ReducerVersion, r.ReducerVersion(), r.Engine(), ErrReducerVersionSkew)
		}
		if snap.Engine != r.Engine() {
			return nil, nil, fmt.Errorf(
				"fold: snapshot engine %q != reducer engine %q: %w",
				snap.Engine, r.Engine(), ErrEngineMismatch)
		}
		loaded, err := r.UnmarshalSnapshot(snap.SnapshotFormatVersion, snap.State)
		if err != nil {
			return nil, nil, fmt.Errorf("fold: unmarshal snapshot (stream %q, covered %d): %w", snap.StreamID, snap.CoveredSeq, err)
		}
		state = loaded
		expected = snap.CoveredSeq + 1
		stream = snap.StreamID
	}

	deltas := make([]Delta, 0, len(tail))
	for _, e := range tail {
		if e.StreamID != stream {
			return nil, nil, fmt.Errorf("fold: event stream %q != fold stream %q at seq %d: %w", e.StreamID, stream, e.Seq, ErrForeignEvent)
		}
		if e.Engine != r.Engine() {
			return nil, nil, fmt.Errorf("fold: event engine %q != reducer engine %q at seq %d: %w", e.Engine, r.Engine(), e.Seq, ErrForeignEvent)
		}
		if e.Seq != expected {
			return nil, nil, fmt.Errorf("fold: stream %q got seq %d, expected %d: %w", e.StreamID, e.Seq, expected, ErrNonContiguousTail)
		}
		up, err := applyUpcasters(e)
		if err != nil {
			return nil, nil, err
		}
		next, delta, err := r.Apply(state, up)
		if err != nil {
			return nil, nil, fmt.Errorf("fold: apply (%s, %s) at seq %d: %w", up.Engine, up.Type, up.Seq, err)
		}
		state = next
		deltas = append(deltas, delta)
		expected++
	}
	return state, deltas, nil
}

// Upcaster is a pure rewrite of one event from an older IR contract to the next.
// It returns the rewritten (type, ir_contract_version, payload). Downcasting is
// unsupported by design (§3.3).
type Upcaster func(typ, irVersion string, payload []byte) (newTyp, newIRVersion string, newPayload []byte, err error)

type upcasterKey struct {
	engine    string
	typ       string
	irVersion string
}

var (
	upcasterMu       sync.RWMutex
	upcasterRegistry = map[upcasterKey]Upcaster{}
)

// RegisterUpcaster registers up for events matching (engine, typ, fromIRVersion).
// Registration is keyed on the OLD contract; Fold composes upcasters until no
// registered rewrite matches the event's current contract. Panics on a duplicate
// key — an upcaster table with two rewrites for one contract is a programming
// error, not a runtime condition.
func RegisterUpcaster(engine, typ, fromIRVersion string, up Upcaster) {
	if up == nil {
		panic("fold: RegisterUpcaster with nil upcaster")
	}
	key := upcasterKey{engine: engine, typ: typ, irVersion: fromIRVersion}
	upcasterMu.Lock()
	defer upcasterMu.Unlock()
	if _, dup := upcasterRegistry[key]; dup {
		panic(fmt.Sprintf("fold: duplicate upcaster for (%q, %q, %q)", engine, typ, fromIRVersion))
	}
	upcasterRegistry[key] = up
}

func lookupUpcaster(engine, typ, irVersion string) (Upcaster, bool) {
	upcasterMu.RLock()
	defer upcasterMu.RUnlock()
	up, ok := upcasterRegistry[upcasterKey{engine: engine, typ: typ, irVersion: irVersion}]
	return up, ok
}

// applyUpcasters composes registered upcasters until the event reaches a contract
// with no further rewrite. It bounds the chain length and refuses a rewrite that
// fails to advance the contract, so a cyclic or fixed-point registration is a
// loud error rather than an infinite loop.
func applyUpcasters(e Event) (Event, error) {
	const maxHops = 64
	for hop := 0; ; hop++ {
		if hop > maxHops {
			return Event{}, fmt.Errorf("fold: upcaster chain for (%s, %s) exceeded %d hops (cycle?)", e.Engine, e.Type, maxHops)
		}
		up, ok := lookupUpcaster(e.Engine, e.Type, e.IRContractVersion)
		if !ok {
			return e, nil
		}
		newTyp, newIR, newPayload, err := up(e.Type, e.IRContractVersion, e.Payload)
		if err != nil {
			return Event{}, fmt.Errorf("fold: upcasting (%s, %s, %s): %w", e.Engine, e.Type, e.IRContractVersion, err)
		}
		if newTyp == e.Type && newIR == e.IRContractVersion {
			return Event{}, fmt.Errorf("fold: upcaster for (%s, %s, %s) did not advance the contract", e.Engine, e.Type, e.IRContractVersion)
		}
		e.Type = newTyp
		e.IRContractVersion = newIR
		e.Payload = newPayload
	}
}
