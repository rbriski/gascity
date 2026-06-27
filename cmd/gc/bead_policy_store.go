package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	beadStorageHistory   = config.BeadStorageHistory
	beadStorageNoHistory = config.BeadStorageNoHistory
	beadStorageEphemeral = config.BeadStorageEphemeral

	beadPolicyWisp          = "wisp"
	beadPolicyWorkflow      = "workflow"
	beadPolicyOrderTracking = "order_tracking"
	beadPolicySession       = "session"
	beadPolicyWait          = "wait"
	beadPolicyNudge         = "nudge"
)

type beadPolicyStore struct {
	beads.Store
	cfg *config.City
	// graphStore is the dedicated graph-class store (resolveGraphStore, legacy
	// .gc/beads.sqlite). When the graph class is not relocated it equals Store, so
	// the class-aware create-chokepoint below is a no-op (byte-identical default).
	graphStore beads.Store
}

type beadPolicyGraphStore struct {
	*beadPolicyStore
	applier      beads.GraphApplyStore // work-class plans (GraphApplyFor(Store))
	graphApplier beads.GraphApplyStore // graph-class plans (GraphApplyFor(graphStore)); nil when graph not relocated
}

var _ beads.ConditionalAssignmentReleaser = (*beadPolicyStore)(nil)

// wrapStoreWithBeadPolicies wraps store in the bead-storage policy layer. cityPath
// (optional, but ALWAYS passed by production callers) lets the policy store resolve
// the dedicated graph store via resolveGraphStore — the legacy <cityPath>/.gc/beads.sqlite
// location — so graph-class Create/ApplyGraphPlan route there with the correct storage
// tier. This is the class-aware create-chokepoint that keeps graph beads off the work
// (Dolt) store once coordrouter is retired: any graph bead created through a policy
// store lands on the graph store, even from a caller that is not itself class-aware.
//
// When the graph class is not relocated, resolveGraphStore returns store, so graphStore
// == Store and the chokepoint is a no-op (byte-identical default). The graph applier is
// sourced from graphStore (not store) for the same reason. cityPath is variadic so the
// many graph=bd unit tests stay unchanged; a graph-relocated city MUST pass it or the
// chokepoint silently disables (the loud log flags that caller bug).
func wrapStoreWithBeadPolicies(store beads.Store, cfg *config.City, cityPath ...string) beads.Store {
	if store == nil {
		return nil
	}
	graphStore := store
	switch {
	case len(cityPath) > 0 && strings.TrimSpace(cityPath[0]) != "":
		graphStore = resolveGraphStore(store, cfg, cityPath[0], nil)
	case graphRelocated(cfg):
		log.Printf("beads: wrapStoreWithBeadPolicies called without cityPath for a graph-relocated city; graph-class creates stay on the work store (caller must pass cityPath)")
	}
	policyStore := &beadPolicyStore{
		Store:      store,
		cfg:        cfg,
		graphStore: graphStore,
	}
	if applier, ok := beads.GraphApplyFor(store); ok {
		gs := &beadPolicyGraphStore{
			beadPolicyStore: policyStore,
			applier:         applier,
		}
		// When graph is relocated, a graph-class plan applies to the graph store; a
		// work-class plan (legacy recipe) stays on the work applier. Sourcing the
		// graph applier from graphStore (not store) is what keeps graph pours off the
		// work store once the Router — which used to route the plan by class — is gone.
		if graphStore != store {
			if graphApplier, ok := beads.GraphApplyFor(graphStore); ok {
				gs.graphApplier = graphApplier
			}
		}
		return gs
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

func (s *beadPolicyStore) Create(b beads.Bead) (beads.Bead, error) {
	_, storage := s.policyForCreate(b)
	return createWithStoragePolicy(s.createTarget(b), b, storage)
}

// createTarget routes a graph-class bead to the dedicated graph store — the
// create-chokepoint that prevents graph beads orphaning onto the work store once the
// per-class Router is gone. When graph is not relocated graphStore == Store, so this
// returns Store for every bead (byte-identical default).
func (s *beadPolicyStore) createTarget(b beads.Bead) beads.Store {
	if s.graphStore != nil && s.graphStore != s.Store && coordclass.Classify(b) == coordclass.ClassGraph {
		return s.graphStore
	}
	return s.Store
}

// getForPolicy resolves a bead by id across the work and graph stores so a
// graph-resident root (e.g. a wisp root, used to derive a child's storage tier) is
// found even after the Router is gone. Byte-identical default: when graph is not
// relocated the set is just Store.
func (s *beadPolicyStore) getForPolicy(id string) (beads.Bead, error) {
	if s.graphStore != nil && s.graphStore != s.Store {
		return storeref.Resolve(id, []beads.Store{s.Store, s.graphStore})
	}
	return s.Store.Get(id)
}

func (s *beadPolicyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	query = expandPolicyReadTier(query)
	return s.Store.List(query)
}

func (s *beadPolicyStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.Store.Ready(expandPolicyReadyQuery(query...))
}

// Count implements beads.Counter with the same read-tier expansion as List.
// The embedded Store interface does not promote optional capabilities, so
// the delegation must be explicit. Inner stores without a Counter report
// ErrCountUnsupported, signaling callers to fall back to List.
func (s *beadPolicyStore) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	counter, ok := s.Store.(beads.Counter)
	if !ok {
		return 0, fmt.Errorf("counting beads: policy-wrapped store: %w", beads.ErrCountUnsupported)
	}
	return counter.Count(ctx, expandPolicyReadTier(query), excludeTypes...)
}

func (s *beadPolicyStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Cached = beadPolicyCachedReader{CachedReader: handles.Cached}
	handles.Live = beadPolicyLiveReader{LiveReader: handles.Live}
	handles.Writer = s
	return handles
}

// ReadyGraphOnlyHandle forwards the graph-only-ready capability to the backing
// store (the Router under graph_store=sqlite). The embedded Store interface does
// not promote optional capabilities, so the delegation is explicit. ok is false
// when the backing has no distinct ClassGraph backend, so capability presence
// gates the worker/dispatcher readiness path on graph_store=sqlite without a
// config lookup. Policy read-tier expansion is preserved.
func (s *beadPolicyStore) ReadyGraphOnlyHandle() (beads.GraphOnlyReadyStore, bool) {
	inner, ok := beads.GraphOnlyReadyFor(s.Store)
	if !ok {
		return nil, false
	}
	return policyGraphOnlyReader{inner: inner}, true
}

type policyGraphOnlyReader struct {
	inner beads.GraphOnlyReadyStore
}

func (r policyGraphOnlyReader) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.inner.ReadyGraphOnly(expandPolicyReadyQuery(query...))
}

