package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// ErrCommandRepositoryCheckpointConflict reports that authority changed
// between the stable checkpoint source read and revision-CAS publication.
var ErrCommandRepositoryCheckpointConflict = errors.New("durable nudge command checkpoint publication conflict")

// PublishCheckpoint incorporates at most pageSize newly closed command rows
// into bounded compacted coverage and publishes it with a repository-revision
// CAS. caughtUp is false after a full page because another bounded call is
// required to prove that no terminal tail remains.
func (r *CommandRepository) PublishCheckpoint(ctx context.Context, pageSize int) (state CommandRepositoryState, caughtUp bool, err error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandRepositoryState{}, false, err
	}
	if pageSize <= 0 || pageSize > beads.MaxAtomicReadSnapshotPageSize {
		return CommandRepositoryState{}, false, fmt.Errorf("checkpoint page size %d is outside 1..%d: %w", pageSize, beads.MaxAtomicReadSnapshotPageSize, ErrCommandRepositorySnapshotLimit)
	}
	before, err := r.State(ctx)
	if err != nil {
		before, err = r.repairLineage(ctx, "pre-checkpoint lineage repair")
		if err != nil {
			return CommandRepositoryState{}, false, err
		}
	}
	source, prior, candidate, caughtUp, err := r.reader.buildCheckpointCandidate(ctx, before, pageSize)
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	if candidate == nil {
		return source, true, nil
	}
	published := source
	published.Revision = candidate.PublishedRevision
	if err := r.publishCheckpointCAS(ctx, source, prior, *candidate); err != nil {
		if recovered, recoverErr := r.recoverCheckpointPublication(ctx, published, *candidate); recoverErr == nil && recovered {
			return published, caughtUp, nil
		}
		return CommandRepositoryState{}, false, err
	}
	advanced, err := r.repairLineage(ctx, "post-checkpoint lineage advance")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	if advanced != published {
		return CommandRepositoryState{}, false, fmt.Errorf("checkpoint publication state %#v changed before lineage advance to %#v: %w", published, advanced, ErrCommandRepositoryCheckpointConflict)
	}
	return published, caughtUp, nil
}

func (r commandRepositoryReader) buildCheckpointCandidate(ctx context.Context, before CommandRepositoryState, pageSize int) (CommandRepositoryState, *commandRepositoryCheckpoint, *commandRepositoryCheckpoint, bool, error) {
	var (
		state     CommandRepositoryState
		prior     *commandRepositoryCheckpoint
		candidate *commandRepositoryCheckpoint
		caughtUp  bool
	)
	err := r.snapshots.AtomicReadSnapshot(ctx, func(tx beads.AtomicReadSnapshotTx) error {
		var err error
		state, err = readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if state != before {
			return fmt.Errorf("checkpoint source changed from %#v to %#v: %w", before, state, ErrCommandRepositoryCheckpointConflict)
		}
		checkpoint, found, err := readCommandRepositoryCheckpointFromSnapshot(tx, state)
		if err != nil {
			return err
		}
		if found {
			prior = &checkpoint
		}
		query := beads.AtomicReadSnapshotPageQuery{
			IDPrefix: commandIDPrefix,
			Status:   "closed",
			Order:    beads.AtomicReadSnapshotOrderUpdatedAtID,
			Limit:    pageSize,
		}
		if prior != nil {
			query.After = beads.AtomicReadSnapshotCursor{UpdatedAt: prior.TerminalCursor.UpdatedAt, ID: prior.TerminalCursor.ID}
		}
		page, err := tx.ListHistoryPage(query)
		if err != nil {
			return fmt.Errorf("listing durable nudge command checkpoint tail: %w", err)
		}
		caughtUp = len(page.Rows) < pageSize
		if len(page.Rows) == 0 {
			return nil
		}
		if state.Revision == math.MaxUint64 {
			return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable checkpoint revision"}
		}
		next := commandRepositoryCheckpoint{
			Version:           commandRepositoryCheckpointVersion,
			Store:             state.Store,
			SourceRevision:    state.Revision,
			PublishedRevision: state.Revision + 1,
			SequenceHighWater: state.SequenceHighWater,
			Ranges:            make([]CommandIndexSequenceRange, 0, len(page.Rows)),
			FingerprintSHA256: "",
		}
		if prior != nil {
			next.Ranges = append(next.Ranges, prior.Ranges...)
			next.TerminalCount = prior.TerminalCount
			next.TombstoneCount = prior.TombstoneCount
		}
		for _, row := range page.Rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry, _, err := decodeCommandRecord(row, state)
			if err != nil {
				return err
			}
			if entry.Opaque != nil || entry.Command == nil || !commandIsTerminalState(entry.Command.State) {
				return &CommandRepositoryRecordError{CommandID: row.ID, Err: errors.New("closed command is opaque or non-terminal and cannot be compacted")}
			}
			var added bool
			next.Ranges, added = addCommandRepositoryCheckpointSequence(next.Ranges, entry.Command.Order.Sequence)
			if added {
				if next.TerminalCount == math.MaxUint64 {
					return errors.New("command repository checkpoint terminal count overflows uint64")
				}
				next.TerminalCount++
			}
		}
		last := page.Rows[len(page.Rows)-1]
		next.TerminalCursor = &commandRepositoryCheckpointCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
		sealed, err := sealCommandRepositoryCheckpoint(next)
		if err != nil {
			return fmt.Errorf("sealing durable nudge command checkpoint: %w", err)
		}
		candidate = &sealed
		return nil
	})
	if err != nil {
		return CommandRepositoryState{}, nil, nil, false, err
	}
	if err := r.verify(ctx, "checkpoint source", state); err != nil {
		return CommandRepositoryState{}, nil, nil, false, err
	}
	return state, prior, candidate, caughtUp, nil
}

