package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
)

// ErrCommandRepositoryPartition reports that a read could not prove which
// trusted city partition owns a command before exposing it to an index.
var ErrCommandRepositoryPartition = errors.New("durable nudge command city partition is unverified")

// TrustedCityPartition is an opaque capability produced only by trusted
// ingress authority. Its identity has no exported field or constructor, so a
// command's caller-authored city scope cannot create one.
type TrustedCityPartition struct {
	identity [sha256.Size]byte
}

func (p TrustedCityPartition) valid() bool {
	return p.identity != [sha256.Size]byte{}
}

// TrustedCityPartitionResolver revalidates one untrusted ingress reference
// against independent authority and returns its opaque city partition. The
// reference, including CityScope, is lookup input only and is never authority.
type TrustedCityPartitionResolver interface {
	ResolveCommandPartition(context.Context, TrustedIngressReference) (TrustedCityPartition, error)
}

// CommandRepositoryPartitionError identifies the exact read boundary that
// could not prove trusted city ownership. It never copies command content.
type CommandRepositoryPartitionError struct {
	Operation string
	CommandID string
	Err       error
}

// Error describes the fail-closed partition refusal.
func (e *CommandRepositoryPartitionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.CommandID == "" {
		return fmt.Sprintf("%s during %s: %v", ErrCommandRepositoryPartition, e.Operation, e.Err)
	}
	return fmt.Sprintf("%s during %s for command %q: %v", ErrCommandRepositoryPartition, e.Operation, e.CommandID, e.Err)
}

// Unwrap preserves the partition classification and resolver cause.
func (e *CommandRepositoryPartitionError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrCommandRepositoryPartition}
	}
	return []error{ErrCommandRepositoryPartition, e.Err}
}

// CommandPartitionReader is the only read surface that may feed a city-local
// command index. It contains no repository writer or provider capability.
type CommandPartitionReader struct {
	repository commandRepositoryReader
	partition  TrustedCityPartition
	resolver   TrustedCityPartitionResolver
}

// NewCommandPartitionReader binds repository reads to one opaque trusted city
// capability and an independent authority resolver. Missing or typed-nil
// authority fails closed.
func NewCommandPartitionReader(repository *CommandRepository, partition TrustedCityPartition, resolver TrustedCityPartitionResolver) (*CommandPartitionReader, error) {
	if repository == nil {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("command repository is required")}
	}
	if !partition.valid() {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted city partition capability is required")}
	}
	if isNilRepositoryDependency(resolver) {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted city partition resolver is required")}
	}
	return &CommandPartitionReader{repository: repository.reader, partition: partition, resolver: resolver}, nil
}

// Snapshot returns only commands independently resolved to this reader's
// trusted city. Foreign commands contribute content-free partition gaps so the
// global repository watermark remains dense without entering the local index.
func (r *CommandPartitionReader) Snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot", Err: errors.New("partition reader is not fully bound")}
	}
	snapshot, err := r.repository.snapshot(ctx, maxCommands)
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	if len(snapshot.Tombstones) != 0 {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot", Err: errors.New("tombstone has no trusted partition authority")}
	}
	filtered := CommandIndexSnapshot{
		Store:             snapshot.Store,
		Entries:           make([]CommandIndexEntry, 0, len(snapshot.Entries)),
		PartitionGaps:     make([]CommandIndexPartitionGap, 0, len(snapshot.Entries)),
		Coverage:          cloneCommandIndexCoverage(snapshot.Coverage),
		Revision:          snapshot.Revision,
		SequenceHighWater: snapshot.SequenceHighWater,
	}
	for _, entry := range snapshot.Entries {
		partition, routing, err := r.resolveEntryPartition(ctx, "snapshot", entry)
		if err != nil {
			return CommandIndexSnapshot{}, err
		}
		if partition == r.partition {
			filtered.Entries = append(filtered.Entries, entry)
		} else {
			filtered.PartitionGaps = append(filtered.PartitionGaps, CommandIndexPartitionGap{Sequence: routing.Sequence})
		}
	}
	sort.Slice(filtered.Entries, func(i, j int) bool {
		return commandIndexEntryRouting(filtered.Entries[i]).Sequence < commandIndexEntryRouting(filtered.Entries[j]).Sequence
	})
	sort.Slice(filtered.PartitionGaps, func(i, j int) bool {
		return filtered.PartitionGaps[i].Sequence < filtered.PartitionGaps[j].Sequence
	})
	if _, err := BuildCommandIndex(filtered); err != nil {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot validation", Err: err}
	}
	return filtered, nil
}

// Get returns an exact command only when independent authority resolves it to
// this reader's partition. A valid foreign command is indistinguishable from a
// missing ID at this city-local boundary.
func (r *CommandPartitionReader) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read", CommandID: commandID, Err: errors.New("partition reader is not fully bound")}
	}
	resolution, err := r.repository.get(ctx, commandID)
	if err != nil || !resolution.Found {
		return resolution, err
	}
	partition, _, err := r.resolveEntryPartition(ctx, "exact read", resolution.Entry)
	if err != nil {
		return CommandIndexResolution{}, err
	}
	if partition != r.partition {
		resolution.Entry = CommandIndexEntry{}
		resolution.Found = false
	}
	return resolution, nil
}

func (r *CommandPartitionReader) resolveEntryPartition(ctx context.Context, operation string, entry CommandIndexEntry) (TrustedCityPartition, CommandRoutingHeader, error) {
	routing := commandIndexEntryRouting(entry)
	if entry.Command == nil {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       errors.New("command version does not expose a supported trusted-ingress reference"),
		}
	}
	partition, err := r.resolver.ResolveCommandPartition(ctx, entry.Command.TrustedIngress)
	if err != nil {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{Operation: operation, CommandID: routing.CommandID, Err: err}
	}
	if !partition.valid() {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       errors.New("resolver returned no trusted city partition"),
		}
	}
	return partition, routing, nil
}
