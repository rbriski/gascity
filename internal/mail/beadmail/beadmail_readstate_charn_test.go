package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Characterization tests pinning the read-state DOUBLE ENCODE and the current
// Rig behavior BEFORE the MailStore extraction. The P1 coverage audit flagged
// silent read-state corruption as the top mail risk: read state is encoded in
// BOTH the "read" label and the mail.read metadata key, and beadToMessage
// prefers the metadata value over the label — so a refactor that updates only
// one source would silently diverge. These tests lock today's behavior so the
// extraction is provably byte-identical. They are expected to pass against the
// current code (characterization, not TDD-red).

// TestCharacterize_MarkReadMarkUnreadWriteBothLabelAndMetadata pins that the
// read-toggle write path keeps the "read" label and the mail.read metadata key
// consistent. TestMarkReadMarkUnread only observes Inbox visibility (the label
// path); this asserts the metadata backup too.
func TestCharacterize_MarkReadMarkUnreadWriteBothLabelAndMetadata(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "toggle me")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := p.MarkRead(sent.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("Get after MarkRead: %v", err)
	}
	if !hasLabel(b.Labels, "read") {
		t.Errorf("MarkRead did not add the %q label; labels=%v", "read", b.Labels)
	}
	if got := b.Metadata["mail.read"]; got != "true" {
		t.Errorf("MarkRead mail.read = %q, want %q", got, "true")
	}

	if err := p.MarkUnread(sent.ID); err != nil {
		t.Fatalf("MarkUnread: %v", err)
	}
	b, err = store.Get(sent.ID)
	if err != nil {
		t.Fatalf("Get after MarkUnread: %v", err)
	}
	if hasLabel(b.Labels, "read") {
		t.Errorf("MarkUnread did not remove the %q label; labels=%v", "read", b.Labels)
	}
	if got := b.Metadata["mail.read"]; got != "false" {
		t.Errorf("MarkUnread mail.read = %q, want %q", got, "false")
	}
}

// TestCharacterize_ReadMarksBothLabelAndMetadata pins that Read() (which marks a
// previously-unread message read as a side effect) writes both the label and the
// metadata backup.
func TestCharacterize_ReadMarksBothLabelAndMetadata(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	sent, err := p.Send("human", "mayor", "", "read me")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := p.Read(sent.ID); err != nil {
		t.Fatalf("Read: %v", err)
	}
	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("Get after Read: %v", err)
	}
	if !hasLabel(b.Labels, "read") {
		t.Errorf("Read did not add the %q label; labels=%v", "read", b.Labels)
	}
	if got := b.Metadata["mail.read"]; got != "true" {
		t.Errorf("Read mail.read = %q, want %q", got, "true")
	}
}

// TestCharacterize_BeadToMessageReadPrecedence pins beadToMessage's read-state
// resolution: the "read" label sets read true, and the mail.read metadata value
// (when present) OVERRIDES the label in either direction. This precedence is the
// exact rule the MailStore row translation must preserve.
func TestCharacterize_BeadToMessageReadPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		meta   map[string]string
		want   bool
	}{
		{"label only", []string{"read"}, nil, true},
		{"neither label nor metadata", nil, nil, false},
		{"metadata true overrides absent label", nil, map[string]string{"mail.read": "true"}, true},
		{"metadata false overrides present label", []string{"read"}, map[string]string{"mail.read": "false"}, false},
		{"metadata true with present label", []string{"read"}, map[string]string{"mail.read": "true"}, true},
		{"unrecognized metadata value falls back to label", []string{"read"}, map[string]string{"mail.read": "maybe"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := beadToMessage(beads.Bead{ID: "m1", Type: "message", Labels: tc.labels, Metadata: tc.meta}).Read
			if got != tc.want {
				t.Fatalf("beadToMessage Read = %v, want %v (labels=%v meta=%v)", got, tc.want, tc.labels, tc.meta)
			}
		})
	}
}

// TestCharacterize_BeadToMessageDoesNotPopulateRig pins that beadToMessage does
// NOT set Message.Rig today (it is always empty), even when a rig-ish metadata
// key is present. The design's "model Rig" is therefore a deliberate future
// behavior change, not something the byte-identical extraction may introduce; if
// this test starts failing, a Rig-population change was made and must be flagged.
func TestCharacterize_BeadToMessageDoesNotPopulateRig(t *testing.T) {
	got := beadToMessage(beads.Bead{
		ID:       "m1",
		Type:     "message",
		Metadata: map[string]string{"rig": "r1", "mail.rig": "r1"},
	})
	if got.Rig != "" {
		t.Fatalf("beadToMessage Rig = %q, want empty (Rig is unmodeled today)", got.Rig)
	}
}
