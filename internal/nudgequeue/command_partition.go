package nudgequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	commandPartitionSchemaVersion = "1"
	commandPartitionRoutePrefix   = "gc:control-partition:v1:"
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

func commandPartitionRoute(partition TrustedCityPartition) string {
	if !partition.valid() {
		return ""
	}
	return commandPartitionRoutePrefix + hex.EncodeToString(partition.identity[:])
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
// trusted city. The exact partition route is pushed into the backing indexed
// query; foreign commands are represented by content-free sequence ranges so
// the global watermark remains dense without materializing foreign rows.
func (r *CommandPartitionReader) Snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot", Err: errors.New("partition reader is not fully bound")}
	}
	partitionRoute := commandPartitionRoute(r.partition)
	return r.repository.snapshotPartition(ctx, maxCommands, partitionRoute, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) error {
		partition, err := r.resolveProjectedPartition(ctx, "snapshot", row, entry)
		if err != nil {
			return err
		}
		if partition != r.partition {
			return &CommandRepositoryPartitionError{
				Operation: "snapshot",
				CommandID: row.ID,
				Err:       errors.New("indexed partition projection resolves to a different trusted city"),
			}
		}
		return nil
	})
}

func (r *CommandPartitionReader) resolveProjectedPartition(ctx context.Context, operation string, row beads.Bead, entry CommandIndexEntry) (TrustedCityPartition, error) {
	projectedRoute, err := commandPartitionProjection(row)
	if err != nil {
		return TrustedCityPartition{}, &CommandRepositoryPartitionError{Operation: operation, CommandID: row.ID, Err: err}
	}
	partition, routing, err := r.resolveEntryPartition(ctx, operation, entry)
	if err != nil {
		return TrustedCityPartition{}, err
	}
	wantRoute := commandPartitionRoute(partition)
	if projectedRoute != wantRoute {
		return TrustedCityPartition{}, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       fmt.Errorf("stored partition projection %q differs from trusted authority", projectedRoute),
		}
	}
	return partition, nil
}

func commandPartitionProjection(row beads.Bead) (string, error) {
	route := row.Assignee
	if route == "" || row.Metadata[commandRecordPartitionKeyMetadataKey] != route {
		return "", errors.New("command partition projection is missing or inconsistent")
	}
	if row.Metadata[commandRecordPartitionSchemaMetadataKey] != commandPartitionSchemaVersion {
		return "", fmt.Errorf("command partition projection schema %q is unsupported", row.Metadata[commandRecordPartitionSchemaMetadataKey])
	}
	hexIdentity := strings.TrimPrefix(route, commandPartitionRoutePrefix)
	decoded, err := hex.DecodeString(hexIdentity)
	if !strings.HasPrefix(route, commandPartitionRoutePrefix) || err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != hexIdentity {
		return "", fmt.Errorf("command partition projection %q is non-canonical", route)
	}
	return route, nil
}

func commandPartitionSequenceComplement(sequenceHighWater uint64, coverage *CommandIndexCompactedCoverage, entries []CommandIndexEntry) []CommandIndexPartitionGap {
	occupied := make([]commandIndexSequenceInterval, 0, len(commandIndexCoverageRanges(coverage))+len(entries))
	for _, sequenceRange := range commandIndexCoverageRanges(coverage) {
		occupied = append(occupied, commandIndexSequenceInterval{first: sequenceRange.FirstSequence, last: sequenceRange.LastSequence})
	}
	for _, entry := range entries {
		sequence := commandIndexEntryRouting(entry).Sequence
		occupied = append(occupied, commandIndexSequenceInterval{first: sequence, last: sequence})
	}
	sort.Slice(occupied, func(i, j int) bool {
		if occupied[i].first != occupied[j].first {
			return occupied[i].first < occupied[j].first
		}
		return occupied[i].last < occupied[j].last
	})

	next := uint64(1)
	gaps := make([]CommandIndexPartitionGap, 0, len(occupied)+1)
	for _, interval := range occupied {
		if interval.first > next {
			gaps = append(gaps, CommandIndexPartitionGap{FirstSequence: next, LastSequence: interval.first - 1})
		}
		if interval.last == ^uint64(0) {
			next = 0
			break
		}
		if interval.last >= next {
			next = interval.last + 1
		}
	}
	if next != 0 && next <= sequenceHighWater {
		gaps = append(gaps, CommandIndexPartitionGap{FirstSequence: next, LastSequence: sequenceHighWater})
	}
	return gaps
}

// Get returns an exact command only when independent authority resolves it to
// this reader's partition. A valid foreign command is indistinguishable from a
// missing ID at this city-local boundary.
func (r *CommandPartitionReader) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read", CommandID: commandID, Err: errors.New("partition reader is not fully bound")}
	}
	return r.repository.getPartition(ctx, commandID, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) (bool, error) {
		partition, err := r.resolveProjectedPartition(ctx, "exact read", row, entry)
		if err != nil {
			return false, err
		}
		return partition == r.partition, nil
	})
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
