package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
)

// rootWalkLimit caps the parent_id walk that maps a bead to its root during the
// ErrRootMigrating write-gate. Control molecules are shallow; the cap only guards
// against a pathological or cyclic parent chain so the guard can never loop.
const rootWalkLimit = 256

// errCrossResidenceDependency reports that a dependency (or a graph-apply plan)
// would link two beads that live on different residence legs. Cross-store
// dependency edges are forbidden during the journal-migration window
// (08-blocker-resolutions §B1): a graph node that must wait on a bead in the
// other leg uses a typed wait-reference + settlement event (P4/P5), never a
// cross-leg DepAdd. It is unreachable at P1 exit (no journal-resident graph
// beads exist on v1/v2 paths) and fails loudly rather than corrupting either
// leg if it ever fires.
var errCrossResidenceDependency = errors.New("cross-residence dependency rejected: graph beads on different residence legs cannot share a dependency edge")

// residenceRoutingGraphStore is the graph-class beads.Store during the
// journal-migration window (ADR-0001 §2, 08-blocker-resolutions §B1):
// journal-resident roots are served by the journal leg, everything else by the
// legacy leg. Residence is root-atomic and consulted per id via a journal
// membership probe; it is never a prefix split. Both legs arrive already
// policy-wrapped, so this store composes them without re-wrapping.
//
// Overlay contract (HIGH-1): this router's global reads return legacy ∪ journal
// (List/Ready/ListByLabel fan both legs out and dedupe), so a caller that also
// iterated the legacy leg (the city store) separately would count every
// legacy-resident bead twice. The router therefore implements beads.StoreOverlay
// (OverlaysStore below): the API projection/delete arms that build a store list
// from GraphBeadStore()+CityBeadStore() (workflowStores in
// handler_convoy_dispatch.go, consumed by orders_feed.go and
// huma_handlers_convoys.go) drop the redundant city entry when this router
// overlays it, so each workflow root is projected once and deleted once.
//
// P2: this router does not yet forward the optional ConditionalAssignmentReleaser
// or Counter capabilities to the routed leg (HandlesFor's Writer is the router,
// which lacks them). Those capability drops are latent — no opted city exercises
// them at P1 exit — and are wired in the P2 capability-forwarding pass.
type residenceRoutingGraphStore struct {
	journal beads.Store // JournalStore adapter, policy-wrapped (never nil)
	legacy  beads.Store // what resolveGraphStore returned before P1.5 (never nil)

	// residence is the journal leg's explicit tri-state residence record (P3.2).
	// It is nil when the journal leg does not expose the capability (e.g. a
	// MemStore test leg), in which case the ErrRootMigrating write-gate is inert.
	// It is what makes routing consult the EXPLICIT record instead of the implicit
	// membership probe: a `migrating` root's half-copied journal rows are hidden by
	// the JournalStore visibility gate (so residentBead already routes them
	// legacy), and controller writes to such a root are blocked here.
	residence beads.ResidenceStore

	// cityPath is the city root this router serves, used only to stat the P3.3
	// generational cutover marker (.gc/graph/cutover) when minting a NEW root. It
	// is "" for a router with no city context (the legacy two-arg constructor and
	// most unit tests), which reads as never-armed — so a new root mints legacy,
	// byte-identical to the pre-P3.3 behavior. When it is set and the marker is
	// present, new roots are born journal-resident (see newRootLeg).
	cityPath string
}

var (
	_ beads.Store                            = (*residenceRoutingGraphStore)(nil)
	_ beads.StoreOverlay                     = (*residenceRoutingGraphStore)(nil)
	_ beads.GraphApplyHandleProvider         = (*residenceRoutingGraphStore)(nil)
	_ beads.ControlFrontierHandleProvider    = (*residenceRoutingGraphStore)(nil)
	_ beads.AppendLogHandleProvider          = (*residenceRoutingGraphStore)(nil)
	_ beads.ConditionalVersionHandleProvider = (*residenceRoutingGraphStore)(nil)
	_ beads.CachedReader                     = residenceRoutingReader{}
	_ beads.LiveReader                       = residenceRoutingReader{}
)

