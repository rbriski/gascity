package convoy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
// The canonical representation is a ref-by-id: gc.tracking_convoy_id is stamped on
// the ITEM (in its own store), pointing at the convoy. This is cross-store safe — a
// synthetic (graph-class) convoy can track a work-class member in a different store,
// and a metadata string needs no cross-store FK resolution, unlike a `tracks` dep
// whose dep-add must resolve both endpoints in one store's dep table.
//
// For backward compatibility with dep-based readers and dep-graph views, TrackItem
// ALSO adds the legacy `tracks` dep, but ONLY when convoy and item are both resident
// in store (same-store). A cross-store pair skips the dep — the metadata ref is
// authoritative and a cross-store dep-add is exactly the failure this replaces.
// memberStores is variadic so same-store callers pass nothing and behave as before,
// minus the added metadata stamp.
func TrackItem(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) error {
	owner := ownerStore(itemID, append([]beads.Store{store}, memberStores...))
	if owner == nil {
		return fmt.Errorf("getting tracked item %s: %w", itemID, beads.ErrNotFound)
	}
	if err := owner.SetMetadata(itemID, beadmeta.TrackingConvoyIDMetadataKey, convoyID); err != nil {
		return fmt.Errorf("stamping tracking convoy %s on item %s: %w", convoyID, itemID, err)
	}
	// Add the legacy tracks dep only when convoy and item physically co-reside, so a
	// cross-store dep-add (a dep row cannot span two stores) never fires. Compare id
	// prefixes — bd's own cross-prefix classifier routes a differing-prefix target to
	// depends_on_external — which is the exact, federation-proof predicate for "will a
	// same-store dep-add succeed". A store.Get residency probe is NOT safe: a federating
	// handle (beadPolicyStore.Get) resolves a graph convoy through a work store, which
	// reintroduced the exact "resolving gcg-2: no issue found" failure this guards.
	if owner == store && sameStoreByPrefix(convoyID, itemID) {
		if err := store.DepAdd(convoyID, itemID, TrackingDepType); err != nil {
			return fmt.Errorf("adding %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
		}
	}
	return nil
}

// sameStoreByPrefix reports whether two bead ids route to the same physical store,
// by comparing their id prefixes (the segment before the first '-'). It mirrors bd's
// cross-prefix dependency classifier, so it is the precise predicate for whether a
// same-store dep-add will succeed — and, unlike a store.Get residency probe, it is
// immune to a federating read handle.
func sameStoreByPrefix(a, b string) bool {
	return idPrefix(a) == idPrefix(b)
}

func idPrefix(id string) string {
	if i := strings.IndexByte(id, '-'); i >= 0 {
		return id[:i]
	}
	return id
}

// ownerStore returns the first store in stores that holds id, or nil if none do.
// It is the store whose SetMetadata must be used to stamp id's metadata and the
// signal for whether a same-store tracks dep can be added.
func ownerStore(id string, stores []beads.Store) beads.Store {
	for _, s := range stores {
		if s == nil {
			continue
		}
		if _, err := s.Get(id); err == nil {
			return s
		}
	}
	return nil
}

// UntrackItem removes a convoy membership edge from convoyID to itemID. It clears
// the authoritative ref-by-id (gc.tracking_convoy_id on the item, when it points at
// this convoy) and removes the legacy same-store `tracks` dep, keeping the two
// representations in sync. memberStores is variadic so a cross-store (work-class)
// member can be reached; same-store callers pass nothing.
func UntrackItem(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) error {
	// Clear the ref-by-id first — it is authoritative and, for a cross-store member,
	// the only representation (there is no legacy dep to remove). A ref pointing at a
	// different convoy is left untouched, and a missing item is a lenient no-op.
	if owner := ownerStore(itemID, append([]beads.Store{store}, memberStores...)); owner != nil {
		item, err := owner.Get(itemID)
		if err != nil {
			return fmt.Errorf("getting tracked item %s: %w", itemID, err)
		}
		if item.Metadata[beadmeta.TrackingConvoyIDMetadataKey] == convoyID {
			if err := owner.SetMetadata(itemID, beadmeta.TrackingConvoyIDMetadataKey, ""); err != nil {
				return fmt.Errorf("clearing tracking convoy on item %s: %w", itemID, err)
			}
		}
	}

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

	// Ref-by-id members: items that stamped gc.tracking_convoy_id = convoyID (the
	// cross-store-safe representation — no tracks dep needed). Query it across the
	// convoy's own store plus the member stores, since a cross-store member lives in
	// a work store. `add` dedupes, so a same-store member found by both the dep and
	// the metadata collapses to one row.
	for _, s := range memberResolveStores {
		// Span both tiers so a closed member (in the closed tier) is still found; the
		// dep path above is tier-agnostic (DepList returns every edge) and `add`
		// filters terminal members unless includeClosed, so this stays consistent.
		refItems, err := s.ListByMetadata(map[string]string{beadmeta.TrackingConvoyIDMetadataKey: convoyID}, 0, beads.WithBothTiers)
		if err != nil {
			return nil, fmt.Errorf("listing tracking-convoy members of %s: %w", convoyID, err)
		}
		for _, it := range refItems {
			add(it)
		}
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

// TrackingConvoysForItem returns convoy beads that track itemID. It prefers the
// ref-by-id representation — gc.tracking_convoy_id stamped on the item, resolved
// across the item's own store plus any memberStores (the cross-store-safe path) —
// and falls back to the legacy `tracks` dependency for pre-ref-by-id data. Dangling
// sources are ignored. memberStores is variadic so same-store callers pass nothing.
func TrackingConvoysForItem(store beads.Store, itemID string, memberStores ...beads.Store) ([]beads.Bead, error) {
	resolveStores := append([]beads.Store{store}, memberStores...)
	seen := make(map[string]bool)
	convoys := make([]beads.Bead, 0)
	addConvoy := func(b beads.Bead) {
		if b.Type == "convoy" && !seen[b.ID] {
			seen[b.ID] = true
			convoys = append(convoys, b)
		}
	}

	// Ref-by-id: the item points up at its convoy via gc.tracking_convoy_id.
	if item, err := storeref.Resolve(itemID, resolveStores); err == nil {
		if convoyID := item.Metadata[beadmeta.TrackingConvoyIDMetadataKey]; convoyID != "" {
			convoy, cerr := storeref.Resolve(convoyID, resolveStores)
			if cerr == nil {
				addConvoy(convoy)
			} else if !errors.Is(cerr, beads.ErrNotFound) {
				return nil, fmt.Errorf("getting tracking convoy %s: %w", convoyID, cerr)
			}
		}
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, fmt.Errorf("resolving item %s: %w", itemID, err)
	}

	// Legacy: `tracks` dependencies (same-store convoys created before ref-by-id).
	deps, err := store.DepList(itemID, "up")
	if err != nil {
		return nil, fmt.Errorf("listing dependents of item %s: %w", itemID, err)
	}
	for _, dep := range deps {
		if dep.Type != TrackingDepType || seen[dep.IssueID] {
			continue
		}
		b, err := storeref.Resolve(dep.IssueID, resolveStores)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("getting tracking convoy %s: %w", dep.IssueID, err)
		}
		addConvoy(b)
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
