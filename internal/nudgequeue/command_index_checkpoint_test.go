package nudgequeue

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCommandIndexRebuildAcceptsMechanicallyCompleteCompactedCoverage(t *testing.T) {
	active := indexTestCommand("command-2", "session-a", 2, 9, CommandStatePending)
	opaque := opaqueIndexTestEntry(t, 2, "command-4", "session-b", 1, 4, 10, "future-wire")

	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:   indexTestStoreBinding(),
		Entries: []CommandIndexEntry{knownIndexTestEntry(active), opaque},
		Coverage: &CommandIndexCompactedCoverage{
			PublishedRevision: 8,
			Ranges: []CommandIndexSequenceRange{
				{FirstSequence: 1, LastSequence: 1},
				{FirstSequence: 3, LastSequence: 3},
				{FirstSequence: 5, LastSequence: 5},
			},
			TerminalCount:     2,
			TombstoneCount:    1,
			FingerprintSHA256: strings.Repeat("a", 64),
		},
		Revision:          10,
		SequenceHighWater: 5,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if got := index.Status(); got.SequenceHighWater != 5 || got.Revision != 10 || !got.Synced {
		t.Fatalf("Status = %#v, want synced revision 10 sequence 5", got)
	}
	page, err := index.Page("session-a", 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(session-a): %v", err)
	}
	if got := indexEntryIDs(page.Entries); len(got) != 1 || got[0] != active.ID {
		t.Fatalf("active page IDs = %v, want [%s]", got, active.ID)
	}
	resolved, err := index.Resolve("command-1")
	if err != nil {
		t.Fatalf("Resolve(compacted terminal): %v", err)
	}
	if resolved.Found {
		t.Fatalf("Resolve(compacted terminal) = %#v, want absent from active projection", resolved)
	}
	opaqueResolved, err := index.Resolve("command-4")
	if err != nil || !opaqueResolved.Found || opaqueResolved.Entry.Opaque == nil || string(opaqueResolved.Entry.Opaque.Raw) != string(opaque.Opaque.Raw) {
		t.Fatalf("Resolve(opaque) = %#v, err=%v, want exact future wire", opaqueResolved, err)
	}
}

func TestCommandIndexRebuildRejectsInvalidCompactedCoverage(t *testing.T) {
	active := knownIndexTestEntry(indexTestCommand("command-2", "session-a", 2, 4, CommandStatePending))
	valid := CommandIndexSnapshot{
		Store:   indexTestStoreBinding(),
		Entries: []CommandIndexEntry{active},
		Coverage: &CommandIndexCompactedCoverage{
			PublishedRevision: 3,
			Ranges: []CommandIndexSequenceRange{
				{FirstSequence: 1, LastSequence: 1},
				{FirstSequence: 3, LastSequence: 3},
			},
			TerminalCount:     2,
			FingerprintSHA256: strings.Repeat("b", 64),
		},
		Revision:          4,
		SequenceHighWater: 3,
	}

	tests := map[string]func(*CommandIndexSnapshot){
		"gap": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.Ranges = snapshot.Coverage.Ranges[:1]
			snapshot.Coverage.TerminalCount = 1
		},
		"overlap with active": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.Ranges[1] = CommandIndexSequenceRange{FirstSequence: 2, LastSequence: 3}
			snapshot.Coverage.TerminalCount = 3
		},
		"overlapping ranges": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.Ranges[1] = CommandIndexSequenceRange{FirstSequence: 1, LastSequence: 3}
			snapshot.Coverage.TerminalCount = 4
		},
		"noncanonical adjacent ranges": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.Ranges = []CommandIndexSequenceRange{
				{FirstSequence: 1, LastSequence: 1},
				{FirstSequence: 2, LastSequence: 2},
			}
			snapshot.Entries = []CommandIndexEntry{knownIndexTestEntry(indexTestCommand("command-3", "session-a", 3, 4, CommandStatePending))}
		},
		"count mismatch": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.TerminalCount++
		},
		"bad fingerprint": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.FingerprintSHA256 = strings.Repeat("A", 64)
		},
		"checkpoint ahead": func(snapshot *CommandIndexSnapshot) {
			snapshot.Coverage.PublishedRevision = snapshot.Revision + 1
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := valid
			coverage := *valid.Coverage
			coverage.Ranges = append([]CommandIndexSequenceRange(nil), valid.Coverage.Ranges...)
			snapshot.Coverage = &coverage
			mutate(&snapshot)
			if _, err := BuildCommandIndex(snapshot); err == nil {
				t.Fatal("BuildCommandIndex accepted invalid compacted coverage")
			}
		})
	}
}