// newResidenceRoutingGraphStore composes a journal leg and a legacy leg into a
// residence-routing graph store with no city context (cityPath ""). New roots
// mint legacy — the pre-P3.3 inert policy — which is what most unit tests want.
// Constructed per accessor call (cheap); the long-lived handles live on the
// controller state / one-shot cache, so the reload-swap story keeps working
// without router invalidation.
func newResidenceRoutingGraphStore(journal, legacy beads.Store) *residenceRoutingGraphStore {
	return newResidenceRoutingGraphStoreForCity(journal, legacy, "")
}

// newResidenceRoutingGraphStoreForCity is newResidenceRoutingGraphStore that also
// carries the city root, so the router can honor the P3.3 cutover marker when
// minting new roots. Production resolveGraphStore uses this form; the cutover
// marker is stat'd live per new-root create, keeping marker removal reversible
// without a restart.
func newResidenceRoutingGraphStoreForCity(journal, legacy beads.Store, cityPath string) *residenceRoutingGraphStore {
	residence, _ := beads.ResidenceStoreFor(journal)
	return &residenceRoutingGraphStore{journal: journal, legacy: legacy, residence: residence, cityPath: cityPath}
}

// guardNotMigrating blocks a controller-path write to a bead whose root is
// mid-migration (residence `migrating`) with beads.ErrRootMigrating (09a §A-2
// step 1a). It short-circuits on the common path: when no root is migrating (the
// residence capability is absent, or MigratingRoots is empty) it does zero extra
// work per write. Only when a migration is actually in flight does it resolve the
// bead's root by walking the legacy parent chain (a `migrating` bead still lives
// on the legacy leg — its journal copy is hidden) and block if that root matches.
// A hard error resolving residence fails the write loudly rather than letting a
// racing write slip past the quarantine.
func (s *residenceRoutingGraphStore) guardNotMigrating(id string) error {
	if s.residence == nil {
		return nil
	}
	roots, err := s.residence.MigratingRoots(context.Background())
	if err != nil {
		return fmt.Errorf("residence routing: checking migrating roots: %w", err)
	}
	if len(roots) == 0 {
		return nil
	}
	root, err := s.rootOfLegacy(id)
	if err != nil {
		return err
	}
	if slices.Contains(roots, root) {
		return fmt.Errorf("%w: root %q", beads.ErrRootMigrating, root)
	}
	return nil
}

// rootOfLegacy walks id's parent_id chain on the legacy leg up to the root (the
// first bead with no parent). A `migrating` bead is still legacy-resident, so the
// walk resolves there. A bead that is absent legacy-side (already journal-resident
// or unknown) resolves to itself, which no migrating root matches. The walk is
// depth-capped so a cyclic parent chain can never loop.
func (s *residenceRoutingGraphStore) rootOfLegacy(id string) (string, error) {
	current := id
	for i := 0; i < rootWalkLimit; i++ {
		b, err := s.legacy.Get(current)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return current, nil
			}
			return "", fmt.Errorf("residence routing: resolving root of %q: %w", id, err)
		}
		if b.ParentID == "" || b.ParentID == current {
			return current, nil
		}
		current = b.ParentID
	}
	return current, nil
}

// OverlaysStore reports whether other is this router's legacy (non-journal) leg
// — the store whose beads the router already returns via fan-out (its global
// reads are legacy ∪ journal, and its by-id reads route to the owning leg). When
// it is, a caller that iterated both this router and other would project/delete
// every legacy-resident bead twice, so the caller drops the redundant leg. Only
// the legacy leg can be an overlay target: journal-resident beads have no
// separate entry to conflate with. Identity is deliberate — the legacy leg is
// the exact store pointer resolveGraphStore held before wrapping it, which is
// the same value CityBeadStore() returns on a non-relocated-graph city, so a
// graph-split (disjoint) legacy leg correctly reports no overlap with the city
// store.
func (s *residenceRoutingGraphStore) OverlaysStore(other beads.Store) bool {
	return other != nil && s.legacy == other
}

