package nudgequeue

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestCommandIndexBuildResolvesEveryCurrentRecordAndPagesOrderingDomain(t *testing.T) {
	pending := indexTestCommand("command-pending", "session-a", 2, 3, CommandStatePending)
	inFlight := indexTestCommand("command-flight", "session-a", 1, 4, CommandStateInFlight)
	delivered := indexTestCommand("command-delivered", "session-a", 3, 5, CommandStateDelivered)
	upgradeRequired := indexTestCommand("command-upgrade", "session-a", 4, 6, CommandStateUpgradeRequired)

	index, err := BuildCommandIndex(indexTestSnapshot([]Command{pending, delivered, inFlight, upgradeRequired}, 6))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	for _, want := range []Command{pending, inFlight, delivered, upgradeRequired} {
		got, ok := indexTestResolve(t, index, want.ID)
		if !ok {
			t.Fatalf("Resolve(%q) missed actionable command", want.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Resolve(%q) = %#v, want %#v", want.ID, got, want)
		}
	}
	if got, ok := indexTestResolve(t, index, "missing"); ok {
		t.Fatalf("Resolve(missing) = %#v, true", got)
	}
	page, err := index.Page("session-a", 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(session-a): %v", err)
	}
	if got, want := indexCommandIDs(page.Entries), []string{"command-flight", "command-pending", "command-upgrade"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordering-domain page IDs = %v, want %v; terminal record must resolve but not page", got, want)
	}

	status := index.Status()
	if !status.Synced || status.Revision != 6 || status.CompletedAuditRevision != 6 {
		t.Fatalf("Status() = %#v, want synced revision/audit 6", status)
	}
}

func TestCommandIndexTrustedPartitionGapsPreserveDenseSequenceCoverage(t *testing.T) {
	owned := indexTestCommand("command-owned", "session-owned", 2, 3, CommandStatePending)
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{knownIndexTestEntry(owned)},
		PartitionGaps:     []CommandIndexPartitionGap{{Sequence: 1}, {Sequence: 3}},
		Revision:          3,
		SequenceHighWater: 3,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	page, err := index.Page(owned.Target.SessionID, 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(owned): %v", err)
	}
	if got := indexEntryIDs(page.Entries); !reflect.DeepEqual(got, []string{owned.ID}) {
		t.Fatalf("owned page IDs = %v, want only %q", got, owned.ID)
	}
	status := index.Status()
	if status.SequenceHighWater != 3 || status.Revision != 3 || !status.Synced {
		t.Fatalf("partitioned status = %#v, want complete global watermark", status)
	}
}

func TestCommandIndexTrustedPartitionGapsFailClosedOnInvalidCoverage(t *testing.T) {
	owned := indexTestCommand("command-owned", "session-owned", 1, 1, CommandStatePending)
	tests := map[string][]CommandIndexPartitionGap{
		"zero sequence":        {{Sequence: 0}},
		"duplicate gap":        {{Sequence: 2}, {Sequence: 2}},
		"overlaps owned entry": {{Sequence: 1}},
	}
	for name, gaps := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := BuildCommandIndex(CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           []CommandIndexEntry{knownIndexTestEntry(owned)},
				PartitionGaps:     gaps,
				Revision:          2,
				SequenceHighWater: 2,
			})
			if err == nil {
				t.Fatal("BuildCommandIndex accepted invalid trusted partition coverage")
			}
		})
	}
}

func TestCommandIndexRequiresOneExplicitStoreLineage(t *testing.T) {
	store := indexTestStoreBinding()
	command := indexTestCommand("command-1", "shared-session", 1, 1, CommandStatePending)

	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             store,
		Entries:           indexTestKnownEntries([]Command{command}),
		Revision:          1,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if got := index.Status().Store; got != store {
		t.Fatalf("Status store = %#v, want %#v", got, store)
	}

	otherStore := store
	otherStore.StoreUUID = "store-2"
	foreign := indexTestCommand("command-2", "shared-session", 2, 2, CommandStatePending)
	foreign.Store = otherStore
	foreignEntry := knownIndexTestEntry(foreign)
	if err := index.Apply(CommandIndexMutation{
		Store:    otherStore,
		Revision: 2,
		Entry:    &foreignEntry,
	}); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply cross-store command error = %v, want ErrCommandIndexUnsynced", err)
	}
	if _, ok := index.diagnosticResolve(foreign.ID); ok {
		t.Fatal("cross-store command contaminated the index")
	}

	if _, err := BuildCommandIndex(CommandIndexSnapshot{}); err == nil {
		t.Fatal("BuildCommandIndex accepted an implicit empty-snapshot lineage")
	}
}

func TestCommandIndexPageUsesStableSequenceOrderAndStrictBounds(t *testing.T) {
	commands := []Command{
		indexTestCommand("command-4", "session-a", 4, 1, CommandStatePending),
		indexTestCommand("other-1", "session-b", 1, 2, CommandStatePending),
		indexTestCommand("command-2", "session-a", 2, 3, CommandStatePending),
		indexTestCommand("command-3", "session-a", 3, 4, CommandStateInFlight),
	}
	index, err := BuildCommandIndex(indexTestSnapshot(commands, 4))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	first, err := index.Page("session-a", 0, 2)
	if err != nil {
		t.Fatalf("Page(first): %v", err)
	}
	if got, want := indexCommandIDs(first.Entries), []string{"command-2", "command-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first page IDs = %v, want %v", got, want)
	}
	if first.NextAfterSequence != 3 {
		t.Fatalf("first NextAfterSequence = %d, want 3", first.NextAfterSequence)
	}

	second, err := index.Page("session-a", first.NextAfterSequence, 2)
	if err != nil {
		t.Fatalf("Page(second): %v", err)
	}
	if got, want := indexCommandIDs(second.Entries), []string{"command-4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second page IDs = %v, want %v", got, want)
	}
	if second.NextAfterSequence != 0 {
		t.Fatalf("final NextAfterSequence = %d, want 0", second.NextAfterSequence)
	}

	between, err := index.Page("session-a", 2, 1)
	if err != nil {
		t.Fatalf("Page(between): %v", err)
	}
	if got, want := indexCommandIDs(between.Entries), []string{"command-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("between page IDs = %v, want %v", got, want)
	}

	empty, err := index.Page("missing-session", 0, 1)
	if err != nil {
		t.Fatalf("Page(missing): %v", err)
	}
	if len(empty.Entries) != 0 || empty.NextAfterSequence != 0 {
		t.Fatalf("missing-session page = %#v, want empty", empty)
	}

	for _, limit := range []int{-1, 0, MaxCommandIndexPageSize + 1} {
		if _, err := index.Page("session-a", 0, limit); err == nil {
			t.Fatalf("Page limit %d returned nil error", limit)
		}
	}
	if _, err := index.Page("session-a", 0, MaxCommandIndexPageSize); err != nil {
		t.Fatalf("Page maximum limit: %v", err)
	}
}

