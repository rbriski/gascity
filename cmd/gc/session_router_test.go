package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// sessionSQLiteCfg returns a city config that opts the session class onto the
// embedded SQLite backend (the relocated, non-default backend).
func sessionSQLiteCfg() *config.City {
	cfg := &config.City{}
	cfg.Beads.Classes = map[string]config.BeadClassConfig{
		config.BeadClassSessions: {Backend: config.BeadsBackendSQLite},
	}
	return cfg
}

func TestSessionRelocatedPredicate(t *testing.T) {
	if sessionRelocated(&config.City{}) {
		t.Fatal("default city (sessions on bd work store) must report sessionRelocated=false")
	}
	if !sessionRelocated(sessionSQLiteCfg()) {
		t.Fatal("[beads.classes.sessions].backend=sqlite must report sessionRelocated=true")
	}
}

// TestRoutedPolicyStoreBuildsRouterForSessionRelocation proves the opt-in
// boundary for sessions mirrors graph: no session relocation → plain
// policy(workBackend), no Router (byte-identical); sessions relocated → a Router
// with a registered ClassSessions backend.
func TestRoutedPolicyStoreBuildsRouterForSessionRelocation(t *testing.T) {
	off := routedPolicyStore(beads.NewMemStore(), &config.City{}, t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(off) })
	base, _, ok := unwrapBeadPolicyStore(off)
	if !ok {
		t.Fatal("expected the default result to be policy-wrapped")
	}
	if _, isRouter := base.(*coordrouter.Router); isRouter {
		t.Fatal("sessions-not-relocated must not insert a Router")
	}

	on := routedPolicyStore(beads.NewMemStore(), sessionSQLiteCfg(), t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(on) })
	base, _, ok = unwrapBeadPolicyStore(on)
	if !ok {
		t.Fatal("expected the opted-in result to be policy-wrapped")
	}
	router, isRouter := base.(*coordrouter.Router)
	if !isRouter {
		t.Fatalf("sessions=sqlite must insert a *coordrouter.Router, got %T", base)
	}
	// The session backend must be a DISTINCT store from the work primary.
	if router.Backend(coordclass.ClassSessions) == router.Backend(coordclass.ClassWork) {
		t.Fatal("ClassSessions backend must be distinct from the work backend after relocation")
	}
}

// TestSessionBeadsRelocateOnNoHistoryTierThroughPolicyRouter is the Phase C
// (tier) guard for the Router-relocated session class. The tier divergence the
// migration review flagged — relocated stores opening RAW, bypassing the policy
// wrapper, so infra beads land on the main/issues tier and only a label exclusion
// guards Ready — does NOT apply to sessions, because routedPolicyStore wraps the
// Router as policy(Router(...)): the policy computes the no-history tier and
// Router.CreateWithStorage forwards it to the session backend. This proves a
// session bead AND a durable wait bead created through the city store land on the
// no-history tier on the relocated session backend, so the tier gate (not just the
// label exclusion) keeps them out of Ready after relocation.
func TestSessionBeadsRelocateOnNoHistoryTierThroughPolicyRouter(t *testing.T) {
	dir := t.TempDir()
	store := routedPolicyStore(beads.NewMemStore(), sessionSQLiteCfg(), dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })

	for _, b := range []beads.Bead{
		{Title: "session", Type: "session", Labels: []string{"gc:session"}},
		{Title: "wait", Type: "gate", Labels: []string{"gc:wait"}},
	} {
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %s through policy(Router): %v", b.Title, err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("get %s: %v", created.ID, err)
		}
		if !got.NoHistory {
			t.Fatalf("%s bead %s must be stored on the no-history tier (policy wraps the Router so the seam tier divergence does NOT apply to Router-relocated sessions); got NoHistory=false", b.Title, created.ID)
		}
	}
}

// TestRelocatedSessionBeadsNeverLeakIntoReady is the end-to-end Phase C guard.
// The Router FEDERATES Ready() across its backends, so after relocation the
// controller's Ready scan also reaches the session backend. Session beads
// (type=session) and durable waits (type=gate) must never surface as actionable
// Ready work. This is guaranteed store-independently by readyExcludeTypes
// (session + gate) plus the gc:session label exclusion — stronger than the tier
// gate, which is why the nudge-style tier divergence does not endanger sessions.
// A normal work task created alongside MUST still be Ready (positive control).
func TestRelocatedSessionBeadsNeverLeakIntoReady(t *testing.T) {
	dir := t.TempDir()
	work, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	t.Cleanup(func() { _ = work.(interface{ CloseStore() error }).CloseStore() })
	store := routedPolicyStore(work, sessionSQLiteCfg(), dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })

	sess, err := store.Create(beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}, Status: "open"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	wait, err := store.Create(beads.Bead{Title: "wait", Type: "gate", Labels: []string{"gc:wait"}, Status: "open"})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	task, err := store.Create(beads.Bead{Title: "real work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("federated Ready(): %v", err)
	}
	inReady := map[string]bool{}
	for _, b := range ready {
		inReady[b.ID] = true
	}
	if inReady[sess.ID] {
		t.Fatalf("session bead %s leaked into federated Ready() after relocation", sess.ID)
	}
	if inReady[wait.ID] {
		t.Fatalf("wait bead %s leaked into federated Ready() after relocation", wait.ID)
	}
	if !inReady[task.ID] {
		t.Fatalf("real work bead %s missing from Ready() — the exclusion must be specific to session/wait beads", task.ID)
	}
}