// residentBead probes the journal leg for id. Returns (bead, true, nil) when
// journal-resident, (zero, false, nil) on a clean miss, and (zero, false, err)
// on a hard failure — which write callers MUST NOT flatten into "legacy". The
// probe reads through the journal leg's authoritative live handle so a cached
// wrapper can never mask residence.
func (s *residenceRoutingGraphStore) residentBead(id string) (beads.Bead, bool, error) {
	got, err := beads.HandlesFor(s.journal).Live.Get(id)
	switch {
	case err == nil:
		return got, true, nil
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	default:
		return beads.Bead{}, false, err
	}
}

// resolveLeg returns the store that owns id and whether it is the journal leg.
// A hard probe error is returned and MUST NOT be flattened into "legacy".
func (s *residenceRoutingGraphStore) resolveLeg(id string) (leg beads.Store, journal bool, err error) {
	_, resident, err := s.residentBead(id)
	if err != nil {
		return nil, false, err
	}
	if resident {
		return s.journal, true, nil
	}
	return s.legacy, false, nil
}

// legFor is resolveLeg without the leg-identity flag, for the by-id write ops.
func (s *residenceRoutingGraphStore) legFor(id string) (beads.Store, error) {
	leg, _, err := s.resolveLeg(id)
	return leg, err
}

// --- by-id reads -----------------------------------------------------------

// Get returns the bead by id. Journal-first probe (storeref.Resolve discipline):
// a journal hit returns immediately; a clean miss falls to the legacy leg; a
// hard journal probe error is preserved and surfaced only if the legacy leg
// also misses, so an unreachable journal never looks like a deleted bead.
func (s *residenceRoutingGraphStore) Get(id string) (beads.Bead, error) {
	got, resident, err := s.residentBead(id)
	if resident {
		return got, nil
	}
	legGot, legErr := s.legacy.Get(id)
	if legErr == nil {
		return legGot, nil
	}
	if err != nil && errors.Is(legErr, beads.ErrNotFound) {
		return beads.Bead{}, err
	}
	return beads.Bead{}, legErr
}

// DepList routes by the bead's residence; a clean miss reads the legacy leg.
func (s *residenceRoutingGraphStore) DepList(id, direction string) ([]beads.Dep, error) {
	leg, err := s.legFor(id)
	if err != nil {
		return nil, err
	}
	return leg.DepList(id, direction)
}

// Children routes by the parent's residence (root-atomic co-residence beats
// fan-out); a parent that is resident nowhere reads the legacy leg's result.
func (s *residenceRoutingGraphStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	leg, err := s.legFor(parentID)
	if err != nil {
		return nil, err
	}
	return leg.Children(parentID, opts...)
}

// --- by-id writes (hard probe error fails the write) -----------------------

// Update routes by residence; a migrating root is blocked, a hard probe error
// fails the write.
func (s *residenceRoutingGraphStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.Update(id, opts)
}

// Close routes by residence; a migrating root is blocked, a hard probe error
// fails the write.
func (s *residenceRoutingGraphStore) Close(id string) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.Close(id)
}

// Reopen routes by residence; a migrating root is blocked, a hard probe error
// fails the write.
func (s *residenceRoutingGraphStore) Reopen(id string) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.Reopen(id)
}

// SetMetadata routes by residence; a migrating root is blocked, a hard probe
// error fails the write.
func (s *residenceRoutingGraphStore) SetMetadata(id, key, value string) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.SetMetadata(id, key, value)
}

// SetMetadataBatch routes by residence; a migrating root is blocked, a hard probe
// error fails the write.
func (s *residenceRoutingGraphStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.SetMetadataBatch(id, kvs)
}

// Delete routes by residence; a migrating root is blocked, a hard probe error
// fails the write.
func (s *residenceRoutingGraphStore) Delete(id string) error {
	if err := s.guardNotMigrating(id); err != nil {
		return err
	}
	leg, err := s.legFor(id)
	if err != nil {
		return err
	}
	return leg.Delete(id)
}

