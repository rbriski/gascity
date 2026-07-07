// Package engine is the minimal Lumen executor: the walking skeleton that runs
// a LINEAR formula of block/exec/settle/lit/interp nodes end-to-end directly on
// the graphstore journal substrate. It is a single-writer driver that repeats a
// decide -> persist -> act -> persist cycle: it appends a typed journal event,
// folds it through the pure lumenReducer, and applies the resulting Tier-A delta
// so the node/frontier projection advances in lockstep with the log.
//
// It talks to internal/graphstore directly — no beads.Store adapter, no gc
// dispatcher — because those are integration breadth the walking skeleton does
// not need. Node kinds outside the linear set (do/agent, channels, scatter,
// gather, dispatch, retry, run, async, …) are refused with ErrUnsupportedNode
// before any effect runs.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/exechost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// leaseHolder identifies the executor as the writer-lease holder.
const leaseHolder = "lumen-engine"

// leaseTTL bounds how long a run holds the writer lease without renewal. A
// linear walking-skeleton run is short; this is generous headroom.
const leaseTTL = 30 * time.Second

// RunResult is the outcome of a completed linear run.
type RunResult struct {
	// StreamID is the journal stream (and root node id) the run wrote to.
	StreamID string
	// Outcome is the run's aggregated outcome (pass unless a step failed).
	Outcome string
	// NodeOutputs maps each executed step id to its captured output value
	// (trimmed stdout for exec; the settled value for settle/lit/interp).
	NodeOutputs map[string]string
	// Events is the full committed journal for the run, in seq order.
	Events []graphstore.StoredEvent
}

// RegisterVocabulary registers the executor's closed event vocabulary against
// the store so Append will accept its events. Registration is idempotent.
func RegisterVocabulary(store *graphstore.Store) {
	for _, t := range EventTypes {
		store.RegisterEventType(Engine, t)
	}
}

// Run executes doc as a linear formula against store, threading input into
// {{var}} interpolation, and returns the run's outcome, per-step outputs, and
// the committed journal. It is the single writer for the run's stream: it
// acquires the writer lease, appends run.started, executes each flattened step
// (folding a node.settled event and advancing the projection after each), then
// appends run.closed with the aggregated outcome.
func Run(ctx context.Context, store *graphstore.Store, doc *ir.IR, input map[string]any) (RunResult, error) {
	if store == nil {
		return RunResult{}, fmt.Errorf("lumen: nil store")
	}
	if doc == nil {
		return RunResult{}, fmt.Errorf("lumen: nil IR document")
	}

	steps, err := flatten(doc.Nodes)
	if err != nil {
		return RunResult{}, err
	}

	streamID := streamIDFor(doc.Name)
	irVersion := doc.Contract.Version
	RegisterVocabulary(store)

	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: acquire writer lease %q: %w", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	reducer := lumenReducer{}
	d := &driver{
		ctx:      ctx,
		store:    store,
		streamID: streamID,
		irVer:    irVersion,
		epoch:    lease.Epoch,
		reducer:  reducer,
		state:    reducer.Zero(streamID),
	}

	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := d.append(EventRunStarted, streamID+":run:started", runStartedPayload{
		RootID:    streamID,
		Name:      doc.Name,
		CreatedAt: createdAt,
	}); err != nil {
		return RunResult{}, err
	}

	nodeOutputs := make(map[string]string)
	scope := baseScope(input)

	var (
		anyFailed   bool
		lastNonSkip = OutcomePass
		haveOutcome bool
	)

	// MVP: every flattened leaf runs, even after an upstream step failed. The
	// aggregated run outcome stays correct (failed dominates below), but a
	// data-dependent successor still executes and its projected status may read
	// pass where the spec would want skipped. A proper skip-cascade over the
	// `after` edges is a later phase.
	for _, s := range steps {
		outcome, output, emit, err := d.execStep(s, scope, nodeOutputs)
		if err != nil {
			return RunResult{}, err
		}
		if s.id != "" {
			scope[s.id] = output
			nodeOutputs[s.id] = output
		}
		if emit {
			if err := d.append(EventNodeSettled, streamID+":"+s.id+":0", nodeSettledPayload{
				ID:      s.id,
				Outcome: outcome,
				Output:  output,
			}); err != nil {
				return RunResult{}, err
			}
			haveOutcome = true
			if outcome == OutcomeFailed {
				anyFailed = true
			}
			if outcome != OutcomeSkipped {
				lastNonSkip = outcome
			}
		}
	}

	runOutcome := OutcomePass
	if haveOutcome {
		runOutcome = lastNonSkip
	}
	if anyFailed {
		runOutcome = OutcomeFailed
	}

	if err := d.append(EventRunClosed, streamID+":run:closed", runClosedPayload{Outcome: runOutcome}); err != nil {
		return RunResult{}, err
	}

	events, err := store.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: read stream %q: %w", streamID, err)
	}

	return RunResult{
		StreamID:    streamID,
		Outcome:     runOutcome,
		NodeOutputs: nodeOutputs,
		Events:      events,
	}, nil
}

