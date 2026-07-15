package nudgequeue

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

func TestRestoreAnchorRepositoryVerifierProvisioningIsOneShot(t *testing.T) {
	cityPath := t.TempDir()
	verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
	state := restoreAnchorRepositoryState("store-a", 1, 0, 0)

	err := verifier.VerifyCommandRepositoryLineage(context.Background(), state)
	requireRestoreAnchorDisposition(t, err, RestoreAnchorFirstInitialization)
	if _, exists, loadErr := LoadRestoreAnchor(context.Background(), RestoreAnchorPath(cityPath)); loadErr != nil || exists {
		t.Fatalf("ordinary Verify created anchor: exists=%t err=%v", exists, loadErr)
	}

	err = verifier.ProvisionCommandRepositoryLineage(context.Background(), state, CommandRepositoryProvisioningEvidence{})
	if !errors.Is(err, ErrRestoreAnchorAdmission) {
		t.Fatalf("Provision with zero evidence error = %v, want ErrRestoreAnchorAdmission", err)
	}
	if _, exists, loadErr := LoadRestoreAnchor(context.Background(), RestoreAnchorPath(cityPath)); loadErr != nil || exists {
		t.Fatalf("invalid Provision created anchor: exists=%t err=%v", exists, loadErr)
	}

	evidence := restoreAnchorProvisioningEvidence(state)
	if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), state, evidence); err != nil {
		t.Fatalf("Provision with one-shot evidence: %v", err)
	}
	if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), state, evidence); err != nil {
		t.Fatalf("idempotent Provision retry: %v", err)
	}
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(state))
	if err := verifier.VerifyCommandRepositoryLineage(context.Background(), state); err != nil {
		t.Fatalf("Verify equal provisioned state: %v", err)
	}

	restarted := NewRestoreAnchorRepositoryVerifier(cityPath)
	if err := restarted.VerifyCommandRepositoryLineage(context.Background(), state); err != nil {
		t.Fatalf("restarted Verify equal state: %v", err)
	}
	if err := os.Remove(RestoreAnchorPath(cityPath)); err != nil {
		t.Fatalf("Remove anchor: %v", err)
	}
	err = restarted.VerifyCommandRepositoryLineage(context.Background(), state)
	requireRestoreAnchorDisposition(t, err, RestoreAnchorInvalid)
	err = restarted.ProvisionCommandRepositoryLineage(context.Background(), state, evidence)
	requireRestoreAnchorDisposition(t, err, RestoreAnchorInvalid)
	if _, exists, loadErr := LoadRestoreAnchor(context.Background(), RestoreAnchorPath(cityPath)); loadErr != nil || exists {
		t.Fatalf("Verify recreated deleted anchor: exists=%t err=%v", exists, loadErr)
	}
}

func TestRestoreAnchorRepositoryVerifierFreezesOnAmbiguousPublishThenRepairs(t *testing.T) {
	cityPath := t.TempDir()
	verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
	initial := restoreAnchorRepositoryState("store-a", 1, 0, 0)
	if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), initial, restoreAnchorProvisioningEvidence(initial)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	injected := errors.New("rename acknowledgement lost")
	ops := osRestoreAnchorFileOps
	ops.rename = func(oldPath, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		return injected
	}
	verifier.ops = ops
	advanced := restoreAnchorRepositoryState("store-a", 1, 1, 1)
	err := verifier.VerifyCommandRepositoryLineage(context.Background(), advanced)
	if !errors.Is(err, ErrRestoreAnchorAdmission) || !errors.Is(err, ErrRestoreAnchorDurabilityUncertain) || !errors.Is(err, injected) {
		t.Fatalf("ambiguous Verify error = %v, want admission + durability-uncertain + injected", err)
	}
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(advanced))
	if verifier.previouslyAccepted == nil || *verifier.previouslyAccepted != restoreAnchorFromRepositoryState(initial) {
		t.Fatalf("ambiguous publish advanced in-process floor: %#v", verifier.previouslyAccepted)
	}

	verifier.ops = osRestoreAnchorFileOps
	if err := verifier.VerifyCommandRepositoryLineage(context.Background(), advanced); err != nil {
		t.Fatalf("Verify did not resync and repair ambiguous publish: %v", err)
	}
	if verifier.previouslyAccepted == nil || *verifier.previouslyAccepted != restoreAnchorFromRepositoryState(advanced) {
		t.Fatalf("repaired Verify floor = %#v, want advanced", verifier.previouslyAccepted)
	}
}