// CloseAll partitions ids by residence, closes each subset on its owning leg,
// and sums the counts. A hard probe error aborts before any close; the first
// close error (legacy first) wins after both legs are attempted.
func (s *residenceRoutingGraphStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	var journalIDs, legacyIDs []string
	for _, id := range ids {
		if err := s.guardNotMigrating(id); err != nil {
			return 0, err
		}
		_, journal, err := s.resolveLeg(id)
		if err != nil {
			return 0, err
		}
		if journal {
			journalIDs = append(journalIDs, id)
		} else {
			legacyIDs = append(legacyIDs, id)
		}
	}
	total := 0
	nLegacy, errLegacy := s.legacy.CloseAll(legacyIDs, metadata)
	total += nLegacy
	nJournal, errJournal := s.journal.CloseAll(journalIDs, metadata)
	total += nJournal
	if errLegacy != nil {
		return total, errLegacy
	}
	return total, errJournal
}

// --- creates ---------------------------------------------------------------

// newRootLeg returns the leg a brand-new root (no ParentID) is minted on. Before
// the P3.3 generational cutover marker is armed it is the legacy leg — the
// P1.5/P2 inert policy, byte-identical to the pre-cutover behavior. Once
// `gc migrate graph-journal cutover --parity-verified` writes .gc/graph/cutover,
// a new root is born journal-resident: it mints a genuinely journal-resident root
// (a fresh gcg-j<seq> id) on the journal leg, with no residence record needed —
// journal membership IS its residence (residentBead finds it on the journal leg
// and every child co-resides there). The marker is stat'd here, per new-root
// create, so removing it restores legacy minting immediately (generational — any
// root already born on the journal leg stays journal).
func (s *residenceRoutingGraphStore) newRootLeg() beads.Store {
	if cityGraphCutoverArmed(s.cityPath) {
		return s.journal
	}
	return s.legacy
}

// Create routes a child by its parent's residence (root-atomic co-residence); a
// new root (no ParentID) mints on newRootLeg — legacy until the cutover marker
// arms journal minting. A child create under a `migrating` root is blocked with
// ErrRootMigrating, the same fence the by-id mutations apply, so a new step cannot
// slip onto the old leg mid-copy and be silently absent from the journal copy
// (BLOCKER-1).
func (s *residenceRoutingGraphStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ParentID == "" {
		return s.newRootLeg().Create(b)
	}
	if err := s.guardNotMigrating(b.ParentID); err != nil {
		return beads.Bead{}, err
	}
	leg, err := s.legFor(b.ParentID)
	if err != nil {
		return beads.Bead{}, err
	}
	return leg.Create(b)
}

// CreateWithStorage routes like Create and preserves the policy-selected storage
// tier, delegating to the routed leg's StorageCreateStore when it asserts one.
func (s *residenceRoutingGraphStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	var leg beads.Store
	if b.ParentID == "" {
		// Only a NEW root consults the cutover marker (an os.Stat inside newRootLeg);
		// a child routes by its parent's residence, so it must not pay that stat.
		leg = s.newRootLeg()
	} else {
		if err := s.guardNotMigrating(b.ParentID); err != nil {
			return beads.Bead{}, err
		}
		routed, err := s.legFor(b.ParentID)
		if err != nil {
			return beads.Bead{}, err
		}
		leg = routed
	}
	if sc, ok := leg.(beads.StorageCreateStore); ok {
		return sc.CreateWithStorage(b, storage)
	}
	return leg.Create(b)
}

// --- dependencies (both ends must agree on residence) ----------------------

// DepAdd rejects a dependency whose two ends live on different residence legs;
// otherwise it routes the edge to the shared leg.
func (s *residenceRoutingGraphStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.guardNotMigrating(issueID); err != nil {
		return err
	}
	leg, err := s.agreeingLeg(issueID, dependsOnID)
	if err != nil {
		return err
	}
	return leg.DepAdd(issueID, dependsOnID, depType)
}

