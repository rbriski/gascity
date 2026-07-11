// Package splittest provides strict, prefix-disjoint bead-store test doubles
// for the domain/infra store split.
//
// # Why this package exists
//
// The split-store bug class — code resolving "which store owns this class of
// bead" differently on different paths — has produced eighteen audited
// landmines plus a live spawn/drain treadmill (rig pool demand blind to
// infra-resident routed wisps on warm ticks, so the reconciler re-spawned
// what the drain path could not see). The in-process MemStore hides that bug
// class: it is lenient exactly where the production bd/Dolt backend is
// strict. MemStore.DepAdd appends an edge without resolving either endpoint,
// so a cross-store dependency that hard-fails in production ("no issue
// found") silently succeeds in tests, and MemStore.Create happily mints a
// row under any explicit id, so an infra-prefixed bead can appear inside a
// work store without a peep. StrictStore closes both gaps at the LEAF store:
// cmd/gc's beadPolicyStore does not override DepAdd (the embedded Store
// interface delegates it straight down), so wrapping the leaf keeps the
// strict checks on every path through the production policy stack.
//
// Two lessons from the live incidents are first-class requirements here, not
// afterthoughts:
//
//  1. Wisp/ephemeral tier. Production molecules materialize as ephemeral
//     wisps (gcg-wisp-* ids, the wisps table), not main-tier rows. The
//     strict wrapper is tier-transparent: ephemeral beads create, read, and
//     dep-link through it exactly as through the leaf, so fixtures and
//     invariants can (and must) cover the wisp tier.
//  2. Warm-tick invariants. Demand/readiness invariants must hold on warm
//     ticks, not just cold-wake. A fixture pair from NewSplitStores keeps
//     cross-store references fail-loud, so a warm-tick path that resolves
//     the wrong store fails in-process the way the real backend fails in
//     production, instead of passing on MemStore leniency.
package splittest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// StrictStore wraps a LEAF beads.Store and makes the leniencies of in-process
// stores fail loudly, the way the bd/Dolt backend fails in production:
//
//   - DepAdd resolves BOTH endpoints in this store first and rejects a
//     missing one with a bd-shaped "no issue found" error, preserving the
//     parent-child short-circuit exactly as BdStore.DepAdd does.
//   - Create (including Tx creates and CreateWithStorage on storage-capable
//     leaves) rejects an explicit id whose prefix segment differs from the
//     store's declared id prefix, mirroring bd's rejection of a mismatched
//     --id without --force, and fails loudly when the leaf mints a row
//     outside the declared namespace.
//
// Reads are untouched. Optional leaf capabilities that production code
// discovers by type-assertion are forwarded (see the method set and the
// "deliberately dropped" notes on Strict).
type StrictStore struct {
	beads.Store
	// prefix is the normalized id-prefix segment this store mints under
	// ("gcg" for the infra store). Empty means no declared namespace: the
	// create guard is inert and IDPrefix reports "", which storeref skips —
	// the same routing behavior as a store without the accessor.
	prefix string
	// mintIDs pre-fills prefix-<n> ids on explicit-id-less creates, for
	// leaves that honor explicit ids but mint their own default sequence
	// (beads.NewMemStoreHonoringIDs). It mirrors the native infra-store
	// minting (`bd graph apply` mints <prefix>-<n>) at the leaf level.
	mintIDs bool
	seq     atomic.Int64
}

// Compile-time capability contracts. There is deliberately NO beads.Claimer
// here: this base has no such interface — claim routing is the composite
// claimableStore in cmd/gc plus the concrete BdStore.Claim.
var (
	_ beads.Store                         = (*StrictStore)(nil)
	_ beads.ConditionalAssignmentReleaser = (*StrictStore)(nil)
	_ beads.BatchDeleter                  = (*StrictStore)(nil)
	_ beads.ForeignIDCreator              = (*StrictStore)(nil)
	_ beads.Counter                       = (*StrictStore)(nil)
	_ beads.GraphApplyHandleProvider      = (*StrictStore)(nil)
	_ beads.AtomicTxStore                 = (*StrictStore)(nil)
	_ beads.ParentProjectionWaiter        = (*StrictStore)(nil)
	_ storeref.HasIDPrefix                = (*StrictStore)(nil)
)

