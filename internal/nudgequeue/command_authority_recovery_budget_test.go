package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestLocalNudgeAuthorityRecoverCommandAuthorityYieldsAndResumesLongUndecidedTail(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	for sequence := 1; sequence <= 2; sequence++ {
		requestID := fmt.Sprintf("bounded-recovery-tail-%04d", sequence)
		command := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		entry, created, err := repository.createForTest(t.Context(), requestID, command)
		if err != nil || !created || entry.Command == nil || entry.Command.Order.Sequence != uint64(sequence) {
			t.Fatalf("create forged command %d = entry:%#v created:%t err:%v", sequence, entry, created, err)
		}
	}

	budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
	budget.work = commandAuthorityRecoveryMaxWork - 1
	err = authority.RecoverCommandAuthority(budgetCtx, repository)
	if !errors.Is(err, ErrCommandAuthorityRecoveryYield) {
		t.Fatalf("first RecoverCommandAuthority error = %v, want ErrCommandAuthorityRecoveryYield", err)
	}
	dense, err := authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil {
		t.Fatalf("dense decision high-water after yield: %v", err)
	}
	if dense != 1 {
		t.Fatalf("dense decision high-water after yield = %d, want one safely completed decision", dense)
	}

	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("resuming RecoverCommandAuthority after yield: %v", err)
	}
	finalState, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	dense, err = authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil {
		t.Fatalf("final dense decision high-water: %v", err)
	}
	if dense != finalState.SequenceHighWater {
		t.Fatalf("resumed recovery fence = dense:%d repository:%d, want equality", dense, finalState.SequenceHighWater)
	}
}

func TestLocalNudgeAuthorityRecoverCommandAuthorityYieldsWhenRepositoryNeverStabilizes(t *testing.T) {
	store := &commandAuthorityContinuousMovementStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	initialStableFenceSeen := false
	movements := 0
	store.hook = func() {
		dense, err := authority.localAuthorityDenseDecisionHighWater(t.Context())
		if err != nil {
			t.Fatalf("read dense decision high-water in movement hook: %v", err)
		}
		store.mu.Lock()
		metadata := cloneRepositoryMetadata(store.metadata)
		store.mu.Unlock()
		current := commandRepositoryStateFromMetadata(t, metadata)
		if dense != current.SequenceHighWater {
			return
		}
		if !initialStableFenceSeen {
			initialStableFenceSeen = true
			return
		}
		movements++
		requestID := fmt.Sprintf("continuous-recovery-movement-%04d", movements)
		command := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		entry, created, err := repository.createForTest(t.Context(), requestID, command)
		if err != nil || !created || entry.Command == nil {
			t.Fatalf("create moving command %d = entry:%#v created:%t err:%v", movements, entry, created, err)
		}
	}
	store.enabled = true

	err = authority.RecoverCommandAuthority(t.Context(), repository)
	if !errors.Is(err, ErrCommandAuthorityRecoveryYield) {
		t.Fatalf("RecoverCommandAuthority under continuous movement error = %v, want ErrCommandAuthorityRecoveryYield", err)
	}
	if movements != commandAuthorityRecoveryMaxPasses {
		t.Fatalf("repository movements before yield = %d, want pass budget %d", movements, commandAuthorityRecoveryMaxPasses)
	}

	store.enabled = false
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority after movement stops: %v", err)
	}
	finalState, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	dense, err := authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil {
		t.Fatalf("final dense decision high-water: %v", err)
	}
	if dense != finalState.SequenceHighWater {
		t.Fatalf("recovery after writer stops = dense:%d repository:%d, want equality", dense, finalState.SequenceHighWater)
	}
}

type commandAuthorityContinuousMovementStore struct {
	*repositoryAtomicTestStore

	hookMu  sync.Mutex
	enabled bool
	inHook  bool
	hook    func()
}

func (s *commandAuthorityContinuousMovementStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	err := s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, fn)
	if err != nil || commitMessage != "gc: read durable nudge command repository for lineage repair" {
		return err
	}
	s.hookMu.Lock()
	if !s.enabled || s.inHook || s.hook == nil {
		s.hookMu.Unlock()
		return nil
	}
	s.inHook = true
	hook := s.hook
	s.hookMu.Unlock()
	hook()
	s.hookMu.Lock()
	s.inHook = false
	s.hookMu.Unlock()
	return nil
}
