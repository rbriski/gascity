package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

func messagingSQLiteCfg() *config.City {
	return &config.City{Beads: config.BeadsConfig{
		Classes: map[string]config.BeadClassConfig{
			config.BeadClassMessaging: {Backend: config.BeadsBackendSQLite},
		},
	}}
}

func countMessages(t *testing.T, s beads.Store) int {
	t.Helper()
	msgs, err := s.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	return len(msgs)
}

// TestResolveMailMessagesStore_DefaultReturnsWorkStore proves the default backend
// keeps the message seam on the work store (byte-identical bd phase).
func TestResolveMailMessagesStore_DefaultReturnsWorkStore(t *testing.T) {
	work := beads.NewMemStore()
	got := resolveMailMessagesStore(work, &config.City{}, t.TempDir(), nil)
	if any(got) != any(work) {
		t.Fatal("default backend should return the work store as the message seam")
	}
}

// TestResolveMailMessagesStore_RoutesToSQLiteWhenConfigured proves that with
// messaging=sqlite the message seam is a distinct SQLite store, and a beadmail
// built on the split seams writes messages to SQLite while the work store (the
// session seam) receives none.
func TestResolveMailMessagesStore_RoutesToSQLiteWhenConfigured(t *testing.T) {
	work := beads.NewMemStore()
	cityPath := t.TempDir()
	got := resolveMailMessagesStore(work, messagingSQLiteCfg(), cityPath, nil)
	if got == nil {
		t.Fatal("expected a SQLite message store, got nil")
	}
	if any(got) == any(work) {
		t.Fatal("expected a distinct SQLite store, got the work store")
	}

	p := beadmail.NewCachedWithStores(got, work)
	if _, err := p.Send("human", "worker", "subj", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := countMessages(t, work); n != 0 {
		t.Fatalf("work store has %d messages, want 0 (mail should land in SQLite)", n)
	}
	if n := countMessages(t, got.(beads.Store)); n != 1 {
		t.Fatalf("SQLite store has %d messages, want 1", n)
	}
}

// TestNewCityMailProvider_DefaultByteIdentical proves the default provider writes
// to the work store exactly as before the cutover wiring.
func TestNewCityMailProvider_DefaultByteIdentical(t *testing.T) {
	t.Setenv("GC_MAIL", "") // ensure the default beadmail backend
	work := beads.NewMemStore()
	p := newCityMailProvider(work, &config.City{}, t.TempDir(), nil)
	if _, err := p.Send("human", "worker", "subj", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := countMessages(t, work); n != 1 {
		t.Fatalf("default provider wrote %d messages to the work store, want 1", n)
	}
}

func hasEventType(t *testing.T, fake *events.Fake, typ string) bool {
	t.Helper()
	evs, err := fake.List(events.Filter{})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// TestBeadEventRowRecorder_TranslatesOps proves the store-edge RowChange ->
// bead.* translation: created->bead.created (full payload), updated-with-closed->
// bead.closed, deleted->bead.deleted (type carried from the RowChange).
func TestBeadEventRowRecorder_TranslatesOps(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Type: "message", Title: "hi"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := created.ID
	fake := events.NewFake()
	emit := beadEventRowRecorder(store.Get, fake)

	emit(beads.RowChange{ID: id, Type: "message", Op: beads.RowCreated})
	closed := "closed"
	if err := store.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	emit(beads.RowChange{ID: id, Type: "message", Op: beads.RowUpdated})
	emit(beads.RowChange{ID: id, Type: "message", Op: beads.RowDeleted})

	evs, err := fake.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantTypes := []string{events.BeadCreated, events.BeadClosed, events.BeadDeleted}
	if len(evs) != len(wantTypes) {
		t.Fatalf("emitted %d events, want %d: %+v", len(evs), len(wantTypes), evs)
	}
	for i, e := range evs {
		if e.Type != wantTypes[i] {
			t.Errorf("event %d type = %q, want %q", i, e.Type, wantTypes[i])
		}
		if e.Subject != id {
			t.Errorf("event %d subject = %q, want %q", i, e.Subject, id)
		}
	}
	// The created payload carries the full bead snapshot.
	var p api.BeadEventPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatalf("decode created payload: %v", err)
	}
	if p.Bead.ID != id || p.Bead.Title != "hi" {
		t.Fatalf("created payload bead = %+v, want id=%q title=hi", p.Bead, id)
	}
}

// TestMailSQLiteStore_EmitsBeadEventsOnWrites proves a controller mail provider
// backed by the SQLite store (messaging=sqlite) keeps the bus fed: Send emits
// bead.created, Archive emits bead.deleted — so order triggers / bead-feed
// observers do not go dark after the flip.
func TestMailSQLiteStore_EmitsBeadEventsOnWrites(t *testing.T) {
	work := beads.NewMemStore()
	fake := events.NewFake()
	messages := resolveMailMessagesStore(work, messagingSQLiteCfg(), t.TempDir(), fake)
	if any(messages) == any(work) {
		t.Fatal("expected the SQLite message store")
	}
	p := beadmail.NewCachedWithStores(messages, work)

	msg, err := p.Send("human", "worker", "subj", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !hasEventType(t, fake, events.BeadCreated) {
		t.Error("Send did not emit bead.created from the SQLite mail store")
	}
	if err := p.Archive(msg.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !hasEventType(t, fake, events.BeadDeleted) {
		t.Error("Archive did not emit bead.deleted from the SQLite mail store")
	}
}
