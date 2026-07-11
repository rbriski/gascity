package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// splitFrontdoorAgent is the qualified agent identity every session bead in this
// file is labeled for. controlDispatcherRuntimeMissing selects beads by their
// agent:<qualified> label, so the label must match the qualified name the tests
// query with.
const splitFrontdoorAgent = "gc-contrib/control-dispatcher"

// splitFrontdoorSessionName is the session_name/mailbox identity used by the
// session beads in this file. It is the durable handle the mail and assignee
// resolvers match against.
const splitFrontdoorSessionName = "control-dispatcher"

// splitSessionBead builds an open session bead resolvable by name/agent-label so
// a split-city test can place it in the SESSION (infra) store and assert the
// handler reads it there rather than on the work store.
func splitSessionBead(id, state, sleepReason string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{"agent:" + splitFrontdoorAgent, session.LabelSession},
		Metadata: map[string]string{
			"session_name": splitFrontdoorSessionName,
			"state":        state,
			"sleep_reason": sleepReason,
		},
	}
}

// TestControlDispatcherRuntimeMissingReadsSessionStoreOnSplitCity pins the
// session-read routing fix in handler_sling.go: controlDispatcherRuntimeMissing
// projects the runtime-missing lifecycle reason off SESSION-class beads, so on a
// split city it must read the infra (session) store — not the work store. A
// session bead placed ONLY in the session store must be seen; the same bead
// placed ONLY in the work store must be invisible (proving the read is routed).
func TestControlDispatcherRuntimeMissingReadsSessionStoreOnSplitCity(t *testing.T) {
	qualified := splitFrontdoorAgent
	bead := splitSessionBead("gc-cd1", "asleep", "runtime-missing")

	t.Run("session bead in the infra store is seen", func(t *testing.T) {
		work := beads.NewMemStore()
		sessions := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
		st := newFakeState(t)
		st.cityBeadStore = work
		st.sessionsBeadStore = sessions // distinct infra store owns the session bead
		s := New(st)

		if !s.controlDispatcherRuntimeMissing(qualified) {
			t.Fatalf("controlDispatcherRuntimeMissing = false; want true (session bead lives in the infra store)")
		}
	})

	t.Run("session bead only in the work store is not read", func(t *testing.T) {
		work := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
		sessions := beads.NewMemStore() // empty infra store
		st := newFakeState(t)
		st.cityBeadStore = work
		st.sessionsBeadStore = sessions
		s := New(st)

		if s.controlDispatcherRuntimeMissing(qualified) {
			t.Fatalf("controlDispatcherRuntimeMissing = true; want false (handler must read the session store, not the work store)")
		}
	})
}

// TestControlDispatcherRuntimeMissingByteIdenticalOnSingleStoreCity confirms the
// fix is a no-op on a legacy single-store city: SessionsBeadStore().Store ==
// CityBeadStore(), so the session bead on the sole store is still found.
func TestControlDispatcherRuntimeMissingByteIdenticalOnSingleStoreCity(t *testing.T) {
	bead := splitSessionBead("gc-cd1", "asleep", "runtime-missing")

	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
	st := newFakeState(t)
	st.cityBeadStore = store
	// sessionsBeadStore left nil => SessionsBeadStore() collapses onto cityBeadStore.
	s := New(st)

	if !s.controlDispatcherRuntimeMissing(splitFrontdoorAgent) {
		t.Fatalf("controlDispatcherRuntimeMissing = false on a single-store city; want true")
	}
}

// TestSlingSplitGraphStoreRoutesToGraphStoreOnSplitCity pins the highest-severity
// fix: the API sling path must set SlingDeps.GraphStore to the graph-class store
// on a split city so the molecule explosion lands in the graph (infra) store, and
// must leave it nil on a single-store city so SlingDeps.graphStore() collapses
// onto deps.Store — byte-identical to the pre-seam rig/work-store destination.
func TestSlingSplitGraphStoreRoutesToGraphStoreOnSplitCity(t *testing.T) {
	t.Run("split city returns the distinct graph store", func(t *testing.T) {
		work := beads.NewMemStore()
		graph := beads.NewMemStore()
		st := newFakeState(t)
		st.cityBeadStore = work
		st.graphBeadStore = graph // dedicated, distinct graph store
		s := New(st)

		got := s.slingSplitGraphStore()
		if got != graph {
			t.Fatalf("slingSplitGraphStore = %p, want the graph store %p on a split city", got, graph)
		}
		if got == work {
			t.Fatalf("slingSplitGraphStore returned the work store; molecule writes would leak to the work store")
		}
	})

	t.Run("single-store city returns nil so graph collapses onto deps.Store", func(t *testing.T) {
		store := beads.NewMemStore()
		st := newFakeState(t)
		st.cityBeadStore = store
		// graphBeadStore left nil => GraphBeadStore() == CityBeadStore().
		s := New(st)

		if got := s.slingSplitGraphStore(); got != nil {
			t.Fatalf("slingSplitGraphStore = %p on a single-store city; want nil (leave GraphStore unset so it collapses onto deps.Store)", got)
		}
	})
}

