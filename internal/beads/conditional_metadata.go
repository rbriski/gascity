package beads

import (
	"context"
	"errors"
	"fmt"
)

// ErrConditionalMetadataUnsupported reports that a store cannot atomically
// compare-and-set a single metadata key. It mirrors ErrConditionalReleaseUnsupported:
// a wrapper that must route SetMetadataIf but whose target leg lacks the
// capability returns this loudly rather than silently dropping the write.
var ErrConditionalMetadataUnsupported = errors.New("conditional metadata update unsupported")

// ErrMetadataCASConflict is the typed, LOUD conflict signal a conditional
// metadata write raises when the store-level compare-and-set lost the race: the
// bead's observed value for the key no longer equals the value the caller just
// read (a concurrent writer advanced it), so NOTHING was written. It is never a
// silent lost update. The control-epoch fence (dispatch/control_fence.go)
// consumes it as its retry trigger — re-read, re-decide, converge — exactly as
// the journal branch consumes graphstore.ErrWrongExpectedVersion. Callers that
// might surface it outside a retry loop classify it transient so a loser
// re-dispatches rather than closing a workflow.
var ErrMetadataCASConflict = errors.New("conditional metadata update lost the race")

// ConditionalMetadataStore is implemented by stores that can update a single
// metadata key only when its current value still matches an expected snapshot,
// atomically with respect to concurrent writers (one store transaction / one
// conditional UPDATE). It is the store-level compare-and-set primitive the
// legacy v1/v2 control-epoch write is hardened onto in P5.2; P5.1 only defines,
// implements, forwards, and tests it (zero callers).
//
// SetMetadataIf sets metadata[key]=next on bead id, but only when the bead's
// current OBSERVED value for key equals expected.
//
//   - swapped=true, err=nil — the precondition held and the value was written.
//     A precondition-holding no-op (next == expected) still reports swapped=true:
//     the value is already next, so nothing is written, but the compare succeeded.
//   - swapped=false, err=nil — the bead exists but its observed value for key does
//     NOT equal expected. This is the typed, non-error conflict signal a fenced
//     caller acts on (it never silently loses an update). A bead that does not
//     exist is likewise treated as a non-match (swapped=false, nil error),
//     mirroring ConditionalAssignmentReleaser.ReleaseIfCurrent — a compare against
//     an absent bead trivially fails.
//   - swapped=false, err!=nil — a genuine store failure. For the JournalStore this
//     includes ErrFoldOwnedWriteClosed when id names a fold-owned Tier-A row (that
//     façade never mutates fold-owned rows).
//
// Empty-string / absent-key semantics follow the store's metadata empty-string
// clear contract (TestMetadataEmptyStringClearContract): a key that is absent is
// observed as "", so expected == "" matches an absent-or-empty key, and next == ""
// clears the key to that observably-empty state. The comparison and the write are
// always against the observed string value, never a JSON null vs. empty-string
// distinction.
type ConditionalMetadataStore interface {
	SetMetadataIf(ctx context.Context, id, key, expected, next string) (swapped bool, err error)
}

// ConditionalMetadataHandleProvider lets a wrapper expose a delegated
// ConditionalMetadataStore without claiming the interface globally, mirroring
// AppendLogHandleProvider and the other journal-capability handle providers.
type ConditionalMetadataHandleProvider interface {
	ConditionalMetadataHandle() (ConditionalMetadataStore, bool)
}

// ConditionalMetadataStoreFor returns the conditional-metadata CAS capability for
// store when available. It preserves ordinary ConditionalMetadataStore
// implementations and lets wrappers expose a delegated handle. A store that does
// not support the capability returns (nil, false) — the honest "absent" signal,
// never a silently degraded stub. Follows the AppendLogStoreFor probe idiom
// exactly.
func ConditionalMetadataStoreFor(store Store) (ConditionalMetadataStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(ConditionalMetadataStore); ok {
		return s, true
	}
	if p, ok := store.(ConditionalMetadataHandleProvider); ok {
		return p.ConditionalMetadataHandle()
	}
	return nil, false
}

// SetMetadataConditionally writes metadata[key]=next on bead id, conditioned on
// the value the caller just observed (expected). It is the shared write half of
// the control-epoch fence, used by both the dispatch epoch site
// (syncControlEpochToAttempt) and the molecule epoch site (bumpEpochIfCurrent),
// so the decide-then-write is identical on the journal-resident and legacy paths.
//
//   - Capable store (ConditionalMetadataStoreFor ok), CAS holds → the value is
//     written; nil. A precondition-holding no-op (next == expected) is a nil
//     success too (SetMetadataIf reports swapped=true without writing).
//   - Capable store, CAS lost (a concurrent writer changed the value first) →
//     ErrMetadataCASConflict, the LOUD typed signal the fence retries on. Never a
//     silent lost update, never a silent regression.
//   - Store WITHOUT the capability → today's exact unconditional SetMetadata. This
//     is the only remaining non-loud path in the now-total fence: a store that can
//     neither append-CAS (journal) nor metadata-CAS (this) cannot detect a
//     cross-process lost update, so it degrades to the byte-identical pre-P5
//     baseline. Neither Dolt-backed production store reaches here (BdStore and
//     NativeDoltStore both implement ConditionalMetadataStore); the honest degrade
//     class is the exec provider (internal/beads/exec), which has no CAS verb, so an
//     exec:-provider city takes this fallback for its legacy control writes. This is
//     a genuine capability gap, not a lost CAS. Extending an exec-contract CAS verb
//     — so exec cities also get the S0.4 kill for legacy control writes — is a
//     documented follow-up, not implemented in P5.2.
func SetMetadataConditionally(ctx context.Context, store Store, id, key, expected, next string) error {
	if cas, ok := ConditionalMetadataStoreFor(store); ok {
		swapped, err := cas.SetMetadataIf(ctx, id, key, expected, next)
		if err != nil {
			return err
		}
		if !swapped {
			return fmt.Errorf("conditional metadata write on %s (%s: %q→%q): %w", id, key, expected, next, ErrMetadataCASConflict)
		}
		return nil
	}
	return store.SetMetadata(id, key, next)
}

// Compile-time assertions that every in-package store surfaces the capability.
// Wrapper forwards (CachingStore) and the cmd/gc wrappers (beadPolicyStore,
// residenceRoutingGraphStore) are asserted in their own files.
var (
	_ ConditionalMetadataStore = (*MemStore)(nil)
	_ ConditionalMetadataStore = (*FileStore)(nil)
	_ ConditionalMetadataStore = (*NativeDoltStore)(nil)
	_ ConditionalMetadataStore = (*BdStore)(nil)
	_ ConditionalMetadataStore = (*JournalStore)(nil)
	_ ConditionalMetadataStore = (*CachingStore)(nil)
)
