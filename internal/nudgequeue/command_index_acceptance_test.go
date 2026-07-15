package nudgequeue

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestCommandIndexReadsFailClosedAndCarryOneWatermark(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	resolution, err := index.Resolve(command.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolution.Found || resolution.Entry.Command == nil || !reflect.DeepEqual(*resolution.Entry.Command, command) ||
		resolution.Store != indexTestStoreBinding() || resolution.Revision != 1 || resolution.CompletedAuditRevision != 1 {
		t.Fatalf("Resolve result = %#v, want command at build watermark 1", resolution)
	}
	page, err := index.Page(command.Target.SessionID, 0, 1)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.Store != resolution.Store || page.Revision != resolution.Revision ||
		page.CompletedAuditRevision != resolution.CompletedAuditRevision {
		t.Fatalf("Page watermark = %#v, resolution watermark = %#v", page, resolution)
	}

	gapped := indexTestCommand("command-2", "session-a", 2, 3, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(3, &gapped)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply gap error = %v, want ErrCommandIndexUnsynced", err)
	}
	if _, err := index.Resolve(command.ID); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Resolve unsynced error = %v, want ErrCommandIndexUnsynced", err)
	}
	if _, err := index.Page(command.Target.SessionID, 0, 1); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Page unsynced error = %v, want ErrCommandIndexUnsynced", err)
	}
	if got, ok := index.diagnosticResolve(command.ID); !ok || !reflect.DeepEqual(got, command) {
		t.Fatalf("diagnostic state after rejected gap = %#v, %v", got, ok)
	}
}

func TestCommandIndexAuditCannotBlessRewrittenOrResurrectedRecords(t *testing.T) {
	tests := []struct {
		name     string
		original Command
		rewrite  func(Command) Command
	}{
		{
			name:     "payload rewrite",
			original: indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending),
			rewrite: func(command Command) Command {
				return indexMutateCommand(command, func(command *Command) {
					command.Order.Revision = 2
					command.Message = "rewritten by restore"
				})
			},
		},
		{
			name:     "authorization rewrite",
			original: indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending),
			rewrite: func(command Command) Command {
				return indexMutateCommand(command, func(command *Command) {
					command.Order.Revision = 2
					command.TrustedIngress.PrincipalID = "other-principal"
				})
			},
		},
		{
			name:     "terminal resurrection",
			original: indexTestCommand("command-1", "session-a", 1, 1, CommandStateDelivered),
			rewrite: func(command Command) Command {
				return indexTestCommand(command.ID, command.Target.SessionID, command.Order.Sequence, 2, CommandStatePending)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{tc.original}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			rewritten := tc.rewrite(tc.original)
			installed, err := index.CompleteAudit(1, indexTestSnapshot([]Command{rewritten}, 2))
			if err == nil || installed {
				t.Fatalf("CompleteAudit rewrite = %v, %v; want false/error", installed, err)
			}
			got, ok := indexTestResolve(t, index, tc.original.ID)
			if !ok || !reflect.DeepEqual(got, tc.original) {
				t.Fatalf("rejected audit changed record: got %#v, %v, want %#v", got, ok, tc.original)
			}
		})
	}
}

func TestCommandIndexAuditCannotTombstoneActiveWork(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	tombstone := indexTestTombstone(command, 2)
	tombstone.PriorState = CommandStateExpired
	audit := CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Tombstones:        []CommandIndexTombstone{tombstone},
		Revision:          2,
		SequenceHighWater: 1,
	}
	if installed, err := index.CompleteAudit(1, audit); err == nil || installed {
		t.Fatalf("CompleteAudit active deletion = %v, %v; want false/error", installed, err)
	}
	if got, ok := indexTestResolve(t, index, command.ID); !ok || !reflect.DeepEqual(got, command) {
		t.Fatalf("rejected active deletion changed record: got %#v, %v", got, ok)
	}
}