// DepRemove rejects a removal spanning both legs; otherwise routes to the
// shared leg.
func (s *residenceRoutingGraphStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.guardNotMigrating(issueID); err != nil {
		return err
	}
	leg, err := s.agreeingLeg(issueID, dependsOnID)
	if err != nil {
		return err
	}
	return leg.DepRemove(issueID, dependsOnID)
}

// agreeingLeg resolves both ids' residence and requires them to agree. Mixed
// residence returns errCrossResidenceDependency; a hard probe error is
// preserved. Neither leg is written when the ends disagree.
func (s *residenceRoutingGraphStore) agreeingLeg(a, b string) (beads.Store, error) {
	_, aJournal, err := s.resolveLeg(a)
	if err != nil {
		return nil, err
	}
	_, bJournal, err := s.resolveLeg(b)
	if err != nil {
		return nil, err
	}
	if aJournal != bJournal {
		return nil, fmt.Errorf("%w: %q and %q", errCrossResidenceDependency, a, b)
	}
	if aJournal {
		return s.journal, nil
	}
	return s.legacy, nil
}

// --- global reads (fan-out + dedupe + global merge-sort + limit) -----------
//
// Each fan-out sends the query (Limit included) to BOTH legs, so a naive
// concatenation could return up to 2×Limit rows and would always order journal
// rows after legacy rows regardless of priority/created_at (MEDIUM-3). Every
// method below therefore merge-sorts the deduped union into the SAME global
// order the single-store reader imposes, THEN truncates to the effective Limit.
// Dedupe keeps the legacy row when an id appears on both legs; a hard error on
// either leg fails the whole call — a silently missing leg is a wrong answer.

// List fans out, dedupes, merge-sorts by the query's Sort contract, and caps at
// query.Limit.
func (s *residenceRoutingGraphStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	legacy, err := s.legacy.List(query)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.List(query)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, query.Sort) }, query.Limit), nil
}

// ListOpen fans out and merge-sorts by the documented default (creation-order)
// so journal rows interleave with legacy rows rather than always trailing.
func (s *residenceRoutingGraphStore) ListOpen(status ...string) ([]beads.Bead, error) {
	legacy, err := s.legacy.ListOpen(status...)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.ListOpen(status...)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, beads.SortCreatedAsc) }, 0), nil
}

// Ready fans out, dedupes, merge-sorts into canonical ready order
// (priority, created_at, id), and caps at the query's Limit — so a bounded ready
// read cuts the same globally-ordered prefix regardless of which leg served each
// row.
func (s *residenceRoutingGraphStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	legacy, err := s.legacy.Ready(query...)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.Ready(query...)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, beads.SortBeadsReady, readyQueryLimit(query)), nil
}

// ListByLabel fans out and merge-sorts newest-first (the single-store contract),
// capped at limit.
func (s *residenceRoutingGraphStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	legacy, err := s.legacy.ListByLabel(label, limit, opts...)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.ListByLabel(label, limit, opts...)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, beads.SortCreatedDesc) }, limit), nil
}

// ListByAssignee fans out and merge-sorts newest-first, capped at limit.
func (s *residenceRoutingGraphStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	legacy, err := s.legacy.ListByAssignee(assignee, status, limit)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.ListByAssignee(assignee, status, limit)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, beads.SortCreatedDesc) }, limit), nil
}

// ListByMetadata fans out and merge-sorts newest-first, capped at limit.
func (s *residenceRoutingGraphStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	legacy, err := s.legacy.ListByMetadata(filters, limit, opts...)
	if err != nil {
		return nil, err
	}
	journal, err := s.journal.ListByMetadata(filters, limit, opts...)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, beads.SortCreatedDesc) }, limit), nil
}

// readyQueryLimit returns the Limit of the first ReadyQuery, or 0 (unlimited)
// when none was supplied — mirroring the single-store Ready limit semantics.
func readyQueryLimit(query []beads.ReadyQuery) int {
	if len(query) == 0 {
		return 0
	}
	return query[0].Limit
}