func TestCommandIndexOwnsStoredAndReturnedCommandValues(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStateInFlight)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	command.Message = "caller mutated input"
	command.Claim.ID = "caller-mutated-claim"
	command.Retry.AttemptCount = 99
	command.Reference.ID = "caller-mutated-reference"
	first, ok := indexTestResolve(t, index, "command-1")
	if !ok {
		t.Fatal("Resolve(command-1) missed command")
	}
	if first.Message == command.Message || first.Claim.ID == command.Claim.ID ||
		first.Retry.AttemptCount == command.Retry.AttemptCount || first.Reference.ID == command.Reference.ID {
		t.Fatalf("index aliases build input: got %#v, mutated input %#v", first, command)
	}

	first.Message = "caller mutated output"
	first.Claim.ID = "caller-mutated-output-claim"
	first.Retry.AttemptCount = 100
	first.Reference.ID = "caller-mutated-output-reference"
	second, ok := indexTestResolve(t, index, "command-1")
	if !ok {
		t.Fatal("second Resolve(command-1) missed command")
	}
	if second.Message == first.Message || second.Claim.ID == first.Claim.ID ||
		second.Retry.AttemptCount == first.Retry.AttemptCount || second.Reference.ID == first.Reference.ID {
		t.Fatalf("Resolve aliases index state: first %#v, second %#v", first, second)
	}

	page, err := index.Page("session-a", 0, 1)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	page.Entries[0].Command.Claim.ID = "caller-mutated-page-claim"
	third, _ := indexTestResolve(t, index, "command-1")
	if third.Claim.ID == page.Entries[0].Command.Claim.ID {
		t.Fatal("Page result aliases index state")
	}
}

func TestCommandIndexDeepCopiesRetryEligibility(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	command.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: command.DeliverAfter}
	nextEligibleAt := command.DeliverAfter.Add(time.Minute)
	command.Retry = &CommandRetry{
		AttemptCount:               1,
		LastAttemptAt:              command.DeliverAfter,
		ClaimID:                    "claim-1",
		OperationID:                command.ID,
		AttemptID:                  "attempt-1",
		BoundLaunchIdentity:        command.Binding.LaunchIdentity,
		AuthorizationDecisionID:    "claim-decision-1",
		AuthorizationPolicyVersion: "policy-v1",
		NextEligibleAt:             &nextEligibleAt,
		ErrorClass:                 CommandErrorClassProviderBusy,
		ErrorDetail:                "provider asked the controller to retry",
	}
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	*command.Retry.NextEligibleAt = command.ExpiresAt.Add(time.Hour)
	first, _ := indexTestResolve(t, index, command.ID)
	if first.Retry.NextEligibleAt.Equal(*command.Retry.NextEligibleAt) {
		t.Fatal("index aliases build-input retry eligibility")
	}
	*first.Retry.NextEligibleAt = first.ExpiresAt.Add(2 * time.Hour)
	second, _ := indexTestResolve(t, index, command.ID)
	if second.Retry.NextEligibleAt.Equal(*first.Retry.NextEligibleAt) {
		t.Fatal("Resolve aliases retry eligibility in index state")
	}
}

func TestBuildCommandIndexRejectsAmbiguousOrNoncanonicalSnapshots(t *testing.T) {
	base := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	tests := []struct {
		name     string
		commands []Command
		revision uint64
	}{
		{name: "duplicate command id", commands: []Command{base, base}, revision: 1},
		{name: "duplicate session sequence", commands: []Command{base, indexTestCommand("command-2", "session-a", 1, 1, CommandStatePending)}, revision: 1},
		{name: "empty command id", commands: []Command{indexMutateCommand(base, func(c *Command) { c.ID = "" })}, revision: 1},
		{name: "empty session id", commands: []Command{indexMutateCommand(base, func(c *Command) { c.Target.SessionID = "" })}, revision: 1},
		{name: "zero sequence", commands: []Command{indexMutateCommand(base, func(c *Command) { c.Order.Sequence = 0 })}, revision: 1},
		{name: "zero command revision", commands: []Command{indexMutateCommand(base, func(c *Command) { c.Order.Revision = 0 })}, revision: 1},
		{name: "command newer than snapshot", commands: []Command{indexMutateCommand(base, func(c *Command) { c.Order.Revision = 2 })}, revision: 1},
		{name: "unknown state", commands: []Command{indexMutateCommand(base, func(c *Command) { c.State = CommandState("mystery") })}, revision: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildCommandIndex(indexTestSnapshot(tc.commands, tc.revision)); err == nil {
				t.Fatal("BuildCommandIndex returned nil error")
			}
		})
	}
}

