package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	got := resolveMailMessagesStore(work, &config.City{}, t.TempDir())
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
	got := resolveMailMessagesStore(work, messagingSQLiteCfg(), cityPath)
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
	p := newCityMailProvider(work, &config.City{}, t.TempDir())
	if _, err := p.Send("human", "worker", "subj", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := countMessages(t, work); n != 1 {
		t.Fatalf("default provider wrote %d messages to the work store, want 1", n)
	}
}
