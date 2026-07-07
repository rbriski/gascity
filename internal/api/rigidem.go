package api

// rigidem.go implements the request_id idempotency state machine for
// rig-create (G13; C4a-state-machine-design.md). It is the self-contained
// core the async rig-create handler (C4b) drives: an in-process live index
// that is authoritative for admission decisions, a durable bead record that
// backs crash recovery, and the admission function that resolves the six
// responses of G13 §4.2 against them.
//
// This slice deliberately owns no HTTP wiring, spawns no goroutines, and
// emits no events — those belong to C4b. The one exported symbol,
// RigCreateBody, is defined here so the digest can be computed by value; C6
// promotes RigCreateInput.Body to it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// Durable-record metadata keys and enum values (G13 §3.2). The record is a
// legal "task" bead carrying the machine state in flat string metadata —
// never a new issue_type, which bd would hard-fail on DoltLite.
const (
	idemKindRigCreate = "rig-create"

	idemStateInFlight   = "in_flight"
	idemStateSucceeded  = "succeeded"
	idemStateRolledBack = "rolled_back"

	metaIdemKind        = "gc.idem.kind"
	metaIdemCity        = "gc.idem.city"
	metaIdemRequestID   = "gc.idem.request_id"
	metaIdemDigest      = "gc.idem.digest"
	metaIdemState       = "gc.idem.state"
	metaIdemEventCursor = "gc.idem.event_cursor"
	metaIdemRigName     = "gc.idem.rig_name"

	metaIdemResultRig    = "gc.idem.result.rig"
	metaIdemResultPrefix = "gc.idem.result.prefix"
	metaIdemResultBranch = "gc.idem.result.branch"

	// idemLabel / idemLabelRigCreate are the coarse markers the G13 §6 boot
	// sweep scans to find orphan in_flight records. They are NOT used to
	// rebuild the live index (which always starts empty).
	idemLabel          = "gc-idem"
	idemLabelRigCreate = "gc-idem-rig-create"
)

// errInvalidRequestID reports a client-supplied request_id that fails the
// G13 §2 validation. The handler (C4b) renders it as the 400
// invalid_request_id typed error — never a 500, never a silently-minted
// substitute.
var errInvalidRequestID = errors.New("invalid request_id")

// errInvalidRigName reports a rig name that is empty or JSON-inferable
// (the same bd --metadata-field foot-gun as request_id). The handler
// renders it as a 400.
var errInvalidRigName = errors.New("invalid rig name")

// requestIDCharset is the G13 §2 opaque-id charset: safe for the digest
// preimage, the DoltLite metadata JSON column, and the bd --metadata-field
// filter. It excludes control chars, whitespace, and the JSON quote by
// construction.
var requestIDCharset = regexp.MustCompile(`^[A-Za-z0-9._~:-]{8,200}$`)

// validateRequestID enforces G13 §2 for a client-supplied request_id. It
// runs at the handler edge before any lock, index, or store access;
// admitRigCreate assumes its input has already passed. The json.Valid guard
// rejects exactly the literals a JSON parser would type-infer (numbers,
// booleans, null, exponent forms) — the values bd's equality filter would
// compare as a non-string and then never match the JSON-string-stored
// metadata, silently missing the (city, request_id) lookup and re-cloning.
// A UUIDv4 (the recommended client id) or any id containing a letter run
// passes trivially.
func validateRequestID(id string) error {
	if !requestIDCharset.MatchString(id) {
		return errInvalidRequestID
	}
	if json.Valid([]byte(id)) {
		return errInvalidRequestID
	}
	return nil
}

// validateRigName enforces the non-empty + non-JSON-inferable constraint on a
// rig name before it is used as the G13 §4.4 name-axis metadata filter value
// or the G16 per-rig-name lock key. Huma already enforces non-empty via
// minLength; this is the additional bd-filter guard (a purely numeric name
// hits the identical foot-gun on the durable rig_name scan).
func validateRigName(name string) error {
	if name == "" {
		return errInvalidRigName
	}
	if json.Valid([]byte(name)) {
		return errInvalidRigName
	}
	return nil
}