func TestCommandIndexApplyRequiresContiguousRevisionsAndRetainsTerminalRecords(t *testing.T) {
	index, err := BuildCommandIndex(indexTestEmptySnapshot())
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	pending := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(1, &pending)); err != nil {
		t.Fatalf("Apply(pending): %v", err)
	}

	inFlight := indexMutateCommand(pending, func(command *Command) {
		command.State = CommandStateInFlight
		command.Order.Revision = 2
		attemptAt := command.CreatedAt.Add(2 * time.Second)
		command.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: attemptAt}
		command.Retry = &CommandRetry{
			AttemptCount:               1,
			LastAttemptAt:              attemptAt,
			ClaimID:                    "claim-1",
			OperationID:                command.ID,
			AttemptID:                  "attempt-1",
			BoundLaunchIdentity:        command.Binding.LaunchIdentity,
			AuthorizationDecisionID:    "claim-decision-1",
			AuthorizationPolicyVersion: "policy-v1",
		}
		command.Claim = &CommandClaim{
			ID:                         "claim-1",
			OwnerID:                    "controller-1",
			OperationID:                command.ID,
			AttemptID:                  "attempt-1",
			BoundLaunchIdentity:        "launch-1",
			AuthorizationDecisionID:    "claim-decision-1",
			AuthorizationPolicyVersion: "policy-v1",
			ClaimedAt:                  attemptAt,
			LeaseUntil:                 command.CreatedAt.Add(time.Minute),
		}
	})
	if err := index.Apply(indexTestCommandMutation(2, &inFlight)); err != nil {
		t.Fatalf("Apply(in-flight): %v", err)
	}

	delivered := indexMutateCommand(inFlight, func(command *Command) {
		command.State = CommandStateDelivered
		command.Order.Revision = 3
		command.Claim = nil
		command.Terminal = &CommandTerminal{
			At:                         command.CreatedAt.Add(2 * time.Minute),
			ActionResult:               CommandActionResultDelivered,
			ClaimID:                    command.Retry.ClaimID,
			OperationID:                command.Retry.OperationID,
			AttemptID:                  command.Retry.AttemptID,
			BoundLaunchIdentity:        command.Retry.BoundLaunchIdentity,
			AuthorizationDecisionID:    command.Retry.AuthorizationDecisionID,
			AuthorizationPolicyVersion: command.Retry.AuthorizationPolicyVersion,
			ProviderStage:              ProviderStageAccepted,
			Completion:                 CompletionStateCompleted,
		}
	})
	if err := index.Apply(indexTestCommandMutation(3, &delivered)); err != nil {
		t.Fatalf("Apply(delivered): %v", err)
	}
	if got, ok := indexTestResolve(t, index, delivered.ID); !ok || !reflect.DeepEqual(got, delivered) {
		t.Fatalf("Resolve(delivered) = %#v, %v; terminal record must remain visible", got, ok)
	}
	page, err := index.Page("session-a", 0, 1)
	if err != nil {
		t.Fatalf("Page after terminal: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("Page after terminal = %#v, want empty ordering domain", page.Entries)
	}

	if err := index.Apply(indexTestTombstoneMutation(4, delivered)); err != nil {
		t.Fatalf("Apply(tombstone): %v", err)
	}
	if got, ok := indexTestResolve(t, index, delivered.ID); ok {
		t.Fatalf("Resolve after tombstone = %#v, true", got)
	}
	status := index.Status()
	if !status.Synced || status.Revision != 4 || status.CompletedAuditRevision != 0 {
		t.Fatalf("Status after mutations = %#v, want synced revision 4 with audit watermark 0", status)
	}
}

func TestCommandIndexApplyIdenticalStaleReplayIsIdempotent(t *testing.T) {
	index, err := BuildCommandIndex(indexTestEmptySnapshot())
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	mutation := indexTestCommandMutation(1, &command)
	if err := index.Apply(mutation); err != nil {
		t.Fatalf("Apply(first): %v", err)
	}
	if err := index.Apply(mutation); err != nil {
		t.Fatalf("Apply(identical replay): %v", err)
	}
	if status := index.Status(); !status.Synced || status.Revision != 1 {
		t.Fatalf("Status after replay = %#v", status)
	}
	page, err := index.Page("session-a", 0, 2)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := indexCommandIDs(page.Entries); !reflect.DeepEqual(got, []string{"command-1"}) {
		t.Fatalf("IDs after replay = %v, want one command", got)
	}
}

func TestCommandIndexBuildSeedsReplayIdentityForCurrentRecords(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 3, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 3))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if err := index.Apply(indexTestCommandMutation(3, &command)); err != nil {
		t.Fatalf("Apply identical snapshot replay: %v", err)
	}
	if status := index.Status(); !status.Synced || status.Revision != 3 || status.CompletedAuditRevision != 3 {
		t.Fatalf("Status after snapshot replay = %#v", status)
	}
}

func TestCommandIndexApplyConflictingStaleReplayMarksUnsyncedWithoutMutation(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestEmptySnapshot())
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if err := index.Apply(indexTestCommandMutation(1, &command)); err != nil {
		t.Fatalf("Apply(first): %v", err)
	}
	conflict := indexMutateCommand(command, func(command *Command) { command.Message = "conflicting replay" })
	if err := index.Apply(indexTestCommandMutation(1, &conflict)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply(conflict) error = %v, want ErrCommandIndexUnsynced", err)
	}
	status := index.Status()
	if status.Synced || status.Revision != 1 || status.UnsyncedReason == "" {
		t.Fatalf("Status after conflict = %#v", status)
	}
	got, ok := index.diagnosticResolve(command.ID)
	if !ok || got.Message != command.Message {
		t.Fatalf("conflict changed current record: %#v, %v", got, ok)
	}

	next := indexTestCommand("command-2", "session-a", 2, 2, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(2, &next)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply after unsynced error = %v, want ErrCommandIndexUnsynced", err)
	}
	if _, ok := index.diagnosticResolve(next.ID); ok {
		t.Fatal("unsynced index accepted later mutation")
	}
}

func TestCommandIndexApplyGapMarksUnsyncedWithoutAdvancing(t *testing.T) {
	index, err := BuildCommandIndex(indexTestEmptySnapshot())
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	command := indexTestCommand("command-2", "session-a", 2, 2, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(2, &command)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply(gap) error = %v, want ErrCommandIndexUnsynced", err)
	}
	status := index.Status()
	if status.Synced || status.Revision != 0 || status.UnsyncedReason == "" {
		t.Fatalf("Status after gap = %#v", status)
	}
	if _, ok := index.diagnosticResolve(command.ID); ok {
		t.Fatal("gap mutation entered the index")
	}
}

