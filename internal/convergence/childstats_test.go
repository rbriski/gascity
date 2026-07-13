package convergence

import (
	"testing"
	"time"
)

func TestChildStats(t *testing.T) {
	const bead = "root-1"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	prefixedNonParseable := IdempotencyKeyPrefix(bead) + "abc"

	tests := []struct {
		name    string
		in      []BeadInfo
		want    ChildStats
		wantErr bool
	}{
		{
			name: "empty",
			in:   nil,
			want: ChildStats{HighestClosedIter: -1, HighestOpenIter: -1},
		},
		{
			name: "unrelated keys ignored",
			in: []BeadInfo{
				{ID: "x", Status: "closed", IdempotencyKey: "unrelated-key"},
			},
			want: ChildStats{HighestClosedIter: -1, HighestOpenIter: -1},
		},
		{
			name: "closed count and highest closed",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 1)},
				{ID: "w3", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 3)},
				{ID: "w2", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 2)},
				{ID: "wo", Status: "in_progress", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 4)},
			},
			want: ChildStats{
				ClosedCount:        3,
				HighestClosed:      BeadInfo{ID: "w3", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 3)},
				HighestClosedIter:  3,
				HighestClosedFound: true,
				HighestOpen:        BeadInfo{ID: "wo", Status: "in_progress", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 4)},
				HighestOpenIter:    4,
				HighestOpenFound:   true,
			},
		},
		{
			name: "malformed convergence child is rejected",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 1)},
				{ID: "bad", Status: "closed", ParentID: bead, IdempotencyKey: prefixedNonParseable},
			},
			wantErr: true,
		},
		{
			name: "cumulative duration skips zero timestamps",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 1), CreatedAt: base, ClosedAt: base.Add(2 * time.Second)},
				{ID: "w2", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 2), CreatedAt: base}, // ClosedAt zero -> skipped
				{ID: "w3", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 3), ClosedAt: base},  // CreatedAt zero -> skipped
			},
			want: ChildStats{
				ClosedCount:        3,
				CumulativeDur:      2 * time.Second,
				HighestClosed:      BeadInfo{ID: "w3", Status: "closed", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 3), ClosedAt: base},
				HighestClosedIter:  3,
				HighestClosedFound: true,
				HighestOpenIter:    -1,
			},
		},
		{
			name: "highest open picks max across open and in_progress",
			in: []BeadInfo{
				{ID: "o1", Status: "open", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 5)},
				{ID: "o2", Status: "in_progress", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 7)},
				{ID: "o3", Status: "open", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 6)},
			},
			want: ChildStats{
				HighestClosedIter: -1,
				HighestOpen:       BeadInfo{ID: "o2", Status: "in_progress", ParentID: bead, IdempotencyKey: IdempotencyKey(bead, 7)},
				HighestOpenIter:   7,
				HighestOpenFound:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := childStats(tt.in, bead)
			if tt.wantErr {
				if err == nil {
					t.Fatal("childStats succeeded, want evidence error")
				}
				return
			}
			if err != nil {
				t.Fatalf("childStats: %v", err)
			}
			if got != tt.want {
				t.Errorf("childStats() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}
