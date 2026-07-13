package beads

import (
	"testing"
	"time"
)

func TestBoundedExactParentMetadataLookupEligibilityIsNarrow(t *testing.T) {
	base := ListQuery{
		ParentID:      "root-1",
		Metadata:      map[string]string{"idempotency_key": "converge:root-1:iter:2"},
		Limit:         2,
		IncludeClosed: true,
		TierMode:      TierBoth,
	}
	if !base.isBoundedExactParentMetadataLookup() {
		t.Fatal("canonical convergence ambiguity probe is not eligible")
	}

	tests := []struct {
		name   string
		mutate func(*ListQuery)
	}{
		{name: "different limit", mutate: func(q *ListQuery) { q.Limit = 1 }},
		{name: "status", mutate: func(q *ListQuery) { q.Status = "closed" }},
		{name: "type", mutate: func(q *ListQuery) { q.Type = "molecule" }},
		{name: "label", mutate: func(q *ListQuery) { q.Label = "scope" }},
		{name: "assignee", mutate: func(q *ListQuery) { q.Assignee = "worker" }},
		{name: "assignees", mutate: func(q *ListQuery) { q.Assignees = []string{"worker"} }},
		{name: "parent ids", mutate: func(q *ListQuery) { q.ParentIDs = []string{"root-2"} }},
		{name: "multiple metadata fields", mutate: func(q *ListQuery) { q.Metadata["other"] = "value" }},
		{name: "closed rows excluded", mutate: func(q *ListQuery) { q.IncludeClosed = false }},
		{name: "single tier", mutate: func(q *ListQuery) { q.TierMode = TierIssues }},
		{name: "live", mutate: func(q *ListQuery) { q.Live = true }},
		{name: "scan opt in", mutate: func(q *ListQuery) { q.AllowScan = true }},
		{name: "sort", mutate: func(q *ListQuery) { q.Sort = SortCreatedAsc }},
		{name: "created before", mutate: func(q *ListQuery) { q.CreatedBefore = time.Now() }},
		{name: "updated before", mutate: func(q *ListQuery) { q.UpdatedBefore = time.Now() }},
		{name: "missing parent", mutate: func(q *ListQuery) { q.ParentID = "" }},
		{name: "missing metadata", mutate: func(q *ListQuery) { q.Metadata = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := base
			query.Metadata = map[string]string{"idempotency_key": base.Metadata["idempotency_key"]}
			tt.mutate(&query)
			if query.isBoundedExactParentMetadataLookup() {
				t.Fatalf("query unexpectedly eligible: %+v", query)
			}
		})
	}
}