func TestRestoreAnchorRepositoryVerifierRereadsBeforeAcceptingWrites(t *testing.T) {
	corruptAfterRename := func(ops *restoreAnchorFileOps) {
		ops.rename = func(oldPath, newPath string) error {
			if err := os.Rename(oldPath, newPath); err != nil {
				return err
			}
			return os.WriteFile(newPath, []byte("corrupt after publish\n"), 0o600)
		}
	}
	t.Run("provision", func(t *testing.T) {
		cityPath := t.TempDir()
		ops := osRestoreAnchorFileOps
		corruptAfterRename(&ops)
		verifier := newRestoreAnchorRepositoryVerifier(RestoreAnchorPath(cityPath), ops)
		state := restoreAnchorRepositoryState("store-a", 1, 0, 0)
		err := verifier.ProvisionCommandRepositoryLineage(context.Background(), state, restoreAnchorProvisioningEvidence(state))
		requireRestoreAnchorDisposition(t, err, RestoreAnchorInvalid)
		if verifier.previouslyAccepted != nil {
			t.Fatalf("corrupt provision updated in-process floor: %#v", verifier.previouslyAccepted)
		}
	})
	t.Run("advance", func(t *testing.T) {
		cityPath := t.TempDir()
		verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
		initial := restoreAnchorRepositoryState("store-a", 1, 0, 0)
		if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), initial, restoreAnchorProvisioningEvidence(initial)); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		ops := osRestoreAnchorFileOps
		corruptAfterRename(&ops)
		verifier.ops = ops
		advanced := restoreAnchorRepositoryState("store-a", 1, 1, 1)
		err := verifier.VerifyCommandRepositoryLineage(context.Background(), advanced)
		requireRestoreAnchorDisposition(t, err, RestoreAnchorInvalid)
		if verifier.previouslyAccepted == nil || *verifier.previouslyAccepted != restoreAnchorFromRepositoryState(initial) {
			t.Fatalf("corrupt advance changed in-process floor: %#v", verifier.previouslyAccepted)
		}
	})
}

func TestRestoreAnchorRepositoryVerifierAdvancesAndRepairsDatabaseAhead(t *testing.T) {
	cityPath := t.TempDir()
	verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
	initial := restoreAnchorRepositoryState("store-a", 1, 0, 0)
	if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), initial, restoreAnchorProvisioningEvidence(initial)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	advanced := restoreAnchorRepositoryState("store-a", 1, 2, 1)
	if err := verifier.VerifyCommandRepositoryLineage(context.Background(), advanced); err != nil {
		t.Fatalf("Verify normal advance: %v", err)
	}
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(advanced))

	// A new verifier seeing a database commit ahead of the anchor models death
	// after the database commit but before the post-commit verifier completed.
	committedAhead := restoreAnchorRepositoryState("store-a", 1, 3, 2)
	restarted := NewRestoreAnchorRepositoryVerifier(cityPath)
	if err := restarted.VerifyCommandRepositoryLineage(context.Background(), committedAhead); err != nil {
		t.Fatalf("Verify repairs safe same-epoch database-ahead window: %v", err)
	}
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(committedAhead))

	err := restarted.VerifyCommandRepositoryLineage(context.Background(), advanced)
	requireRestoreAnchorDisposition(t, err, RestoreAnchorDatabaseRewind)
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(committedAhead))
}