func TestCommandIndexGeneratedHistoriesMatchIndependentRebuild(t *testing.T) {
	for seed := int64(1); seed <= 16; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			index, err := BuildCommandIndex(indexTestEmptySnapshot())
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			commands := make(map[string]Command)
			tombstones := make(map[string]CommandIndexTombstone)
			liveIDs := make([]string, 0, 128)
			var revision uint64
			var sequence uint64

			for step := 0; step < 300; step++ {
				revision++
				if len(liveIDs) == 0 || rng.Intn(100) < 35 {
					sequence++
					id := fmt.Sprintf("command-%d", sequence)
					command := indexTestCommand(id, fmt.Sprintf("session-%d", sequence%7), sequence, revision, CommandStatePending)
					if err := index.Apply(indexTestCommandMutation(revision, &command)); err != nil {
						t.Fatalf("Apply new command at step %d: %v", step, err)
					}
					commands[id] = command
					liveIDs = append(liveIDs, id)
					continue
				}

				position := rng.Intn(len(liveIDs))
				id := liveIDs[position]
				current := commands[id]
				if commandIsTerminalState(current.State) {
					tombstone := indexTestTombstone(current, revision)
					mutation := CommandIndexMutation{Store: indexTestStoreBinding(), Revision: revision, Tombstone: &tombstone}
					if err := index.Apply(mutation); err != nil {
						t.Fatalf("Apply tombstone at step %d: %v", step, err)
					}
					delete(commands, id)
					tombstones[id] = tombstone
					liveIDs[position] = liveIDs[len(liveIDs)-1]
					liveIDs = liveIDs[:len(liveIDs)-1]
					continue
				}

				nextState := generatedNextCommandState(rng, current.State)
				updated := indexTestTransitionCommand(current, revision, nextState)
				if err := index.Apply(indexTestCommandMutation(revision, &updated)); err != nil {
					t.Fatalf("Apply %q -> %q at step %d: %v", current.State, nextState, step, err)
				}
				commands[id] = updated
			}

			commandSnapshot := make([]Command, 0, len(commands))
			for _, command := range commands {
				commandSnapshot = append(commandSnapshot, command)
			}
			tombstoneSnapshot := make([]CommandIndexTombstone, 0, len(tombstones))
			for _, tombstone := range tombstones {
				tombstoneSnapshot = append(tombstoneSnapshot, tombstone)
			}
			rebuilt, err := BuildCommandIndex(CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           indexTestKnownEntries(commandSnapshot),
				Tombstones:        tombstoneSnapshot,
				Revision:          revision,
				SequenceHighWater: sequence,
			})
			if err != nil {
				t.Fatalf("independent BuildCommandIndex: %v", err)
			}
			assertEquivalentCommandIndexes(t, index, rebuilt, commands, tombstones)
		})
	}
}

func generatedNextCommandState(rng *rand.Rand, state CommandState) CommandState {
	switch state {
	case CommandStatePending:
		states := []CommandState{
			CommandStatePending,
			CommandStateInFlight,
			CommandStateExpired,
			CommandStateSuperseded,
			CommandStateDeadLettered,
		}
		return states[rng.Intn(len(states))]
	case CommandStateInFlight:
		states := []CommandState{
			CommandStatePending,
			CommandStateInFlight,
			CommandStateDelivered,
			CommandStateInjectedUnconfirmed,
			CommandStateDeliveryUnknown,
			CommandStateExpired,
			CommandStateSuperseded,
			CommandStateDeadLettered,
		}
		return states[rng.Intn(len(states))]
	default:
		return state
	}
}

