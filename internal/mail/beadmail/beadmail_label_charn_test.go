package beadmail

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func intPtr(n int) *int { return &n }

// TestCharacterize_ReadLabelVsMetadataDivergence pins the dual-oracle read-state
// semantics: Inbox membership is driven by the "read" LABEL, while the returned
// Message.Read flag is driven by the mail.read METADATA override. They can
// legitimately diverge, so a SQLite codec MUST persist BOTH sources — persisting
// only one would make Inbox membership and the Read flag silently contradict each
// other while the rest of the suite stays green.
func TestCharacterize_ReadLabelVsMetadataDivergence(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	// Case 1: "read" label present, mail.read=false.
	sent, err := p.Send("human", "worker", "", "labelled read, metadata unread")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := store.Update(sent.ID, beads.UpdateOpts{
		Labels:   []string{"read"},
		Metadata: map[string]string{"mail.read": "false"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	inbox, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("Inbox = %d, want 0 — the 'read' label excludes the message from the inbox", len(inbox))
	}
	all, err := p.All("worker")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("All = %d, want 1", len(all))
	}
	if all[0].Read {
		t.Fatal("All()[0].Read = true, want false — mail.read metadata overrides the 'read' label")
	}
}

// TestCharacterize_BeadToMessageLabelEncodedFields pins that beadToMessage derives
// ThreadID/ReplyTo/Priority/CC from LABELS and ignores the typed Bead.Priority
// column. These label-encoded fields recur in orders/convoy; pinning once that a
// promoted column must NOT "helpfully" reconcile with label-derived values
// prevents a behavior change when a codec later promotes such a column.
func TestCharacterize_BeadToMessageLabelEncodedFields(t *testing.T) {
	b := beads.Bead{
		ID:       "m1",
		Type:     "message",
		Priority: intPtr(9), // the typed column — must be IGNORED by beadToMessage
		Labels:   []string{"thread:t1", "reply-to:r1", "priority:5", "cc:a@x", "cc:b@y"},
	}
	m := beadToMessage(b)
	if m.ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want t1", m.ThreadID)
	}
	if m.ReplyTo != "r1" {
		t.Errorf("ReplyTo = %q, want r1", m.ReplyTo)
	}
	if m.Priority != 5 {
		t.Errorf("Priority = %d, want 5 (from the priority: label, NOT the Bead.Priority column 9)", m.Priority)
	}
	if !slices.Equal(m.CC, []string{"a@x", "b@y"}) {
		t.Errorf("CC = %v, want [a@x b@y]", m.CC)
	}
}

// TestCharacterize_ExtractPriorityNonNumericIsZero pins that a malformed
// priority: label parses to 0 rather than erroring.
func TestCharacterize_ExtractPriorityNonNumericIsZero(t *testing.T) {
	if got := extractPriority([]string{"priority:notanumber"}); got != 0 {
		t.Fatalf("extractPriority(non-numeric) = %d, want 0", got)
	}
	if got := extractPriority([]string{"thread:x"}); got != 0 {
		t.Fatalf("extractPriority(absent) = %d, want 0", got)
	}
}