func readCommandRepositoryCheckpointFromSnapshot(tx beads.AtomicReadSnapshotTx, state CommandRepositoryState) (commandRepositoryCheckpoint, bool, error) {
	row, err := tx.GetIssue(commandRepositoryCheckpointID)
	if errors.Is(err, beads.ErrNotFound) {
		return commandRepositoryCheckpoint{}, false, nil
	}
	if err != nil {
		return commandRepositoryCheckpoint{}, false, fmt.Errorf("getting durable nudge command checkpoint: %w", err)
	}
	checkpoint, err := decodeCommandRepositoryCheckpointRecord(row)
	if err != nil {
		return commandRepositoryCheckpoint{}, false, err
	}
	if checkpoint.Store != state.Store || checkpoint.PublishedRevision > state.Revision || checkpoint.SequenceHighWater > state.SequenceHighWater {
		return commandRepositoryCheckpoint{}, false, &CommandRepositoryLineageError{Operation: "read checkpoint", State: state, Err: fmt.Errorf("checkpoint store/revision/sequence %#v exceeds current authority", checkpoint)}
	}
	return checkpoint, true, nil
}

func addCommandRepositoryCheckpointSequence(ranges []CommandIndexSequenceRange, sequence uint64) ([]CommandIndexSequenceRange, bool) {
	for _, sequenceRange := range ranges {
		if sequenceRange.FirstSequence <= sequence && sequence <= sequenceRange.LastSequence {
			return ranges, false
		}
	}
	merged := append(append([]CommandIndexSequenceRange(nil), ranges...), CommandIndexSequenceRange{FirstSequence: sequence, LastSequence: sequence})
	sort.Slice(merged, func(i, j int) bool { return merged[i].FirstSequence < merged[j].FirstSequence })
	canonical := merged[:0]
	for _, sequenceRange := range merged {
		last := len(canonical) - 1
		if last >= 0 && canonical[last].LastSequence != math.MaxUint64 && sequenceRange.FirstSequence <= canonical[last].LastSequence+1 {
			canonical[last].LastSequence = max(canonical[last].LastSequence, sequenceRange.LastSequence)
			continue
		}
		canonical = append(canonical, sequenceRange)
	}
	return canonical, true
}

func (r *CommandRepository) publishCheckpointCAS(ctx context.Context, source CommandRepositoryState, prior *commandRepositoryCheckpoint, candidate commandRepositoryCheckpoint) error {
	record, err := commandRepositoryCheckpointRecord(candidate)
	if err != nil {
		return err
	}
	return r.reader.store.AtomicReadWrite(ctx, "gc: publish durable nudge command checkpoint", func(tx beads.AtomicReadWriteTx) error {
		current, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if current != source {
			return fmt.Errorf("checkpoint revision CAS found %#v, want %#v: %w", current, source, ErrCommandRepositoryCheckpointConflict)
		}
		existing, err := tx.GetIssue(commandRepositoryCheckpointID)
		switch {
		case errors.Is(err, beads.ErrNotFound) && prior == nil:
			if _, err := tx.Create(record); err != nil {
				return fmt.Errorf("creating durable nudge command checkpoint: %w", err)
			}
		case err != nil:
			return fmt.Errorf("reading durable nudge command checkpoint for CAS: %w", err)
		case prior == nil:
			return fmt.Errorf("checkpoint appeared after source snapshot: %w", ErrCommandRepositoryCheckpointConflict)
		default:
			decoded, err := decodeCommandRepositoryCheckpointRecord(existing)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(decoded, *prior) {
				return fmt.Errorf("checkpoint changed after source snapshot: %w", ErrCommandRepositoryCheckpointConflict)
			}
			title, status, issueType := record.Title, record.Status, record.Type
			if err := tx.Update(commandRepositoryCheckpointID, beads.UpdateOpts{
				Title: &title, Status: &status, Type: &issueType, Metadata: record.Metadata,
			}); err != nil {
				return fmt.Errorf("updating durable nudge command checkpoint: %w", err)
			}
		}
		if err := tx.SetMetadata(commandRepositoryRevisionMetadataKey, fmt.Sprint(candidate.PublishedRevision)); err != nil {
			return fmt.Errorf("publishing command repository checkpoint revision: %w", err)
		}
		stored, err := tx.GetIssue(commandRepositoryCheckpointID)
		if err != nil {
			return fmt.Errorf("reading published durable nudge command checkpoint: %w", err)
		}
		decoded, err := decodeCommandRepositoryCheckpointRecord(stored)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(decoded, candidate) {
			return errors.New("published durable nudge command checkpoint differs from candidate")
		}
		return nil
	})
}

func (r *CommandRepository) recoverCheckpointPublication(ctx context.Context, published CommandRepositoryState, candidate commandRepositoryCheckpoint) (bool, error) {
	state, err := r.repairLineage(ctx, "ambiguous checkpoint lineage repair")
	if err != nil || state != published {
		return false, err
	}
	var checkpoint commandRepositoryCheckpoint
	err = atomicCommandRepositoryRead(ctx, r.reader.store, "gc: verify ambiguous durable nudge command checkpoint", func(tx commandRepositoryReadTx) error {
		row, err := tx.GetIssue(commandRepositoryCheckpointID)
		if err != nil {
			return err
		}
		checkpoint, err = decodeCommandRepositoryCheckpointRecord(row)
		return err
	})
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(checkpoint, candidate), nil
}
