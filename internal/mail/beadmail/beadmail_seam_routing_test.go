package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// recordingStore wraps a MemStore and counts the operations routed to it, so a
// test can prove which seam (message vs session) a given Provider call uses. The
// whole point of the two-store split is that messages and sessions diverge at the
// SQLite cutover; until then both constructors hardwire them equal, so without
// these tests a misrouted call would compile, satisfy the MailStore assertion,
// and pass the functional suite. These tests exercise the divergence directly.
type recordingStore struct {
	*beads.MemStore
	creates, gets, lists, updates, deletes int
}

func newRecordingStore() *recordingStore { return &recordingStore{MemStore: beads.NewMemStore()} }

func (r *recordingStore) Create(b beads.Bead) (beads.Bead, error) {
	r.creates++
	return r.MemStore.Create(b)
}
func (r *recordingStore) Get(id string) (beads.Bead, error) { r.gets++; return r.MemStore.Get(id) }
func (r *recordingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	r.lists++
	return r.MemStore.List(q)
}
func (r *recordingStore) Update(id string, o beads.UpdateOpts) error {
	r.updates++
	return r.MemStore.Update(id, o)
}
func (r *recordingStore) Delete(id string) error { r.deletes++; return r.MemStore.Delete(id) }

// TestSeamRouting_MessageWriteHitsMessageStoreOnly proves a Send with a human
// sender (no session resolution) touches only the message store.
func TestSeamRouting_MessageWriteHitsMessageStoreOnly(t *testing.T) {
	msg, sess := newRecordingStore(), newRecordingStore()
	p := NewWithStores(msg, sess)

	if _, err := p.Send("human", "worker", "subj", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msg.creates != 1 {
		t.Fatalf("message store Create count = %d, want 1", msg.creates)
	}
	if sess.creates != 0 || sess.gets != 0 || sess.lists != 0 || sess.updates != 0 || sess.deletes != 0 {
		t.Fatalf("session store touched during a human-sender Send: %+v", sess)
	}
	// And the message bead exists only in the message store. Messages are created
	// Ephemeral (wisp tier), so the placement check must scan both tiers.
	inMsg, _ := msg.MemStore.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	inSess, _ := sess.MemStore.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	if len(inMsg) != 1 || len(inSess) != 0 {
		t.Fatalf("message bead placement: messageStore=%d sessionStore=%d, want 1/0", len(inMsg), len(inSess))
	}
}

// TestSeamRouting_InboxUsesBothSeams proves Inbox routes recipient resolution to
// the session store and the message scan to the message store.
func TestSeamRouting_InboxUsesBothSeams(t *testing.T) {
	msg, sess := newRecordingStore(), newRecordingStore()
	p := NewWithStores(msg, sess)

	if _, err := p.Inbox("worker"); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if sess.gets == 0 && sess.lists == 0 {
		t.Fatal("Inbox recipient routing did not consult the session store")
	}
	if msg.lists == 0 {
		t.Fatal("Inbox message scan did not consult the message store")
	}
}

// TestSeamRouting_MessageReadHitsMessageStore proves a message point-read (Get)
// uses the message store and never the session store.
func TestSeamRouting_MessageReadHitsMessageStore(t *testing.T) {
	msg, sess := newRecordingStore(), newRecordingStore()
	p := NewWithStores(msg, sess)

	sent, err := p.Send("human", "worker", "subj", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sessGetsBefore := sess.gets
	msgGetsBefore := msg.gets
	if _, err := p.Get(sent.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if msg.gets <= msgGetsBefore {
		t.Fatal("message Get did not hit the message store")
	}
	if sess.gets != sessGetsBefore {
		t.Fatalf("message Get leaked to the session store: gets %d -> %d", sessGetsBefore, sess.gets)
	}
}

// TestSeamRouting_DefaultConstructorsUseSameStore guards the byte-identical bd
// phase: New/NewCached must wire both seams to the same store.
func TestSeamRouting_DefaultConstructorsUseSameStore(t *testing.T) {
	store := newRecordingStore()
	p := New(store)
	if p.messages == nil || p.sessions == nil {
		t.Fatal("New left a seam nil")
	}
	// A human-sender Send + Get exercises only message ops; both must land on the
	// single shared store (creates+gets observed via the recorder).
	sent, err := p.Send("human", "worker", "s", "b")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := p.Get(sent.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if store.creates == 0 || store.gets == 0 {
		t.Fatalf("New did not route message ops to the shared store: %+v", store)
	}
}