// Strict wraps a leaf store in the strict split-store checks. The store's id
// prefix is taken from the leaf's own storeref.HasIDPrefix accessor when it
// exposes one; use StrictWithPrefix for leaves that do not (MemStore).
//
// Wrap the LEAF store, not a policy wrapper: cmd/gc's beadPolicyStore does
// not override DepAdd, so a strict leaf keeps the dependency check live on
// every path through the policy stack, while a strict wrapper AROUND the
// policy store would be bypassed by any code holding the inner store.
//
// Capability forwarding: production code discovers optional store
// capabilities by direct type-assertion, and an interface-embedding wrapper
// silently strips everything outside beads.Store. StrictStore forwards
// Handles (with a strict Writer), IDPrefix, graph-apply (via
// beads.GraphApplyHandleProvider, so beads.GraphApplyFor keeps working
// without a false claim), Counter, ConditionalAssignmentReleaser,
// BatchDeleter, ForeignIDCreator, DepListBatch, CloseStore, AtomicTx,
// Backing, and WaitForParentProjection. StorageCreateStore is forwarded only
// when the leaf implements it (a capability-preserving variant type), so the
// wrapper never falsely claims CreateWithStorage for leaves without it —
// createWithStoragePolicy's flag-based fallback must keep firing for
// MemStore leaves.
//
// Deliberately dropped: StorageGraphApplyStore (asserted on the graph-apply
// HANDLE, which is forwarded verbatim, never on the store itself) and any
// bd-only unexported surfaces. Graph-apply plans bypass the strict DepAdd
// guard by construction: a plan's nodes and edges land atomically in ONE
// store, and real appliers validate edges internally; MemStore leaves have
// no applier at all. Create-time deps (Bead.Needs) are also left
// unstrictened: molecule step Needs carry formula step refs, not bead ids,
// on some fixture paths, and bd's --deps validation behavior is not pinned
// by a contract test on this base — enforcing here could reject valid
// fixtures rather than catch real cross-store bugs.
func Strict(s beads.Store) beads.Store {
	prefix := ""
	if p, ok := s.(storeref.HasIDPrefix); ok {
		prefix = p.IDPrefix()
	}
	return newStrict(s, prefix, false)
}

// StrictWithPrefix wraps a leaf store like Strict, additionally declaring the
// id-prefix segment the store mints under (e.g. "gcg" for an infra store).
// The declared prefix arms the foreign-prefix create guard and is reported
// through IDPrefix for storeref prefix routing. Use it for leaves that do
// not expose storeref.HasIDPrefix themselves (MemStore).
func StrictWithPrefix(s beads.Store, prefix string) beads.Store {
	return newStrict(s, prefix, false)
}

// newStrict builds the wrapper, choosing the StorageCreateStore-preserving
// variant when (and only when) the leaf implements CreateWithStorage.
// mintIDs is package-internal: NewSplitStores arms it for the infra store so
// explicit-id-less creates mint under the reserved prefix.
func newStrict(s beads.Store, prefix string, mintIDs bool) beads.Store {
	if s == nil {
		return nil
	}
	strict := &StrictStore{Store: s, prefix: normalizePrefix(prefix), mintIDs: mintIDs}
	if storage, ok := s.(beads.StorageCreateStore); ok {
		return &strictStorageStore{StrictStore: strict, storage: storage}
	}
	return strict
}

// normalizePrefix mirrors the beads package's internal id-prefix
// normalization (CachingStore): lowercase, trimmed, no trailing dashes, so
// "GCG-" and "gcg" declare the same namespace.
func normalizePrefix(prefix string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "-")
}

// Create rejects an explicit id outside the store's declared namespace
// before delegating, and fails loudly if the leaf minted a row outside it
// (e.g. an explicit-id collision made beads.NewMemStoreHonoringIDs fall back
// to its own sequence). A post-check failure leaves the offending row in the
// leaf — this is a test double, and loud beats tidy.
func (s *StrictStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := s.Store.Create(s.mintID(b))
	if err != nil {
		return beads.Bead{}, err
	}
	if err := s.checkMintedID(created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// CreateWithForeignID forwards the leaf's forced foreign-prefix create. It
// DELIBERATELY bypasses the foreign-prefix guard: this capability IS the
// forced path (BdStore passes --force), used by the store migration to keep
// a legacy HQ/rig-era id when copying a bead into the infra store.
func (s *StrictStore) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	creator, ok := s.Store.(beads.ForeignIDCreator)
	if !ok {
		return beads.Bead{}, fmt.Errorf("strict store: leaf store %T does not support foreign-id create", s.Store)
	}
	return creator.CreateWithForeignID(b)
}

// DepAdd resolves both endpoints in THIS store before delegating, mirroring
// the bd backend, which hard-fails `bd dep add` when either id does not
// resolve in the target database. MemStore.DepAdd appends unconditionally —
// the exact leniency that lets a cross-store dependency (work bead → infra
// bead or vice versa) succeed in-process while production wedges on
// "no issue found".
//
// The parent-child short-circuit is preserved exactly as BdStore.DepAdd has
// it: a parent-child dep that merely restates the bead's own ParentID
// returns nil BEFORE endpoint resolution — on a split store the parent may
// legitimately live elsewhere, and bd never sees the call.
//
// The missing-endpoint error is shaped like the bd backend's output
// ("resolving issue ID <id>: no issue found", wrapped in BdStore's
// "adding dep" context) and intentionally does NOT wrap beads.ErrNotFound:
// bd's real failure is a subprocess stderr string that callers can only
// classify textually, so a typed error here would let in-process tests pass
// on errors.Is checks that production could never satisfy.
func (s *StrictStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	for _, id := range []string{issueID, dependsOnID} {
		if _, err := s.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: no issue found (endpoint not in this store — cross-store dependency?)", issueID, dependsOnID, id)
			}
			return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: %w", issueID, dependsOnID, id, err)
		}
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

// Tx wraps the leaf transaction so creates inside the callback go through
// the same explicit-id guard and prefix minting as Create — without this,
// Tx.Create would be an unguarded side door for foreign-prefix rows.
func (s *StrictStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&strictTx{tx: tx, store: s})
	})
}