func assertEquivalentCommandIndexes(
	t *testing.T,
	incremental *CommandIndex,
	rebuilt *CommandIndex,
	commands map[string]Command,
	tombstones map[string]CommandIndexTombstone,
) {
	t.Helper()
	incrementalStatus := incremental.Status()
	rebuiltStatus := rebuilt.Status()
	if incrementalStatus.Store != rebuiltStatus.Store ||
		incrementalStatus.Revision != rebuiltStatus.Revision ||
		incrementalStatus.SequenceHighWater != rebuiltStatus.SequenceHighWater ||
		!incrementalStatus.Synced || !rebuiltStatus.Synced {
		t.Fatalf("status mismatch: incremental=%#v rebuilt=%#v", incrementalStatus, rebuiltStatus)
	}
	ids := make([]string, 0, len(commands))
	for id := range commands {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		incrementalCommand, incrementalFound := indexTestResolve(t, incremental, id)
		rebuiltCommand, rebuiltFound := indexTestResolve(t, rebuilt, id)
		if !incrementalFound || !rebuiltFound || !reflect.DeepEqual(incrementalCommand, rebuiltCommand) {
			t.Fatalf("Resolve(%q) mismatch: incremental=%#v/%v rebuilt=%#v/%v", id, incrementalCommand, incrementalFound, rebuiltCommand, rebuiltFound)
		}
	}
	if !reflect.DeepEqual(incremental.tombstones, rebuilt.tombstones) || !reflect.DeepEqual(incremental.tombstones, tombstones) {
		t.Fatalf("tombstone mismatch: incremental=%#v rebuilt=%#v model=%#v", incremental.tombstones, rebuilt.tombstones, tombstones)
	}
	for session := 0; session < 7; session++ {
		name := fmt.Sprintf("session-%d", session)
		incrementalIDs := drainCommandIndexSession(t, incremental, name, 7)
		rebuiltIDs := drainCommandIndexSession(t, rebuilt, name, 7)
		if !reflect.DeepEqual(incrementalIDs, rebuiltIDs) {
			t.Fatalf("session %q mismatch: incremental=%v rebuilt=%v", name, incrementalIDs, rebuiltIDs)
		}
	}
}