// mergeSortLimitBeads dedupes the two legs' rows (legacy wins on a shared id),
// applies sortFn to impose one global order over the union, then truncates to
// limit (0 = unlimited). This is what keeps a fan-out from returning up to
// 2×limit rows or ordering journal rows unconditionally after legacy rows.
func mergeSortLimitBeads(legacy, journal []beads.Bead, sortFn func([]beads.Bead), limit int) []beads.Bead {
	merged := mergeDedupeBeads(legacy, journal)
	if sortFn != nil {
		sortFn(merged)
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// mergeDedupeBeads unions the two legs' rows, preferring the JOURNAL row on a
// shared id (P3.2, Risk 1 — residence-aware dedupe). Residence normally makes the
// sets disjoint, so this is a belt over that suspenders; the one window where an
// id appears on BOTH legs is the flip→tombstone crash window, where the residence
// record already says `journal` and the legacy row is a stale not-yet-tombstoned
// copy. Journal-wins is exactly correct there, and safe everywhere else: the
// JournalStore's residence-visibility gate hides a `migrating` root's rows, so any
// row the journal leg actually RETURNS is authoritative (flipped or journal-born)
// — never a half-copied migrating one. The returned slice is freshly owned, so
// callers may sort it in place. A caller-supplied sortFn re-imposes global order,
// so the interleave order here is not load-bearing.
func mergeDedupeBeads(legacy, journal []beads.Bead) []beads.Bead {
	if len(journal) == 0 {
		return legacy
	}
	seen := make(map[string]struct{}, len(journal))
	out := make([]beads.Bead, 0, len(legacy)+len(journal))
	for _, b := range journal {
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}
	for _, b := range legacy {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		out = append(out, b)
	}
	return out
}

// --- transactions & liveness -----------------------------------------------

// Tx routes to the legacy leg at P1.5. Journal-side transactional writes go
// through ApplyGraphPlan / the journal adapter's own capabilities; no
// journal-resident bead reaches a Tx call site until P3 migrates roots.
//
// NOTE-1 (unguarded by design): the fn callback is opaque, so guardNotMigrating
// cannot inspect which beads it mutates — a controller Tx touching a migrating root
// therefore bypasses the migrating fence and is covered only by the same
// detect-not-prevent backstop as an external bd writer (re-verify + ensureTombstone's
// pre-close/post-close hashes). This is not a live control-path hole: control writes
// reach a migrating root through the guarded by-id ops (Update/Close/SetMetadata/…),
// not through Tx, which serves the legacy leg's own transactional API.
func (s *residenceRoutingGraphStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	return s.legacy.Tx(commitMsg, fn)
}

// Ping probes the legacy leg then the journal leg, returning the first error so
// an opted city never silently loses its journal leg.
func (s *residenceRoutingGraphStore) Ping() error {
	if err := s.legacy.Ping(); err != nil {
		return err
	}
	return s.journal.Ping()
}

// --- explicit read handles (residence-routed) ------------------------------

// Handles residence-routes the explicit cached/live read handles instead of
// letting HandlesFor(router) fall back to logical wrappers over the router's
// own Store methods. Those wrappers would degrade every Live.Get / Live.DepList
// on a legacy id to the legacy leg's plain (cache-tier) Get — breaking the live
// parent/attachment reads wisp_autoclose.go depends on (MEDIUM-1b). Here each
// by-id read routes to the OWNING leg's own live/cached handle, and each global
// read fans the leg handles out with the same merge-sort discipline as the
// Store-level reads. The Writer stays the router so writes keep routing by
// residence.
func (s *residenceRoutingGraphStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: residenceRoutingReader{s: s, live: false},
		Live:   residenceRoutingReader{s: s, live: true},
		Writer: s,
	}
}

// residenceRoutingReader satisfies both beads.CachedReader and beads.LiveReader
// (identical method sets). live selects which tier handle of the owning leg a
// read resolves to; residence itself is always determined by the authoritative
// journal live probe (residentBead), so a cached wrapper can never mask it.
type residenceRoutingReader struct {
	s    *residenceRoutingGraphStore
	live bool
}

// tierReader is the shared method set of beads.LiveReader and beads.CachedReader,
// so a single routing reader can dispatch to either leg's tier handle.
type tierReader interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	Ready(query ...beads.ReadyQuery) ([]beads.Bead, error)
	DepList(id, direction string) ([]beads.Dep, error)
}