type beadPolicyCachedReader struct {
	beads.CachedReader
}

func (r beadPolicyCachedReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	return r.CachedReader.List(expandPolicyReadTier(query))
}

func (r beadPolicyCachedReader) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.CachedReader.Ready(expandPolicyReadyQuery(query...))
}

type beadPolicyLiveReader struct {
	beads.LiveReader
}

func (r beadPolicyLiveReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	return r.LiveReader.List(expandPolicyReadTier(query))
}

func (r beadPolicyLiveReader) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.LiveReader.Ready(expandPolicyReadyQuery(query...))
}

func (s *beadPolicyStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		ParentID:      parentID,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		Sort:          beads.SortCreatedAsc,
		TierMode:      policyTierFromOpts(opts),
	})
}

func (s *beadPolicyStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		TierMode:      policyTierFromOpts(opts),
	})
}

func (s *beadPolicyStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		TierMode: beads.TierBoth,
	})
}

func (s *beadPolicyStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		TierMode:      policyTierFromOpts(opts),
	})
}

func (s *beadPolicyStore) ListOpen(status ...string) ([]beads.Bead, error) {
	wantStatus := "open"
	if len(status) > 0 && strings.TrimSpace(status[0]) != "" {
		wantStatus = status[0]
	}
	return s.List(beads.ListQuery{
		Status:    wantStatus,
		AllowScan: true,
		TierMode:  beads.TierBoth,
	})
}