func TestRestoreAnchorRepositoryVerifierRejectsUnsafeLineage(t *testing.T) {
	base := restoreAnchorRepositoryState("store-a", 7, 41, 17)
	tests := []struct {
		name  string
		state CommandRepositoryState
		want  RestoreAnchorDisposition
	}{
		{name: "revision rewind", state: restoreAnchorRepositoryState("store-a", 7, 40, 17), want: RestoreAnchorDatabaseRewind},
		{name: "sequence rewind", state: restoreAnchorRepositoryState("store-a", 7, 42, 16), want: RestoreAnchorDatabaseRewind},
		{name: "epoch rewind", state: restoreAnchorRepositoryState("store-a", 6, 99, 50), want: RestoreAnchorDatabaseRewind},
		{name: "unaccepted epoch advance", state: restoreAnchorRepositoryState("store-a", 9, 3, 2), want: RestoreAnchorEpochAdvance},
		{name: "foreign store", state: restoreAnchorRepositoryState("store-b", 7, 41, 17), want: RestoreAnchorForeignStore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
			if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), base, restoreAnchorProvisioningEvidence(base)); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			err := verifier.VerifyCommandRepositoryLineage(context.Background(), tc.state)
			requireRestoreAnchorDisposition(t, err, tc.want)
			assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(base))
		})
	}

	t.Run("corrupt anchor", func(t *testing.T) {
		cityPath := t.TempDir()
		verifier := NewRestoreAnchorRepositoryVerifier(cityPath)
		if err := verifier.ProvisionCommandRepositoryLineage(context.Background(), base, restoreAnchorProvisioningEvidence(base)); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if err := os.WriteFile(RestoreAnchorPath(cityPath), []byte("corrupt\n"), 0o600); err != nil {
			t.Fatalf("WriteFile corrupt anchor: %v", err)
		}
		err := verifier.VerifyCommandRepositoryLineage(context.Background(), base)
		requireRestoreAnchorDisposition(t, err, RestoreAnchorInvalid)
	})
}

func TestRestoreAnchorRepositoryVerifierConcurrentCASNeverMovesBackward(t *testing.T) {
	cityPath := t.TempDir()
	initial := restoreAnchorRepositoryState("store-a", 1, 0, 0)
	provisioner := NewRestoreAnchorRepositoryVerifier(cityPath)
	if err := provisioner.ProvisionCommandRepositoryLineage(context.Background(), initial, restoreAnchorProvisioningEvidence(initial)); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	low := NewRestoreAnchorRepositoryVerifier(cityPath)
	high := NewRestoreAnchorRepositoryVerifier(cityPath)
	lowState := restoreAnchorRepositoryState("store-a", 1, 2, 1)
	highState := restoreAnchorRepositoryState("store-a", 1, 3, 2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, call := range []func() error{
		func() error { return low.VerifyCommandRepositoryLineage(context.Background(), lowState) },
		func() error { return high.VerifyCommandRepositoryLineage(context.Background(), highState) },
	} {
		wg.Add(1)
		go func(call func() error) {
			defer wg.Done()
			<-start
			errs <- call()
		}(call)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil || errors.Is(err, ErrRestoreAnchorBusy) || errors.Is(err, ErrRestoreAnchorConflict) || errors.Is(err, ErrRestoreAnchorAdmission) {
			continue
		}
		t.Fatalf("concurrent Verify returned unexpected error: %v", err)
	}
	if err := high.VerifyCommandRepositoryLineage(context.Background(), highState); err != nil {
		t.Fatalf("final high Verify: %v", err)
	}
	assertLoadedRestoreAnchor(t, RestoreAnchorPath(cityPath), restoreAnchorFromRepositoryState(highState))
	err := low.VerifyCommandRepositoryLineage(context.Background(), lowState)
	requireRestoreAnchorDisposition(t, err, RestoreAnchorDatabaseRewind)
}

func restoreAnchorRepositoryState(storeUUID string, epoch, revision, sequence uint64) CommandRepositoryState {
	return CommandRepositoryState{
		Store:             CommandStoreBinding{StoreUUID: storeUUID, RestoreEpoch: epoch},
		SchemaVersion:     CommandRepositorySchemaVersion,
		WriterVersion:     CommandRepositoryWriterVersion,
		Revision:          revision,
		SequenceHighWater: sequence,
	}
}

func restoreAnchorProvisioningEvidence(state CommandRepositoryState) CommandRepositoryProvisioningEvidence {
	var nonce [32]byte
	nonce[0] = 1
	return CommandRepositoryProvisioningEvidence{nonce: nonce, store: state.Store}
}

func requireRestoreAnchorDisposition(t *testing.T, err error, want RestoreAnchorDisposition) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want disposition %q", want)
	}
	var decisionErr *RestoreAnchorDecisionError
	if !errors.As(err, &decisionErr) {
		t.Fatalf("error = %v, want RestoreAnchorDecisionError", err)
	}
	if decisionErr.Decision.Disposition != want {
		t.Fatalf("disposition = %q, want %q (error %v)", decisionErr.Decision.Disposition, want, err)
	}
}