// legHandle returns the owning leg's live or cached tier handle per r.live.
func (r residenceRoutingReader) legHandle(leg beads.Store) tierReader {
	h := beads.HandlesFor(leg)
	if r.live {
		return h.Live
	}
	return h.Cached
}

// Get resolves residence via the authoritative journal live probe, then reads
// the owning leg's tier handle — so a journal id reaches the journal leg's live
// handle and a legacy id reaches the legacy leg's live handle (never a plain
// cached Get). Reconciliation mirrors the Store-level Get: a hard journal probe
// error surfaces only if the legacy leg also misses.
func (r residenceRoutingReader) Get(id string) (beads.Bead, error) {
	_, resident, probeErr := r.s.residentBead(id)
	if resident {
		return r.legHandle(r.s.journal).Get(id)
	}
	legGot, legErr := r.legHandle(r.s.legacy).Get(id)
	if legErr == nil {
		return legGot, nil
	}
	if probeErr != nil && errors.Is(legErr, beads.ErrNotFound) {
		return beads.Bead{}, probeErr
	}
	return beads.Bead{}, legErr
}

// DepList routes by the bead's residence to the owning leg's tier handle.
func (r residenceRoutingReader) DepList(id, direction string) ([]beads.Dep, error) {
	leg, err := r.s.legFor(id)
	if err != nil {
		return nil, err
	}
	return r.legHandle(leg).DepList(id, direction)
}

// List fans both legs' tier handles out and merge-sorts by the query contract.
func (r residenceRoutingReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	legacy, err := r.legHandle(r.s.legacy).List(query)
	if err != nil {
		return nil, err
	}
	journal, err := r.legHandle(r.s.journal).List(query)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, func(b []beads.Bead) { beads.SortBeads(b, query.Sort) }, query.Limit), nil
}

// Ready fans both legs' tier handles out and merge-sorts into ready order.
func (r residenceRoutingReader) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	legacy, err := r.legHandle(r.s.legacy).Ready(query...)
	if err != nil {
		return nil, err
	}
	journal, err := r.legHandle(r.s.journal).Ready(query...)
	if err != nil {
		return nil, err
	}
	return mergeSortLimitBeads(legacy, journal, beads.SortBeadsReady, readyQueryLimit(query)), nil
}

// --- graph apply -----------------------------------------------------------

// GraphApplyHandle exposes a routing graph-applier so callers that assert the
// graph-apply capability on the resolveGraphStore result reach residence
// routing instead of one fixed leg.
func (s *residenceRoutingGraphStore) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	return graphRoutingApplier{s: s}, true
}

// ControlFrontierHandle exposes the JOURNAL leg's control-dispatcher frontier
// read. ControlFrontier is a journal-only capability: it is an indexed SELECT
// over the journal projection tables and never reads the legacy leg (legacy
// roots keep the `bd | jq` serve-tick frontier). So the router forwards straight
// to the journal leg's capability rather than composing both legs; the serve
// tick unions the journal frontier with the legacy `bd | jq` frontier itself
// (dispatch_journal_frontier.go). Returns (nil, false) when the journal leg does
// not expose the capability.
func (s *residenceRoutingGraphStore) ControlFrontierHandle() (beads.ControlFrontierStore, bool) {
	return beads.ControlFrontierStoreFor(s.journal)
}

