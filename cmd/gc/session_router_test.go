package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordrouter"
	"github.com/gastownhall/gascity/internal/events"
)

// sessionSQLiteCfg returns a city config that relocates ONLY the session class
// onto the embedded SQLite backend (graph stays on the bd work store). After the
// session class came off the coordrouter.Router, relocating sessions no longer
// constructs a Router — sessions reach their store through the class-aware
// accessors (resolveSessionStore / cr.sessionBeadStore()).
func sessionSQLiteCfg() *config.City {
	cfg := &config.City{}
	cfg.Beads.Classes = map[string]config.BeadClassConfig{
		config.BeadClassSessions: {Backend: config.BeadsBackendSQLite},
	}
	return cfg
}

// TestRoutedPolicyStoreNoRouterForRelocatedSessions is the keystone guard for the
// sessions-off-the-Router cutover. Relocating ONLY the session class must NOT
// insert a coordrouter.Router: sessions are class-aware (resolveSessionStore), so
// the city store stays a plain policy(workBackend). A Router here would mean the
// session federation was not actually retired.
func TestRoutedPolicyStoreNoRouterForRelocatedSessions(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.City
	}{
		{"default", &config.City{}},
		{"sessions=sqlite", sessionSQLiteCfg()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := routedPolicyStore(beads.NewMemStore(), tc.cfg, t.TempDir())
			t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
			base, _, ok := unwrapBeadPolicyStore(store)
			if !ok {
				t.Fatal("expected the result to be policy-wrapped")
			}
			if _, isRouter := base.(*coordrouter.Router); isRouter {
				t.Fatalf("%s must NOT insert a *coordrouter.Router — sessions are class-aware, not Router-routed", tc.name)
			}
		})
	}
}

// TestRoutedPolicyStoreBuildsRouterForGraphRelocation confirms graph — the last
// class on the Router — still inserts one, so retiring sessions did not regress
// graph routing.
func TestRoutedPolicyStoreBuildsRouterForGraphRelocation(t *testing.T) {
	store := routedPolicyStore(beads.NewMemStore(), graphSQLiteCfg(), t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	base, _, ok := unwrapBeadPolicyStore(store)
	if !ok {
		t.Fatal("expected the result to be policy-wrapped")
	}
	if _, isRouter := base.(*coordrouter.Router); !isRouter {
		t.Fatalf("graph=sqlite must still insert a *coordrouter.Router, got %T", base)
	}
}

// TestResolveSessionStoreRoutesByBackend proves the class-aware accessor: at the
// default bd backend it returns the work store UNCHANGED (byte-identical — same
// handle), and at the relocated SQLite backend it returns a DISTINCT store that
// mints the disjoint gcs- prefix.
func TestResolveSessionStoreRoutesByBackend(t *testing.T) {
	work := beads.NewMemStore()

	// Default: identity. resolveSessionStore must return the exact work handle.
	if got := resolveSessionStore(work, &config.City{}, t.TempDir(), nil); got != work {
		t.Fatal("sessions on the bd work store: resolveSessionStore must return the work store unchanged (byte-identical)")
	}

	// Relocated: a distinct SQLite session store with the gcs- prefix.
	sessionStore := resolveSessionStore(work, sessionSQLiteCfg(), t.TempDir(), nil)
	t.Cleanup(func() { _ = closeBeadStoreHandle(sessionStore) })
	if sessionStore == work {
		t.Fatal("sessions=sqlite: resolveSessionStore must return a store distinct from the work store")
	}
	sess, err := sessionStore.Create(beads.Bead{Title: "agent session", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatalf("create session bead on the relocated session store: %v", err)
	}
	if !strings.HasPrefix(sess.ID, "gcs-") {
		t.Fatalf("relocated session bead id %q must carry the disjoint gcs- prefix", sess.ID)
	}
	if _, err := work.Get(sess.ID); err == nil {
		t.Fatalf("session bead %s must NOT reside on the work store", sess.ID)
	}
}

// TestRelocatedSessionWriteEmitsBeadEvent proves the recorder is threaded: a
// session write through the relocated session store (opened WITH the controller
// recorder) emits a bead.* event, so the dashboard bead feed / cache observers
// keep seeing relocated session writes. This is the Q4 "thread the recorder"
// outcome — relocated session writes are NOT event-silent.
func TestRelocatedSessionWriteEmitsBeadEvent(t *testing.T) {
	rec := &capturingRecorder{}
	sessionStore := resolveSessionStore(beads.NewMemStore(), sessionSQLiteCfg(), t.TempDir(), rec)
	t.Cleanup(func() { _ = closeBeadStoreHandle(sessionStore) })

	created, err := sessionStore.Create(beads.Bead{Title: "worker", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	var sawCreate bool
	for _, e := range rec.events {
		if e.Type == events.BeadCreated && e.Subject == created.ID {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Fatalf("relocated session Create did not emit a %s event for %s; the recorder was not threaded into the session store", events.BeadCreated, created.ID)
	}
}

// TestRelocatedSessionBeadsExcludedFromWorkReady is the class-aware successor to
// the Phase-C Ready-leak guard. With sessions on a separate store, session and
// wait beads are PHYSICALLY absent from the work store, so the controller's Ready
// scan (which reads the work store) can never surface them as actionable work — a
// stronger isolation than the old federated-Router exclusion. A real work task on
// the work store MUST still be Ready (positive control).
func TestRelocatedSessionBeadsExcludedFromWorkReady(t *testing.T) {
	work, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	t.Cleanup(func() { _ = work.(interface{ CloseStore() error }).CloseStore() })

	sessionStore := resolveSessionStore(work, sessionSQLiteCfg(), t.TempDir(), nil)
	t.Cleanup(func() { _ = closeBeadStoreHandle(sessionStore) })

	if _, err := sessionStore.Create(beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}, Status: "open"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := sessionStore.Create(beads.Bead{Title: "wait", Type: "gate", Labels: []string{"gc:wait"}, Status: "open"}); err != nil {
		t.Fatalf("create wait: %v", err)
	}
	task, err := work.Create(beads.Bead{Title: "real work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ready, err := work.Ready()
	if err != nil {
		t.Fatalf("work Ready(): %v", err)
	}
	if len(ready) != 1 || ready[0].ID != task.ID {
		t.Fatalf("work store Ready() must contain only the real work task %s; got %d beads — session/wait beads must live on the session store, never the work store", task.ID, len(ready))
	}
}