func TestCommandIndexApplyRejectsImmutableTargetSequenceAndSequenceReuse(t *testing.T) {
	tests := []struct {
		name   string
		change func(Command) Command
	}{
		{
			name: "target session",
			change: func(command Command) Command {
				command.Target.SessionID = "session-b"
				return command
			},
		},
		{
			name: "target continuation",
			change: func(command Command) Command {
				command.Target.ContinuationIdentity = "other-continuation"
				return command
			},
		},
		{
			name: "sequence",
			change: func(command Command) Command {
				command.Order.Sequence = 2
				return command
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{original}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			changed := tc.change(cloneIndexTestCommand(original))
			changed.Order.Revision = 2
			changed.TrustedIngress.TargetSessionID = changed.Target.SessionID
			changed.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(changed)
			if err := index.Apply(indexTestCommandMutation(2, &changed)); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply immutable change error = %v, want ErrCommandIndexUnsynced", err)
			}
			got, _ := index.diagnosticResolve(original.ID)
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("immutable violation changed record: got %#v, want %#v", got, original)
			}
		})
	}

	original := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{original}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex(sequence reuse): %v", err)
	}
	reused := indexTestCommand("command-2", "session-a", 1, 2, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(2, &reused)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply sequence reuse error = %v, want ErrCommandIndexUnsynced", err)
	}
}

func TestCommandIndexApplyRejectsImmutableEnvelopeRewrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "message", mutate: func(command *Command) { command.Message = "rewritten payload" }},
		{name: "source", mutate: func(command *Command) {
			command.Source = CommandSourceMail
			command.Reference.Kind = CommandReferenceBead
		}},
		{name: "reference", mutate: func(command *Command) { command.Reference.ID = "rewritten-reference" }},
		{name: "requester principal", mutate: func(command *Command) { command.TrustedIngress.PrincipalID = "other-principal" }},
		{name: "authorization decision", mutate: func(command *Command) { command.TrustedIngress.PolicyDecisionID = "other-decision" }},
		{name: "intent generation", mutate: func(command *Command) { command.Target.IntentGeneration++ }},
		{name: "delivery window", mutate: func(command *Command) { command.DeliverAfter = command.DeliverAfter.Add(time.Second) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{original}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			rewritten := indexMutateCommand(original, func(command *Command) {
				command.Order.Revision = 2
				tc.mutate(command)
			})
			if err := index.Apply(indexTestCommandMutation(2, &rewritten)); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply immutable rewrite error = %v, want ErrCommandIndexUnsynced", err)
			}
			got, _ := index.diagnosticResolve(original.ID)
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("immutable rewrite changed record: got %#v, want %#v", got, original)
			}
		})
	}
}

func TestCommandIndexApplyEnforcesClosedLifecycleTransitions(t *testing.T) {
	tests := []struct {
		name string
		from CommandState
		to   CommandState
		ok   bool
	}{
		{name: "pending stays pending", from: CommandStatePending, to: CommandStatePending, ok: true},
		{name: "pending is claimed", from: CommandStatePending, to: CommandStateInFlight, ok: true},
		{name: "pending expires before provider", from: CommandStatePending, to: CommandStateExpired, ok: true},
		{name: "pending is superseded before provider", from: CommandStatePending, to: CommandStateSuperseded, ok: true},
		{name: "pending is dead-lettered before provider", from: CommandStatePending, to: CommandStateDeadLettered, ok: true},
		{name: "pending cannot claim delivery", from: CommandStatePending, to: CommandStateDelivered},
		{name: "pending cannot claim unconfirmed injection", from: CommandStatePending, to: CommandStateInjectedUnconfirmed},
		{name: "pending cannot claim unknown delivery", from: CommandStatePending, to: CommandStateDeliveryUnknown},
		{name: "pending cannot become upgrade-required implicitly", from: CommandStatePending, to: CommandStateUpgradeRequired},
		{name: "in-flight lease can renew", from: CommandStateInFlight, to: CommandStateInFlight, ok: true},
		{name: "in-flight can return pending", from: CommandStateInFlight, to: CommandStatePending, ok: true},
		{name: "in-flight can deliver", from: CommandStateInFlight, to: CommandStateDelivered, ok: true},
		{name: "in-flight can become ambiguous", from: CommandStateInFlight, to: CommandStateDeliveryUnknown, ok: true},
		{name: "in-flight can expire", from: CommandStateInFlight, to: CommandStateExpired, ok: true},
		{name: "in-flight cannot become upgrade-required implicitly", from: CommandStateInFlight, to: CommandStateUpgradeRequired},
		{name: "upgrade-required stays parked", from: CommandStateUpgradeRequired, to: CommandStateUpgradeRequired, ok: true},
		{name: "upgrade-required cannot become pending implicitly", from: CommandStateUpgradeRequired, to: CommandStatePending},
		{name: "delivered cannot resurrect", from: CommandStateDelivered, to: CommandStatePending},
		{name: "expired cannot resurrect", from: CommandStateExpired, to: CommandStateInFlight},
		{name: "terminal cannot be rewritten", from: CommandStateDelivered, to: CommandStateDelivered},
		{name: "terminal cannot change kind", from: CommandStateDelivered, to: CommandStateDeadLettered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := indexTestCommand("command-1", "session-a", 1, 1, tc.from)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{original}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			updated := indexTestTransitionCommand(original, 2, tc.to)
			err = index.Apply(indexTestCommandMutation(2, &updated))
			if tc.ok {
				if err != nil {
					t.Fatalf("Apply allowed transition: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply forbidden transition error = %v, want ErrCommandIndexUnsynced", err)
			}
			got, _ := index.diagnosticResolve(original.ID)
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("forbidden transition changed record: got %#v, want %#v", got, original)
			}
		})
	}
}

func TestCommandIndexReplayHistoryIsBoundedAndEvictedReplayFailsClosed(t *testing.T) {
	index, err := BuildCommandIndex(indexTestEmptySnapshot())
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	first := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	for revision := uint64(1); revision <= MaxCommandIndexReplayHistory+1; revision++ {
		command := indexTestCommand(fmt.Sprintf("command-%d", revision), "session-a", revision, revision, CommandStatePending)
		if err := index.Apply(indexTestCommandMutation(revision, &command)); err != nil {
			t.Fatalf("Apply revision %d: %v", revision, err)
		}
		if got := len(index.replays); got > MaxCommandIndexReplayHistory {
			t.Fatalf("replay history size = %d, want <= %d", got, MaxCommandIndexReplayHistory)
		}
	}
	if err := index.Apply(indexTestCommandMutation(1, &first)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply evicted replay error = %v, want ErrCommandIndexUnsynced", err)
	}
}