// AppendLogHandle and ConditionalVersionHandle expose the JOURNAL leg's journal
// CAS capabilities, which back the control-epoch fence. Like ControlFrontier
// these are journal-only surfaces — they operate on the journal event streams,
// a data domain the legacy leg does not have — so the router forwards straight
// to the journal leg rather than composing both. Without these forwards, a
// journal-resident control bead reached through this router would probe caps
// as absent and the fence would (before this) silently degrade to an unfenced
// write; now the fence treats that as a loud wiring bug, so the forwards are
// what keep an opted city's control writes actually fenced. Returns
// (nil, false) when the journal leg does not expose the capability.
func (s *residenceRoutingGraphStore) AppendLogHandle() (beads.AppendLogStore, bool) {
	return beads.AppendLogStoreFor(s.journal)
}

func (s *residenceRoutingGraphStore) ConditionalVersionHandle() (beads.ConditionalVersionStore, bool) {
	return beads.ConditionalVersionStoreFor(s.journal)
}

// graphRoutingApplier routes a graph-apply plan to the leg its anchors reside
// on. An un-anchored plan (a brand-new root — the dominant molecule/formula-v2
// root-mint path) applies to newRootLeg(): the journal leg once the P3.3 cutover
// marker is armed, the legacy leg otherwise — the same new-root policy as Create.
type graphRoutingApplier struct {
	s *residenceRoutingGraphStore
}

var _ beads.GraphApplyStore = graphRoutingApplier{}

// ApplyGraphPlan resolves the residence of every existing-bead anchor in the
// plan (node ParentIDs, edge FromIDs/ToIDs). Anchors that disagree are rejected
// as cross-residence; an anchored plan routes to that leg; an un-anchored plan (a
// new root) routes to newRootLeg() — journal when the cutover marker is armed,
// legacy otherwise.
func (a graphRoutingApplier) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}
	anchors := collectPlanAnchors(plan)
	for _, anchor := range anchors {
		if err := a.s.guardNotMigrating(anchor); err != nil {
			return nil, err
		}
	}
	leg, err := a.s.applyLeg(anchors)
	if err != nil {
		return nil, err
	}
	applier, ok := beads.GraphApplyFor(leg)
	if !ok {
		return nil, fmt.Errorf("graph apply: residence leg lacks graph-apply capability")
	}
	return applier.ApplyGraphPlan(ctx, plan)
}

// applyLeg returns the leg a graph-apply plan routes to. No anchors means a
// brand-new root — the dominant molecule/formula-v2 root-mint shape, whose
// workflow root deliberately carries no ParentID anchor (graph_apply.go's
// KindWorkflow rule) — so it mints on newRootLeg(): journal once the P3.3 cutover
// marker is armed, legacy otherwise, MATCHING Create's new-root policy. An
// all-journal or all-legacy anchor set routes to that leg (the plan's own
// ParentKey children co-reside there); anchors spanning both legs →
// cross-residence error.
func (s *residenceRoutingGraphStore) applyLeg(anchors []string) (beads.Store, error) {
	if len(anchors) == 0 {
		return s.newRootLeg(), nil
	}
	var haveJournal, haveLegacy bool
	for _, id := range anchors {
		_, journal, err := s.resolveLeg(id)
		if err != nil {
			return nil, err
		}
		if journal {
			haveJournal = true
		} else {
			haveLegacy = true
		}
	}
	if haveJournal && haveLegacy {
		return nil, fmt.Errorf("%w: graph apply plan anchors span both residence legs", errCrossResidenceDependency)
	}
	if haveJournal {
		return s.journal, nil
	}
	return s.legacy, nil
}

// collectPlanAnchors returns the deduped set of existing-bead ids the plan
// references: node ParentIDs and edge FromIDs/ToIDs. Intra-plan symbolic keys
// (ParentKey/FromKey/ToKey) are not anchors — they resolve to nodes minted by
// the plan itself.
func collectPlanAnchors(plan *beads.GraphApplyPlan) []string {
	seen := make(map[string]struct{})
	var anchors []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		anchors = append(anchors, id)
	}
	for _, node := range plan.Nodes {
		add(node.ParentID)
	}
	for _, edge := range plan.Edges {
		add(edge.FromID)
		add(edge.ToID)
	}
	return anchors
}