func (s *beadPolicyStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// Claim forwards an atomic claim to the wrapped store's claim capability, so the
// policy wrapper (the controller's city store = policy(Router(...))) is itself a
// Claimer. For graph_store=sqlite the inner store is the Router, which routes a
// graph-class claim to the SQLite backend with the explicit assignee.
func (s *beadPolicyStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	if c, ok := s.Store.(beads.Claimer); ok {
		return c.Claim(id, assignee)
	}
	if c, ok := s.Store.(beads.EnvActorClaimer); ok {
		return c.Claim(id)
	}
	return beads.Bead{}, false, beads.ErrClaimUnsupported
}

func (s *beadPolicyStore) policyForCreate(b beads.Bead) (string, string) {
	if rootID := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" {
		root, err := s.getForPolicy(rootID)
		if err == nil && policyNameForBead(root) == beadPolicyWisp {
			return beadPolicyWisp, storageFromPersistedWispRoot(root)
		}
	}
	policyName := policyNameForBead(b)
	if policyName == "" {
		return "", ""
	}
	return policyName, effectiveBeadStorage(s.cfg, policyName)
}

func storageFromPersistedWispRoot(root beads.Bead) string {
	switch {
	case root.Ephemeral:
		return beadStorageEphemeral
	case root.NoHistory:
		return beadStorageNoHistory
	default:
		return beadStorageHistory
	}
}

// applierForPlan picks the graph or work applier by plan class — the graph-apply half
// of the create-chokepoint, so a graph.v2 pour lands on the graph store while a
// legacy/work plan stays on the work store once the per-class Router (which used to
// route the plan) is gone. nil graphApplier (graph not relocated) means every plan
// uses the work applier — byte-identical default.
func (s *beadPolicyGraphStore) applierForPlan(plan *beads.GraphApplyPlan) beads.GraphApplyStore {
	if s.graphApplier != nil && coordclass.ClassifyGraphPlan(plan) == coordclass.ClassGraph {
		return s.graphApplier
	}
	return s.applier
}

func (s *beadPolicyGraphStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	applier := s.applierForPlan(plan)
	if plan == nil {
		return applier.ApplyGraphPlan(ctx, plan)
	}
	policyName := policyNameForGraphPlan(plan)
	if policyName == "" {
		return applier.ApplyGraphPlan(ctx, plan)
	}
	storage := effectiveBeadStorage(s.cfg, policyName)
	if storageApplier, ok := applier.(beads.StorageGraphApplyStore); ok {
		return storageApplier.ApplyGraphPlanWithStorage(ctx, plan, beadStorageClass(storage))
	}
	return applier.ApplyGraphPlan(ctx, plan)
}

func policyNameForGraphPlan(plan *beads.GraphApplyPlan) string {
	for _, node := range plan.Nodes {
		if isWispPolicyMetadata(node.Metadata) || hasBeadLabel(node.Labels, "gc:wisp") || hasBeadLabel(node.Labels, "wisp") {
			return beadPolicyWisp
		}
	}
	for _, node := range plan.Nodes {
		if isWorkflowPolicyMetadata(node.Metadata) || isWorkflowPolicyMetadata(node.MetadataRefs) {
			return beadPolicyWorkflow
		}
	}
	return ""
}

func policyNameForBead(b beads.Bead) string {
	switch {
	case isWispPolicyMetadata(b.Metadata) || b.Type == "wisp" || hasBeadLabel(b.Labels, "gc:wisp") || hasBeadLabel(b.Labels, "wisp"):
		return beadPolicyWisp
	case hasBeadLabel(b.Labels, labelOrderTracking):
		return beadPolicyOrderTracking
	case hasBeadLabel(b.Labels, session.LabelSession) || b.Type == session.BeadType:
		return beadPolicySession
	case hasBeadLabel(b.Labels, session.WaitBeadLabel):
		return beadPolicyWait
	case hasBeadLabel(b.Labels, nudgeBeadLabel):
		return beadPolicyNudge
	case isWorkflowPolicyMetadata(b.Metadata):
		return beadPolicyWorkflow
	default:
		return ""
	}
}

func isWispPolicyMetadata(metadata map[string]string) bool {
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp
}

func isWorkflowPolicyMetadata(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow ||
		metadata[beadmeta.FormulaContractMetadataKey] == "graph.v2" ||
		strings.TrimSpace(metadata[beadmeta.RootBeadIDMetadataKey]) != ""
}