func TestCommandIndexApplyRejectsMalformedMutation(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	entry := knownIndexTestEntry(command)
	tombstone := indexTestTombstone(indexTestCommand("command-1", "session-a", 1, 1, CommandStateExpired), 2)
	noncanonicalTombstone := tombstone
	noncanonicalTombstone.CommandID = " command-1"
	tests := []struct {
		name     string
		mutation CommandIndexMutation
	}{
		{name: "empty", mutation: CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 1}},
		{name: "command and tombstone", mutation: CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 1, Entry: &entry, Tombstone: &tombstone}},
		{name: "revision mismatch", mutation: CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Entry: &entry}},
		{name: "noncanonical tombstone", mutation: CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Tombstone: &noncanonicalTombstone}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index, err := BuildCommandIndex(indexTestEmptySnapshot())
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			if err := index.Apply(tc.mutation); err == nil {
				t.Fatal("Apply returned nil error")
			}
			if status := index.Status(); !status.Synced || status.Revision != 0 {
				t.Fatalf("malformed input poisoned index: %#v", status)
			}
		})
	}
}

func TestCommandIndexSequenceHighWaterSurvivesTombstoneAndRebuild(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStateExpired)
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           indexTestKnownEntries([]Command{command}),
		Revision:          1,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if err := index.Apply(indexTestTombstoneMutation(2, command)); err != nil {
		t.Fatalf("Apply(tombstone): %v", err)
	}
	if got := index.Status().SequenceHighWater; got != 1 {
		t.Fatalf("sequence high-water after tombstone = %d, want 1", got)
	}
	tombstone := indexTestTombstone(command, 2)

	rebuilt, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Tombstones:        []CommandIndexTombstone{tombstone},
		Revision:          2,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex(tombstoned): %v", err)
	}
	reused := indexTestCommand("command-reuse", "session-b", 1, 3, CommandStatePending)
	if err := rebuilt.Apply(indexTestCommandMutation(3, &reused)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply reused tombstoned sequence error = %v, want ErrCommandIndexUnsynced", err)
	}

	rebuilt, err = BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Tombstones:        []CommandIndexTombstone{tombstone},
		Revision:          2,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex(second tombstoned): %v", err)
	}
	next := indexTestCommand("command-2", "session-b", 2, 3, CommandStatePending)
	if err := rebuilt.Apply(indexTestCommandMutation(3, &next)); err != nil {
		t.Fatalf("Apply next sequence: %v", err)
	}
	if got := rebuilt.Status().SequenceHighWater; got != 2 {
		t.Fatalf("sequence high-water after append = %d, want 2", got)
	}
}

func TestCommandIndexTombstonedIdentityCannotBeResurrected(t *testing.T) {
	deleted := indexTestCommand("command-1", "session-a", 1, 1, CommandStateExpired)
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Tombstones:        []CommandIndexTombstone{indexTestTombstone(deleted, 2)},
		Revision:          2,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	resurrected := indexTestCommand("command-1", "session-b", 2, 3, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(3, &resurrected)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply resurrected ID error = %v, want ErrCommandIndexUnsynced", err)
	}
}

func TestCommandIndexTombstoneConservesAcceptedWork(t *testing.T) {
	for _, state := range []CommandState{CommandStatePending, CommandStateInFlight, CommandStateUpgradeRequired} {
		t.Run(string(state), func(t *testing.T) {
			command := indexTestCommand("command-1", "session-a", 1, 1, state)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			tombstone := indexTestTombstone(command, 2)
			tombstone.PriorState = CommandStateExpired
			mutation := CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Tombstone: &tombstone}
			if err := index.Apply(mutation); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply active tombstone error = %v, want ErrCommandIndexUnsynced", err)
			}
			if got, ok := index.diagnosticResolve(command.ID); !ok || !reflect.DeepEqual(got, command) {
				t.Fatalf("active tombstone erased accepted work: got %#v, %v", got, ok)
			}
		})
	}

	terminal := indexTestCommand("command-terminal", "session-a", 1, 1, CommandStateExpired)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{terminal}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex terminal: %v", err)
	}
	if err := index.Apply(indexTestTombstoneMutation(2, terminal)); err != nil {
		t.Fatalf("Apply terminal tombstone: %v", err)
	}
	if _, ok := indexTestResolve(t, index, terminal.ID); ok {
		t.Fatal("terminal tombstone retained the record")
	}

	index, err = BuildCommandIndex(CommandIndexSnapshot{Store: indexTestStoreBinding(), Revision: 1})
	if err != nil {
		t.Fatalf("BuildCommandIndex empty: %v", err)
	}
	prior := indexTestCommand("prior-command", "session-a", 1, 1, CommandStateExpired)
	if err := index.Apply(indexTestTombstoneMutation(2, prior)); err != nil {
		t.Fatalf("Apply unknown-ID tombstone: %v", err)
	}
}

func TestCommandIndexFullValidationRejectsInvalidTrustAndClaimEvidence(t *testing.T) {
	base := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "restore epoch", mutate: func(command *Command) { command.Store.RestoreEpoch = 0 }},
		{name: "trusted ingress", mutate: func(command *Command) { command.TrustedIngress.PolicyDecisionID = "" }},
		{name: "target policy", mutate: func(command *Command) { command.Target.Policy = TargetPolicyExactLaunch }},
		{name: "claim on pending", mutate: func(command *Command) {
			command.Claim = &CommandClaim{ID: "claim-1", ClaimedAt: command.CreatedAt, LeaseUntil: command.CreatedAt.Add(time.Minute)}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			invalid := indexMutateCommand(base, tc.mutate)
			if _, err := BuildCommandIndex(CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           indexTestKnownEntries([]Command{invalid}),
				Revision:          1,
				SequenceHighWater: 1,
			}); err == nil {
				t.Fatal("BuildCommandIndex returned nil error")
			}
		})
	}
}