// driver holds the single-writer append/fold/project loop state for one run.
type driver struct {
	ctx      context.Context
	store    *graphstore.Store
	streamID string
	irVer    string
	epoch    uint64
	reducer  lumenReducer
	state    fold.State
	head     uint64
}

// append canonicalizes payload, commits it to the journal at head+1, folds the
// committed event, and applies the resulting Tier-A delta in its own
// transaction — the decide -> persist -> act -> persist cycle for one event.
//
// The idem token dedupes an identical re-append of the SAME bytes, but it is not
// a replay mechanism for re-running a formula: a second Run of the same formula
// name mints a fresh run.started payload (CreatedAt: time.Now()), whose bytes
// differ from the first, so the reused idem token is rejected with
// ErrIdemTokenReuse — safe and loud, but not an at-most-once replay.
// Deterministic replay is a later-phase feature.
func (d *driver) append(eventType, idemToken string, payload any) error {
	body, err := canonPayload(payload)
	if err != nil {
		return fmt.Errorf("lumen: encoding %s payload: %w", eventType, err)
	}
	ev := graphstore.JournalEvent{
		Type:              eventType,
		IRContractVersion: d.irVer,
		IdemToken:         idemToken,
		Payload:           body,
	}
	res, err := d.store.Append(d.ctx, d.streamID, Engine, d.head, d.epoch, []graphstore.JournalEvent{ev})
	if err != nil {
		return fmt.Errorf("lumen: append %s: %w", eventType, err)
	}
	seq := res.FirstSeq

	next, delta, err := d.reducer.Apply(d.state, fold.Event{
		StreamID:          d.streamID,
		Seq:               seq,
		Engine:            Engine,
		Type:              ev.Type,
		IRContractVersion: ev.IRContractVersion,
		IdemToken:         ev.IdemToken,
		Payload:           ev.Payload,
	})
	if err != nil {
		return fmt.Errorf("lumen: fold %s at seq %d: %w", eventType, seq, err)
	}
	d.state = next

	tx, err := d.store.DB().BeginTx(d.ctx, nil)
	if err != nil {
		return fmt.Errorf("lumen: begin projection tx: %w", err)
	}
	if err := graphstore.ApplyDelta(d.ctx, tx, delta); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("lumen: apply delta for %s: %w", eventType, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lumen: commit projection for %s: %w", eventType, err)
	}

	d.head = seq
	return nil
}