func effectiveBeadStorage(cfg *config.City, policyName string) string {
	if cfg != nil {
		if policy, ok := cfg.Beads.Policies[policyName]; ok {
			if storage := normalizeBeadStorage(policy.Storage); storage != "" {
				if config.ValidBeadPolicyStorage(storage) && compatibleBeadPolicyStorage(cfg.Beads, policyName, storage) {
					return storage
				}
				return defaultBeadStorage(cfg.Beads, policyName)
			}
		}
		return defaultBeadStorage(cfg.Beads, policyName)
	}
	return defaultBeadStorage(config.BeadsConfig{}, policyName)
}

func defaultBeadStorage(beadsCfg config.BeadsConfig, policyName string) string {
	if beadsCfg.UsesBD105ReadySemantics() {
		switch policyName {
		case beadPolicyWisp:
			return beadStorageEphemeral
		case beadPolicyWorkflow:
			return beadStorageNoHistory
		case beadPolicySession, beadPolicyWait, beadPolicyNudge, beadPolicyOrderTracking:
			return beadStorageNoHistory
		default:
			return ""
		}
	}
	switch policyName {
	case beadPolicySession, beadPolicyWait, beadPolicyNudge, beadPolicyOrderTracking:
		return beadStorageNoHistory
	case beadPolicyWisp, beadPolicyWorkflow:
		return beadStorageHistory
	default:
		return ""
	}
}

func compatibleBeadPolicyStorage(beadsCfg config.BeadsConfig, policyName, storage string) bool {
	if beadsCfg.UsesBD105ReadySemantics() {
		switch policyName {
		case beadPolicyWisp:
			return storage == beadStorageEphemeral || storage == beadStorageNoHistory || storage == beadStorageHistory
		case beadPolicyWorkflow:
			return storage == beadStorageNoHistory || storage == beadStorageHistory
		case beadPolicySession, beadPolicyWait, beadPolicyNudge, beadPolicyOrderTracking:
			return storage == beadStorageNoHistory || storage == beadStorageHistory
		default:
			return true
		}
	}
	switch policyName {
	case beadPolicyWisp, beadPolicyWorkflow:
		return storage == beadStorageHistory
	case beadPolicySession, beadPolicyWait, beadPolicyNudge, beadPolicyOrderTracking:
		return storage == beadStorageNoHistory || storage == beadStorageHistory
	default:
		return true
	}
}

func normalizeBeadStorage(storage string) string {
	return config.NormalizeBeadPolicyStorage(storage)
}

func createWithStoragePolicy(store beads.Store, b beads.Bead, storage string) (beads.Bead, error) {
	if storage == "" {
		return store.Create(b)
	}
	if storageStore, ok := store.(beads.StorageCreateStore); ok {
		return storageStore.CreateWithStorage(b, beadStorageClass(storage))
	}
	return store.Create(applyBeadStorage(b, storage))
}

func beadStorageClass(storage string) beads.StorageClass {
	switch normalizeBeadStorage(storage) {
	case beadStorageEphemeral:
		return beads.StorageEphemeral
	case beadStorageNoHistory:
		return beads.StorageNoHistory
	case beadStorageHistory:
		return beads.StorageHistory
	default:
		return beads.StorageDefault
	}
}

func applyBeadStorage(b beads.Bead, storage string) beads.Bead {
	switch beadStorageClass(storage) {
	case beads.StorageEphemeral:
		b.Ephemeral = true
		b.NoHistory = false
	case beads.StorageNoHistory:
		b.Ephemeral = false
		b.NoHistory = true
	case beads.StorageHistory:
		b.Ephemeral = false
		b.NoHistory = false
	}
	return b
}

func expandPolicyReadTier(query beads.ListQuery) beads.ListQuery {
	if query.TierMode == beads.TierIssues {
		query.TierMode = beads.TierBoth
	}
	return query
}

func expandPolicyReadyQuery(query ...beads.ReadyQuery) beads.ReadyQuery {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	if q.TierMode == beads.TierIssues {
		q.TierMode = beads.TierBoth
	}
	return q
}

func policyTierFromOpts(opts []beads.QueryOpt) beads.TierMode {
	tier := beads.TierModeFromOpts(opts)
	if tier == beads.TierIssues {
		return beads.TierBoth
	}
	return tier
}

func hasBeadLabel(labels []string, label string) bool {
	return slices.Contains(labels, label)
}
