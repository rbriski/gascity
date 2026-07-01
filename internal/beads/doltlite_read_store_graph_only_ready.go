//go:build gascity_native_beads

package beads

// ReadyGraphOnly implements GraphOnlyReadyStore. It unconditionally queries the
// wisp table set with TierWisps, ignoring any TierMode the caller supplies.
// The normal readyExcludeTypes filter is not applied because molecule steps are
// the primary actionable type on this path; blocking-dependency filtering is
// applied via doltliteBlockingDepsWhere.
func (s *DoltliteReadStore) ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error) {
	rq := readyQueryFromArgs(query)

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
	readyWhere, readyArgs := s.doltliteBlockingDepsWhere(doltliteWispTables)
	return s.queryIssuesOrderedInTables(q, []doltliteTableSet{doltliteWispTables}, readyWhere, readyArgs, q.Limit, "ORDER BY COALESCE(i.priority, 2) ASC, i.created_at ASC, i.id ASC")
}
