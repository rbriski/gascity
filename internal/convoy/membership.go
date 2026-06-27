package convoy

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TrackingDepType is the dependency type used for convoy membership edges.
const TrackingDepType = "tracks"

const trackedStatusUnknown = "unknown"

// IsTerminalStatus reports whether a tracked item should count as complete for
// convoy progress and auto-close decisions.
func IsTerminalStatus(status string) bool {
	return status == "closed" || status == "tombstone"
}

// TrackItem records that convoyID tracks itemID without changing itemID's
// parent-child relationship.
//
// A synthetic (graph-class) convoy can track a work-class member that physically
// lives in a different store, so the itemID existence pre-check resolves across the
// convoy's own store plus any memberStores the caller supplies — the class-aware
// successor to coordrouter.Router's federated by-id read. The tracks DepAdd stays on
// store (the convoy bead's home), where the edge belongs. memberStores is variadic
// so same-class callers (convoy and members co-resident) pass nothing and are
// byte-identical to the prior single-store behavior.
func TrackItem(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) error {
	if _, err := storeref.Resolve(itemID, append([]beads.Store{store}, memberStores...)); err != nil {
		return fmt.Errorf("getting tracked item %s: %w", itemID, err)
	}
	if err := store.DepAdd(convoyID, itemID, TrackingDepType); err != nil {
		return fmt.Errorf("adding %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// UntrackItem removes a convoy membership edge from convoyID to itemID.
func UntrackItem(store beads.Store, convoyID, itemID string) error {
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	hasTrack := false
	var mixedTypes []string
	for _, dep := range deps {
		if dep.IssueID != convoyID || dep.DependsOnID != itemID {
			continue
		}
		if dep.Type == TrackingDepType {
			hasTrack = true
			continue
		}
		mixedTypes = append(mixedTypes, dep.Type)
	}
	if !hasTrack {
		return nil
	}
	if len(mixedTypes) > 0 {
		return fmt.Errorf("not removing ambiguous %s dependency %s -> %s with other dependency types: %v", TrackingDepType, convoyID, itemID, mixedTypes)
	}
	if err := store.DepRemove(convoyID, itemID); err != nil {
		return fmt.Errorf("removing %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// Members returns beads tracked by a convoy. It supports both the current
// tracks dependency relation and legacy parent-child convoy membership.
// Unresolved tracks dependencies are returned with unknown status so completion
// paths never mistake missing dependency details for completed work.
//
// A synthetic (graph-class) convoy can track work-class members in a different
// store, so each tracked member is resolved across the convoy's own store plus any
// memberStores the caller supplies (the class-aware successor to the Router's
// federated member read). The convoy's own DepList/List stay on store (the convoy
// bead's home). memberStores is variadic so same-class callers pass nothing and stay
// byte-identical to the prior single-store behavior.
func Members(store beads.Store, convoyID string, includeClosed bool, memberStores ...beads.Store) ([]beads.Bead, error) {
	memberResolveStores := append([]beads.Store{store}, memberStores...)
	legacyChildren, err := store.List(beads.ListQuery{
		ParentID:      convoyID,
		IncludeClosed: includeClosed,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing legacy convoy children of %s: %w", convoyID, err)
	}

	seen := make(map[string]bool, len(legacyChildren))
	members := make([]beads.Bead, 0, len(legacyChildren))
	add := func(b beads.Bead) {
		if seen[b.ID] {
			return
		}
		if !includeClosed && IsTerminalStatus(b.Status) {
			return
		}
		seen[b.ID] = true
		members = append(members, b)
	}
	for _, child := range legacyChildren {
		add(child)
	}

	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	for _, dep := range deps {
		if dep.Type != TrackingDepType {
			continue
		}
		item, err := storeref.Resolve(dep.DependsOnID, memberResolveStores)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				add(unresolvedTrackedItem(dep.DependsOnID))
				continue
			}
			return nil, fmt.Errorf("getting tracked item %s: %w", dep.DependsOnID, err)
		}
		add(item)
	}

	sortMembers(members)
	return members, nil
}

func unresolvedTrackedItem(id string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  id,
		Type:   "task",
		Status: trackedStatusUnknown,
	}
}

// IsUnresolvedTrackedItem reports whether b is a synthetic placeholder for a
// dangling tracks dependency whose target bead is unavailable.
func IsUnresolvedTrackedItem(b beads.Bead) bool {
	return b.Status == trackedStatusUnknown && b.Type == "task" && b.Title == b.ID
}

// HasTrack reports whether convoyID has a tracks dependency to itemID.
func HasTrack(store beads.Store, convoyID, itemID string) (bool, error) {
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return false, fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	for _, dep := range deps {
		if dep.Type == TrackingDepType && dep.IssueID == convoyID && dep.DependsOnID == itemID {
			return true, nil
		}
	}
	return false, nil
}

// TrackingConvoysForItem returns convoy beads that track itemID via a tracks
// dependency. Dangling dependency sources are ignored.
func TrackingConvoysForItem(store beads.Store, itemID string) ([]beads.Bead, error) {
	deps, err := store.DepList(itemID, "up")
	if err != nil {
		return nil, fmt.Errorf("listing dependents of item %s: %w", itemID, err)
	}

	seen := make(map[string]bool, len(deps))
	convoys := make([]beads.Bead, 0, len(deps))
	for _, dep := range deps {
		if dep.Type != TrackingDepType || seen[dep.IssueID] {
			continue
		}
		b, err := store.Get(dep.IssueID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("getting tracking convoy %s: %w", dep.IssueID, err)
		}
		if b.Type != "convoy" {
			continue
		}
		seen[b.ID] = true
		convoys = append(convoys, b)
	}
	sortMembers(convoys)
	return convoys, nil
}

func sortMembers(items []beads.Bead) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i]
		right := items[j]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return left.ID < right.ID
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
}