func TestCommandIndexConcurrentReadsRemainOwnedAndRaceFree(t *testing.T) {
	commands := make([]Command, 64)
	for i := range commands {
		sequence := uint64(i + 1)
		commands[i] = indexTestCommand(fmt.Sprintf("command-%d", sequence), "session-a", sequence, sequence, CommandStatePending)
	}
	index, err := BuildCommandIndex(indexTestSnapshot(commands, 64))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	const readerCount = 8
	start := make(chan struct{})
	errorsFound := make(chan error, readerCount+1)
	var wait sync.WaitGroup
	for reader := 0; reader < readerCount; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < 500; iteration++ {
				resolution, err := index.Resolve("command-1")
				if err != nil {
					errorsFound <- fmt.Errorf("Resolve: %w", err)
					return
				}
				if !resolution.Found || resolution.Entry.Command == nil || resolution.Entry.Command.Order.Revision > resolution.Revision {
					errorsFound <- fmt.Errorf("inconsistent resolution %#v", resolution)
					return
				}
				resolution.Entry.Command.Reference.ID = "reader-owned-copy"
				page, err := index.Page("session-a", 0, 16)
				if err != nil {
					errorsFound <- fmt.Errorf("Page: %w", err)
					return
				}
				for position, entry := range page.Entries {
					if entry.Command == nil || entry.Command.Order.Revision > page.Revision || position > 0 && page.Entries[position-1].Command.Order.Sequence >= entry.Command.Order.Sequence {
						errorsFound <- fmt.Errorf("inconsistent page %#v", page)
						return
					}
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for revision := uint64(65); revision <= 512; revision++ {
			command := indexTestCommand(fmt.Sprintf("command-%d", revision), "session-a", revision, revision, CommandStatePending)
			if err := index.Apply(indexTestCommandMutation(revision, &command)); err != nil {
				errorsFound <- fmt.Errorf("Apply revision %d: %w", revision, err)
				return
			}
		}
	}()
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if status := index.Status(); !status.Synced || status.Revision != 512 || status.SequenceHighWater != 512 {
		t.Fatalf("final status = %#v, want synced revision/high-water 512", status)
	}
}

func TestCommandIndex100KResolveAndBoundedDrain(t *testing.T) {
	const commandCount = 100_000
	commands := make([]Command, commandCount)
	for i := range commands {
		sequence := uint64(i + 1)
		commands[i] = indexTestCommand(fmt.Sprintf("command-%d", sequence), "session-a", sequence, sequence, CommandStatePending)
	}
	index, err := BuildCommandIndex(indexTestSnapshot(commands, commandCount))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	last, found := indexTestResolve(t, index, "command-100000")
	if !found || last.Order.Sequence != commandCount {
		t.Fatalf("Resolve last = %#v, %v", last, found)
	}
	ids := drainCommandIndexSession(t, index, "session-a", MaxCommandIndexPageSize)
	if len(ids) != commandCount || ids[0] != "command-1" || ids[len(ids)-1] != "command-100000" {
		t.Fatalf("drained %d commands, endpoints %q/%q", len(ids), ids[0], ids[len(ids)-1])
	}
	if page, err := index.Page("other-session", 0, MaxCommandIndexPageSize); err != nil || len(page.Entries) != 0 {
		t.Fatalf("cross-session page = %#v, %v; want empty", page, err)
	}
}

func drainCommandIndexSession(t *testing.T, index *CommandIndex, sessionID string, limit int) []string {
	t.Helper()
	var ids []string
	var after uint64
	for {
		page, err := index.Page(sessionID, after, limit)
		if err != nil {
			t.Fatalf("Page(%q, %d): %v", sessionID, after, err)
		}
		if len(page.Entries) > limit {
			t.Fatalf("Page returned %d commands above limit %d", len(page.Entries), limit)
		}
		for _, entry := range page.Entries {
			if entry.Command == nil {
				t.Fatalf("Page returned opaque entry in known-only drain: %#v", entry)
			}
			command := *entry.Command
			if command.Order.Sequence <= after {
				t.Fatalf("Page sequence %d did not advance cursor %d", command.Order.Sequence, after)
			}
			after = command.Order.Sequence
			ids = append(ids, command.ID)
		}
		if page.NextAfterSequence == 0 {
			return ids
		}
		if page.NextAfterSequence != after {
			t.Fatalf("NextAfterSequence = %d, last sequence = %d", page.NextAfterSequence, after)
		}
	}
}

var (
	benchmarkCommandIndexResult CommandIndexResolution
	benchmarkCommandIndexPage   CommandIndexPage
	benchmarkLinearCommand      Command
)

func BenchmarkCommandIndex100KAgainstLinearScan(b *testing.B) {
	const commandCount = 100_000
	commands := make([]Command, commandCount)
	for i := range commands {
		sequence := uint64(i + 1)
		commands[i] = indexTestCommand(fmt.Sprintf("command-%d", sequence), "session-a", sequence, sequence, CommandStatePending)
	}
	index, err := BuildCommandIndex(indexTestSnapshot(commands, commandCount))
	if err != nil {
		b.Fatalf("BuildCommandIndex: %v", err)
	}

	b.Run("resolve-index", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			result, err := index.Resolve("command-100000")
			if err != nil {
				b.Fatal(err)
			}
			benchmarkCommandIndexResult = result
		}
	})
	b.Run("resolve-linear", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, command := range commands {
				if command.ID == "command-100000" {
					benchmarkLinearCommand = command
					break
				}
			}
		}
	})
	b.Run("page-index", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			page, err := index.Page("session-a", commandCount-32, 32)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkCommandIndexPage = page
		}
	})
	b.Run("page-linear", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			page := CommandIndexPage{Entries: make([]CommandIndexEntry, 0, 32)}
			for _, command := range commands {
				if command.Target.SessionID == "session-a" && command.Order.Sequence > commandCount-32 {
					owned := cloneIndexedCommand(command)
					page.Entries = append(page.Entries, CommandIndexEntry{Command: &owned})
				}
			}
			benchmarkCommandIndexPage = page
		}
	})
}