// RigCreateBody is the provisioning-relevant body of POST
// /v0/city/{cityName}/rigs, owned by the idempotency slice so
// rigCreateDigest can hash it by value. C6 promotes the anonymous
// RigCreateInput.Body to this named type.
//
// FIELD ORDER IS LOAD-BEARING: encoding/json emits struct fields in
// declaration order and the digest (rigCreateDigest) is computed over that
// encoding. Append new fields at the end; never reorder or change a tag —
// the golden-digest test fails the build otherwise, which is deliberate: a
// silent digest change turns every in-flight retry across a deploy into a
// spurious 409 body-mismatch.
type RigCreateBody struct {
	Name          string `json:"name" doc:"Rig name." minLength:"1"`
	Path          string `json:"path,omitempty" doc:"Filesystem path (server-derived for git_url clones)."`
	Prefix        string `json:"prefix,omitempty" doc:"Session name prefix."`
	DefaultBranch string `json:"default_branch,omitempty" doc:"Mainline branch (e.g. main, master). Auto-detected when omitted."`
	GitURL        string `json:"git_url,omitempty" doc:"Git URL to clone (triggers async provisioning)."`
	RequestID     string `json:"request_id,omitempty" doc:"Client-supplied idempotency key; reuse across retries."`
}

// rigCreateDigest returns hex(sha256(json.Marshal(body with RequestID
// zeroed))) — G13 §3.3. It binds a request_id to the exact provisioning
// request it first named, so a retry with a different body is a detectable
// 409 body-mismatch. Deterministic: encoding/json emits struct fields in
// declaration order and RigCreateBody has no maps. Because request_id carries
// omitempty, zeroing it drops the key from the encoding entirely, so the
// digest covers only the provisioning fields.
//
// Distinct from citywriteauth.ReqDigest, which digests
// method\npath[\nquery]\nhex(sha256(body)) to bind a write-auth grant to one
// HTTP request; this digest binds a request_id to one logical body. Do not
// conflate or reuse.
func rigCreateDigest(body RigCreateBody) (string, error) {
	body.RequestID = ""
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("digesting rig-create body: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// idemKey identifies one logical request: (city, request_id). G13 §0.
type idemKey struct {
	city      string
	requestID string
}

// nameKey is the second dedupe axis: (city, rig name). G13 §4.4.
type nameKey struct {
	city string
	rig  string
}

// liveProvision is one currently-running async rig provision. A single value
// is shared by pointer between the inflight and byName maps so terminal
// removal is atomic across both axes and both observe the same done channel.
type liveProvision struct {
	requestID   string        // client-supplied, or synthetic newRequestID() (G13 §1)
	digest      string        // hex sha256 of the zeroed body (rigCreateDigest)
	eventCursor string        // decimal seq captured before the goroutine (G13 §5)
	rigName     string        // rig name (the byName axis key)
	beadID      string        // durable record ID; "" when synthetic (no dedup record)
	synthetic   bool          // true when the client sent no request_id
	done        chan struct{} // closed exactly once at the terminal step
}

// rigIdemIndex is the in-process live index (G13 §3.5): authoritative for
// admission, holding ONLY currently-running provisions. It starts empty at
// boot and is never rebuilt from durable records. Unlike idempotencyCache it
// is not TTL/cap-evicted — entries are removed only by their provision's
// terminal step, the same "pending entries are never evicted" rule
// idempotencyCache pins, for the same double-execute reason. Single-replica
// by accepted constraint (G13 §12).
type rigIdemIndex struct {
	mu       sync.Mutex
	inflight map[idemKey]*liveProvision
	byName   map[nameKey]*liveProvision
}

// newRigIdemIndex returns an empty live index.
func newRigIdemIndex() *rigIdemIndex {
	return &rigIdemIndex{
		inflight: make(map[idemKey]*liveProvision),
		byName:   make(map[nameKey]*liveProvision),
	}
}

// register inserts e under both the request_id and rig-name keys. The caller
// must hold the per-rig-name admission lock and must have already confirmed
// (under that lock) that neither key is occupied — admission consults the
// index before reaching here, so a collision is a programming error.
func (x *rigIdemIndex) register(city string, e *liveProvision) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.inflight[idemKey{city, e.requestID}] = e
	x.byName[nameKey{city, e.rigName}] = e
}

// remove drops e from both maps and closes its done channel. It is the
// provision goroutine's terminal step (C4b), guarded by pointer identity so a
// stale or duplicate terminal for e cannot evict a re-clone successor that has
// since reused the same keys. done is closed only when e was actually present,
// so a duplicate remove(e) is a no-op rather than a close-of-closed-channel
// panic.
func (x *rigIdemIndex) remove(city string, e *liveProvision) {
	x.mu.Lock()
	defer x.mu.Unlock()
	removed := false
	ik := idemKey{city, e.requestID}
	if cur, ok := x.inflight[ik]; ok && cur == e {
		delete(x.inflight, ik)
		removed = true
	}
	nk := nameKey{city, e.rigName}
	if cur, ok := x.byName[nk]; ok && cur == e {
		delete(x.byName, nk)
		removed = true
	}
	if removed {
		close(e.done)
	}
}

// lookup returns the live provision for (city, request_id), if any.
func (x *rigIdemIndex) lookup(city, requestID string) (*liveProvision, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.inflight[idemKey{city, requestID}]
	return e, ok
}

// lookupByName returns the live provision holding (city, rig name), if any.
func (x *rigIdemIndex) lookupByName(city, rig string) (*liveProvision, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.byName[nameKey{city, rig}]
	return e, ok
}

// createIdemRecord reserves the durable idempotency record (G13 §3.2/§5.1).
// It creates the "task" bead and closes it in a single Store.Tx: an OPEN
// "task" bead is Ready()-eligible actionable work the dispatcher could claim
// ("task" is absent from beads.readyExcludeTypes and "gc-idem" is not a
// ready-excluded label), so the record is closed at birth to stay out of
// every open/ready view. All machine lookups use IncludeClosed:true. Returns
// the new record's ID.
func createIdemRecord(store beads.Store, city, requestID, digest, cursor, rigName, state string) (string, error) {
	var id string
	err := store.Tx("gc: idem reserve rig-create "+requestID, func(tx beads.Tx) error {
		rec, err := tx.Create(beads.Bead{
			Type:   "task",
			Title:  "idem: rig-create " + requestID,
			Labels: []string{idemLabel, idemLabelRigCreate},
			Metadata: beads.StringMap{
				metaIdemKind:        idemKindRigCreate,
				metaIdemCity:        city,
				metaIdemRequestID:   requestID,
				metaIdemDigest:      digest,
				metaIdemState:       state,
				metaIdemEventCursor: cursor,
				metaIdemRigName:     rigName,
			},
		})
		if err != nil {
			return fmt.Errorf("creating idem record: %w", err)
		}
		id = rec.ID
		if err := tx.Close(rec.ID); err != nil {
			return fmt.Errorf("closing idem record %s: %w", rec.ID, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// lookupIdemRecord returns the durable record for (city, request_id), or nil
// when absent (G13 §5.2). IncludeClosed is mandatory: records are closed at
// create. A result count above one is an invariant violation.
func lookupIdemRecord(store beads.Store, city, requestID string) (*beads.Bead, error) {
	matches, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			metaIdemKind:      idemKindRigCreate,
			metaIdemCity:      city,
			metaIdemRequestID: requestID,
		},
		IncludeClosed: true,
		Limit:         2,
	})
	if err != nil {
		return nil, fmt.Errorf("idem lookup %s/%s: %w", city, requestID, err)
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("idem invariant: %d records for (%s, %s)", len(matches), city, requestID)
	}
}

// durableRigNameScan reports whether any durable record for (city, rig name)
// is in state in_flight or succeeded — the G13 §4.4 backstop that closes the
// window where a provision has committed succeeded but the rig is not yet
// visible in config, and covers pre-boot orphans. A rolled_back record does
// not block a new name (the name is free to reuse).
func durableRigNameScan(store beads.Store, city, rigName string) (bool, error) {
	matches, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			metaIdemKind:    idemKindRigCreate,
			metaIdemCity:    city,
			metaIdemRigName: rigName,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return false, fmt.Errorf("idem rig-name scan %s/%s: %w", city, rigName, err)
	}
	for i := range matches {
		switch matches[i].Metadata[metaIdemState] {
		case idemStateInFlight, idemStateSucceeded:
			return true, nil
		}
	}
	return false, nil
}

// markIdemSucceeded transitions a record to succeeded and merges the result
// fields (G13 §5.3). C4b calls it from the provision goroutine ONLY after the
// G17 visibility barrier is satisfied, and before removing the live entry.
func markIdemSucceeded(store beads.Store, beadID, rigName, prefix, defaultBranch string) error {
	if err := store.SetMetadataBatch(beadID, map[string]string{
		metaIdemState:        idemStateSucceeded,
		metaIdemResultRig:    rigName,
		metaIdemResultPrefix: prefix,
		metaIdemResultBranch: defaultBranch,
	}); err != nil {
		return fmt.Errorf("marking idem record %s succeeded: %w", beadID, err)
	}
	return nil
}

// markIdemRolledBack transitions a record to the re-executable rolled_back
// terminal (G13 §5.3/§6). C4b calls it from the goroutine ONLY after the
// partial dir/DB/config for the rig has been fully removed (drop-then-mark),
// and before removing the live entry.
func markIdemRolledBack(store beads.Store, beadID string) error {
	if err := store.SetMetadataBatch(beadID, map[string]string{
		metaIdemState: idemStateRolledBack,
	}); err != nil {
		return fmt.Errorf("marking idem record %s rolled back: %w", beadID, err)
	}
	return nil
}

// requestIDConflictError reports a request_id reused for a different request
// body (G13 §4.3). The binding request_id↔digest is fixed for the id's
// lifetime, so this is returned in every state, including rolled_back. C4b
// renders it as the 409 request_id_conflict typed error.
type requestIDConflictError struct {
	RequestID string
}

func (e *requestIDConflictError) Error() string {
	return fmt.Sprintf("request_id %q reused with a different request body", e.RequestID)
}

// rigNameConflictError reports a rig-name collision under a different (or no)
// request_id (G13 §4.4). InFlightRequestID and InFlightCursor are populated
// only when the collision is with a live provision, so a coordinating client
// can attach to its event stream. C4b renders it as the 409 rig_name_conflict
// typed error.
type rigNameConflictError struct {
	Rig               string
	InFlightRequestID string
	InFlightCursor    string
}

func (e *rigNameConflictError) Error() string {
	return fmt.Sprintf("rig %q already exists or is being provisioned", e.Rig)
}

// rigAdmitOutcome is one of the four success admissions of G13 §4.2. The two
// 409 conflicts are carried out-of-band as typed errors, not as an outcome.
type rigAdmitOutcome int

const (
	// rigAdmitNew: no prior record — reserve, register a live entry, spawn
	// (HTTP 202).
	rigAdmitNew rigAdmitOutcome = iota
	// rigAdmitInflightReplay: a live entry already exists for this
	// request_id — return its cursor, do NOT spawn (HTTP 202).
	rigAdmitInflightReplay
	// rigAdmitExisting: a durable succeeded record exists — served
	// synchronously from the record (HTTP 200).
	rigAdmitExisting
	// rigAdmitReclone: a durable rolled_back or orphan in_flight record
	// exists — reset it, register a fresh live entry, spawn (HTTP 202).
	rigAdmitReclone
)

// rigAdmitResult is the admission decision the handler (C4b) acts on. entry is
// non-nil for New/Reclone (the caller spawns the provision with it); record is
// non-nil for Existing (the result fields are read from its metadata).
type rigAdmitResult struct {
	outcome     rigAdmitOutcome
	requestID   string // echoed verbatim: the client's id, or the synthetic one
	eventCursor string
	entry       *liveProvision
	record      *beads.Bead
}

// admitRigCreate runs the G13 §4 admission state machine for one rig-create
// request and returns the decision the async handler (C4b) acts on. A
// rig_name or request_id conflict is returned as a typed error
// (*rigNameConflictError / *requestIDConflictError) rather than an outcome;
// the four success shapes ride rigAdmitResult.
//
// Preconditions (enforced at the handler edge, not re-checked here): body.Name
// is non-empty and non-JSON-inferable (validateRigName), and body.RequestID —
// when present — has passed validateRequestID.
//
// Collaborators are passed explicitly so the core is unit-testable without a
// Server. In production (C4b) store is s.state.CityBeadStore(), cursor is
// s.currentCityEventCursor, and rigInConfig reports whether s.state.Config()
// already holds the rig. The whole call MUST run inside the per-rig-name lock
// (G13 §7) so the index reads/writes for one name are a critical section.
//
// The live index is consulted FIRST for the request_id and rig-name axes
// (strong consistency); the durable store is read only for keys the index does
// not hold — records committed strictly in the past (succeeded, rolled_back,
// orphan in_flight) where the hosted ledger's read-after-write lag cannot
// invert the answer (G13 §3.5). This is what defeats the double-clone a plain
// lookup-then-Create would suffer within the lag window.
func admitRigCreate(
	idx *rigIdemIndex,
	store beads.Store,
	cursor func() (string, error),
	rigInConfig func(name string) bool,
	city string,
	body RigCreateBody,
) (rigAdmitResult, error) {
	digest, err := rigCreateDigest(body)
	if err != nil {
		return rigAdmitResult{}, err
	}

	// (1) request_id axis — live index first, durable store on miss.
	if body.RequestID != "" {
		if live, ok := idx.lookup(city, body.RequestID); ok {
			if live.digest != digest {
				return rigAdmitResult{}, &requestIDConflictError{RequestID: body.RequestID} // row 2
			}
			return rigAdmitResult{ // row 1: in-flight replay, no spawn
				outcome:     rigAdmitInflightReplay,
				requestID:   body.RequestID,
				eventCursor: live.eventCursor,
			}, nil
		}
		rec, err := lookupIdemRecord(store, city, body.RequestID)
		if err != nil {
			return rigAdmitResult{}, err
		}
		if rec != nil {
			if rec.Metadata[metaIdemDigest] != digest {
				return rigAdmitResult{}, &requestIDConflictError{RequestID: body.RequestID} // row 4
			}
			switch rec.Metadata[metaIdemState] {
			case idemStateSucceeded: // row 5: existing, served from the record
				return rigAdmitResult{
					outcome:     rigAdmitExisting,
					requestID:   body.RequestID,
					eventCursor: rec.Metadata[metaIdemEventCursor],
					record:      rec,
				}, nil
			case idemStateInFlight, idemStateRolledBack:
				// row 6 (rolled_back) and row 7 (orphan in_flight: the live
				// index missed, so no goroutine is running) both re-clone.
				// C4b's re-clone path drops any leftover dir/DB/config for the
				// rig before it clones (G13 §6 drop-then-mark), which covers a
				// post-boot orphan's un-swept staging.
				return admitFreshLocked(idx, store, cursor, city, body, digest, rec.ID)
			default:
				return rigAdmitResult{}, fmt.Errorf(
					"idem record %s for (%s, %s) has unknown state %q",
					rec.ID, city, body.RequestID, rec.Metadata[metaIdemState])
			}
		}
	}

	// (2) name-collision axis (G13 §4.4): live byName → config → durable scan.
	if live, ok := idx.lookupByName(city, body.Name); ok {
		return rigAdmitResult{}, &rigNameConflictError{ // row 8
			Rig:               body.Name,
			InFlightRequestID: live.requestID,
			InFlightCursor:    live.eventCursor,
		}
	}
	if rigInConfig != nil && rigInConfig(body.Name) {
		return rigAdmitResult{}, &rigNameConflictError{Rig: body.Name} // row 8
	}
	hit, err := durableRigNameScan(store, city, body.Name)
	if err != nil {
		return rigAdmitResult{}, err
	}
	if hit {
		return rigAdmitResult{}, &rigNameConflictError{Rig: body.Name} // row 8 backstop
	}

	// (3) admit new (rows 3, 9).
	return admitFreshLocked(idx, store, cursor, city, body, digest, "")
}

// admitFreshLocked captures the event cursor, reserves or resets the durable
// record, registers a live entry, and returns the New or Reclone result. The
// cursor is captured strictly before the entry is registered (and, in C4b,
// before the goroutine) so the client's after_seq never misses the terminal
// event (G13 §5). existingBeadID is "" for a brand-new admission, or the id of
// the record being re-cloned. An absent client request_id mints a synthetic
// correlation id (newRequestID) and creates NO durable record — name
// protection via byName never depends on the dedup opt-in (G13 §1/§3.5).
func admitFreshLocked(
	idx *rigIdemIndex,
	store beads.Store,
	cursor func() (string, error),
	city string,
	body RigCreateBody,
	digest, existingBeadID string,
) (rigAdmitResult, error) {
	cur, err := cursor()
	if err != nil {
		return rigAdmitResult{}, err
	}

	requestID, synthetic := body.RequestID, false
	if requestID == "" {
		requestID, err = newRequestID()
		if err != nil {
			return rigAdmitResult{}, err
		}
		synthetic = true
	}

	beadID := existingBeadID
	switch {
	case synthetic:
		// No durable record — correlation only (G13 §1).
	case existingBeadID != "":
		// Re-clone: reset the durable record to in_flight with a fresh cursor
		// (G13 §4.2/§5.3).
		if err := store.SetMetadataBatch(existingBeadID, map[string]string{
			metaIdemState:       idemStateInFlight,
			metaIdemEventCursor: cur,
		}); err != nil {
			return rigAdmitResult{}, fmt.Errorf("resetting idem record %s for re-clone: %w", existingBeadID, err)
		}
	default:
		// Brand new: reserve the durable record.
		beadID, err = createIdemRecord(store, city, requestID, digest, cur, body.Name, idemStateInFlight)
		if err != nil {
			return rigAdmitResult{}, err
		}
	}

	entry := &liveProvision{
		requestID:   requestID,
		digest:      digest,
		eventCursor: cur,
		rigName:     body.Name,
		beadID:      beadID,
		synthetic:   synthetic,
		done:        make(chan struct{}),
	}
	idx.register(city, entry)

	outcome := rigAdmitNew
	if existingBeadID != "" {
		outcome = rigAdmitReclone
	}
	return rigAdmitResult{
		outcome:     outcome,
		requestID:   requestID,
		eventCursor: cur,
		entry:       entry,
	}, nil
}