// execStep runs one flattened step and returns its outcome, output value, and
// whether it should emit a node.settled event (exec and settle do; the pure
// value nodes lit/interp fold into scope without an event). block nodes never
// reach here — they are transparent and flattened away.
func (d *driver) execStep(s step, scope map[string]string, _ map[string]string) (outcome, output string, emit bool, err error) {
	switch s.kind {
	case ir.NodeExec:
		script := interpolate(s.script, scope)
		stdout, _, exitCode, runErr := exechost.Run(d.ctx, s.program, script, s.cwd, s.env)
		if runErr != nil {
			return "", "", false, fmt.Errorf("lumen: exec %q: %w", s.id, runErr)
		}
		return outcomeForExit(exitCode, s.passCodes), strings.TrimRight(stdout, "\n"), true, nil

	case ir.NodeSettle:
		value := ""
		if raw, ok := s.raw["value"]; ok {
			value, err = evalValue(raw, scope)
			if err != nil {
				return "", "", false, fmt.Errorf("lumen: settle %q value: %w", s.id, err)
			}
		}
		outcome = s.outcome
		if outcome == "" {
			outcome = OutcomePass
		}
		return outcome, value, true, nil

	case ir.NodeLit:
		value, err := evalValue(s.raw["value"], scope)
		if err != nil {
			return "", "", false, fmt.Errorf("lumen: lit %q value: %w", s.id, err)
		}
		return OutcomePass, value, false, nil

	case ir.NodeInterp:
		value, err := evalInterp(s.raw, scope)
		if err != nil {
			return "", "", false, fmt.Errorf("lumen: interp %q: %w", s.id, err)
		}
		return OutcomePass, value, false, nil

	default:
		return "", "", false, fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, s.kind, s.id)
	}
}

// outcomeForExit maps an exit code onto a step outcome, honoring the exec node's
// exitMap.pass set. With no pass set declared, only exit 0 passes.
func outcomeForExit(exitCode int, passCodes []int) string {
	if len(passCodes) == 0 {
		if exitCode == 0 {
			return OutcomePass
		}
		return OutcomeFailed
	}
	for _, c := range passCodes {
		if c == exitCode {
			return OutcomePass
		}
	}
	return OutcomeFailed
}

var interpRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// interpolate substitutes {{name}} tokens in s with values from scope. An
// unknown name is left verbatim (the literal source wins when no binding
// exists).
//
// SECURITY: interpolated values are spliced into the script text VERBATIM — they
// are NOT shell-quoted or escaped. The result is handed to `sh -c` by the
// exec-host, so a value carrying shell metacharacters (e.g. `; rm -rf …`)
// executes. Untrusted input is therefore unsafe here — Lumen feedback 0020.
// Proper shell-quoting / argv-based execution is a later phase; the current
// walking-skeleton demo performs no interpolation.
func interpolate(s string, scope map[string]string) string {
	return interpRe.ReplaceAllStringFunc(s, func(m string) string {
		name := interpRe.FindStringSubmatch(m)[1]
		if v, ok := scope[name]; ok {
			return v
		}
		return m
	})
}

// baseScope seeds the interpolation scope from the run input, stringifying each
// value (a string as-is, anything else via its JSON form).
func baseScope(input map[string]any) map[string]string {
	scope := make(map[string]string, len(input))
	for k, v := range input {
		if s, ok := v.(string); ok {
			scope[k] = s
			continue
		}
		if b, err := json.Marshal(v); err == nil {
			scope[k] = string(b)
		}
	}
	return scope
}

// canonPayload marshals v to R-CANON bytes so payload_hash and chain_hash are
// reproducible.
func canonPayload(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canon.Canonicalize(raw)
}

// streamIDFor derives a deterministic stream id from the formula name, so tests
// (and reruns of the same formula) address a stable stream.
//
// CAVEAT: stream_id = sha256(name)[:12] is a pure function of the formula name,
// so two runs of the same-named formula collide on one stream. That is fine for
// the per-run / throwaway stores used here, but before this backs `gc run`
// against a shared city store the id needs a run-unique component (e.g. a run
// nonce) so concurrent or repeated runs do not contend on one stream.
func streamIDFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "gcg-run-" + hex.EncodeToString(sum[:])[:12]
}