func TestCommandIndexAuditCASRejectsRacedSnapshotWithoutChangingWatermark(t *testing.T) {
	first := indexTestCommand("command-1", "session-a", 1, 5, CommandStatePending)
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           indexTestKnownEntries([]Command{first}),
		Revision:          5,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	auditSnapshot := CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           indexTestKnownEntries([]Command{first}),
		Revision:          5,
		SequenceHighWater: 1,
	}
	second := indexTestCommand("command-2", "session-a", 2, 6, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(6, &second)); err != nil {
		t.Fatalf("Apply concurrent mutation: %v", err)
	}

	installed, err := index.CompleteAudit(5, auditSnapshot)
	if err != nil {
		t.Fatalf("CompleteAudit: %v", err)
	}
	if installed {
		t.Fatal("raced audit snapshot installed despite revision CAS")
	}
	status := index.Status()
	if status.Revision != 6 || status.CompletedAuditRevision != 5 || !status.Synced {
		t.Fatalf("Status after rejected audit = %#v, want revision 6/audit 5/synced", status)
	}
	if _, ok := indexTestResolve(t, index, second.ID); !ok {
		t.Fatal("rejected audit discarded concurrent mutation")
	}
}

func TestCommandIndexAuditRecoversUnsyncedProjectionAndAdvancesWatermark(t *testing.T) {
	first := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{first}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	third := indexTestCommand("command-3", "session-b", 3, 3, CommandStatePending)
	if err := index.Apply(indexTestCommandMutation(3, &third)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply gap error = %v, want ErrCommandIndexUnsynced", err)
	}
	second := indexTestCommand("command-2", "session-a", 2, 2, CommandStateDelivered)
	audit := CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           indexTestKnownEntries([]Command{first, second, third}),
		Revision:          3,
		SequenceHighWater: 3,
	}
	installed, err := index.CompleteAudit(1, audit)
	if err != nil {
		t.Fatalf("CompleteAudit: %v", err)
	}
	if !installed {
		t.Fatal("CompleteAudit did not install matching independent rebuild")
	}
	status := index.Status()
	if !status.Synced || status.Revision != 3 || status.CompletedAuditRevision != 3 || status.UnsyncedReason != "" {
		t.Fatalf("Status after audit recovery = %#v", status)
	}
	if got, ok := indexTestResolve(t, index, second.ID); !ok || !reflect.DeepEqual(got, second) {
		t.Fatalf("Resolve audited terminal = %#v, %v; want %#v", got, ok, second)
	}
	page, err := index.Page("session-a", 0, 10)
	if err != nil {
		t.Fatalf("Page audited session: %v", err)
	}
	if got, want := indexCommandIDs(page.Entries), []string{"command-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audited session page = %v, want %v", got, want)
	}
}

func TestCommandIndexAuditRejectsRewindAndInvalidSnapshot(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 2, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 2))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	if installed, err := index.CompleteAudit(2, CommandIndexSnapshot{Store: indexTestStoreBinding(), Revision: 1, SequenceHighWater: 1}); err == nil || installed {
		t.Fatalf("CompleteAudit rewind = %v, %v; want false/error", installed, err)
	}
	invalid := indexTestSnapshot([]Command{command}, 2)
	invalid.SequenceHighWater = 0
	if installed, err := index.CompleteAudit(2, invalid); err == nil || installed {
		t.Fatalf("CompleteAudit invalid = %v, %v; want false/error", installed, err)
	}
	if status := index.Status(); status.Revision != 2 || status.CompletedAuditRevision != 2 || !status.Synced {
		t.Fatalf("invalid audit changed status: %#v", status)
	}
}

func indexTestCommand(id, sessionID string, sequence, revision uint64, state CommandState) Command {
	created := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC).Add(time.Duration(sequence) * time.Second)
	command := Command{
		Version: CommandVersion1,
		ID:      id,
		State:   state,
		Mode:    DeliveryModeQueue,
		Target: CommandTarget{
			SessionID:            sessionID,
			IntentGeneration:     1,
			ContinuationIdentity: "continuation-" + sessionID,
			Policy:               TargetPolicyContinuation,
		},
		Store: CommandStoreBinding{
			StoreUUID:    "store-1",
			RestoreEpoch: 1,
		},
		Order: CommandOrder{
			Sequence: sequence,
			Revision: revision,
		},
		TrustedIngress: TrustedIngressReference{
			Issuer:           "test-ingress",
			ReferenceID:      "ingress-" + id,
			PrincipalID:      "principal-1",
			TenantScope:      "tenant-1",
			CityScope:        "city-1",
			CredentialClass:  "controller-ingress",
			PolicyVersion:    "policy-v1",
			PolicyDecisionID: "decision-1",
			Action:           NudgeCommandAction,
			TargetSessionID:  sessionID,
			IssuedAt:         created.Add(-time.Minute),
			ExpiresAt:        created.Add(10 * time.Minute),
		},
		Source:  CommandSourceWait,
		Message: "message for " + id,
		Reference: &Reference{
			Kind: CommandReferenceWait,
			ID:   "reference-" + id,
		},
		CreatedAt:    created,
		DeliverAfter: created.Add(time.Second),
		ExpiresAt:    created.Add(time.Hour),
	}
	switch state {
	case CommandStateInFlight:
		attemptAt := created.Add(2 * time.Second)
		command.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: created.Add(time.Second)}
		command.Retry = &CommandRetry{
			AttemptCount:               1,
			LastAttemptAt:              attemptAt,
			ClaimID:                    "claim-" + id,
			OperationID:                id,
			AttemptID:                  "attempt-1",
			BoundLaunchIdentity:        command.Binding.LaunchIdentity,
			AuthorizationDecisionID:    "claim-decision-1",
			AuthorizationPolicyVersion: "policy-v1",
		}
		command.Claim = &CommandClaim{
			ID:                         command.Retry.ClaimID,
			OwnerID:                    "controller-1",
			OperationID:                command.Retry.OperationID,
			AttemptID:                  command.Retry.AttemptID,
			BoundLaunchIdentity:        command.Retry.BoundLaunchIdentity,
			AuthorizationDecisionID:    command.Retry.AuthorizationDecisionID,
			AuthorizationPolicyVersion: command.Retry.AuthorizationPolicyVersion,
			ClaimedAt:                  attemptAt,
			LeaseUntil:                 created.Add(time.Minute),
		}
	case CommandStateDelivered, CommandStateInjectedUnconfirmed, CommandStateDeliveryUnknown:
		attemptAt := created.Add(2 * time.Minute)
		command.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: created.Add(time.Second)}
		command.Retry = &CommandRetry{
			AttemptCount:               1,
			LastAttemptAt:              attemptAt,
			ClaimID:                    "claim-" + id,
			OperationID:                id,
			AttemptID:                  "attempt-1",
			BoundLaunchIdentity:        command.Binding.LaunchIdentity,
			AuthorizationDecisionID:    "claim-decision-1",
			AuthorizationPolicyVersion: "policy-v1",
		}
		command.Terminal = &CommandTerminal{
			At:                         attemptAt,
			ClaimID:                    command.Retry.ClaimID,
			OperationID:                command.Retry.OperationID,
			AttemptID:                  command.Retry.AttemptID,
			BoundLaunchIdentity:        command.Retry.BoundLaunchIdentity,
			AuthorizationDecisionID:    command.Retry.AuthorizationDecisionID,
			AuthorizationPolicyVersion: command.Retry.AuthorizationPolicyVersion,
			ProviderStage:              ProviderStageAccepted,
			Completion:                 CompletionStateCompleted,
		}
		switch state {
		case CommandStateDelivered:
			command.Terminal.ActionResult = CommandActionResultDelivered
		case CommandStateInjectedUnconfirmed:
			command.Terminal.ActionResult = CommandActionResultInjectedUnconfirmed
		case CommandStateDeliveryUnknown:
			command.Terminal.ActionResult = CommandActionResultDeliveryUnknown
			command.Terminal.ErrorClass = CommandErrorClassProviderAmbiguous
			command.Terminal.Detail = "provider delivery outcome is unknown"
			command.Terminal.ProviderStage = ProviderStageMayHaveEntered
			command.Terminal.Completion = CompletionStateUnknown
		}
	case CommandStateExpired, CommandStateSuperseded, CommandStateDeadLettered:
		command.Terminal = &CommandTerminal{
			At:            created.Add(2 * time.Minute),
			ProviderStage: ProviderStageNotEntered,
			Completion:    CompletionStateNotCompleted,
		}
		switch state {
		case CommandStateExpired:
			command.Terminal.At = command.ExpiresAt
			command.Terminal.ActionResult = CommandActionResultExpired
			command.Terminal.ErrorClass = CommandErrorClassExpired
			command.Terminal.Detail = "command delivery window expired"
		case CommandStateSuperseded:
			command.Terminal.ActionResult = CommandActionResultSuperseded
			command.Terminal.ErrorClass = CommandErrorClassSuperseded
			command.Terminal.Detail = "target intent was superseded"
		case CommandStateDeadLettered:
			command.Terminal.ActionResult = CommandActionResultDeadLettered
			command.Terminal.ErrorClass = CommandErrorClassInvalidCommand
			command.Terminal.Detail = "command failed strict validation"
		}
	}
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	return command
}

