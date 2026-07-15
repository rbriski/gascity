package nudgequeue

import (
	"errors"
	"testing"
	"time"
)

func TestCommandIndexDeepCopiesDurableLaunchBinding(t *testing.T) {
	command := indexTestCommand("command-1", "session-a", 1, 1, CommandStateInFlight)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{command}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	command.Binding.LaunchIdentity = "caller-mutated-input"
	first, found := indexTestResolve(t, index, command.ID)
	if !found {
		t.Fatal("Resolve missed command")
	}
	if first.Binding == nil || first.Binding.LaunchIdentity == command.Binding.LaunchIdentity {
		t.Fatalf("index aliases input binding: resolved=%#v input=%#v", first.Binding, command.Binding)
	}

	first.Binding.LaunchIdentity = "caller-mutated-output"
	second, found := indexTestResolve(t, index, command.ID)
	if !found {
		t.Fatal("second Resolve missed command")
	}
	if second.Binding == nil || second.Binding.LaunchIdentity == first.Binding.LaunchIdentity {
		t.Fatalf("Resolve aliases index binding: first=%#v second=%#v", first.Binding, second.Binding)
	}
}

func TestCommandIndexRejectsSameAttemptBindingAndAuthorizationRewrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{
			name: "retarget durable binding",
			mutate: func(command *Command) {
				command.Binding.LaunchIdentity = "replacement-launch"
				command.Retry.BoundLaunchIdentity = "replacement-launch"
			},
		},
		{
			name: "rewrite authorization decision",
			mutate: func(command *Command) {
				command.Retry.AuthorizationDecisionID = "replacement-decision"
			},
		},
		{
			name: "rewrite attempt identity",
			mutate: func(command *Command) {
				command.Retry.AttemptID = "replacement-attempt"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pending := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{pending}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			inFlight := indexTestCommand("command-1", "session-a", 1, 2, CommandStateInFlight)
			if err := index.Apply(indexTestCommandMutation(2, &inFlight)); err != nil {
				t.Fatalf("Apply in-flight: %v", err)
			}
			retryPending := indexMutateCommand(inFlight, func(command *Command) {
				command.State = CommandStatePending
				command.Order.Revision = 3
				command.Claim = nil
				nextEligible := command.Retry.LastAttemptAt.Add(1)
				command.Retry.NextEligibleAt = &nextEligible
				command.Retry.ErrorClass = CommandErrorClassProviderBusy
				command.Retry.ErrorDetail = "provider asked the controller to retry"
				tc.mutate(command)
			})
			if err := index.Apply(indexTestCommandMutation(3, &retryPending)); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply rewritten evidence error = %v, want ErrCommandIndexUnsynced", err)
			}
		})
	}
}

func TestCommandIndexRejectsTerminalAttemptEvidenceWithoutPriorClaimState(t *testing.T) {
	pending := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{pending}, 1))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	inFlight := indexTestCommand("command-1", "session-a", 1, 1, CommandStateInFlight)
	inventedTerminal := indexTestTransitionCommand(inFlight, 2, CommandStateExpired)
	if err := index.Apply(indexTestCommandMutation(2, &inventedTerminal)); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply invented terminal attempt error = %v, want ErrCommandIndexUnsynced", err)
	}
}

func TestCommandIndexRejectsActiveClaimOwnerAndLeaseRewrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "owner", mutate: func(command *Command) { command.Claim.OwnerID = "replacement-owner" }},
		{name: "claim time", mutate: func(command *Command) {
			command.Claim.ClaimedAt = command.Claim.ClaimedAt.Add(time.Nanosecond)
			command.Retry.LastAttemptAt = command.Claim.ClaimedAt
		}},
		{name: "lease rewind", mutate: func(command *Command) { command.Claim.LeaseUntil = command.Claim.LeaseUntil.Add(-time.Nanosecond) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inFlight := indexTestCommand("command-1", "session-a", 1, 1, CommandStateInFlight)
			index, err := BuildCommandIndex(indexTestSnapshot([]Command{inFlight}, 1))
			if err != nil {
				t.Fatalf("BuildCommandIndex: %v", err)
			}
			rewritten := indexMutateCommand(inFlight, func(command *Command) {
				command.Order.Revision = 2
				tc.mutate(command)
			})
			if err := index.Apply(indexTestCommandMutation(2, &rewritten)); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply rewritten claim error = %v, want ErrCommandIndexUnsynced", err)
			}
		})
	}
}