func TestCommandIndexCompactedCoverageAndTrustedPartitionGapsAreDisjoint(t *testing.T) {
	t.Parallel()

	snapshot := CommandIndexSnapshot{
		Store:         indexTestStoreBinding(),
		Entries:       []CommandIndexEntry{knownIndexTestEntry(indexTestCommand("command-3", "session-a", 3, 4, CommandStatePending))},
		PartitionGaps: []CommandIndexPartitionGap{{FirstSequence: 2, LastSequence: 2}},
		Coverage: &CommandIndexCompactedCoverage{
			PublishedRevision: 3,
			Ranges:            []CommandIndexSequenceRange{{FirstSequence: 1, LastSequence: 1}},
			TerminalCount:     1,
			FingerprintSHA256: strings.Repeat("d", 64),
		},
		Revision:          4,
		SequenceHighWater: 3,
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex with disjoint compacted and partition evidence: %v", err)
	}

	snapshot.PartitionGaps[0] = CommandIndexPartitionGap{FirstSequence: 1, LastSequence: 1}
	if _, err := BuildCommandIndex(snapshot); err == nil {
		t.Fatal("BuildCommandIndex accepted a trusted partition gap inside compacted coverage")
	}
}

func TestCommandIndexCompressedCoverageScalesWithActiveCommandsNotTerminalLifetime(t *testing.T) {
	const sequenceHighWater = 100_003
	activeSequences := []uint64{1, 10_001, 50_001, 100_003}
	entries := make([]CommandIndexEntry, 0, len(activeSequences))
	for revision, sequence := range activeSequences {
		command := indexTestCommand(
			fmt.Sprintf("command-%06d", sequence),
			fmt.Sprintf("session-%d", revision%2),
			sequence,
			uint64(sequenceHighWater+revision+1),
			CommandStatePending,
		)
		entries = append(entries, knownIndexTestEntry(command))
	}
	coverage := CommandIndexCompactedCoverage{
		PublishedRevision: sequenceHighWater,
		TerminalCount:     sequenceHighWater - uint64(len(activeSequences)),
		FingerprintSHA256: strings.Repeat("c", 64),
	}
	var first uint64 = 1
	for _, active := range activeSequences {
		if first < active {
			coverage.Ranges = append(coverage.Ranges, CommandIndexSequenceRange{FirstSequence: first, LastSequence: active - 1})
		}
		first = active + 1
	}
	if first <= sequenceHighWater {
		coverage.Ranges = append(coverage.Ranges, CommandIndexSequenceRange{FirstSequence: first, LastSequence: sequenceHighWater})
	}

	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           entries,
		Coverage:          &coverage,
		Revision:          sequenceHighWater + uint64(len(activeSequences)),
		SequenceHighWater: sequenceHighWater,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if len(coverage.Ranges) > len(activeSequences)+1 {
		t.Fatalf("coverage ranges = %d, active = %d", len(coverage.Ranges), len(activeSequences))
	}
	for _, entry := range entries {
		id := commandIndexEntryRouting(entry).CommandID
		resolved, err := index.Resolve(id)
		if err != nil || !resolved.Found {
			t.Fatalf("Resolve(%s) = %#v, err=%v", id, resolved, err)
		}
	}

	resurrected := knownIndexTestEntry(indexTestCommand("resurrected", "session-a", 2, sequenceHighWater+uint64(len(activeSequences))+1, CommandStatePending))
	err = index.Apply(CommandIndexMutation{
		Store:    indexTestStoreBinding(),
		Revision: sequenceHighWater + uint64(len(activeSequences)) + 1,
		Entry:    &resurrected,
	})
	if !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply resurrected compacted sequence error = %v, want ErrCommandIndexUnsynced", err)
	}
}
