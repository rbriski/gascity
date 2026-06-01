package main

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	beadStorageHistory   = config.BeadStorageHistory
	beadStorageNoHistory = config.BeadStorageNoHistory
	beadStorageEphemeral = config.BeadStorageEphemeral
)

type beadPolicyStore struct {
	beads.Store
	cfg *config.City

	mu               sync.Mutex
	wispRootStorage  map[string]string
	wispRootPolicies map[string]string
}

type beadPolicyGraphStore struct {
	*beadPolicyStore
	applier beads.GraphApplyStore
}

func wrapStoreWithBeadPolicies(store beads.Store, cfg *config.City) beads.Store {
	if store == nil {
		return nil
	}
	policyStore := &beadPolicyStore{
		Store:            store,
		cfg:              cfg,
		wispRootStorage:  make(map[string]string),
		wispRootPolicies: make(map[string]string),
	}
	if applier, ok := store.(beads.GraphApplyStore); ok {
		return &beadPolicyGraphStore{
			beadPolicyStore: policyStore,
			applier:         applier,
		}
	}
	return policyStore
}

func unwrapBeadPolicyStore(store beads.Store) (beads.Store, *beadPolicyStore, bool) {
	switch s := store.(type) {
	case *beadPolicyGraphStore:
		return s.Store, s.beadPolicyStore, true
	case *beadPolicyStore:
		return s.Store, s, true
	default:
		return store, nil, false
	}
}

func (s *beadPolicyStore) Handles() beads.StoreHandles {
	if s == nil || s.Store == nil {
		return beads.StoreHandles{}
	}
	handles := beads.HandlesFor(s.Store)
	handles.Writer = s
	return handles
}

func (s *beadPolicyStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	prefixStore, ok := s.Store.(explicitBeadIDStore)
	if !ok {
		return ""
	}
	return prefixStore.IDPrefix()
}

func (s *beadPolicyStore) Create(b beads.Bead) (beads.Bead, error) {
	policyName, storage := s.policyForCreate(b)
	if storage != "" {
		b = applyBeadStorage(b, storage)
	}
	created, err := s.Store.Create(b)
	if err != nil {
		return created, err
	}
	if policyName == beadPolicyWisp && created.ID != "" {
		s.mu.Lock()
		s.wispRootStorage[created.ID] = storage
		s.wispRootPolicies[created.ID] = policyName
		s.mu.Unlock()
	}
	return created, nil
}

func (s *beadPolicyStore) policyForCreate(b beads.Bead) (string, string) {
	if rootID := strings.TrimSpace(b.Metadata["gc.root_bead_id"]); rootID != "" {
		s.mu.Lock()
		storage := s.wispRootStorage[rootID]
		policyName := s.wispRootPolicies[rootID]
		s.mu.Unlock()
		if storage != "" {
			return policyName, storage
		}
	}
	policyName := policyNameForBead(b)
	if policyName == "" {
		return "", ""
	}
	return policyName, effectiveBeadStorage(s.cfg, policyName)
}

func (s *beadPolicyGraphStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	if plan == nil {
		return s.applier.ApplyGraphPlan(ctx, plan)
	}
	policyName := policyNameForGraphPlan(plan)
	if policyName == "" {
		return s.applier.ApplyGraphPlan(ctx, plan)
	}
	storage := effectiveBeadStorage(s.cfg, policyName)
	next := *plan
	next = applyGraphStorage(next, storage)
	return s.applier.ApplyGraphPlan(ctx, &next)
}

func policyNameForGraphPlan(plan *beads.GraphApplyPlan) string {
	for _, node := range plan.Nodes {
		if hasBeadLabel(node.Labels, molecule.WispLabel) || node.Metadata["gc.kind"] == "wisp" {
			return beadPolicyWisp
		}
	}
	return ""
}

func policyNameForBead(b beads.Bead) string {
	switch {
	case hasBeadLabel(b.Labels, molecule.WispLabel) || b.Metadata["gc.kind"] == "wisp":
		return beadPolicyWisp
	case hasBeadLabel(b.Labels, labelOrderTracking):
		return beadPolicyOrderTracking
	case hasBeadLabel(b.Labels, session.LabelSession) || b.Type == session.BeadType:
		return beadPolicySession
	case hasBeadLabel(b.Labels, session.WaitBeadLabel):
		return beadPolicyWait
	case hasBeadLabel(b.Labels, nudgeBeadLabel):
		return beadPolicyNudge
	default:
		return ""
	}
}

func effectiveBeadStorage(cfg *config.City, policyName string) string {
	if cfg != nil {
		if policy, ok := cfg.Beads.Policies[policyName]; ok {
			if storage := normalizeBeadStorage(policy.Storage); storage != "" {
				if config.ValidBeadPolicyStorage(storage) {
					return storage
				}
				return defaultBeadStorage(policyName)
			}
		}
	}
	return defaultBeadStorage(policyName)
}

func defaultBeadStorage(policyName string) string {
	switch policyName {
	case beadPolicyWisp, beadPolicyOrderTracking:
		return beadStorageEphemeral
	case beadPolicySession, beadPolicyWait, beadPolicyNudge:
		return beadStorageNoHistory
	default:
		return ""
	}
}

func normalizeBeadStorage(storage string) string {
	return config.NormalizeBeadPolicyStorage(storage)
}

func applyBeadStorage(b beads.Bead, storage string) beads.Bead {
	switch normalizeBeadStorage(storage) {
	case beadStorageEphemeral:
		b.Ephemeral = true
		b.NoHistory = false
	case beadStorageNoHistory:
		b.Ephemeral = false
		b.NoHistory = true
	case beadStorageHistory:
		b.Ephemeral = false
		b.NoHistory = false
	}
	return b
}

func applyGraphStorage(plan beads.GraphApplyPlan, storage string) beads.GraphApplyPlan {
	switch normalizeBeadStorage(storage) {
	case beadStorageEphemeral:
		plan.Ephemeral = true
		plan.NoHistory = false
	case beadStorageNoHistory:
		plan.Ephemeral = false
		plan.NoHistory = true
	case beadStorageHistory:
		plan.Ephemeral = false
		plan.NoHistory = false
	}
	return plan
}

func hasBeadLabel(labels []string, label string) bool {
	return slices.Contains(labels, label)
}
