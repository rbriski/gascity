//go:build integration

package nudgequeue

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadslib "github.com/steveyegge/beads"
)

func TestCommandRepositoryCheckpointReconstructionOnRealNativeDolt(t *testing.T) {
	ctx := t.Context()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := beads.NewNativeDoltStoreWithStorageForTesting(storage)
	verifier := &repositoryLineageTestVerifier{}
	repo, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	state, err := repo.Provision(ctx)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	requestID := "native-checkpoint-terminal"
	pending := repositoryCommandForRequest(t, state.Store, requestID, "native terminal")
	created, admitted, err := repo.createForTest(ctx, requestID, pending)
	if err != nil || !admitted || created.Command == nil {
		t.Fatalf("Create = (%#v, admitted=%v, err=%v)", created, admitted, err)
	}

	deliveredRow := repositoryCheckpointCommandRowForTest(t, CommandRepositoryState{Store: state.Store}, requestID, CommandStateDelivered, 1, created.Command.CreatedAt)
	delivered := deliveredRow.Metadata[commandRecordWireMetadataKey]
	closed := "closed"
	capability, ok := beads.AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(real NativeDolt) = false")
	}
	if err := capability.AtomicReadWrite(ctx, "test: terminalize durable command", func(tx beads.AtomicReadWriteTx) error {
		if err := tx.Update(created.Command.ID, beads.UpdateOpts{
			Status: &closed,
			Metadata: map[string]string{
				commandRecordWireMetadataKey: delivered,
			},
		}); err != nil {
			return err
		}
		return tx.SetMetadata(commandRepositoryRevisionMetadataKey, "2")
	}); err != nil {
		t.Fatalf("terminalize real command: %v", err)
	}
	if _, err := repo.RepairLineage(ctx); err != nil {
		t.Fatalf("RepairLineage after terminal write: %v", err)
	}
	if _, err := repo.Snapshot(ctx, 1); err == nil {
		t.Fatal("Snapshot before checkpoint unexpectedly accepted terminal tail")
	}
	published, caughtUp, err := repo.PublishCheckpoint(ctx, 8)
	if err != nil {
		t.Fatalf("PublishCheckpoint: %v", err)
	}
	if !caughtUp || published.Revision != 3 || published.SequenceHighWater != 1 {
		t.Fatalf("published state = (%#v, caughtUp=%v)", published, caughtUp)
	}
	snapshot, err := repo.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot after checkpoint: %v", err)
	}
	if len(snapshot.Entries) != 0 || snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != 1 || snapshot.Revision != 3 || snapshot.SequenceHighWater != 1 {
		t.Fatalf("real checkpoint snapshot = %#v", snapshot)
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex(real checkpoint snapshot): %v", err)
	}

	nextRequest := "native-checkpoint-active"
	nextCommand := repositoryCommandForRequest(t, state.Store, nextRequest, "native active")
	if _, admitted, err := repo.createForTest(ctx, nextRequest, nextCommand); err != nil || !admitted {
		t.Fatalf("Create after checkpoint = (admitted=%v, err=%v)", admitted, err)
	}
	snapshot, err = repo.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot active after checkpoint: %v", err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.ID != nextCommand.ID || snapshot.Revision != 4 || snapshot.SequenceHighWater != 2 {
		t.Fatalf("real active checkpoint snapshot = %#v", snapshot)
	}
}