// Handles returns explicit read/write handles with this strict store as the
// Writer, so HandlesFor-discovered write paths (Writer.DepAdd, Writer.Create)
// keep the strict checks. Readers keep the leaf's native handle guarantees.
func (s *StrictStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Writer = s
	return handles
}

// IDPrefix implements storeref.HasIDPrefix, reporting the declared id-prefix
// segment ("" when none was declared or inferred — storeref.PrefixOwner
// skips empty prefixes, matching a store without the accessor).
func (s *StrictStore) IDPrefix() string {
	return s.prefix
}

// GraphApplyHandle forwards the leaf's graph-apply capability when it has
// one. Implementing beads.GraphApplyHandleProvider (instead of claiming
// beads.GraphApplyStore outright) keeps beads.GraphApplyFor working on the
// wrapper without a false claim for leaves that cannot graph-apply.
func (s *StrictStore) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	return beads.GraphApplyFor(s.Store)
}

// Count forwards the leaf's beads.Counter capability. Leaves without one
// report beads.ErrCountUnsupported, signaling callers to fall back to List —
// the same contract cmd/gc's beadPolicyStore forwards.
func (s *StrictStore) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	counter, ok := s.Store.(beads.Counter)
	if !ok {
		return 0, fmt.Errorf("counting beads: strict-wrapped store: %w", beads.ErrCountUnsupported)
	}
	return counter.Count(ctx, query, excludeTypes...)
}

// ReleaseIfCurrent forwards the leaf's conditional assignment release, or
// reports beads.ErrConditionalReleaseUnsupported when the leaf lacks it,
// matching the beadPolicyStore forwarding contract.
func (s *StrictStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// DeleteAllOrphaning forwards the leaf's orphan-preserving batch delete. A
// leaf without the capability errors — never a single-id fallback, which
// would defeat the orphan-preserving contract (same rule as beadPolicyStore).
func (s *StrictStore) DeleteAllOrphaning(ids []string) (int, error) {
	deleter, ok := s.Store.(beads.BatchDeleter)
	if !ok {
		return 0, fmt.Errorf("strict store: leaf store %T does not support orphan-preserving batch delete", s.Store)
	}
	return deleter.DeleteAllOrphaning(ids)
}

// DepListBatch forwards the leaf's batched "down" dep listing (asserted by
// internal/dispatch's scope-skip walk and the store migration). Leaves
// without it fall back to per-id DepList — byte-identical to the fallback
// those callers run themselves.
func (s *StrictStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	if batch, ok := s.Store.(interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	}); ok {
		return batch.DepListBatch(ids)
	}
	result := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := s.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for %q: %w", id, err)
		}
		result[id] = deps
	}
	return result, nil
}

// CloseStore releases the leaf's backing handle when it has one (asserted by
// cmd/gc store shutdown). Leaves without one hold nothing to release.
func (s *StrictStore) CloseStore() error {
	if closer, ok := s.Store.(interface{ CloseStore() error }); ok {
		return closer.CloseStore()
	}
	return nil
}

// AtomicTx reports the LEAF's transactional guarantee — wrapping neither
// adds nor removes atomicity. False matches the conservative contract for
// stores that never implemented beads.AtomicTxStore.
func (s *StrictStore) AtomicTx() bool {
	return beads.StoreSupportsAtomicTx(s.Store)
}

