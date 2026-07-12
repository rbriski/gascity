package beads

// FilterReadySnapshot applies the client-side Assignee and Limit projection
// used when several consumers share one unfiltered Ready result. It preserves
// input order, returns deep copies, and bounds the result capacity by Limit so
// a small query cannot retain an allocation sized to the full snapshot.
func FilterReadySnapshot(rows []Bead, query ReadyQuery) []Bead {
	capacity := len(rows)
	if query.Limit > 0 && capacity > query.Limit {
		capacity = query.Limit
	}
	out := make([]Bead, 0, capacity)
	for _, row := range rows {
		if query.Assignee != "" && row.Assignee != query.Assignee {
			continue
		}
		out = append(out, cloneBead(row))
		if query.Limit > 0 && len(out) >= query.Limit {
			break
		}
	}
	return out
}
