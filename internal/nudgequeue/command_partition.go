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

// CommandPartitionCoverageRequest binds an authority proof to one exact
// transaction-consistent repository snapshot. TerminalRanges is the canonical
// compacted terminal sequence set visible in that snapshot.
type CommandPartitionCoverageRequest struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	SequenceHighWater  uint64
	TerminalRanges     []CommandIndexSequenceRange
	TerminalCount      uint64
	Partition          TrustedCityPartition
}

// CommandPartitionCoverageEntry is one authority-admitted command that was
// active in the requested partition at the requested repository revision.
type CommandPartitionCoverageEntry struct {
	CommandID string
	Sequence  uint64
}

// CommandPartitionCoverage is the independently retained, complete active set
// for one partition. Every snapshot identity field must exactly echo the
// request; ActiveEntries must be complete and ordered by sequence.
type CommandPartitionCoverage struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	SequenceHighWater  uint64
	AdmittedCount      uint64
	TerminalRanges     []CommandIndexSequenceRange
	TerminalCount      uint64
	Partition          TrustedCityPartition
	ActiveEntries      []CommandPartitionCoverageEntry
}

// CommandPartitionMembershipRequest binds one exact command lookup to the
// transaction-consistent repository revision returned by that lookup.
type CommandPartitionMembershipRequest struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Partition          TrustedCityPartition
}

// CommandPartitionMembership reports whether authority admitted the exact
// command to this partition and whether it was active at the requested
// historical revision.
type CommandPartitionMembership struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Partition          TrustedCityPartition
	Found              bool
	Active             bool
	Sequence           uint64
}

// TrustedCommandPartitionCoverageResolver proves that an indexed partition
// query returned every authority-admitted active command. Implementations must
// retain historical membership so a concurrent newer publication cannot make
// an older repository snapshot unverifiable.
type TrustedCommandPartitionCoverageResolver interface {
	ResolveCommandPartitionCoverage(context.Context, CommandPartitionCoverageRequest) (CommandPartitionCoverage, error)
	ResolveCommandPartitionMembership(context.Context, CommandPartitionMembershipRequest) (CommandPartitionMembership, error)
}

// CommandPartitionAdmission is the authority-side membership fact published
// after a durable create. RepositoryRevision is the command's admission
// revision, not a later claim or completion revision.
type CommandPartitionAdmission struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Sequence           uint64
	Partition          TrustedCityPartition
}

// CommandPartitionTerminal is the authority-side membership fact published
// after a durable terminal transition. Replays of the exact fact are
// idempotent; conflicting facts must fail closed.
type CommandPartitionTerminal struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Sequence           uint64
	Partition          TrustedCityPartition
}

// TrustedCommandPartitionMembershipRecorder owns the independent membership
// history used by TrustedCommandPartitionCoverageResolver.
type TrustedCommandPartitionMembershipRecorder interface {
	RecordCommandPartitionAdmission(context.Context, CommandPartitionAdmission) error
	RecordCommandPartitionTerminal(context.Context, CommandPartitionTerminal) error
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
	coverage   TrustedCommandPartitionCoverageResolver
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
	coverage, ok := resolver.(TrustedCommandPartitionCoverageResolver)
	if !ok || isNilRepositoryDependency(coverage) {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted partition coverage resolver is required")}
	}
	return &CommandPartitionReader{repository: repository.reader, partition: partition, resolver: resolver, coverage: coverage}, nil
}

// Snapshot returns only commands independently resolved to this reader's
// trusted city. The exact partition route is pushed into the backing indexed
// query; foreign commands are represented by content-free sequence ranges so
// the global watermark remains dense without materializing foreign rows.
func (r *CommandPartitionReader) Snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) || isNilRepositoryDependency(r.coverage) {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot", Err: errors.New("partition reader is not fully bound")}
	}
	partitionRoute := commandPartitionRoute(r.partition)
	snapshot, err := r.repository.snapshotPartition(ctx, maxCommands, partitionRoute, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) error {
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
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	if err := r.verifySnapshotCoverage(ctx, snapshot, maxCommands); err != nil {
		return CommandIndexSnapshot{}, err
	}
	return snapshot, nil
}

