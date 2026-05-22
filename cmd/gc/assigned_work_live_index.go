package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type assignedWorkLiveIndex struct {
	assigned map[string]map[string]struct{}
	errs     map[string]error
}

// buildAssignedWorkLiveIndex snapshots open and in-progress assigned work with
// one logical live scan per status/store. Callers may only use this
// point-in-time index after proving the target session runtime is stopped;
// running sessions can claim work after the scan and must keep using the
// per-session live guard.
func buildAssignedWorkLiveIndex(store beads.Store, rigStores map[string]beads.Store) assignedWorkLiveIndex {
	idx := assignedWorkLiveIndex{
		assigned: make(map[string]map[string]struct{}),
		errs:     make(map[string]error),
	}
	idx.addStore("", store)
	if len(rigStores) == 0 {
		return idx
	}
	refs := make([]string, 0, len(rigStores))
	for ref := range rigStores {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		idx.addStore(ref, rigStores[ref])
	}
	return idx
}

func (idx assignedWorkLiveIndex) addStore(ref string, store beads.Store) {
	if store == nil {
		return
	}
	assigned := make(map[string]struct{})
	reader := beads.HandlesFor(store).Live
	for _, status := range []string{"open", "in_progress"} {
		items, err := reader.List(beads.ListQuery{Status: status, SkipLabels: true})
		if err != nil {
			idx.errs[ref] = err
			return
		}
		for _, item := range items {
			if sessionpkg.IsSessionBeadOrRepairable(item) {
				continue
			}
			assignee := strings.TrimSpace(item.Assignee)
			if assignee == "" {
				continue
			}
			assigned[assignee] = struct{}{}
		}
	}
	idx.assigned[ref] = assigned
}

func (idx assignedWorkLiveIndex) hasOpenAssignedWork(session beads.Bead) (bool, error) {
	for ref, assigned := range idx.assigned {
		if err := idx.errs[ref]; err != nil {
			return false, err
		}
		if assignedWorkIndexHasSession(assigned, session) {
			return true, nil
		}
	}
	for ref, err := range idx.errs {
		if _, ok := idx.assigned[ref]; !ok && err != nil {
			return false, err
		}
	}
	return false, nil
}

func (idx assignedWorkLiveIndex) hasOpenAssignedWorkForReachableStore(
	cityPath string,
	cfg *config.City,
	session beads.Bead,
) (bool, error) {
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	if !ok {
		return idx.hasOpenAssignedWork(session)
	}
	if err := idx.errs[storeRef]; err != nil {
		return false, err
	}
	assigned, ok := idx.assigned[storeRef]
	if !ok {
		if storeRef == "" {
			return false, nil
		}
		return false, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return assignedWorkIndexHasSession(assigned, session), nil
}

func assignedWorkIndexHasSession(assigned map[string]struct{}, session beads.Bead) bool {
	for _, id := range sessionBeadAssigneeIdentities(session) {
		if _, ok := assigned[id]; ok {
			return true
		}
	}
	return false
}
