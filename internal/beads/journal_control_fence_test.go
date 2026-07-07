package beads_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

func TestIsJournalResidentID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"gcg-j1", true},
		{"gcg-j42", true},
		{"gcg-abc", false}, // legacy graph-store id (no j marker)
		{"gcg-", false},
		{"gcg", false},
		{"bd-7", false},
		{"", false},
		{"gcg-jable-but-legacy", true}, // marker is structural: gcg-j prefix
	}
	for _, tc := range cases {
		if got := beads.IsJournalResidentID(tc.id); got != tc.want {
			t.Errorf("IsJournalResidentID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestControlEpochFenceStreamIDStableAndDistinct(t *testing.T) {
	a := beads.ControlEpochFenceStreamID("gcg-j1")
	b := beads.ControlEpochFenceStreamID("gcg-j2")
	if a == b {
		t.Fatalf("distinct beads produced the same fence stream %q", a)
	}
	if a != beads.ControlEpochFenceStreamID("gcg-j1") {
		t.Fatalf("fence stream id is not deterministic")
	}
	if a == "" {
		t.Fatalf("fence stream id must be non-empty")
	}
}

// TestNewJournalStoreRegistersFenceVocab proves NewJournalStore registers the
// fence event type so a control-epoch fence append is accepted (not rejected as
// an unknown (engine, type)), and that the CAS is loud on a stale head.
func TestNewJournalStoreRegistersFenceVocab(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(ctx, path, graphstore.Options{CityID: "fence-vocab"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	store := beads.NewJournalStore(gs)

	al, ok := beads.AppendLogStoreFor(store)
	if !ok {
		t.Fatalf("AppendLogStoreFor = false, want reachable")
	}
	stream := beads.ControlEpochFenceStreamID("gcg-j7")
	if _, err := al.AppendEvent(ctx, stream, beads.ControlFenceEngine, 0, 0,
		[]graphstore.JournalEvent{beads.ControlEpochFenceEvent("gcg-j7")}); err != nil {
		t.Fatalf("fence append rejected: %v", err)
	}
	// Stale expected version loses loudly.
	if _, err := al.AppendEvent(ctx, stream, beads.ControlFenceEngine, 0, 0,
		[]graphstore.JournalEvent{beads.ControlEpochFenceEvent("gcg-j7")}); !errors.Is(err, graphstore.ErrWrongExpectedVersion) {
		t.Fatalf("stale append err = %v, want ErrWrongExpectedVersion", err)
	}
}