func indexTestTransitionCommand(current Command, revision uint64, state CommandState) Command {
	if current.State == state {
		updated := cloneIndexTestCommand(current)
		updated.Order.Revision = revision
		return updated
	}

	switch {
	case current.State == CommandStatePending && state == CommandStateInFlight:
		updated := cloneIndexTestCommand(current)
		updated.State = state
		updated.Order.Revision = revision
		attemptCount := uint32(1)
		attemptAt := updated.CreatedAt.Add(2 * time.Second)
		if updated.Retry != nil {
			attemptCount = updated.Retry.AttemptCount + 1
			attemptAt = updated.Retry.LastAttemptAt.Add(time.Second)
			if updated.Retry.NextEligibleAt != nil && attemptAt.Before(*updated.Retry.NextEligibleAt) {
				attemptAt = *updated.Retry.NextEligibleAt
			}
		}
		if updated.Binding == nil {
			updated.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: attemptAt.Add(-time.Second)}
		}
		attemptID := fmt.Sprintf("attempt-%d", attemptCount)
		claimID := fmt.Sprintf("claim-%s-%d", updated.ID, attemptCount)
		decisionID := fmt.Sprintf("claim-decision-%d", attemptCount)
		updated.Retry = &CommandRetry{
			AttemptCount:               attemptCount,
			LastAttemptAt:              attemptAt,
			ClaimID:                    claimID,
			OperationID:                updated.ID,
			AttemptID:                  attemptID,
			BoundLaunchIdentity:        updated.Binding.LaunchIdentity,
			AuthorizationDecisionID:    decisionID,
			AuthorizationPolicyVersion: "policy-v1",
		}
		updated.Claim = &CommandClaim{
			ID:                         claimID,
			OwnerID:                    "controller-1",
			OperationID:                updated.ID,
			AttemptID:                  attemptID,
			BoundLaunchIdentity:        updated.Binding.LaunchIdentity,
			AuthorizationDecisionID:    decisionID,
			AuthorizationPolicyVersion: "policy-v1",
			ClaimedAt:                  attemptAt,
			LeaseUntil:                 attemptAt.Add(time.Minute),
		}
		updated.Terminal = nil
		return updated

	case current.State == CommandStateInFlight && state == CommandStatePending:
		updated := cloneIndexTestCommand(current)
		updated.State = state
		updated.Order.Revision = revision
		updated.Claim = nil
		nextEligibleAt := updated.Retry.LastAttemptAt.Add(time.Second)
		updated.Retry.NextEligibleAt = &nextEligibleAt
		updated.Retry.ErrorClass = CommandErrorClassProviderBusy
		updated.Retry.ErrorDetail = "provider asked the controller to retry"
		return updated

	case (current.State == CommandStateInFlight || current.Retry != nil) && commandIsTerminalState(state):
		updated := cloneIndexTestCommand(current)
		updated.State = state
		updated.Order.Revision = revision
		updated.Claim = nil
		updated.Retry.NextEligibleAt = nil
		terminalAt := updated.Retry.LastAttemptAt.Add(time.Second)
		updated.Terminal = &CommandTerminal{
			At:                         terminalAt,
			ClaimID:                    updated.Retry.ClaimID,
			OperationID:                updated.Retry.OperationID,
			AttemptID:                  updated.Retry.AttemptID,
			BoundLaunchIdentity:        updated.Retry.BoundLaunchIdentity,
			AuthorizationDecisionID:    updated.Retry.AuthorizationDecisionID,
			AuthorizationPolicyVersion: updated.Retry.AuthorizationPolicyVersion,
		}
		switch state {
		case CommandStateDelivered:
			updated.Terminal.ActionResult = CommandActionResultDelivered
			updated.Terminal.ProviderStage = ProviderStageAccepted
			updated.Terminal.Completion = CompletionStateCompleted
		case CommandStateInjectedUnconfirmed:
			updated.Terminal.ActionResult = CommandActionResultInjectedUnconfirmed
			updated.Terminal.ProviderStage = ProviderStageAccepted
			updated.Terminal.Completion = CompletionStateCompleted
		case CommandStateDeliveryUnknown:
			updated.Terminal.ActionResult = CommandActionResultDeliveryUnknown
			updated.Terminal.ErrorClass = CommandErrorClassProviderAmbiguous
			updated.Terminal.Detail = "provider delivery outcome is unknown"
			updated.Terminal.ProviderStage = ProviderStageMayHaveEntered
			updated.Terminal.Completion = CompletionStateUnknown
		case CommandStateExpired:
			updated.Terminal.At = updated.ExpiresAt
			updated.Terminal.ActionResult = CommandActionResultExpired
			updated.Terminal.ErrorClass = CommandErrorClassExpired
			updated.Terminal.Detail = "command delivery window expired"
			updated.Terminal.ProviderStage = ProviderStageNotEntered
			updated.Terminal.Completion = CompletionStateNotCompleted
		case CommandStateSuperseded:
			updated.Terminal.ActionResult = CommandActionResultSuperseded
			updated.Terminal.ErrorClass = CommandErrorClassSuperseded
			updated.Terminal.Detail = "target intent was superseded"
			updated.Terminal.ProviderStage = ProviderStageNotEntered
			updated.Terminal.Completion = CompletionStateNotCompleted
		case CommandStateDeadLettered:
			updated.Retry.ErrorClass = CommandErrorClassProviderBusy
			updated.Retry.ErrorDetail = "provider rejected the bounded retry"
			updated.Terminal.ActionResult = CommandActionResultRetryExhausted
			updated.Terminal.ErrorClass = CommandErrorClassRetryExhausted
			updated.Terminal.Detail = "bounded provider retry budget exhausted"
			updated.Terminal.ProviderStage = ProviderStageRejected
			updated.Terminal.Completion = CompletionStateNotCompleted
		}
		return updated
	}

	// Construct a locally valid target state for forbidden-transition tests.
	// The immutable request envelope is deterministic for one id/sequence.
	return indexTestCommand(current.ID, current.Target.SessionID, current.Order.Sequence, revision, state)
}

