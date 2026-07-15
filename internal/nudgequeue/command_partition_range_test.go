package nudgequeue

import (
	"testing"
	"time"
)

func TestCommandPartitionReaderKeepsSparseHighWaterWithoutForeignSequenceMaterialization(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	verifier := &repositoryLineageTestVerifier{}
	repository, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}

	const sequenceHighWater = uint64(100_001)
	store.mu.Lock()
	row := store.rows[admitted.Entry.Command.ID]
	command := *admitted.Entry.Command
	command.Order = CommandOrder{Sequence: sequenceHighWater, Revision: sequenceHighWater}
	wire, encodeErr := EncodeCommandV1(command)
	if encodeErr != nil {
		store.mu.Unlock()
		t.Fatalf("EncodeCommandV1: %v", encodeErr)
	}
	row.Metadata[commandRecordWireMetadataKey] = string(wire)
	store.rows[row.ID] = row
	store.metadata[commandRepositoryRevisionMetadataKey] = "100001"
	store.metadata[commandRepositorySequenceHighWaterMetadataKey] = "100001"
	store.mu.Unlock()
	authority.coverage.rewriteAdmissionForTest(admitted.Entry.Command.ID, sequenceHighWater, sequenceHighWater)
	if _, err := repository.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage: %v", err)
	}

	reader, err := NewCommandPartitionReader(repository, admitted.Partition, ingress)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	snapshot, err := reader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.SequenceHighWater != sequenceHighWater || snapshot.Coverage != nil || len(snapshot.PartitionGaps) != 0 {
		t.Fatalf("sparse partition snapshot high-water/coverage = %d/%#v/%#v, want %d/nil/empty", snapshot.SequenceHighWater, snapshot.Coverage, snapshot.PartitionGaps, sequenceHighWater)
	}
	if index, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex sparse partition snapshot: %v", err)
	} else if resolved, err := index.Resolve(admitted.Entry.Command.ID); err != nil || !resolved.Found {
		t.Fatalf("Resolve sparse owned command = %#v, err=%v", resolved, err)
	}
}
