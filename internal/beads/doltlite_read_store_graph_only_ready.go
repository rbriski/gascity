//go:build gascity_native_beads

package beads

// ReadyGraphOnly implements GraphOnlyReadyStore. It unconditionally queries the
// wisp table set with TierWisps, ignoring any TierMode the caller supplies.
// Molecule steps and wisps are the only bead types a controller dispatch loop
// executes, so the normal readyExcludeTypes filter is not applied here.
func (s *DoltliteReadStore) ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error) {
	rq := readyQueryFromArgs(query)
	rq.TierMode = TierWisps

	q := ListQuery{
		Status:        "open",
		AllowScan:     true,
		IncludeClosed: false,
		SkipLabels:    true,
		TierMode:      TierWisps,
	}
	if rq.Assignee != "" {
		q.Assignee = rq.Assignee
	}
	if rq.Limit > 0 {
		q.Limit = rq.Limit
	}
	return s.queryIssuesOrderedInTables(q, []doltliteTableSet{doltliteWispTables}, "", nil, q.Limit, "ORDER BY COALESCE(i.priority, 2) ASC, i.created_at ASC, i.id ASC")
}
