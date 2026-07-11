package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestWaitForSessionCommandable_ReadsInfraStoreSessionOnSplitCity pins the
// split-default fallout behind TestGCLiveContract_BeadsAndEvents: a deferred
// session create waits on WaitForSessionCommandable, which must read the session
// bead from the infra/sessions-class store on a split city. It used to read
// cs.CityBeadStore() (the work store), so a gcg- session id missed
// ("getting session: getting bead ...: bead not found") the instant the create
// path waited on it. Byte-identical on a single-store city (SessionsBeadStore ==
// CityBeadStore there).
func TestWaitForSessionCommandable_ReadsInfraStoreSessionOnSplitCity(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	work := beads.NewMemStore()
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStore(), cfg) // mints gcg- ids like production
	cs := &controllerState{
		cityName:       "test-city",
		cityBeadStore:  work,
		cityInfraStore: infra,
		cfg:            cfg,
		cityPath:       t.TempDir(),
		sp:             runtime.NewFake(),
		eventProv:      events.NewFake(),
	}

	// A session bead is sessions-class: on a split city it lives in the infra
	// store (SessionsBeadStore routes there), exactly as the create handler writes it.
	sessionID, err := session.NewStore(cs.SessionsBeadStore()).CreateSession(session.CreateSpec{
		Title:     "worker-1",
		AgentName: "worker-1",
		Metadata:  map[string]string{"provider": "subprocess", "template": "worker"},
	})
	if err != nil {
		t.Fatalf("seed session bead: %v", err)
	}
	if _, e := infra.Get(sessionID); e != nil {
		t.Fatalf("seeded session %s is not in the infra store: %v", sessionID, e)
	}
	if _, e := work.Get(sessionID); e == nil {
		t.Fatalf("seeded session %s unexpectedly present in the work store", sessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	info, err := cs.WaitForSessionCommandable(ctx, sessionID)
	if err != nil {
		// Locating the session is the contract under test. A ctx deadline ("did
		// not become commandable") means it was found but not yet commandable —
		// acceptable. A not-found means the cross-store read gap is still present.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "getting session") {
			t.Fatalf("WaitForSessionCommandable cannot see the infra-store session %s (cross-store read gap): %v", sessionID, err)
		}
		return
	}
	if info.ID != sessionID {
		t.Fatalf("WaitForSessionCommandable returned session %q, want %q", info.ID, sessionID)
	}
}