func (r *CommandPartitionReader) verifySnapshotCoverage(ctx context.Context, snapshot CommandIndexSnapshot, maxCommands int) error {
	request := commandPartitionCoverageRequest(snapshot, r.partition)
	coverage, err := r.coverage.ResolveCommandPartitionCoverage(ctx, request)
	if err != nil {
		return &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	if err := validateCommandPartitionCoverage(request, coverage, maxCommands); err != nil {
		return &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	actual := make([]CommandPartitionCoverageEntry, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		routing := commandIndexEntryRouting(entry)
		actual = append(actual, CommandPartitionCoverageEntry{CommandID: routing.CommandID, Sequence: routing.Sequence})
	}
	if !commandPartitionCoverageEntriesEqual(actual, coverage.ActiveEntries) {
		return &CommandRepositoryPartitionError{
			Operation: "snapshot coverage",
			Err:       fmt.Errorf("indexed active set differs from trusted coverage: returned %d commands, authority requires %d", len(actual), len(coverage.ActiveEntries)),
		}
	}
	return nil
}

func commandPartitionCoverageEntriesEqual(left, right []CommandPartitionCoverageEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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

func commandPartitionCoverageRequest(snapshot CommandIndexSnapshot, partition TrustedCityPartition) CommandPartitionCoverageRequest {
	request := CommandPartitionCoverageRequest{
		Store:              snapshot.Store,
		RepositoryRevision: snapshot.Revision,
		SequenceHighWater:  snapshot.SequenceHighWater,
		Partition:          partition,
	}
	if snapshot.Coverage != nil {
		request.TerminalRanges = append([]CommandIndexSequenceRange(nil), snapshot.Coverage.Ranges...)
		request.TerminalCount = snapshot.Coverage.TerminalCount
	}
	return request
}

func validateCommandPartitionCoverage(request CommandPartitionCoverageRequest, coverage CommandPartitionCoverage, maxCommands int) error {
	if coverage.Store != request.Store || coverage.RepositoryRevision != request.RepositoryRevision ||
		coverage.SequenceHighWater != request.SequenceHighWater || coverage.AdmittedCount != request.SequenceHighWater ||
		coverage.TerminalCount != request.TerminalCount ||
		coverage.Partition != request.Partition || !commandPartitionRangesEqual(coverage.TerminalRanges, request.TerminalRanges) {
		return errors.New("trusted coverage is not bound to the exact repository snapshot")
	}
	if len(coverage.ActiveEntries) > maxCommands {
		return fmt.Errorf("trusted partition contains more than %d active commands: %w", maxCommands, ErrCommandRepositorySnapshotLimit)
	}
	var previous uint64
	terminalRangeIndex := 0
	seenIDs := make(map[string]struct{}, len(coverage.ActiveEntries))
	for index, entry := range coverage.ActiveEntries {
		if validateCommandIdentity("trusted coverage command id", entry.CommandID) != nil || !strings.HasPrefix(entry.CommandID, commandIDPrefix) ||
			entry.Sequence == 0 || entry.Sequence > request.SequenceHighWater {
			return fmt.Errorf("trusted coverage entry %d is invalid", index)
		}
		if index > 0 && entry.Sequence <= previous {
			return errors.New("trusted coverage active entries are not strictly ordered by sequence")
		}
		for terminalRangeIndex < len(request.TerminalRanges) && request.TerminalRanges[terminalRangeIndex].LastSequence < entry.Sequence {
			terminalRangeIndex++
		}
		if terminalRangeIndex < len(request.TerminalRanges) && request.TerminalRanges[terminalRangeIndex].FirstSequence <= entry.Sequence {
			return fmt.Errorf("trusted coverage active sequence %d is terminal", entry.Sequence)
		}
		if _, duplicate := seenIDs[entry.CommandID]; duplicate {
			return fmt.Errorf("trusted coverage repeats command %q", entry.CommandID)
		}
		seenIDs[entry.CommandID] = struct{}{}
		previous = entry.Sequence
	}
	return nil
}

func commandPartitionRangesEqual(left, right []CommandIndexSequenceRange) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Get returns an exact command only when independent authority resolves it to
// this reader's partition. A valid foreign command is indistinguishable from a
// missing ID at this city-local boundary.
func (r *CommandPartitionReader) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read", CommandID: commandID, Err: errors.New("partition reader is not fully bound")}
	}
	resolution, err := r.repository.getPartition(ctx, commandID, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) (bool, error) {
		partition, err := r.resolveProjectedPartition(ctx, "exact read", row, entry)
		if err != nil {
			return false, err
		}
		return partition == r.partition, nil
	})
	if err != nil {
		return CommandIndexResolution{}, err
	}
	trusted, err := r.coverage.ResolveCommandPartitionMembership(ctx, CommandPartitionMembershipRequest{
		Store: resolution.Store, RepositoryRevision: resolution.Revision,
		CommandID: commandID, Partition: r.partition,
	})
	if err != nil {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read membership", CommandID: commandID, Err: err}
	}
	if err := validateCommandPartitionMembership(resolution, commandID, r.partition, trusted); err != nil {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read membership", CommandID: commandID, Err: err}
	}
	return resolution, nil
}

func validateCommandPartitionMembership(resolution CommandIndexResolution, commandID string, partition TrustedCityPartition, trusted CommandPartitionMembership) error {
	if trusted.Store != resolution.Store || trusted.RepositoryRevision != resolution.Revision ||
		trusted.CommandID != commandID || trusted.Partition != partition {
		return errors.New("trusted membership is not bound to the exact command read")
	}
	if !trusted.Found {
		if trusted.Active || trusted.Sequence != 0 || resolution.Found {
			return errors.New("repository command exists without trusted partition membership")
		}
		return nil
	}
	if !resolution.Found || trusted.Sequence == 0 {
		return errors.New("authority-owned command is absent from the repository read")
	}
	routing := commandIndexEntryRouting(resolution.Entry)
	if routing.CommandID != commandID || routing.Sequence != trusted.Sequence {
		return errors.New("repository command identity or sequence differs from trusted membership")
	}
	if resolution.Entry.Command == nil {
		return errors.New("authority-owned command has no supported lifecycle state")
	}
	active := !commandIsTerminalState(resolution.Entry.Command.State)
	if active != trusted.Active {
		return errors.New("repository command lifecycle differs from trusted membership")
	}
	return nil
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
