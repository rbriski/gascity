//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"
)

func TestFilterReadySnapshotMatchesDoltliteReadyQueries(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	base := time.Now().UTC().Add(10 * time.Minute)
	for _, issue := range []testDoltliteIssue{
		{
			ID:          "gc-snapshot-a-rich",
			Title:       "a-rich",
			Status:      "open",
			IssueType:   "task",
			Priority:    1,
			CreatedAt:   base,
			Assignee:    "worker-a",
			Description: "full-row sentinel",
			Labels:      []string{"snapshot", "rich"},
			Metadata:    map[string]string{"route": "worker-a", "ordinal": "1"},
		},
		{ID: "gc-snapshot-worker-b", Title: "worker-b-middle", Status: "open", IssueType: "task", CreatedAt: base.Add(time.Minute), Assignee: "worker-b", Metadata: map[string]string{"ordinal": "2"}},
		{ID: "gc-snapshot-a-second", Title: "a-second", Status: "open", IssueType: "task", CreatedAt: base.Add(2 * time.Minute), Assignee: "worker-a", Metadata: map[string]string{"ordinal": "3"}},
		{ID: "gc-snapshot-a-third", Title: "a-third", Status: "open", IssueType: "task", CreatedAt: base.Add(3 * time.Minute), Assignee: "worker-a", Metadata: map[string]string{"ordinal": "4"}},
	} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", issue)
	}

	assertReadySnapshotConformance(t, store, "worker-a", "worker-b")
}
