package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// Convoy-drain input-set binding (P0 slice, formula-unification §13 residue #1).
//
// A `gc lumen sling <route> <formula> --input-convoy <field>=<convoyID>` binding
// resolves a PRE-EXISTING convoy to a canonical member-id array at enqueue and seeds
// it into the run input under <field>. The already-landed `for-each over:
// input.<field>` member arm then fans one sub-graph per id — so the engine, the
// reducer, and the IR are untouched. All new code lives on this sling/enqueue seam
// (AGENTS.md: dispatch-specific behavior belongs on the dispatch seam, not the fold).
//
// The membership is FROZEN into the run's input hash at enqueue: the fold never reads
// the mutable convoy. This is faithful to v2's drain, whose own drainManifest freezes
// at first expansion and never re-reads a growing convoy (loadOrBuildDrainManifest).
//
// Ratified divergences from v2 (documented, not blockers):
//   - R-DUP: no cross-drain exclusive reservation. v2 stamped an exclusive-drain
//     reservation on member beads so two concurrent drains never fanned the same
//     convoy; (b) freezes membership into one run's input, so two independent slings
//     of the same convoy would each fan the full set. No real formula needs it —
//     build-from-convoy is slung once per convoy. If ever required it belongs here on
//     the sling seam (a bd reservation stamp at resolve time), never in the fold.
//   - R-GROWTH: draining a convoy that grows during the run is NOT supported and NOT
//     needed. v2 does not support it either (drain freezes at first expansion). Stated
//     so nobody re-adds mid-run freshness as a requirement.
//
// Per-member work exclusivity ("one bead, one claimant") is FREE and needs no code
// here: each fanned run ultimately dispatches one do = one real work bead, claimed by
// exactly one pool session through the standard claim lifecycle.

// errConvoyMemberUnresolved is returned when a convoy has a dangling or cross-store
// tracks target that resolves only to an unresolved placeholder. The sling refuses
// LOUD rather than fan a silently-incomplete (or silently-empty) drain.
var errConvoyMemberUnresolved = errors.New("convoy has an unresolved or cross-store member")

// errConvoyInterMemberOrder is returned when a resolved convoy carries a ready-blocking
// edge between two of its own members. A flat for-each fans all members CONCURRENTLY
// with no inter-member gating, so ordered drain (v2's topological member ordering) is a
// deferred follow-up; the sling refuses LOUD so the ordering loss is never silent
// (design §7 R-ORDER).
var errConvoyInterMemberOrder = errors.New("convoy has an inter-member blocks edge (ordered drain unsupported)")

// inputConvoyBinding is one `--input-convoy <field>=<convoyID>` operator directive:
// resolve <convoyID>'s live membership to a canonical id array and seed run
// input[<field>] with it at enqueue.
type inputConvoyBinding struct {
	Field    string
	ConvoyID string
}

// parseInputConvoyFlag parses a single `--input-convoy` spec of the form
// `<field>=<convoyID>`. Both sides are required and trimmed; a malformed spec is a
// loud CLI error (no discoverable run is ever minted from a bad flag).
func parseInputConvoyFlag(spec string) (inputConvoyBinding, error) {
	field, convoyID, ok := strings.Cut(spec, "=")
	field = strings.TrimSpace(field)
	convoyID = strings.TrimSpace(convoyID)
	if !ok || field == "" || convoyID == "" {
		return inputConvoyBinding{}, fmt.Errorf("invalid --input-convoy %q (want <field>=<convoyID>)", spec)
	}
	return inputConvoyBinding{Field: field, ConvoyID: convoyID}, nil
}