// TestSessionStoreBackendRoutesSessionAndWorkSplit is the keystone guard for the
// sessions-on-the-Router design. It proves that, with a Router over a SEPARATE
// session backend (SQLite, gcs prefix) and a work backend (mem), the federating
// Router routes:
//   - session/wait bead Create → the session backend (by coordclass.Classify),
//   - session/wait by-id Get/Close → the session backend (prefix + federation),
//   - a work List keyed on the session's identity → federated, finding the WORK
//     bead that lives on the work backend (the close family's work-release read),
//   - a work by-id Update → the work backend.
//
// This single test covers BOTH the strawman route-by-query mis-route AND the
// mass-closure landmine: a regression that sent the work read to the (work-empty)
// session backend would return zero work and the assertion on the federated List
// would fail. Sessions and work beads live on different physical backends here,
// so default-backend equality cannot mask a routing bug.
func TestSessionStoreBackendRoutesSessionAndWorkSplit(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	router := coordrouter.New(work)
	registerSessionStoreBackend(router, sessionSQLiteCfg(), cityPath)

	sessionBackend := router.Backend(coordclass.ClassSessions)
	if sessionBackend == work {
		t.Fatal("registerSessionStoreBackend did not register a distinct session backend")
	}

	// 1. A session bead Creates onto the session backend with a gcs- id.
	sess, err := router.Create(beads.Bead{Title: "agent session", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatalf("create session bead via Router: %v", err)
	}
	if !strings.HasPrefix(sess.ID, "gcs-") {
		t.Fatalf("session bead id %q must carry the disjoint gcs- prefix so by-id routing short-circuits", sess.ID)
	}
	if _, err := sessionBackend.Get(sess.ID); err != nil {
		t.Fatalf("session bead %s must reside on the session backend: %v", sess.ID, err)
	}
	if _, err := work.Get(sess.ID); err == nil {
		t.Fatalf("session bead %s must NOT reside on the work backend", sess.ID)
	}

	// 2. A wait bead (gc:wait) also routes to the session backend (waits relocate
	//    with the session class).
	wait, err := router.Create(beads.Bead{Title: "wait", Type: "gate", Labels: []string{"gc:wait"}})
	if err != nil {
		t.Fatalf("create wait bead via Router: %v", err)
	}
	if _, err := sessionBackend.Get(wait.ID); err != nil {
		t.Fatalf("wait bead %s must reside on the session backend: %v", wait.ID, err)
	}

	// 3. A work bead assigned to the session lands on the WORK backend.
	workItem, err := router.Create(beads.Bead{Title: "do the thing", Type: "task", Assignee: sess.ID, Status: "open"})
	if err != nil {
		t.Fatalf("create work bead via Router: %v", err)
	}
	if strings.HasPrefix(workItem.ID, "gcs-") {
		t.Fatalf("work bead id %q must NOT be a session (gcs-) id", workItem.ID)
	}
	if _, err := work.Get(workItem.ID); err != nil {
		t.Fatalf("work bead %s must reside on the work backend: %v", workItem.ID, err)
	}

	// 3b. A session-bead List (by the gc:session label, as the controller's
	//     loadSessionBeads/ListAllSessionBeads snapshot issues) FEDERATES and finds
	//     the session bead on the relocated session backend — without this the
	//     controller's per-tick session snapshot would be empty after relocation.
	sessions, err := router.List(beads.ListQuery{Label: "gc:session"})
	if err != nil {
		t.Fatalf("federated List(Label=gc:session): %v", err)
	}
	foundSession := false
	for _, b := range sessions {
		if b.ID == sess.ID {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatalf("federated List(Label=gc:session) must find the relocated session bead %s on the session backend; got %d", sess.ID, len(sessions))
	}

	// 4. The close-family work read (List by the session's assignee identity)
	//    federates and finds the work bead on the work backend, even though the
	//    session bead lives on the session backend. THIS is the mass-closure gate.
	assigned, err := router.List(beads.ListQuery{Assignee: sess.ID, Status: "open", Live: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("federated List(Assignee=%s): %v", sess.ID, err)
	}
	foundWork := false
	for _, b := range assigned {
		if b.ID == workItem.ID {
			foundWork = true
		}
	}
	if !foundWork {
		t.Fatalf("federated List(Assignee=%s) must find the work bead %s on the work backend; got %d beads — a regression here would see the session as having no assigned work and mass-close live sessions", sess.ID, workItem.ID, len(assigned))
	}

	// 5. Closing the session by id routes to the session backend.
	if err := router.Close(sess.ID); err != nil {
		t.Fatalf("Router.Close(session %s): %v", sess.ID, err)
	}
	closed, err := sessionBackend.Get(sess.ID)
	if err != nil {
		t.Fatalf("get closed session bead: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("session bead %s status = %q after Close, want closed (Close must land on the session backend)", sess.ID, closed.Status)
	}

	// 6. Releasing the work (clearing its assignee) by id routes to the work
	//    backend, not the session backend.
	empty := ""
	if err := router.Update(workItem.ID, beads.UpdateOpts{Assignee: &empty}); err != nil {
		t.Fatalf("Router.Update(work %s): %v", workItem.ID, err)
	}
	released, err := work.Get(workItem.ID)
	if err != nil {
		t.Fatalf("get released work bead: %v", err)
	}
	if strings.TrimSpace(released.Assignee) != "" {
		t.Fatalf("work bead %s assignee = %q after release, want empty (Update must land on the work backend)", workItem.ID, released.Assignee)
	}
}
