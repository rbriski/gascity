package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
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
	// mintReservedClassIDs is set only by wrapInfraStoreWithBeadPolicies: the
	// infra store pre-fills a reserved-prefix ID on every explicit-ID-less create
	// so infra beads carry a reserved class prefix (the ID-prefix half of the
	// boundary invariant). The work store leaves this false and mints ordinary
	// bd/Dolt ids under the scope's EffectivePrefix.
	mintReservedClassIDs bool
}

type beadPolicyGraphStore struct {
	*beadPolicyStore
	applier beads.GraphApplyStore
}

var _ beads.ConditionalAssignmentReleaser = (*beadPolicyStore)(nil)

func wrapStoreWithBeadPolicies(store beads.Store, cfg *config.City) beads.Store {
	return wrapPolicyStore(store, cfg, false)
}

// wrapInfraStoreWithBeadPolicies wraps a city's INFRA store in the same
// production policy stack as the work store, but with reserved-prefix ID minting
// enabled so every infra bead created through the wrapper carries the infra
// scope's reserved class prefix. It is used exclusively when opening the infra
// store (openCityInfraStoreResultAt), so no work-store create path mints reserved
// ids. Like wrapStoreWithBeadPolicies it never introduces a new wrapper LAYER —
// beadPolicyStore stays the single universal wrapper, so the optional-capability
// assertions (GraphApplyFor / HandlesFor / StorageCreateStore) stay intact.
func wrapInfraStoreWithBeadPolicies(store beads.Store, cfg *config.City) beads.Store {
	return wrapPolicyStore(store, cfg, true)
}

func wrapPolicyStore(store beads.Store, cfg *config.City, mintReservedClassIDs bool) beads.Store {
	if store == nil {
		return nil
	}
	policyStore := &beadPolicyStore{
		Store:                store,
		cfg:                  cfg,
		mintReservedClassIDs: mintReservedClassIDs,
	}
	if applier, ok := beads.GraphApplyFor(store); ok {
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

func (s *beadPolicyStore) Create(b beads.Bead) (beads.Bead, error) {
	class := coordclass.Classify(b)
	b = s.mintInfraBeadID(b)
	_, storage := s.policyForCreate(b)
	return createWithStoragePolicy(s.createTarget(class), b, storage)
}

// mintInfraBeadID pre-fills b.ID with an infra-scope reserved-prefix id when this
// is the infra store (mintReservedClassIDs) and the caller supplied no explicit
// id. bd mints an id under the scope's issue_prefix on its own, but the infra
// scope's issue_prefix is InfraScopePrefix ("gcg"), so a bd-native id is already
// reserved-prefixed; the explicit pre-fill makes the reserved prefix hold on ID-
// clobbering stores too (e.g. the MemStore-backed boundary invariant test) and
// keeps the id stable across the create round-trip. The pre-filled id equals the
// scope's issue_prefix, so bd accepts it as --id without --force. A caller-set id
// is respected verbatim (graph-apply plans and stable-id creates keep their ids).
func (s *beadPolicyStore) mintInfraBeadID(b beads.Bead) beads.Bead {
	if !s.mintReservedClassIDs || strings.TrimSpace(b.ID) != "" {
		return b
	}
	b.ID = config.MintInfraBeadID(newInfraBeadIDSuffix())
	return b
}

// newInfraBeadIDSuffix returns a short, collision-resistant hex token for infra
// bead ids. bd's unique-id constraint is the ultimate collision guard; 8 hex
// chars (32 bits) keep ids compact while making an accidental clash negligible.
func newInfraBeadIDSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is effectively impossible on supported platforms;
		// fall back to a time-derived token so a create never fails on entropy.
		return fmt.Sprintf("%08x", uint32(time.Now().UnixNano()))
	}
	return hex.EncodeToString(buf[:])
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

// DeleteAllOrphaning forwards the orphan-preserving batch delete to the inner
// store so the beads.BatchDeleter capability survives the policy wrapper. The
// policy layer adds no delete-side behavior, so this is a pure delegation; a
// backing store without the capability returns an error (never a single-id
// fallback, which would defeat the orphan-preserving contract).
func (s *beadPolicyStore) DeleteAllOrphaning(ids []string) (int, error) {
	deleter, ok := s.Store.(beads.BatchDeleter)
	if !ok {
		return 0, fmt.Errorf("policy store: backing store %T does not support orphan-preserving batch delete", s.Store)
	}
	return deleter.DeleteAllOrphaning(ids)
}

// CreateWithForeignID forwards a forced foreign-prefix create to the inner store
// so the beads.ForeignIDCreator capability survives the policy wrapper. It
// DELIBERATELY bypasses the policy Create path (mintInfraBeadID + storage
// policy): the migration supplies the exact stable id to preserve, so there is
// nothing to mint, and the id-prefix force is the whole point. Storage-tier
// selection still rides the bead's own Ephemeral/NoHistory flags, which the
// migration copies verbatim from the source bead.
func (s *beadPolicyStore) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	creator, ok := s.Store.(beads.ForeignIDCreator)
	if !ok {
		return beads.Bead{}, fmt.Errorf("policy store: backing store %T does not support foreign-id create", s.Store)
	}
	return creator.CreateWithForeignID(b)
}

func (s *beadPolicyStore) policyForCreate(b beads.Bead) (string, string) {
	if rootID := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" {
		root, err := s.createTarget(coordclass.ClassGraph).Get(rootID)
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

func (s *beadPolicyGraphStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	if plan == nil {
		return s.graphApplierFor(coordclass.ClassWork).ApplyGraphPlan(ctx, plan)
	}
	applier := s.graphApplierFor(coordclass.ClassifyGraphPlan(plan))
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

// policyNameForGraphPlan returns the storage-tier policy name for a graph-apply
// plan: wisp if any node looks like a wisp, then workflow if any node looks like
// a workflow, else "" (default work, no storage policy). This is the fine-grained
// tier classifier, kept local to cmd/gc and distinct from coordclass.Classify,
// which decides only store routing. It is the verbatim pre-lift classifier.
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

// policyNameForBead returns the storage-tier policy name for a bead, in the same
// precedence the pre-lift classifier used (wisp -> order_tracking -> session ->
// wait -> nudge -> workflow -> ""). This is the fine-grained tier classifier,
// kept local to cmd/gc and distinct from coordclass.Classify, which decides only
// store routing: the tier mapping (effectiveBeadStorage / defaultBeadStorage) is
// keyed on these names, not on the coordination class.
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
		metadata[beadmeta.FormulaContractMetadataKey] == beadmeta.FormulaContractGraphV2 ||
		strings.TrimSpace(metadata[beadmeta.RootBeadIDMetadataKey]) != ""
}

func hasBeadLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
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