// resolveConvoyMemberIDs resolves convoyID's LIVE membership to a canonically-sorted
// id array, freezing ids ONLY (never member bead snapshots). It is the fork-owned
// resolver at the heart of the slice:
//
//  1. convoycore.Members(store, convoyID, false, memberStores...) — includeClosed=false
//     matches v2 drain's live-set intent.
//  2. Reject unresolved/cross-store members up front (drain discipline
//     convoycore.IsUnresolvedTrackedItem) so a broken convoy fails LOUD (R-UNRESOLVED).
//  3. R-ORDER guard: refuse if any resolved member has a ready-blocking edge to ANOTHER
//     resolved member of the same convoy (a flat fan cannot gate them).
//  4. Project + sort ids by ID ascending. Sorting makes the run's inputHash stable
//     across re-enqueues of the same membership (independent of convoycore.Members'
//     CreatedAt tie order); freezing IDS (not beads) pins the hash to MEMBERSHIP, so a
//     member's title/status changing after the snapshot never perturbs the run identity
//     (design §3.3, the freeze-ids determinism property).
//
// An empty result is legal (R-EMPTY): the caller warns-and-proceeds and the fan settles
// a vacuous PASS. Only unresolved members and inter-member ordering hard-fail here.
func resolveConvoyMemberIDs(store beads.Store, convoyID string, memberStores []beads.Store) ([]string, error) {
	members, err := convoycore.Members(store, convoyID, false, memberStores...)
	if err != nil {
		return nil, fmt.Errorf("resolving convoy %q members: %w", convoyID, err)
	}

	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		if convoycore.IsUnresolvedTrackedItem(m) {
			return nil, fmt.Errorf("%w: convoy %q member %q", errConvoyMemberUnresolved, convoyID, m.ID)
		}
		if id := strings.TrimSpace(m.ID); id != "" {
			memberSet[id] = true
		}
	}

	if err := rejectInterMemberOrdering(store, convoyID, members, memberSet, memberStores); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(members))
	for _, m := range members {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// rejectInterMemberOrdering enforces the R-ORDER guard: it refuses LOUD if any resolved
// member depends (via a ready-blocking edge: blocks / waits-for / conditional-blocks) on
// ANOTHER resolved member of the same convoy. A member's dependency edges are co-resident
// with the member work bead, which may live in a different per-class store than the
// convoy — so it resolves the member's owning store first (the same probe discipline v2
// drain uses in orderDrainMembersByDependencies).
func rejectInterMemberOrdering(store beads.Store, convoyID string, members []beads.Bead, memberSet map[string]bool, memberStores []beads.Store) error {
	for _, m := range members {
		memberID := strings.TrimSpace(m.ID)
		if memberID == "" {
			continue
		}
		depStore, err := convoyMemberOwningStore(store, memberID, memberStores)
		if err != nil {
			return fmt.Errorf("resolving owning store for convoy %q member %q: %w", convoyID, memberID, err)
		}
		deps, err := depStore.DepList(memberID, "down")
		if err != nil {
			return fmt.Errorf("listing dependencies for convoy %q member %q: %w", convoyID, memberID, err)
		}
		for _, dep := range deps {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			blockerID := strings.TrimSpace(dep.DependsOnID)
			if blockerID == "" || blockerID == memberID {
				continue
			}
			if memberSet[blockerID] {
				return fmt.Errorf("%w: convoy %q member %q %s-on member %q — resolve ordered drain before slinging",
					errConvoyInterMemberOrder, convoyID, memberID, dep.Type, blockerID)
			}
		}
	}
	return nil
}

// convoyMemberOwningStore returns the store that owns memberID, probing the primary
// (convoy-owning) store first, then the member-store tail, returning the first whose
// Get succeeds. With no member stores (single-store callers) it returns the primary
// store WITHOUT an owning-store probe read — byte-identical to today's single-store
// behavior. Mirrors v2 drain's drainMemberOwningStore.
func convoyMemberOwningStore(primary beads.Store, memberID string, memberStores []beads.Store) (beads.Store, error) {
	if len(memberStores) == 0 {
		return primary, nil
	}
	probe := make([]beads.Store, 0, 1+len(memberStores))
	probe = append(probe, primary)
	probe = append(probe, memberStores...)
	for _, s := range probe {
		if s == nil {
			continue
		}
		if _, err := s.Get(memberID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, err
		}
		return s, nil
	}
	return primary, nil
}

// lumenConvoyStores opens the WORK stores an --input-convoy binding resolves against:
// the city work store (where slings create convoys) plus each rig store as the
// member-store probe tail (a city convoy may track work beads that live in a rig
// store). It is a package var so tests can inject a prepopulated in-memory convoy
// without a real beads backend; production opens the doltlite/bd-backed city + rig
// stores. Note: beads.Store has no resource-level Close, so the opened stores need no
// teardown here.
var lumenConvoyStores = func(cityPath string) (beads.Store, []beads.Store, error) {
	cityStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening city work store: %w", err)
	}
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	var rigStores []beads.Store
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			rigPath := resolveStoreScopeRoot(cityPath, rig.Path)
			if samePath(rigPath, cityPath) {
				continue
			}
			rs, err := openStoreAtForCity(rigPath, cityPath)
			if err != nil {
				return nil, nil, fmt.Errorf("opening rig %q work store: %w", rig.Name, err)
			}
			rigStores = append(rigStores, rs)
		}
	}
	return cityStore, rigStores, nil
}

// seedInputConvoys resolves each --input-convoy binding and seeds input[field] with the
// sorted id array, returning the augmented input map. It runs BEFORE the CAS input blob
// is written and run.started is appended, so a resolution failure leaves NO discoverable
// run (the fail-loud posture the enqueue gate already takes). Duplicate fields — the
// same field bound twice, or by BOTH --input and --input-convoy — are refused LOUD so
// explicit operator intent stays unambiguous rather than silently shadowed.
//
// An empty-after-resolve convoy is a LEGAL empty drain (R-EMPTY): it warns (the operator
// may have believed it populated) and seeds an empty array, which the fan settles as a
// vacuous PASS. Only unresolved/cross-store members and inter-member ordering hard-fail
// (inside resolveConvoyMemberIDs).
func seedInputConvoys(cityPath string, input map[string]any, bindings []inputConvoyBinding, stderr io.Writer) (map[string]any, error) {
	if len(bindings) == 0 {
		return input, nil
	}
	seen := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if seen[b.Field] {
			return nil, fmt.Errorf("--input-convoy field %q bound more than once", b.Field)
		}
		seen[b.Field] = true
		if _, ok := input[b.Field]; ok {
			return nil, fmt.Errorf("--input-convoy field %q is also set via --input; supply it exactly once", b.Field)
		}
	}

	store, memberStores, err := lumenConvoyStores(cityPath)
	if err != nil {
		return nil, err
	}
	if input == nil {
		input = make(map[string]any, len(bindings))
	}
	for _, b := range bindings {
		ids, err := resolveConvoyMemberIDs(store, b.ConvoyID, memberStores)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			fmt.Fprintf(stderr, "gc lumen sling: --input-convoy %s=%s resolved to zero live members; enqueuing an empty (vacuous-pass) drain\n", b.Field, b.ConvoyID) //nolint:errcheck // best-effort stderr
		}
		input[b.Field] = idsToAnySlice(ids)
	}
	return input, nil
}

// idsToAnySlice projects a sorted id slice into the []any the immutable run input map
// carries (arrayFromInputValue reads a []any member value). An empty input yields a
// non-nil empty slice so the field is PRESENT (a declared-required array input is
// satisfied) and pins the empty-membership snapshot into inputHash.
func idsToAnySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
