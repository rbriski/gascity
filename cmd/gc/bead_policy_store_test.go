package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

type captureCreateStore struct {
	beads.Store
	created []beads.Bead
}

func (s *captureCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	s.created = append(s.created, b)
	return s.Store.Create(b)
}

type captureGraphStore struct {
	beads.Store
	plan *beads.GraphApplyPlan
}

func (s *captureGraphStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) { //nolint:unparam // interface compliance; error always nil in spy
	next := *plan
	s.plan = &next
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		ids[node.Key] = "bd-" + node.Key
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

func TestBeadPolicyStoreAppliesDefaultStorageForAllowlistedCreates(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session",
			bead: beads.Bead{Title: "session", Type: session.BeadType, Labels: []string{session.LabelSession}},
			want: beadStorageNoHistory,
		},
		{
			name: "wait",
			bead: beads.Bead{Title: "wait", Type: session.WaitBeadType, Labels: []string{session.WaitBeadLabel}},
			want: beadStorageNoHistory,
		},
		{
			name: "nudge",
			bead: beads.Bead{Title: "nudge", Type: nudgeBeadType, Labels: []string{nudgeBeadLabel}},
			want: beadStorageNoHistory,
		},
		{
			name: "order tracking",
			bead: beads.Bead{Title: "order:daily", Labels: orderTrackingLabels("daily")},
			want: beadStorageEphemeral,
		},
		{
			name: "wisp root",
			bead: beads.Bead{Title: "wisp", Labels: []string{molecule.WispLabel}},
			want: beadStorageEphemeral,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			backing.created = nil
			if _, err := store.Create(tt.bead); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(backing.created) != 1 {
				t.Fatalf("captured creates = %d, want 1", len(backing.created))
			}
			assertBeadStorage(t, backing.created[0], tt.want)
		})
	}
}

func TestBeadPolicyStoreStorageOverrides(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
			beadPolicySession:       {Storage: beadStorageHistory},
			beadPolicyOrderTracking: {Storage: beadStorageNoHistory},
		}},
	})

	if _, err := store.Create(beads.Bead{
		Title:     "session",
		Type:      session.BeadType,
		Labels:    []string{session.LabelSession},
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	assertBeadStorage(t, backing.created[0], beadStorageHistory)

	if _, err := store.Create(beads.Bead{
		Title:  "order:daily",
		Labels: orderTrackingLabels("daily"),
	}); err != nil {
		t.Fatalf("Create(order tracking): %v", err)
	}
	assertBeadStorage(t, backing.created[1], beadStorageNoHistory)
}

func TestBeadPolicyStoreAppliesWispRootStorageToSequentialChildren(t *testing.T) {
	backing := &captureCreateStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
			beadPolicyWisp: {Storage: beadStorageNoHistory},
		}},
	})

	root, err := store.Create(beads.Bead{Title: "root", Labels: []string{molecule.WispLabel}})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "child",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	}); err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	if len(backing.created) != 2 {
		t.Fatalf("captured creates = %d, want 2", len(backing.created))
	}
	assertBeadStorage(t, backing.created[0], beadStorageNoHistory)
	assertBeadStorage(t, backing.created[1], beadStorageNoHistory)
}

func TestBeadPolicyGraphStoreAppliesWispStorageToGraphPlan(t *testing.T) {
	backing := &captureGraphStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{
		Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
			beadPolicyWisp: {Storage: beadStorageNoHistory},
		}},
	})
	applier, ok := store.(beads.GraphApplyStore)
	if !ok {
		t.Fatal("wrapped graph store does not implement GraphApplyStore")
	}

	_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "Root", Labels: []string{molecule.WispLabel}},
			{Key: "child", Title: "Child", MetadataRefs: map[string]string{"gc.root_bead_id": "root"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.plan == nil {
		t.Fatal("graph plan was not captured")
	}
	if !backing.plan.NoHistory || backing.plan.Ephemeral {
		t.Fatalf("plan storage = ephemeral:%v no_history:%v, want no-history graph", backing.plan.Ephemeral, backing.plan.NoHistory)
	}
}

func TestPolicyReadPathsIncludeHistoryAndNoHistoryRows(t *testing.T) {
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch cmd {
		case "bd list --json --type=session --include-infra --include-gates --limit 0",
			"bd list --json --label=gc:session --include-infra --include-gates --limit 0":
			return []byte(`[
				{"id":"bd-old-session","title":"old session","status":"open","issue_type":"session","created_at":"2026-05-01T00:00:00Z","labels":["gc:session"],"metadata":{"session_name":"old"}},
				{"id":"bd-new-session","title":"new session","status":"open","issue_type":"session","created_at":"2026-05-01T00:00:01Z","labels":["gc:session"],"metadata":{"session_name":"new"},"no_history":true}
			]`), nil
		case "bd list --json --label=gc:wait --include-infra --include-gates --limit 0",
			"bd list --json --label=gc:wait --include-infra --include-gates --limit 1001":
			return []byte(`[
				{"id":"bd-old-wait","title":"old wait","status":"open","issue_type":"gate","created_at":"2026-05-01T00:00:00Z","labels":["gc:wait"],"metadata":{"session_id":"s1"}},
				{"id":"bd-new-wait","title":"new wait","status":"open","issue_type":"gate","created_at":"2026-05-01T00:00:01Z","labels":["gc:wait"],"metadata":{"session_id":"s2"},"no_history":true}
			]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", cmd)
		}
	}
	store := beads.NewBdStore("/city", runner)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want history and no-history rows", sessions)
	}
	if !sessions[1].NoHistory {
		t.Fatalf("new session row = %+v, want no_history parsed", sessions[1])
	}

	waits, err := loadWaitBeads(store)
	if err != nil {
		t.Fatalf("loadWaitBeads: %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("waits = %+v, want history and no-history rows", waits)
	}
	foundNoHistoryWait := false
	for _, wait := range waits {
		if wait.ID == "bd-new-wait" {
			foundNoHistoryWait = wait.NoHistory
			break
		}
	}
	if !foundNoHistoryWait {
		t.Fatalf("waits = %+v, want bd-new-wait with no_history parsed", waits)
	}
}

func assertBeadStorage(t *testing.T, b beads.Bead, want string) {
	t.Helper()
	switch want {
	case beadStorageHistory:
		if b.Ephemeral || b.NoHistory {
			t.Fatalf("bead storage = ephemeral:%v no_history:%v, want history", b.Ephemeral, b.NoHistory)
		}
	case beadStorageEphemeral:
		if !b.Ephemeral || b.NoHistory {
			t.Fatalf("bead storage = ephemeral:%v no_history:%v, want ephemeral", b.Ephemeral, b.NoHistory)
		}
	case beadStorageNoHistory:
		if b.Ephemeral || !b.NoHistory {
			t.Fatalf("bead storage = ephemeral:%v no_history:%v, want no-history", b.Ephemeral, b.NoHistory)
		}
	default:
		t.Fatalf("unknown expected storage %q", want)
	}
}