// Backing forwards the leaf's live-read backing store (asserted by
// beads.ReadyLive). Nil matches a leaf without a caching layer: ReadyLive
// then falls back to the store's own Ready, which is read-only and therefore
// unaffected by strictness either way.
func (s *StrictStore) Backing() beads.Store {
	if backed, ok := s.Store.(interface{ Backing() beads.Store }); ok {
		return backed.Backing()
	}
	return nil
}

// WaitForParentProjection forwards the leaf's projection wait when it has
// one. In-process leaves apply parent updates synchronously, so their
// projection has already converged by the time a caller could ask.
func (s *StrictStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	if waiter, ok := s.Store.(beads.ParentProjectionWaiter); ok {
		return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
	}
	return nil
}

// guardExplicitID rejects a caller-supplied id outside the store's declared
// namespace, mirroring bd's rejection of a mismatched --id without --force.
// An empty id (store-minted) or an undeclared namespace passes.
func (s *StrictStore) guardExplicitID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || s.prefix == "" || s.ownsID(id) {
		return nil
	}
	return fmt.Errorf("creating bead %q: explicit id prefix does not match store id prefix %q (bd rejects a mismatched --id without --force; use CreateWithForeignID for the forced foreign-prefix create)", id, s.prefix)
}

// mintID pre-fills a prefix-<n> id on an explicit-id-less create when
// minting is armed (NewSplitStores' infra store), mirroring the native infra
// minting shape (`bd graph apply` mints <InfraScopePrefix>-<n>).
func (s *StrictStore) mintID(b beads.Bead) beads.Bead {
	if !s.mintIDs || s.prefix == "" || strings.TrimSpace(b.ID) != "" {
		return b
	}
	b.ID = s.prefix + "-" + strconv.FormatInt(s.seq.Add(1), 10)
	return b
}

// checkMintedID fails loudly when the leaf returned a row outside the
// declared namespace — a foreign-prefix row inside a split store is exactly
// the residence-invariant violation this wrapper exists to catch.
func (s *StrictStore) checkMintedID(created beads.Bead) error {
	if s.prefix == "" || s.ownsID(created.ID) {
		return nil
	}
	return fmt.Errorf("store minted bead %q outside its declared id namespace %q: the leaf fell back to its own sequence id (explicit-id collision, or an id-clobbering leaf wrapped with the wrong prefix)", created.ID, s.prefix)
}

// ownsID reports whether id sits in the declared prefix namespace, using the
// same case-insensitive segment match as storeref.PrefixOwner and
// CachingStore. Only meaningful when a prefix is declared.
func (s *StrictStore) ownsID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, s.prefix+"-")
}

// strictStorageStore is the StorageCreateStore-preserving variant of
// StrictStore, returned by the constructors only when the leaf implements
// CreateWithStorage. Keeping the claim conditional matters: production
// storage-policy code (createWithStoragePolicy) type-asserts
// beads.StorageCreateStore and only falls back to flag-based Create when the
// assertion fails, so an unconditional claim on a MemStore leaf would break
// wisp/no-history tier routing instead of preserving it.
type strictStorageStore struct {
	*StrictStore
	storage beads.StorageCreateStore
}

var _ beads.StorageCreateStore = (*strictStorageStore)(nil)

// CreateWithStorage applies the same explicit-id guard and namespace
// post-check as Create, then forwards the policy-selected storage tier to
// the leaf.
func (s *strictStorageStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	if err := s.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := s.storage.CreateWithStorage(s.mintID(b), storage)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := s.checkMintedID(created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// strictTx applies the strict create checks inside a Store.Tx callback.
// Update, SetMetadataBatch, and Close mutate existing rows only and delegate
// verbatim.
type strictTx struct {
	tx    beads.Tx
	store *StrictStore
}

// Create guards and mints exactly like StrictStore.Create, against the
// transaction's write surface.
func (t *strictTx) Create(b beads.Bead) (beads.Bead, error) {
	if err := t.store.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := t.tx.Create(t.store.mintID(b))
	if err != nil {
		return beads.Bead{}, err
	}
	if err := t.store.checkMintedID(created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// Update delegates to the leaf transaction.
func (t *strictTx) Update(id string, opts beads.UpdateOpts) error {
	return t.tx.Update(id, opts)
}

// SetMetadataBatch delegates to the leaf transaction.
func (t *strictTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.tx.SetMetadataBatch(id, kvs)
}

// Close delegates to the leaf transaction.
func (t *strictTx) Close(id string) error {
	return t.tx.Close(id)
}
