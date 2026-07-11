package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// claimableStore is the composite "claimable work" view over a split city's two
// stores: the work/domain store and the infra store (graph-class control + step
// beads, reserved id-prefix "gcg"). Ready/List fan out across both and merge;
// Get and by-id routing send a bead to the store that owns it — a bead lives in
// exactly one store, keyed by id-prefix, so there is no double-claim.
//
// It is deliberately NOT a beads.Store: a store wrapper would break the
// optional-capability type assertions (GraphApplyFor/HandlesFor/ReadyLive) that
// resolve on concrete store values (see internal/beads/class_store.go's
// "do not promote" note). Composing the two already-wrapped stores keeps each
// leg's capabilities intact. On a legacy single-store city infra is nil and
// every operation collapses to the work store, byte-identical to before.
type claimableStore struct {
	work     beads.Store
	infra    beads.Store // nil on a single-store city
	cityPath string
}

// newClaimableStore opens the work store and, on a split city, the infra store.
// A nil infra (single-store city, or a transient infra open failure that
// cachedCityInfraStore already logged and degraded) collapses the composite to
// the work store.
func newClaimableStore(cityPath string, cfg *config.City) (*claimableStore, error) {
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	return &claimableStore{
		work:     work,
		infra:    cachedCityInfraStore(cityPath, cfg),
		cityPath: cityPath,
	}, nil
}

// legs returns the backing stores to fan out across: the work store always, and
// the infra store when the city is split.
func (c *claimableStore) legs() []beads.Store {
	if c.infra == nil {
		return []beads.Store{c.work}
	}
	return []beads.Store{c.work, c.infra}
}

// storeForID routes a by-id operation to the store that owns beadID: the infra
// store for a reserved coordination-class prefix ("gcg-...") on a split city,
// otherwise the work store. Mirrors slingSourceStoreRootForCandidate's gating so
// the read and write sides agree on ownership.
func (c *claimableStore) storeForID(beadID string) beads.Store {
	if c.infra != nil && config.IsReservedClassPrefix(sling.BeadPrefix(beadID)) {
		return c.infra
	}
	return c.work
}

// Get resolves a bead from its owning store.
func (c *claimableStore) Get(beadID string) (beads.Bead, error) {
	return c.storeForID(beadID).Get(beadID)
}

// Ready returns the merged ready set across the legs, deduped by id and returned
// in the canonical ready order. It FAILS LOUD: if any attempted leg errors, the
// whole read errors. A silent work-only result would hide infra-resident graph
// work — the exact fail-open bug the split introduced — so a broken infra store
// must surface, not degrade to "no work".
func (c *claimableStore) Ready(q beads.ReadyQuery) ([]beads.Bead, error) {
	merged, err := c.fanOut(func(leg beads.Store) ([]beads.Bead, error) {
		return beads.HandlesFor(leg).Live.Ready(q)
	})
	if err != nil {
		return nil, fmt.Errorf("claimable ready: %w", err)
	}
	filtered, err := c.filterCrossStoreAttachBlocked(merged)
	if err != nil {
		return nil, err
	}
	sortReadyBeadsCanonical(filtered)
	return applyBeadLimit(filtered, q.Limit), nil
}

// filterCrossStoreAttachBlocked drops beads that are blocked by an attached
// workflow root in the OTHER store. bd cannot express a cross-store `blocks`
// edge, so `gc formula cook --attach` on a split city stamps
// gc.attached_workflow_root on the work-store source bead instead of a dangling
// dep (landmine #4); the composite ready read is the one seam that owns both
// stores, so it enforces the block here: the parent is claimable only once the
// infra-resident root closes. A dangling marker (root missing in its owning
// store) fails LOUD, matching the composite's fail-loud contract. Beads without
// the marker (the overwhelming majority) pass through untouched.
func (c *claimableStore) filterCrossStoreAttachBlocked(beadsIn []beads.Bead) ([]beads.Bead, error) {
	out := make([]beads.Bead, 0, len(beadsIn))
	for _, b := range beadsIn {
		rootID := strings.TrimSpace(b.Metadata[beadmeta.AttachedWorkflowRootMetadataKey])
		if rootID == "" {
			out = append(out, b)
			continue
		}
		root, err := c.storeForID(rootID).Get(rootID)
		if err != nil {
			return nil, fmt.Errorf("claimable ready: resolving attached workflow root %s for %s: %w", rootID, b.ID, err)
		}
		if root.Status == "closed" {
			out = append(out, b) // DAG finished (pass or fail) → parent unblocked
		}
		// else: root still open → parent blocked, drop it.
	}
	return out, nil
}

// List returns the merged List set across the legs, deduped by id, fail-loud on
// any leg error. It backs the in_progress crash-recovery tier, where a graph step
// assigned to a worker that died lives in the infra store.
func (c *claimableStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	merged, err := c.fanOut(func(leg beads.Store) ([]beads.Bead, error) {
		return beads.HandlesFor(leg).Live.List(q)
	})
	if err != nil {
		return nil, fmt.Errorf("claimable list: %w", err)
	}
	beads.SortBeads(merged, beads.SortCreatedAsc)
	return applyBeadLimit(merged, q.Limit), nil
}

// fanOut runs read against every leg and merges the results, deduped by id in
// leg order (work first). It returns the first leg error unwrapped so callers can
// wrap it with their operation name (fail-loud contract).
func (c *claimableStore) fanOut(read func(beads.Store) ([]beads.Bead, error)) ([]beads.Bead, error) {
	var merged []beads.Bead
	seen := make(map[string]bool)
	for _, leg := range c.legs() {
		rows, err := read(leg)
		if err != nil {
			return nil, err
		}
		for _, b := range rows {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, b)
		}
	}
	return merged, nil
}

func applyBeadLimit(items []beads.Bead, limit int) []beads.Bead {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

// sortReadyBeadsCanonical orders beads by (priority, created_at, id) ascending —
// the same total order the SQL-backed ready readers use (a nil priority sorts as
// 2, matching COALESCE(priority, 2)), so a bounded read cuts the same prefix
// regardless of which leg served each bead. Mirrors internal/beads
// sortBeadsReadyOrder, which is unexported.
func sortReadyBeadsCanonical(items []beads.Bead) {
	sort.SliceStable(items, func(i, j int) bool {
		pi, pj := readyBeadPriority(items[i]), readyBeadPriority(items[j])
		if pi != pj {
			return pi < pj
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

func readyBeadPriority(b beads.Bead) int {
	if b.Priority == nil {
		return 2
	}
	return *b.Priority
}