func indexTestStoreBinding() CommandStoreBinding {
	return CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 1}
}

func indexMutateCommand(command Command, mutate func(*Command)) Command {
	command = cloneIndexTestCommand(command)
	mutate(&command)
	if command.Target.SessionID != "" {
		command.TrustedIngress.TargetSessionID = command.Target.SessionID
	}
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	return command
}

func cloneIndexTestCommand(command Command) Command {
	if command.Binding != nil {
		binding := *command.Binding
		command.Binding = &binding
	}
	if command.Retry != nil {
		retry := *command.Retry
		if retry.NextEligibleAt != nil {
			nextEligibleAt := *retry.NextEligibleAt
			retry.NextEligibleAt = &nextEligibleAt
		}
		command.Retry = &retry
	}
	if command.Claim != nil {
		claim := *command.Claim
		command.Claim = &claim
	}
	if command.Terminal != nil {
		terminal := *command.Terminal
		command.Terminal = &terminal
	}
	if command.Reference != nil {
		reference := *command.Reference
		command.Reference = &reference
	}
	return command
}

func indexTestSnapshot(commands []Command, revision uint64) CommandIndexSnapshot {
	var highWater uint64
	for _, command := range commands {
		if command.Order.Sequence > highWater {
			highWater = command.Order.Sequence
		}
	}
	return CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           indexTestKnownEntries(commands),
		Revision:          revision,
		SequenceHighWater: highWater,
	}
}

func indexTestKnownEntries(commands []Command) []CommandIndexEntry {
	entries := make([]CommandIndexEntry, len(commands))
	for i := range commands {
		command := cloneIndexTestCommand(commands[i])
		entries[i] = CommandIndexEntry{Command: &command}
	}
	return entries
}

func indexTestEmptySnapshot() CommandIndexSnapshot {
	return CommandIndexSnapshot{Store: indexTestStoreBinding()}
}

func indexTestCommandMutation(revision uint64, command *Command) CommandIndexMutation {
	var entry *CommandIndexEntry
	if command != nil {
		owned := knownIndexTestEntry(*command)
		entry = &owned
	}
	return CommandIndexMutation{
		Store:    indexTestStoreBinding(),
		Revision: revision,
		Entry:    entry,
	}
}

func indexTestTombstone(command Command, revision uint64) CommandIndexTombstone {
	return CommandIndexTombstone{
		CommandID:        command.ID,
		Store:            command.Store,
		Revision:         revision,
		PriorVersion:     command.Version,
		PriorRevision:    command.Order.Revision,
		PriorState:       command.State,
		TargetSessionID:  command.Target.SessionID,
		IntentGeneration: command.Target.IntentGeneration,
		Sequence:         command.Order.Sequence,
	}
}

func indexTestTombstoneMutation(revision uint64, command Command) CommandIndexMutation {
	tombstone := indexTestTombstone(command, revision)
	return CommandIndexMutation{
		Store:     indexTestStoreBinding(),
		Revision:  revision,
		Tombstone: &tombstone,
	}
}

func indexCommandIDs(entries []CommandIndexEntry) []string {
	return indexEntryIDs(entries)
}

func indexTestResolve(t *testing.T, index *CommandIndex, commandID string) (Command, bool) {
	t.Helper()
	resolution, err := index.Resolve(commandID)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", commandID, err)
	}
	if !resolution.Found {
		return Command{}, false
	}
	if resolution.Entry.Command == nil {
		t.Fatalf("Resolve(%q) returned opaque entry where known command was required", commandID)
	}
	return *resolution.Entry.Command, true
}