// TestSourceWorkflowStoresIncludesGraphStoreOnSplitCity pins the split-city
// singleton/recovery coverage fix: on a split city the workflow molecule roots
// live in the graph (infra) store, so sourceWorkflowStores must include it (with
// an empty StoreRef, matching the CLI) — otherwise a duplicate workflow on a
// relocated graph goes undetected. On a single-store city the arm is skipped so
// coverage stays byte-identical.
func TestSourceWorkflowStoresIncludesGraphStoreOnSplitCity(t *testing.T) {
	t.Run("split city includes the distinct graph store", func(t *testing.T) {
		work := beads.NewMemStore()
		graph := beads.NewMemStore()
		st := newFakeState(t)
		st.cityBeadStore = work
		st.graphBeadStore = graph
		st.stores = nil
		st.cfg.Rigs = nil
		s := New(st)

		got := s.sourceWorkflowStores()
		var sawGraph bool
		for _, sw := range got {
			if sw.Store == graph {
				sawGraph = true
				if sw.StoreRef != "" {
					t.Errorf("graph SourceWorkflowStore.StoreRef = %q, want empty (mirrors the CLI infra entry)", sw.StoreRef)
				}
			}
		}
		if !sawGraph {
			t.Fatalf("sourceWorkflowStores did not include the graph store on a split city; molecule roots would be invisible to singleton/recovery scans")
		}
	})

	t.Run("single-store city does not double-list the sole store", func(t *testing.T) {
		store := beads.NewMemStore()
		st := newFakeState(t)
		st.cityBeadStore = store
		st.stores = nil
		st.cfg.Rigs = nil
		// graphBeadStore left nil => GraphBeadStore().Store == CityBeadStore().
		s := New(st)

		got := s.sourceWorkflowStores()
		count := 0
		for _, sw := range got {
			if sw.Store == store {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("sourceWorkflowStores listed the sole store %d times on a single-store city; want exactly 1 (graph arm must be skipped)", count)
		}
	})
}

// TestMailSendRecipientResolvesAgainstSessionStoreOnSplitCity pins the mail
// session-read routing fix: resolveMailSendRecipientWithContext resolves a
// recipient to a session bead and reads its mailbox identity, so on a split city
// it must read the session (infra) store. A named session bead in the session
// store must resolve; the same bead only on the work store must not.
func TestMailSendRecipientResolvesAgainstSessionStoreOnSplitCity(t *testing.T) {
	const recipient = "control-dispatcher"
	// A session bead with a mailbox address (session_name is a mailbox identity).
	bead := splitSessionBead("gc-md1", "awake", "")

	t.Run("session bead in the infra store resolves", func(t *testing.T) {
		work := beads.NewMemStore()
		sessions := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
		st := newFakeState(t)
		st.cityBeadStore = work
		st.sessionsBeadStore = sessions
		s := New(st)

		addr, err := s.resolveMailSendRecipientWithContext(context.Background(), recipient)
		if err != nil {
			t.Fatalf("resolveMailSendRecipientWithContext error = %v; want the recipient to resolve from the session store", err)
		}
		if addr == "" {
			t.Fatalf("resolveMailSendRecipientWithContext returned empty address; recipient did not resolve")
		}
	})

	t.Run("session bead only on the work store does not resolve", func(t *testing.T) {
		work := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
		sessions := beads.NewMemStore()
		st := newFakeState(t)
		st.cityBeadStore = work
		st.sessionsBeadStore = sessions
		s := New(st)

		if _, err := s.resolveMailSendRecipientWithContext(context.Background(), recipient); err == nil {
			t.Fatalf("resolveMailSendRecipientWithContext resolved a recipient from the work store; want it to read the session store and fail")
		}
	})
}

// TestBeadListAssigneeTermsReadsSessionStoreOnSplitCity pins the beads-handler
// session-read routing fix: beadListAssigneeTerms resolves an assignee to a
// session bead and expands its identity forms, so on a split city it must read
// the session (infra) store. When the session bead lives in the infra store the
// resolved identity forms (session_name) are included; when it is missing there
// the terms collapse to the bare assignee.
func TestBeadListAssigneeTermsReadsSessionStoreOnSplitCity(t *testing.T) {
	const assignee = "gc-md1"
	bead := splitSessionBead("gc-md1", "awake", "")

	work := beads.NewMemStore()
	sessions := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
	st := newFakeState(t)
	st.cityBeadStore = work
	st.sessionsBeadStore = sessions
	s := New(st)

	terms := s.beadListAssigneeTerms(context.Background(), assignee)
	if !containsString(terms, "control-dispatcher") {
		t.Fatalf("beadListAssigneeTerms(%q) = %v; want the resolved session_name term (read from the session store)", assignee, terms)
	}
}
