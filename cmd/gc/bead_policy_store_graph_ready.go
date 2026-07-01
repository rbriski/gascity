package main

import "github.com/gastownhall/gascity/internal/beads"

// ReadyGraphOnlyHandle returns a graph-only-ready handle when the wrapped store
// exposes GraphOnlyReadyStore. The handle applies expandPolicyReadyQuery before
// delegating so TierIssues is widened to TierBoth consistently across all Ready
// surfaces; TierWisps and other explicit modes pass through unchanged.
func (s *beadPolicyStore) ReadyGraphOnlyHandle() (beads.GraphOnlyReadyStore, bool) {
	g, ok := beads.GraphOnlyReadyFor(s.Store)
	if !ok {
		return nil, false
	}
	return beadPolicyGraphOnlyStore{g: g}, true
}

type beadPolicyGraphOnlyStore struct {
	g beads.GraphOnlyReadyStore
}

func (s beadPolicyGraphOnlyStore) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.g.ReadyGraphOnly(expandPolicyReadyQuery(query...))
}
