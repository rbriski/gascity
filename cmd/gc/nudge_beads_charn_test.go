package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// recordingNudgeStore is a NudgeStore spy that records the call sequence and
// arguments, so the nudge-bead translation (create shape, terminalize ordering)
// can be pinned before the seam relocates. The P1 audit flagged the absence of a
// SetMetadataBatch/Close spy for this path.
type recordingNudgeStore struct {
	calls       []string
	created     beads.Bead
	lastSetMeta map[string]string
	listResult  []beads.Bead
}

func (r *recordingNudgeStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID == "" {
		b.ID = "nb-1"
	}
	r.created = b
	r.calls = append(r.calls, "create")
	return b, nil
}
func (r *recordingNudgeStore) Get(string) (beads.Bead, error) { return beads.Bead{}, beads.ErrNotFound }
func (r *recordingNudgeStore) List(beads.ListQuery) ([]beads.Bead, error) {
	r.calls = append(r.calls, "list")
	return r.listResult, nil
}
func (r *recordingNudgeStore) SetMetadata(id, k, v string) error {
	r.calls = append(r.calls, "setmeta:"+id)
	return nil
}
func (r *recordingNudgeStore) SetMetadataBatch(id string, kvs map[string]string) error {
	r.lastSetMeta = kvs
	r.calls = append(r.calls, "setmetabatch:"+id)
	return nil
}
func (r *recordingNudgeStore) Close(id string) error {
	r.calls = append(r.calls, "close:"+id)
	return nil
}

var _ nudgequeue.NudgeStore = (*recordingNudgeStore)(nil)

func TestCharacterize_EnsureQueuedNudgeBeadCreateShape(t *testing.T) {
	s := &recordingNudgeStore{}
	id, created, err := ensureQueuedNudgeBead(s, queuedNudge{
		ID: "n1", Agent: "worker", Source: "wake", Message: "wake up",
		DeliverAfter: time.Unix(100, 0), ExpiresAt: time.Unix(200, 0),
	})
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	if !created || id != "nb-1" {
		t.Fatalf("created=%v id=%q, want true/nb-1", created, id)
	}
	b := s.created
	if b.Type != nudgeBeadType {
		t.Errorf("Type = %q, want %q", b.Type, nudgeBeadType)
	}
	for _, want := range []string{nudgeBeadLabel, "agent:worker", "nudge:n1", "source:wake"} {
		if !slices.Contains(b.Labels, want) {
			t.Errorf("labels %v missing %q", b.Labels, want)
		}
	}
	for k, want := range map[string]string{"nudge_id": "n1", "agent": "worker", "state": "queued", "source": "wake"} {
		if b.Metadata[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, b.Metadata[k], want)
		}
	}
}

func TestCharacterize_MarkQueuedNudgeTerminalSetsBatchThenCloses(t *testing.T) {
	s := &recordingNudgeStore{}
	err := markQueuedNudgeTerminal(s,
		queuedNudge{ID: "n1", BeadID: "nb-9"},
		"failed", "wait-canceled", "delivery-withdrawn", time.Unix(300, 0))
	if err != nil {
		t.Fatalf("markQueuedNudgeTerminal: %v", err)
	}
	// SetMetadataBatch MUST happen before Close, both on the BeadID.
	if !slices.Equal(s.calls, []string{"setmetabatch:nb-9", "close:nb-9"}) {
		t.Fatalf("call sequence = %v, want [setmetabatch:nb-9 close:nb-9]", s.calls)
	}
	for k, want := range map[string]string{
		"state": "failed", "terminal_reason": "wait-canceled", "commit_boundary": "delivery-withdrawn",
	} {
		if s.lastSetMeta[k] != want {
			t.Errorf("terminal metadata[%q] = %q, want %q", k, s.lastSetMeta[k], want)
		}
	}
	if s.lastSetMeta["terminal_at"] == "" {
		t.Error("terminal_at not stamped")
	}
	if len(s.lastSetMeta["close_reason"]) < 20 {
		t.Errorf("close_reason %q must be >=20 chars (validation.on-close=error)", s.lastSetMeta["close_reason"])
	}
	if !strings.Contains(s.lastSetMeta["close_reason"], "nudge") {
		t.Errorf("close_reason %q should mention nudge", s.lastSetMeta["close_reason"])
	}
}
